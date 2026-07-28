import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import FullCalendar from "@fullcalendar/react";
import dayGridPlugin from "@fullcalendar/daygrid";
import timeGridPlugin from "@fullcalendar/timegrid";
import listPlugin from "@fullcalendar/list";
import jaLocale from "@fullcalendar/core/locales/ja";
import type { DatesSetArg, EventClickArg, EventContentArg, EventInput } from "@fullcalendar/core";
import type { ApiClient } from "../api";
import { ApiError } from "../api";
import EventDetail from "../components/EventDetail";
import type { EventOut } from "../types";

// colorPalette はアカウント色の固定パレット。Slack ダイジェストの色割当
// (internal/notify/slack/slack.go の colorPalette)と同じ発想・同じ値を使う
// (凡例・通知の色が食い違わないようにする。デスクトップカレンダービュー設計
// 2026-07-21 §5)。
const COLOR_PALETTE = [
  "#4285F4",
  "#0F9D58",
  "#F4B400",
  "#DB4437",
  "#7B1FA2",
  "#00ACC1",
  "#FF7043",
  "#5C6BC0",
];
const UNKNOWN_ACCOUNT_COLOR = "#999999";

/**
 * アカウント ID から表示色を決める純関数。orderedIds(config の accounts 定義順)で
 * パレットを巡回し、orderedIds に含まれない未知のアカウントは
 * UNKNOWN_ACCOUNT_COLOR にする(internal/notify/slack.Client.colorFor と同じ規則)。
 */
export function colorForAccount(accountId: string, orderedIds: string[]): string {
  const i = orderedIds.indexOf(accountId);
  if (i === -1) return UNKNOWN_ACCOUNT_COLOR;
  return COLOR_PALETTE[i % COLOR_PALETTE.length];
}

/**
 * Date を「閲覧者のローカルオフセット付き RFC3339」文字列に変換する純関数。
 * Date.toISOString() は常に UTC("Z" 付き)を返すため使用禁止 —
 * GET /api/events は from/to が保持するオフセットの現地日付で終日イベントの日付境界を
 * 解釈するため、UTC を送ると JST 等の TZ では終日イベントの表示日が 1 日ずれる
 * (デスクトップカレンダービュー設計 2026-07-21 §4)。
 */
export function formatLocalRFC3339(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  const year = d.getFullYear();
  const month = pad(d.getMonth() + 1);
  const day = pad(d.getDate());
  const hour = pad(d.getHours());
  const minute = pad(d.getMinutes());
  const second = pad(d.getSeconds());
  // getTimezoneOffset() は「UTC − ローカル」を分単位で返す(JST なら -540)ため、
  // 符号を反転すると RFC3339 が要求する「ローカル − UTC」のオフセットになる。
  const offsetMinutes = -d.getTimezoneOffset();
  const sign = offsetMinutes >= 0 ? "+" : "-";
  const offH = pad(Math.floor(Math.abs(offsetMinutes) / 60));
  const offM = pad(Math.abs(offsetMinutes) % 60);
  return `${year}-${month}-${day}T${hour}:${minute}:${second}${sign}${offH}:${offM}`;
}

export interface FullCalendarEventInput extends EventInput {
  // イベントクリック時にアプリ内の詳細モーダル(EventDetail)へそのまま渡せるよう
  // EventOut 全体を運ぶ(デスクトップ予定詳細設計 2026-07-24 §3.2)。個別フィールド
  // (meetingUrl/htmlLink/accountIds)を別々に持たせていた旧実装は廃止し、単一の
  // 情報源(event)に統一した。
  extendedProps: { event: EventOut };
}

/**
 * EventOut[] を FullCalendar のイベント入力形式へ変換する純関数
 * (デスクトップカレンダービュー設計 2026-07-21 §5)。
 * - 時刻ありイベント: start/end をそのまま使い allDay: false
 * - 終日イベント: all_day_start(YYYY-MM-DD)を start にし allDay: true。
 *   all_day_end(複数日イベントのみ非空。排他的終了日)があれば end に設定する —
 *   FullCalendar の all-day イベントの end も同じ「排他的終了日」の規約なので
 *   変換不要でそのまま使える。単日終日イベント(all_day_end が空文字)は end を
 *   指定しない(レビュー Important 1: これが無いと開始日を含まないビューで
 *   複数日終日イベントが完全に消えていた)
 * - title が空文字なら「(無題)」
 * - backgroundColor/borderColor は colorOf(代表アカウント = account_id)から設定
 */
export function toFullCalendarEvents(
  events: EventOut[],
  colorOf: (accountId: string) => string,
): FullCalendarEventInput[] {
  return events.map((ev, i) => {
    const color = colorOf(ev.account_id);
    return {
      id: `${ev.account_id}-${i}`,
      title: ev.title || "(無題)",
      start: ev.all_day ? ev.all_day_start : ev.start,
      ...(ev.all_day ? (ev.all_day_end ? { end: ev.all_day_end } : {}) : { end: ev.end }),
      allDay: ev.all_day,
      backgroundColor: color,
      borderColor: color,
      extendedProps: { event: ev },
    };
  });
}

function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    return e.hint ? `${e.message}(${e.hint})` : e.message;
  }
  return String(e);
}

/**
 * イベントの見出し表示。既定の eventContent を上書きしているため、FullCalendar が
 * 本来自動で付ける時刻表示(月ビューの各イベント行の先頭時刻等)が消えてしまう —
 * arg.timeText(終日イベントやタイムグリッド上のイベントでは空文字)を先頭に
 * 明示的に表示して補う。meeting_url があればネイティブ title 属性でツールチップ
 * 表示する(装飾は最小)。
 */
function renderEventContent(arg: EventContentArg) {
  const ev = arg.event.extendedProps.event as EventOut;
  return (
    <div title={ev.meeting_url || undefined}>
      {arg.timeText && <span className="fc-calsync-event-time">{arg.timeText} </span>}
      {arg.event.title}
    </div>
  );
}

export default function CalendarView({ api }: { api: ApiClient }) {
  const [orderedIds, setOrderedIds] = useState<string[]>([]);
  const [configError, setConfigError] = useState<string | null>(null);
  const [events, setEvents] = useState<EventOut[]>([]);
  const [failed, setFailed] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [fetchError, setFetchError] = useState<string | null>(null);
  // クリックされた予定(モーダル表示中は非 null。デスクトップ予定詳細設計 2026-07-24 §3.2)。
  const [selectedEvent, setSelectedEvent] = useState<EventOut | null>(null);
  // 背景クリックで閉じる判定は mousedown がオーバーレイ自身で始まった場合に限る。
  // click は mousedown/mouseup のターゲットの共通祖先で発火するため、説明文のテキストを
  // ドラッグ選択してモーダル外でマウスを離すと click のターゲットがオーバーレイになり、
  // このゲートが無いと選択操作だけでモーダルが閉じてしまう(レビュー指摘)。
  const overlayMouseDownRef = useRef(false);
  const lastRangeRef = useRef<{ from: string; to: string } | null>(null);
  // リクエスト連番。datesSet の連打(週↔月の素早い切り替え・ドラッグでの範囲変更)で
  // 複数の api.events() が同時に飛んだ場合、後発リクエストより先に古いリクエストの
  // レスポンスが届くと画面が古い期間のイベントに巻き戻る(レビュー Important 2)。
  // 呼び出しごとに採番し、レスポンス到着時に「自分が最新のリクエストか」を確認して
  // からのみ setEvents/setFailed/setFetchError/setLoading を反映し、古ければ破棄する。
  const requestSeqRef = useRef(0);

  // アカウント色は起動時(タブ表示時)に 1 回だけ getConfig() を取得して決める
  // (定義順を色割当の基準にするため。デスクトップカレンダービュー設計 2026-07-21 §5)。
  useEffect(() => {
    api
      .getConfig()
      .then((c) => {
        setOrderedIds((c.raw.accounts ?? []).map((a) => a.id).filter((v): v is string => !!v));
      })
      .catch((e) => setConfigError(describeError(e)));
  }, [api]);

  const colorOf = useCallback((accountId: string) => colorForAccount(accountId, orderedIds), [orderedIds]);

  const loadEvents = useCallback(
    (from: string, to: string, refresh: boolean) => {
      lastRangeRef.current = { from, to };
      const seq = ++requestSeqRef.current;
      const isStale = () => requestSeqRef.current !== seq;
      setLoading(true);
      setFetchError(null);
      api
        .events(from, to, refresh)
        .then((res) => {
          if (isStale()) return; // 自分より新しいリクエストが既に発行済み → 結果を破棄
          setEvents(res.events);
          setFailed(res.failed);
        })
        .catch((e) => {
          if (isStale()) return;
          setFetchError(describeError(e));
        })
        .finally(() => {
          if (isStale()) return;
          setLoading(false);
        });
    },
    [api],
  );

  const handleDatesSet = useCallback(
    (arg: DatesSetArg) => {
      loadEvents(formatLocalRFC3339(arg.start), formatLocalRFC3339(arg.end), false);
    },
    [loadEvents],
  );

  const handleRefresh = () => {
    const range = lastRangeRef.current;
    if (!range) return;
    loadEvents(range.from, range.to, true);
  };

  const handleEventClick = (arg: EventClickArg) => {
    setSelectedEvent(arg.event.extendedProps.event as EventOut);
  };

  // Esc でモーダルを閉じる。モーダルが開いている間だけリスナーを登録し、閉じたら
  // 必ず後片付けする(モーダルを閉じたあとも Esc を拾い続けたり、アンマウント後に
  // リスナーが残ったりしないように)。
  useEffect(() => {
    if (!selectedEvent) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") setSelectedEvent(null);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [selectedEvent]);

  const fcEvents = useMemo(() => toFullCalendarEvents(events, colorOf), [events, colorOf]);

  return (
    <div className="calendar-view">
      {configError && <p className="error">設定の取得に失敗しました: {configError}</p>}
      {fetchError && <p className="error">予定の取得に失敗しました: {fetchError}</p>}
      {failed.length > 0 && (
        <div className="banner banner-warning">
          <p>一時的に取得できないアカウント: {failed.join(", ")}。数分後に再試行してください。</p>
          <button onClick={handleRefresh} disabled={loading}>
            {loading ? "再読み込み中…" : "再読み込み"}
          </button>
        </div>
      )}
      {orderedIds.length > 0 && (
        <div className="calendar-legend">
          {orderedIds.map((id) => (
            <span key={id} className="legend-item">
              <span className="legend-chip" style={{ backgroundColor: colorForAccount(id, orderedIds) }} />
              {id}
            </span>
          ))}
        </div>
      )}
      {loading && <p className="hint">読み込み中…</p>}
      <div className="calendar-grid">
        <FullCalendar
          plugins={[dayGridPlugin, timeGridPlugin, listPlugin]}
          initialView="timeGridWeek"
          headerToolbar={{ left: "prev,next today", center: "title", right: "timeGridWeek,dayGridMonth,listWeek" }}
          // ja ロケールの既定は list ボタンを「予定リスト」にするが、仕様(§2)により
          // 「スケジュール」に上書きする(list 系ビューは listWeek のみ使用)。
          buttonText={{ list: "スケジュール" }}
          locale={jaLocale}
          height="100%"
          events={fcEvents}
          eventContent={renderEventContent}
          datesSet={handleDatesSet}
          eventClick={handleEventClick}
        />
      </div>
      {selectedEvent && (
        // 背景クリックで閉じる(mousedown と click の両方がオーバーレイ自身のときのみ。
        // overlayMouseDownRef のコメント参照)。モーダル本体側は stopPropagation で
        // バブリングを止め、中身のクリックでは閉じないようにする
        // (デスクトップ予定詳細設計 2026-07-24 §3.2)。
        <div
          className="modal-overlay"
          onMouseDown={(e) => {
            overlayMouseDownRef.current = e.target === e.currentTarget;
          }}
          onClick={(e) => {
            if (overlayMouseDownRef.current && e.target === e.currentTarget) setSelectedEvent(null);
          }}
        >
          <div className="modal-content" onClick={(e) => e.stopPropagation()}>
            <button
              className="modal-close"
              onClick={() => setSelectedEvent(null)}
              aria-label="閉じる"
            >
              ✕
            </button>
            <EventDetail event={selectedEvent} colorOf={colorOf} onClose={() => setSelectedEvent(null)} />
          </div>
        </div>
      )}
    </div>
  );
}
