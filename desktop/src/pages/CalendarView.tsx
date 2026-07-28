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
  // cluster: 同時開始グループを 1 箱に統合した合成イベント(2026-07-29 同時刻表示
  // 検討・案1)。event は代表(先頭)を指し、cluster に全員が入る。
  extendedProps: { event: EventOut; cluster?: EventOut[] };
}

/**
 * タイトル先頭の【…】接頭辞(【社内】等)を分離する純関数(2026-07-29 同時刻表示
 * 検討・案4「タイトル正規化」)。接頭辞は小さなチップとして描画し、識別に効く文字を
 * 行の先頭に出す。8 文字を超える【…】や、接頭辞しかないタイトルは触らない。
 */
export function splitTitlePrefix(title: string): { prefix: string | null; rest: string } {
  const m = title.match(/^【([^【】]{1,8})】\s*/);
  if (!m) return { prefix: null, rest: title };
  const rest = title.slice(m[0].length);
  if (!rest) return { prefix: null, rest: title };
  return { prefix: m[1], rest };
}

/**
 * 同時開始(start が同一時刻)の時刻あり予定を 1 つのクラスタに束ねる純関数
 * (2026-07-29 同時刻表示検討・案1)。クラスタは最初の要素の位置に現れ、行順は
 * 「終了が遅い順 → タイトル昇順」(左端のレールが長い順に並び読みやすい)。
 * 終日・start がパース不能な予定・単独の予定はそのまま返す。
 */
export function clusterSameStart(events: EventOut[]): (EventOut | EventOut[])[] {
  const groups = new Map<number, EventOut[]>();
  for (const ev of events) {
    if (ev.all_day) continue;
    const t = Date.parse(ev.start);
    if (Number.isNaN(t)) continue;
    const g = groups.get(t);
    if (g) g.push(ev);
    else groups.set(t, [ev]);
  }
  const emitted = new Set<number>();
  const out: (EventOut | EventOut[])[] = [];
  for (const ev of events) {
    const t = ev.all_day ? NaN : Date.parse(ev.start);
    const g = Number.isNaN(t) ? undefined : groups.get(t);
    if (!g || g.length < 2) {
      out.push(ev);
      continue;
    }
    if (emitted.has(t)) continue; // クラスタは先頭要素の位置で 1 回だけ
    emitted.add(t);
    out.push(
      [...g].sort((a, b) => {
        const ea = Date.parse(a.end) || 0;
        const eb = Date.parse(b.end) || 0;
        if (ea !== eb) return eb - ea;
        return (a.title || "").localeCompare(b.title || "");
      }),
    );
  }
  return out;
}

/** クラスタの箱の終端 = メンバーの最も遅い終了時刻(RFC3339 のまま返す)。 */
function clusterEnd(group: EventOut[]): string {
  let best = group[0].end;
  let bestT = Date.parse(best) || 0;
  for (const ev of group) {
    const t = Date.parse(ev.end) || 0;
    if (t > bestT) {
      bestT = t;
      best = ev.end;
    }
  }
  return best;
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
  return clusterSameStart(events).map((item, i) => {
    if (Array.isArray(item)) {
      // 同時開始クラスタ → 1 つの合成箱(中身の行描画は renderEventContent 側。
      // 2026-07-29 同時刻表示検討・案1)
      const first = item[0];
      const color = colorOf(first.account_id);
      return {
        id: `cluster-${i}`,
        title: item.map((ev) => ev.title || "(無題)").join(" / "),
        start: first.start,
        end: clusterEnd(item),
        allDay: false,
        backgroundColor: color,
        borderColor: color,
        extendedProps: { event: first, cluster: item },
      };
    }
    const ev = item;
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

// FullCalendar が harness に与える配置情報(inline style から読む)。
// level は z-index - 1 = FullCalendar の stackDepth(重なり段数)。
export interface HarnessGeom {
  top: number; // px
  level: number; // 0 = 重なりの一番下
  left: number; // FullCalendar 計算の左インセット(%)
}

/**
 * 被っている予定の左インセットを「1 段につき indentPercent%」へ詰め替える純関数
 * (2026-07-29 実機フィードバック: FullCalendar 既定は 1 段 50% で右半分に潰れる)。
 * 例外: 開始位置(top)がほぼ同じ予定同士は FullCalendar の横並びを維持する —
 * 5% 詰めにすると後の予定が前の予定をほぼ完全に覆い、どちらかが読めなくなるため
 * (Google カレンダーも同時開始は横並び・時間差の重なりだけカスケード)。
 * 戻り値は各要素の新しい left%(null = 変更しない)。
 */
export function compactOverlapLefts(items: HarnessGeom[], indentPercent = 5, topTolerance = 3): (number | null)[] {
  return items.map((it) => {
    if (it.level <= 0) return null;
    const sameStartBelow = items.some(
      (o) => o !== it && o.level < it.level && Math.abs(o.top - it.top) <= topTolerance,
    );
    if (sameStartBelow) return null;
    const compact = it.level * indentPercent;
    return compact < it.left ? compact : null; // FC の方が既に狭いインセットなら触らない
  });
}

/** 行末に付ける「HH:MM - HH:MM」のローカル時刻範囲(クラスタ行用)。 */
function timeRangeLabel(ev: EventOut): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  const clock = (d: Date) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const start = new Date(ev.start);
  const end = new Date(ev.end);
  if (Number.isNaN(start.getTime()) || Number.isNaN(end.getTime())) return "";
  return `${clock(start)} - ${clock(end)}`;
}

/** タイトル(+接頭辞チップ)のインライン描画(単独イベント・クラスタ行で共用)。 */
function renderTitle(title: string) {
  const { prefix, rest } = splitTitlePrefix(title || "(無題)");
  return (
    <>
      {prefix && <span className="fc-calsync-prefix">{prefix}</span>}
      {rest || "(無題)"}
    </>
  );
}

/**
 * 同時開始クラスタの中身(2026-07-29 同時刻表示検討・案1)。件数が一目で分かるよう
 * 4 つの手掛かりを重ねる: ①1 行 = 1 予定(折り返さない) ②行間ディバイダー(CSS)
 * ③右上の「N件」バッジ ④左端のレール(本数 = 件数、高さ = 各予定の長さの比)。
 * 行クリックの判別は data-cluster-index を handleEventClick 側で closest 検索する
 * (FullCalendar のネイティブ eventClick が先に発火するため、行側で stopPropagation
 * しても二重発火は防げない — クリック処理は eventClick に一本化する)。
 */
function renderClusterContent(cluster: EventOut[], colorOf: (accountId: string) => string) {
  const startMs = Date.parse(cluster[0].start) || 0;
  const boxMs = Math.max(...cluster.map((ev) => (Date.parse(ev.end) || startMs) - startMs), 1);
  // ツールチップは単独予定と同じく会議 URL も載せる(レビュー指摘: クラスタ化で
  // ホバーから URL が消えていた)
  const tooltip = cluster
    .map((ev) => `${ev.title || "(無題)"} ${timeRangeLabel(ev)}${ev.meeting_url ? `\n  ${ev.meeting_url}` : ""}`)
    .join("\n");
  // レールは 4 本まで(5 件以上は N件バッジが頼り)。ガター幅は本数に連動させ、
  // 3 件以上でレールが行のチップに被らないようにする(レビュー指摘)
  const railCount = Math.min(cluster.length, 4);
  const gutter = railCount * 5 + 3;
  return (
    <div className="fc-calsync-cluster" title={tooltip} style={{ paddingLeft: `${gutter}px` }}>
      <span className="fc-calsync-cluster-count">{cluster.length}件</span>
      <span className="fc-calsync-cluster-rails" aria-hidden>
        {cluster.slice(0, railCount).map((ev, i) => (
          <span
            key={i}
            className="fc-calsync-cluster-rail"
            style={{ height: `${Math.round((((Date.parse(ev.end) || startMs) - startMs) / boxMs) * 100)}%` }}
          />
        ))}
      </span>
      {cluster.map((ev, i) => (
        <div key={i} className="fc-calsync-cluster-row" data-cluster-index={i}>
          <span className="legend-chip" style={{ backgroundColor: colorOf(ev.account_id) }} />
          <span className="fc-calsync-cluster-row-title">{renderTitle(ev.title)}</span>
          <span className="fc-calsync-event-time">{timeRangeLabel(ev)}</span>
        </div>
      ))}
    </div>
  );
}

/**
 * イベントの見出し表示。既定の eventContent を上書きしているため、FullCalendar が
 * 本来自動で付ける時刻表示(月ビューの各イベント行の先頭時刻等)が消えてしまう —
 * arg.timeText(終日イベントでは空文字)を明示的に表示して補う。枠に収まらない
 * 文字は CSS でクリップするため(2026-07-29 実機フィードバック)、ネイティブ
 * title 属性のツールチップにタイトル全文(+会議 URL)を出して救済する。
 * 週/日(timeGrid)ビューはタイトルを先頭にする — 30 分予定は最初の 1 行しか
 * 見えず、時刻先頭だと「10:30 - 11:00」だけでタイトルが全く読めない(時刻は
 * グリッド上の位置でわかる。2026-07-29 実機フィードバック)。時刻は開始-終了の
 * 範囲を常に後置し、幅が足りないときは末尾から自然にクリップされるに任せる
 * (「表示できるときは終了時刻も見えたほうがいい」— 同日フィードバック)。
 * 月/リストは従来どおり時刻先頭。同時開始クラスタはビューを問わず行リスト。
 */
function renderEventContent(arg: EventContentArg, colorOf: (accountId: string) => string) {
  const props = arg.event.extendedProps as FullCalendarEventInput["extendedProps"];
  // クラスタの行リスト描画は週/日(timeGrid)限定。月/リストはアカウント色の背景箱が
  // 無く、白系 rgba の装飾(レール・ディバイダー等)がライトテーマで不可視になるため
  // (レビュー指摘)、連結タイトル(A / B)の通常描画にフォールバックする
  if (props.cluster && arg.view.type.startsWith("timeGrid")) {
    return renderClusterContent(props.cluster, colorOf);
  }
  const ev = props.event;
  const tooltip = [arg.event.title, ev.meeting_url].filter(Boolean).join("\n") || undefined;
  if (arg.view.type.startsWith("timeGrid")) {
    return (
      <div title={tooltip}>
        {renderTitle(arg.event.title)}
        {arg.timeText && <span className="fc-calsync-event-time"> {arg.timeText}</span>}
      </div>
    );
  }
  return (
    <div title={tooltip}>
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

  // stale-while-revalidate の再取得タイマー(loadEvents 自身を setTimeout から
  // 呼び直すための後方参照。useCallback は自分自身を依存にできない)。
  const staleRetryTimerRef = useRef<number | null>(null);
  const loadEventsRef = useRef<(from: string, to: string, refresh: boolean, retryOnStale?: boolean) => void>(
    () => {},
  );
  useEffect(
    () => () => {
      if (staleRetryTimerRef.current !== null) window.clearTimeout(staleRetryTimerRef.current);
    },
    [],
  );

  const loadEvents = useCallback(
    (from: string, to: string, refresh: boolean, retryOnStale = true) => {
      // 予約済みの stale 再取得はどんな新規リクエストでも無効化する。残しておくと、
      // 手動再読み込み(refresh=1・数秒かかる)の in-flight 中にタイマーが発火して
      // 最新の seq を奪い、後から届く refresh の最新データが isStale() で丸ごと
      // 破棄される(レビュー指摘: 手動更新が「何も変わらない」ように見える)。
      if (staleRetryTimerRef.current !== null) {
        window.clearTimeout(staleRetryTimerRef.current);
        staleRetryTimerRef.current = null;
      }
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
          // appserver が期限切れキャッシュを即返した(stale-while-revalidate)場合、
          // 裏で最新化が進んでいるので 1 回だけ少し後に取り直す。再取得でも stale の
          // ままなら以後は自然な再取得(ビュー切替・手動更新)に任せ、無限リトライに
          // しない。表示範囲が変わっていたら再取得しない(古い範囲を蒸し返さない)。
          if (res.stale && retryOnStale) {
            if (staleRetryTimerRef.current !== null) window.clearTimeout(staleRetryTimerRef.current);
            staleRetryTimerRef.current = window.setTimeout(() => {
              staleRetryTimerRef.current = null;
              const range = lastRangeRef.current;
              if (range && range.from === from && range.to === to) {
                loadEventsRef.current(from, to, false, false);
              }
            }, 4000);
          }
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
  loadEventsRef.current = loadEvents;

  // 被り予定の左インセット詰め(compactOverlapLefts)を FullCalendar の再レンダリング後に
  // DOM へ適用する。FullCalendar は harness の left/right/z-index をインラインスタイルで
  // 再設定するため、events や表示範囲が変わるたびに rAF で描画確定後に詰め直す。
  const calendarGridRef = useRef<HTMLDivElement | null>(null);
  const applyOverlapCompaction = useCallback(() => {
    const root = calendarGridRef.current;
    if (!root) return;
    const cols: HTMLElement[] = Array.from(root.querySelectorAll<HTMLElement>(".fc-timegrid-col-events"));
    for (const col of cols) {
      const harnesses: HTMLElement[] = Array.from(
        col.querySelectorAll<HTMLElement>(":scope > .fc-timegrid-event-harness"),
      );
      const items: HarnessGeom[] = harnesses.map((el) => ({
        top: parseFloat(el.style.top) || 0,
        level: (parseInt(el.style.zIndex || "1", 10) || 1) - 1,
        left: parseFloat(el.style.left) || 0,
      }));
      compactOverlapLefts(items).forEach((v, i) => {
        // !important を付けない(FullCalendar のインライン値の上書きには十分で、
        // ホバー全幅展開の CSS(:hover { left: 0 !important })が勝てるようにする)
        if (v !== null) harnesses[i].style.left = `${v}%`;
      });
    }
  }, []);
  const scheduleCompaction = useCallback(() => {
    requestAnimationFrame(applyOverlapCompaction);
  }, [applyOverlapCompaction]);

  useEffect(() => {
    window.addEventListener("resize", scheduleCompaction);
    return () => window.removeEventListener("resize", scheduleCompaction);
  }, [scheduleCompaction]);

  const handleDatesSet = useCallback(
    (arg: DatesSetArg) => {
      loadEvents(formatLocalRFC3339(arg.start), formatLocalRFC3339(arg.end), false);
      scheduleCompaction();
    },
    [loadEvents, scheduleCompaction],
  );

  const handleRefresh = () => {
    const range = lastRangeRef.current;
    if (!range) return;
    loadEvents(range.from, range.to, true);
  };

  const handleEventClick = (arg: EventClickArg) => {
    const props = arg.event.extendedProps as FullCalendarEventInput["extendedProps"];
    if (props.cluster) {
      // クラスタはクリックされた行の予定の詳細を開く(行が特定できない場所 —
      // バッジ・余白など — は先頭の予定)
      const rowEl = (arg.jsEvent.target as HTMLElement | null)?.closest?.("[data-cluster-index]");
      const idx = rowEl instanceof HTMLElement ? Number(rowEl.dataset.clusterIndex) : 0;
      setSelectedEvent(props.cluster[idx] ?? props.cluster[0]);
      return;
    }
    setSelectedEvent(props.event);
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
  const renderEvent = useCallback((arg: EventContentArg) => renderEventContent(arg, colorOf), [colorOf]);

  // events の変化(FullCalendar が harness を組み直す)後にも詰め直す
  useEffect(() => {
    scheduleCompaction();
  }, [fcEvents, scheduleCompaction]);

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
      <div className="calendar-grid" ref={calendarGridRef}>
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
          eventContent={renderEvent}
          datesSet={handleDatesSet}
          eventClick={handleEventClick}
          // 週/日ビューに現在時刻の水平線を表示する(2026-07-28 実機フィードバック:
          // 現在時刻の位置がわかりにくい)。既定 500ms の再描画間隔はそのまま
          nowIndicator
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
