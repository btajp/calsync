import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { emit, emitTo, listen } from "@tauri-apps/api/event";
import { WebviewWindow } from "@tauri-apps/api/webviewWindow";
import { open } from "@tauri-apps/plugin-shell";
import { ApiClient, ApiError } from "./api";
import { colorForAccount } from "./pages/CalendarView";
import EventDetail from "./components/EventDetail";
import { formatClock, scheduleFetchRange } from "./tray";
import { isHttpsUrl } from "./urlSafety";
import type { EventOut } from "./types";

export interface ScheduleItem {
  time: string; // "HH:MM" または終日イベントは "終日"
  title: string;
  accountId: string;
  // 詳細ビュー(EventDetail)へそのまま渡すための元イベントへの参照
  // (デスクトップ予定詳細設計 2026-07-24 §3.3)。
  event: EventOut;
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
export function buildScheduleList(events: EventOut[], now: Date): ScheduleDay[] {
  const days = new Map<string, { date: Date; items: SortableItem[] }>();

  for (const ev of events) {
    // もう終わった予定は載せない(時刻あり予定のみ判定。開催中は残す)。終日は
    // 取得窓が今日開始のため「昨日までで終わった終日」はそもそも返らず、当日分は
    // 日中ずっと有効とみなして残す。end が読めない場合は安全側で表示する。
    if (!ev.all_day && ev.end) {
      const endTime = Date.parse(ev.end);
      if (!Number.isNaN(endTime) && endTime <= now.getTime()) continue;
    }
    const title = ev.title || "(無題)";
    const date = ev.all_day ? parseLocalDateOnly(ev.all_day_start) : new Date(ev.start);
    const item: SortableItem = ev.all_day
      ? { time: "終日", title, accountId: ev.account_id, sortKey: -1, event: ev }
      : { time: formatClock(date), title, accountId: ev.account_id, sortKey: date.getTime(), event: ev };

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
        .map(({ time, title, accountId, event }) => ({ time, title, accountId, event })),
    }));
}

/**
 * ポップオーバーを開いたとき自動スクロールする先(「現在時刻の位置」)を決める純関数
 * (2026-07-28 実機フィードバック)。終了済みの時刻あり予定は buildScheduleList が
 * 除外済みなので、今日の最初の時刻あり項目が「開催中または次の予定」になる。
 * 今日に時刻あり項目が無い場合は null(リスト先頭のままにする)。
 */
export function scheduleAnchor(days: ScheduleDay[], now: Date): { dateKey: string; index: number } | null {
  const todayKey = localDateKey(now);
  const day = days.find((d) => d.dateKey === todayKey);
  if (!day) return null;
  const index = day.items.findIndex((it) => it.time !== "終日");
  if (index === -1) return null;
  return { dateKey: day.dateKey, index };
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
  // クリックされたスケジュール項目(詳細ビュー表示中は非 null。デスクトップ予定詳細設計
  // 2026-07-24 §3.3)。
  const [selectedItem, setSelectedItem] = useState<ScheduleItem | null>(null);
  // リスト行の「参加」ボタンで会議 URL を開けなかったときのエラー(2026-07-28
  // 実機フィードバック: 詳細を開かずに参加したい)。
  const [joinError, setJoinError] = useState<string | null>(null);

  // 詳細を開かずにリスト行から直接会議へ参加する。https 以外はボタン自体を出さない
  // (EventDetail.showJoinButton と同じ方針)ため、ここでの再チェックは防御的。
  const joinMeeting = (url: string) => {
    if (!isHttpsUrl(url)) return;
    setJoinError(null);
    open(url).catch((e) => setJoinError(e instanceof Error ? e.message : String(e)));
  };

  // 起動時に listen("api-info") の登録が完了してから emit("panel-ready") を発火する
  // (メインが emitTo("panel", "api-info", {port, token}) で応答する)。順序を逆にすると、
  // リスナー登録が完了するより先に emit が届いた場合に応答を取りこぼし、「接続中…」のまま
  // 固まる(レビュー Important 対応。emit/listen は共に非同期 IPC のため、JS の呼び出し順
  // だけでは到達順序を保証できない)。
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
      void emit("panel-ready");
    });
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
        setDays(buildScheduleList(res.events, new Date()));
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

  // パネルは blur → hide で使い回され React state が残るため、詳細ビューを開いたまま
  // 閉じる(「会議に参加」でブラウザへフォーカスが移る場合を含む)と、次にトレイから
  // 開いたときに古い詳細のスナップショットが表示されたままになる。blur(hide 直前)で
  // 必ずリストへ戻す(レビュー指摘)。
  useEffect(() => {
    const onBlur = () => setSelectedItem(null);
    window.addEventListener("blur", onBlur);
    return () => window.removeEventListener("blur", onBlur);
  }, []);

  // 現在時刻の位置(今日の開催中/次の予定)への自動スクロール。パネルは使い回される
  // ため前回のスクロール位置が残っており、表示のたび(setFocus → browser focus)に
  // 戻す。
  const anchor = useMemo(() => scheduleAnchor(days, new Date()), [days]);
  const anchorRef = useRef<HTMLDivElement | null>(null);
  const scrollToNow = useCallback(() => {
    anchorRef.current?.scrollIntoView({ block: "start" });
  }, []);
  useEffect(() => {
    window.addEventListener("focus", scrollToNow);
    return () => window.removeEventListener("focus", scrollToNow);
  }, [scrollToNow]);
  // リストが表示されるタイミング(初回データ到着・「← 戻る」で詳細から戻った直後 =
  // リスト DOM の作り直しでスクロール位置が先頭に戻る)にもスクロールする。依存を
  // anchor オブジェクトではなく値キーにすることで、3分ごとの定期更新(同じ位置)では
  // 再スクロールせず、閲覧中に位置が飛ばないようにする。
  const anchorKey = anchor ? `${anchor.dateKey}:${anchor.index}` : null;
  useEffect(() => {
    if (selectedItem !== null) return;
    scrollToNow();
  }, [selectedItem, anchorKey, scrollToNow]);

  // 色分けはアカウント定義順に依存する(CalendarView と同じ規則)。EventDetail の
  // colorOf props に渡すためのメモ化(orderedIds が変わったときだけ作り直す)。
  const colorOf = useCallback((accountId: string) => colorForAccount(accountId, orderedIds), [orderedIds]);

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
      {joinError && <p className="error">開けませんでした: {joinError}</p>}
      {selectedItem ? (
        <div className="panel-detail">
          <EventDetail
            event={selectedItem.event}
            colorOf={colorOf}
            onClose={() => setSelectedItem(null)}
            closeLabel="← 戻る"
          />
        </div>
      ) : !api ? (
        <p className="hint">接続中…</p>
      ) : days.length === 0 ? (
        <p className="hint">今後7日以内の予定はありません。</p>
      ) : (
        <div className="panel-list">
          {days.map((day) => (
            <div key={day.dateKey} className="panel-day">
              <h3>{day.dateLabel}</h3>
              {day.items.map((item, i) => (
                <div
                  key={i}
                  ref={anchor && day.dateKey === anchor.dateKey && i === anchor.index ? anchorRef : undefined}
                  className="panel-item"
                  onClick={() => setSelectedItem(item)}
                >
                  <span className="legend-chip" style={{ backgroundColor: colorOf(item.accountId) }} />
                  <span className="panel-item-time">{item.time}</span>
                  <span className="panel-item-title">{item.title}</span>
                  {isHttpsUrl(item.event.meeting_url) && (
                    <button
                      className="panel-item-join"
                      onClick={(e) => {
                        // 行クリック(詳細ビューへの遷移)と分離する
                        e.stopPropagation();
                        joinMeeting(item.event.meeting_url);
                      }}
                    >
                      参加
                    </button>
                  )}
                </div>
              ))}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
