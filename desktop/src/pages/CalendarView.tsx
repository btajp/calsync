import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import FullCalendar from "@fullcalendar/react";
import dayGridPlugin from "@fullcalendar/daygrid";
import timeGridPlugin from "@fullcalendar/timegrid";
import jaLocale from "@fullcalendar/core/locales/ja";
import type { DatesSetArg, EventClickArg, EventContentArg, EventInput } from "@fullcalendar/core";
import { open } from "@tauri-apps/plugin-shell";
import type { ApiClient } from "../api";
import { ApiError } from "../api";
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
  extendedProps: { meetingUrl: string; htmlLink: string; accountIds: string[] };
}

/**
 * EventOut[] を FullCalendar のイベント入力形式へ変換する純関数
 * (デスクトップカレンダービュー設計 2026-07-21 §5)。
 * - 時刻ありイベント: start/end をそのまま使い allDay: false
 * - 終日イベント: all_day_start(YYYY-MM-DD)を start にし allDay: true。EventOut に
 *   all_day_end は無い(internal/appserver/events.go に定義が無いことを確認済み)ため
 *   end は指定しない(FullCalendar は単日として描画する)
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
      ...(ev.all_day ? {} : { end: ev.end }),
      allDay: ev.all_day,
      backgroundColor: color,
      borderColor: color,
      extendedProps: {
        meetingUrl: ev.meeting_url,
        htmlLink: ev.html_link,
        accountIds: ev.account_ids,
      },
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
 * shell:allow-open にはコマンド単位の scope(URL allowlist)機能が無い
 * (tauri-plugin-shell 2.3.5 の gen/schemas/acl-manifests.json で確認済み: "Enables
 * the open command without any pre-configured scope")ため、呼び出し側で https の
 * みに絞る。html_link は Google/Microsoft のカレンダー API 由来で通常 https だが、
 * 想定外のスキーム(file: 等)を既定ブラウザ起動に渡さないための防御。
 */
export function isHttpsUrl(value: string): boolean {
  try {
    return new URL(value).protocol === "https:";
  } catch {
    return false;
  }
}

/** イベントの見出し表示。meeting_url があればネイティブ title 属性でツールチップ表示する(装飾は最小)。 */
function renderEventContent(arg: EventContentArg) {
  const meetingUrl = arg.event.extendedProps.meetingUrl as string;
  return <div title={meetingUrl || undefined}>{arg.event.title}</div>;
}

export default function CalendarView({ api }: { api: ApiClient }) {
  const [orderedIds, setOrderedIds] = useState<string[]>([]);
  const [configError, setConfigError] = useState<string | null>(null);
  const [events, setEvents] = useState<EventOut[]>([]);
  const [failed, setFailed] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const lastRangeRef = useRef<{ from: string; to: string } | null>(null);

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
      setLoading(true);
      setFetchError(null);
      api
        .events(from, to, refresh)
        .then((res) => {
          setEvents(res.events);
          setFailed(res.failed);
        })
        .catch((e) => setFetchError(describeError(e)))
        .finally(() => setLoading(false));
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
    const link = arg.event.extendedProps.htmlLink as string;
    if (link && isHttpsUrl(link)) {
      void open(link);
    }
  };

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
      <FullCalendar
        plugins={[dayGridPlugin, timeGridPlugin]}
        initialView="timeGridWeek"
        headerToolbar={{ left: "prev,next today", center: "title", right: "timeGridWeek,dayGridMonth" }}
        locale={jaLocale}
        height="auto"
        events={fcEvents}
        eventContent={renderEventContent}
        datesSet={handleDatesSet}
        eventClick={handleEventClick}
      />
    </div>
  );
}
