import { useCallback, useEffect, useState } from "react";
import { emit, emitTo, listen } from "@tauri-apps/api/event";
import { WebviewWindow } from "@tauri-apps/api/webviewWindow";
import { ApiClient, ApiError } from "./api";
import { colorForAccount } from "./pages/CalendarView";
import { formatClock, scheduleFetchRange } from "./tray";
import type { EventOut } from "./types";

export interface ScheduleItem {
  time: string; // "HH:MM" または終日イベントは "終日"
  title: string;
  accountId: string;
}

export interface ScheduleDay {
  dateKey: string; // "YYYY-MM-DD"(React key 用)
  dateLabel: string; // 例: "7/23(木)"
  items: ScheduleItem[];
}

const WEEKDAY_JA = ["日", "月", "火", "水", "木", "金", "土"];

function localDateKey(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/**
 * ローカルの "YYYY-MM-DD" 文字列から Date を作る。`new Date("YYYY-MM-DD")` は UTC 深夜と
 * 解釈されるため TZ によっては前日にずれる(CalendarView 設計と同じ理由で年月日を分解して構築)。
 */
function parseLocalDateOnly(yyyyMmDd: string): Date {
  const [y, m, d] = yyyyMmDd.split("-").map(Number);
  return new Date(y, (m || 1) - 1, d || 1);
}

interface SortableItem extends ScheduleItem {
  sortKey: number;
}

/**
 * events を日付見出しごとにグループ化し、ポップオーバーのスケジュールリスト用に整形する
 * 純関数(デスクトップトレイ設計 2026-07-23 §3.2: 「日付見出し+時刻+色チップ+タイトル」)。
 * - 終日イベントは all_day_start の日付でグループ化し time は「終日」、各日の先頭に置く
 * - 時刻ありイベントは start のローカル日付でグループ化し time は「HH:MM」、開始時刻昇順
 * - イベントが 1 件も無い日付は見出し自体を出さない
 * - 日付は昇順、タイトルが空文字の予定は「(無題)」にする
 */
export function buildScheduleList(events: EventOut[]): ScheduleDay[] {
  const days = new Map<string, { date: Date; items: SortableItem[] }>();

  for (const ev of events) {
    const title = ev.title || "(無題)";
    const date = ev.all_day ? parseLocalDateOnly(ev.all_day_start) : new Date(ev.start);
    const item: SortableItem = ev.all_day
      ? { time: "終日", title, accountId: ev.account_id, sortKey: -1 }
      : { time: formatClock(date), title, accountId: ev.account_id, sortKey: date.getTime() };

    const key = localDateKey(date);
    let bucket = days.get(key);
    if (!bucket) {
      bucket = { date, items: [] };
      days.set(key, bucket);
    }
    bucket.items.push(item);
  }

  return Array.from(days.entries())
    .sort(([, a], [, b]) => a.date.getTime() - b.date.getTime())
    .map(([key, bucket]) => ({
      dateKey: key,
      dateLabel: `${bucket.date.getMonth() + 1}/${bucket.date.getDate()}(${WEEKDAY_JA[bucket.date.getDay()]})`,
      items: bucket.items
        .sort((a, b) => a.sortKey - b.sortKey)
        .map(({ time, title, accountId }) => ({ time, title, accountId })),
    }));
}

function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    return e.hint ? `${e.message}(${e.hint})` : e.message;
  }
  return String(e);
}

/**
 * トレイのポップオーバー用ミニアプリ(`?panel=1` で main.tsx から描画される。
 * デスクトップトレイ設計 2026-07-23 §3.2)。API 接続情報はメインウィンドウから
 * Tauri イベントで受け取る(localStorage には書かない)。
 */
export default function PanelApp() {
  const [api, setApi] = useState<ApiClient | null>(null);
  const [orderedIds, setOrderedIds] = useState<string[]>([]);
  const [days, setDays] = useState<ScheduleDay[]>([]);
  const [error, setError] = useState<string | null>(null);

  // 起動時に emit("panel-ready") → メインが emitTo("panel", "api-info", {port, token}) で応答する。
  useEffect(() => {
    let unlisten: (() => void) | undefined;
    let cancelled = false;
    listen<{ port: number; token: string }>("api-info", (event) => {
      setApi(new ApiClient(`http://127.0.0.1:${event.payload.port}`, event.payload.token));
    }).then((u) => {
      if (cancelled) {
        u();
        return;
      }
      unlisten = u;
    });
    void emit("panel-ready");
    return () => {
      cancelled = true;
      unlisten?.();
    };
  }, []);

  const loadEvents = useCallback(() => {
    if (!api) return;
    const { from, to } = scheduleFetchRange(new Date());
    api
      .events(from, to)
      .then((res) => {
        setError(null);
        setDays(buildScheduleList(res.events));
      })
      .catch((e) => setError(describeError(e)));
    // 色分けはアカウント定義順に依存する(CalendarView と同じ規則)。取得失敗はベストエフォート。
    api
      .getConfig()
      .then((c) => setOrderedIds((c.raw.accounts ?? []).map((a) => a.id).filter((v): v is string => !!v)))
      .catch(() => {
        /* 色は未知色にフォールバックするだけなので致命的ではない */
      });
  }, [api]);

  useEffect(() => {
    loadEvents();
  }, [loadEvents]);

  // 3分ごとの定期更新(デスクトップトレイ設計 2026-07-23 §3.2)。
  useEffect(() => {
    const id = setInterval(loadEvents, 3 * 60_000);
    return () => clearInterval(id);
  }, [loadEvents]);

  // 表示のたびに再取得する。ポップオーバーは show/hide で使い回されるため、非表示中に
  // 予定が変わっていても、次に表示され setFocus() されたタイミングの browser focus で
  // 最新化する。
  useEffect(() => {
    window.addEventListener("focus", loadEvents);
    return () => window.removeEventListener("focus", loadEvents);
  }, [loadEvents]);

  const openMain = async () => {
    const main = await WebviewWindow.getByLabel("main");
    await main?.show();
    await main?.setFocus();
  };

  // 終了はこのウィンドウから直接 exit() せず、メインウィンドウへ依頼する(App.tsx 参照)。
  // サイドカーの明示 kill はメイン側の状態(kill クロージャ)にしか無く、ウィンドウを跨いだ
  // JS モジュールスコープの共有は無いため、"kill してから exit" の順序を保証するには
  // メイン側で両方実行してもらう必要がある。
  const requestQuit = () => {
    void emitTo("main", "quit-app");
  };

  return (
    <div className="panel">
      <div className="panel-header">
        <button className="link-button" onClick={() => void openMain()}>
          アプリを開く
        </button>
        <button className="link-button" onClick={requestQuit}>
          終了
        </button>
      </div>
      {error && <p className="error">{error}</p>}
      {!api ? (
        <p className="hint">接続中…</p>
      ) : days.length === 0 ? (
        <p className="hint">今後7日以内の予定はありません。</p>
      ) : (
        <div className="panel-list">
          {days.map((day) => (
            <div key={day.dateKey} className="panel-day">
              <h3>{day.dateLabel}</h3>
              {day.items.map((item, i) => (
                <div key={i} className="panel-item">
                  <span
                    className="legend-chip"
                    style={{ backgroundColor: colorForAccount(item.accountId, orderedIds) }}
                  />
                  <span className="panel-item-time">{item.time}</span>
                  <span className="panel-item-title">{item.title}</span>
                </div>
              ))}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
