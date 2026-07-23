import { TrayIcon } from "@tauri-apps/api/tray";
import type { TrayIconEvent } from "@tauri-apps/api/tray";
import { Image } from "@tauri-apps/api/image";
// アイコンは小さい(100バイト強)ため Vite の既定閾値では base64 の data: URI にインライン化
// されてしまう。fetch("data:...") は CSP の connect-src の対象になり、F14 の CSP には data:
// を含めていない(必要最小限にする方針)ため、?no-inline で実ファイルとして出力させ、
// 同一オリジン('self')から fetch できるようにする(Vite 6 の既定機能。node_modules/vite の
// dep-*.js に noInlineRE = /[?&]no-inline\b/ の実装があることを確認済み)。
import trayIconUrl from "./assets/tray-icon.png?no-inline";
import { formatLocalRFC3339 } from "./pages/CalendarView";
import type { EventOut } from "./types";

const MAX_LABEL_LENGTH = 24;
const ONE_HOUR_MS = 60 * 60 * 1000;
const MS_PER_DAY = 24 * 60 * 60 * 1000;
// 「7日以内」の窓。トレイタイトルとポップオーバーのスケジュール取得(今日〜+7日)で共有する
// (デスクトップトレイ設計 2026-07-23 §3.1/3.2)。
const SCHEDULE_WINDOW_DAYS = 7;
const TITLE_UPDATE_INTERVAL_MS = 60_000;

function pad2(n: number): string {
  return String(n).padStart(2, "0");
}

/** Date のローカル時刻を "HH:MM" にする(トレイ・ポップオーバー共通)。 */
export function formatClock(d: Date): string {
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

/** now から to までの「暦日の差」をローカル日付基準で計算する(時刻は無視)。 */
function calendarDaysBetween(from: Date, to: Date): number {
  const fromDate = new Date(from.getFullYear(), from.getMonth(), from.getDate());
  const toDate = new Date(to.getFullYear(), to.getMonth(), to.getDate());
  return Math.round((toDate.getTime() - fromDate.getTime()) / MS_PER_DAY);
}

function truncateLabel(label: string): string {
  if (label.length <= MAX_LABEL_LENGTH) return label;
  return `${label.slice(0, MAX_LABEL_LENGTH - 1)}…`;
}

/**
 * トレイタイトル(次の予定の表示文字列)を決める純関数
 * (デスクトップトレイ設計 2026-07-23 §3.1)。
 * - 対象は時刻あり予定のみ(all_day は除外)。now 以降で最も早く始まる 1 件を選ぶ
 * - 60 分未満: 「N分後 タイトル」/ 60 分以上かつ当日: 「HH:MM タイトル」/
 *   翌日: 「明日HH:MM タイトル」/ 2〜7 日以内: 「M/D タイトル」/
 *   該当予定が無い、または 7 日を超える場合は ""(タイトルなし=アイコンのみ)
 * - 全体を 24 文字に切り詰める(超過時は 23 文字 + 「…」)
 * - タイトルが空文字の予定は「(無題)」にする(CalendarView.toFullCalendarEvents と同じ規約)
 */
export function nextEventLabel(events: EventOut[], now: Date): string {
  const upcoming = events
    .filter((e) => !e.all_day)
    .map((e) => ({ event: e, start: new Date(e.start) }))
    .filter(({ start }) => !Number.isNaN(start.getTime()) && start.getTime() >= now.getTime())
    .sort((a, b) => a.start.getTime() - b.start.getTime());
  if (upcoming.length === 0) return "";

  const { event, start } = upcoming[0];
  const daysDiff = calendarDaysBetween(now, start);
  if (daysDiff > SCHEDULE_WINDOW_DAYS) return "";

  const title = event.title || "(無題)";
  const diffMs = start.getTime() - now.getTime();

  let prefix: string;
  if (diffMs < ONE_HOUR_MS) {
    const minutes = Math.round(diffMs / 60000);
    prefix = `${minutes}分後`;
  } else if (daysDiff === 0) {
    prefix = formatClock(start);
  } else if (daysDiff === 1) {
    prefix = `明日${formatClock(start)}`;
  } else {
    prefix = `${start.getMonth() + 1}/${start.getDate()}`;
  }
  return truncateLabel(`${prefix} ${title}`);
}

/**
 * GET /api/events に渡す「今日〜+7日」の RFC3339 レンジ(ローカルオフセット付き)。
 * トレイのタイトル更新とポップオーバーのスケジュール取得が同じ窓を使うため export する。
 */
export function scheduleFetchRange(now: Date): { from: string; to: string } {
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const end = new Date(start);
  end.setDate(end.getDate() + SCHEDULE_WINDOW_DAYS + 1); // 当日+7日を含む8日分(排他的終了)
  return { from: formatLocalRFC3339(start), to: formatLocalRFC3339(end) };
}

// --- ここから下は Tauri ランタイム依存(TrayIcon 生成・更新)。vitest では対象外。 ---

let trayEvents: EventOut[] = [];
let trayIconHandle: TrayIcon | null = null;
let initPromise: Promise<TrayIcon> | null = null;

/**
 * App(Shell)から今日〜+7日分の events を渡す。5 分ごとの定期取得+取得成功時に
 * 呼ばれる想定(デスクトップトレイ設計 2026-07-23 §3.1)。トレイがまだ生成されていない
 * 呼び出し順でも安全(値を保持しておき、生成完了後の初回描画に反映される)。
 */
export function setTrayEvents(events: EventOut[]): void {
  trayEvents = events;
  void refreshTitle();
}

async function refreshTitle(): Promise<void> {
  if (!trayIconHandle) return;
  const label = nextEventLabel(trayEvents, new Date());
  try {
    await trayIconHandle.setTitle(label || null);
  } catch {
    // ベストエフォート。次の 1 分周期で再試行される
  }
}

async function loadIconBytes(): Promise<Uint8Array> {
  const res = await fetch(trayIconUrl);
  return new Uint8Array(await res.arrayBuffer());
}

async function createTray(onClick: (event: TrayIconEvent) => void): Promise<TrayIcon> {
  const bytes = await loadIconBytes();
  const icon = await Image.fromBytes(bytes);
  const tray = await TrayIcon.new({
    icon,
    // macOS のテンプレートアイコン(ライト/ダーク自動反転)として扱う(デスクトップトレイ設計 §3.1)。
    iconAsTemplate: true,
    action: (event) => {
      if (event.type === "Click") onClick(event);
    },
  });
  trayIconHandle = tray;
  await refreshTitle();
  setInterval(() => { void refreshTitle(); }, TITLE_UPDATE_INTERVAL_MS);
  return tray;
}

/**
 * トレイアイコンを生成する。dev の React.StrictMode によるマウント→クリーンアップ→
 * 再マウントの二重実行でトレイアイコンが 2 つ生成されるのを防ぐため、進行中/完了済みの
 * Promise をモジュールスコープで再利用する(sidecar.ts の startSidecar と同じガード方式)。
 * 失敗時は次回呼び出しで再試行できるようガードを解除する。
 */
export function initTray(onClick: (event: TrayIconEvent) => void): Promise<TrayIcon> {
  if (initPromise) return initPromise;
  initPromise = createTray(onClick).catch((e: unknown) => {
    initPromise = null;
    throw e;
  });
  return initPromise;
}
