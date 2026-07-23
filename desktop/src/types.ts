export interface RawSlack {
  bot_token_env?: string;
  channel?: string;
  morning_digest?: string;
  remind_before?: string;
}
export interface RawAccount {
  id?: string;
  provider?: string;
  email?: string;
  calendars?: string[];
  digest_calendars?: string[];
  blocker_calendar?: string;
  show_origin_in_description?: boolean;
}
export interface RawDetailSync {
  from?: string;
  to?: string;
  fields?: string[];
  visibility?: string;
}
export interface RawConfig {
  poll_interval?: string;
  sync_window?: string;
  blocker_title?: string;
  reconcile_at?: string;
  dedupe_same_meeting?: boolean;
  busy_show_as?: string[];
  notifications?: { slack?: RawSlack };
  providers?: {
    google?: { credentials_file?: string };
    microsoft?: { client_id?: string };
  };
  accounts?: RawAccount[];
  detail_sync?: RawDetailSync[];
}
export interface DaemonInfo { mode: string; running: boolean; detail?: string }
export interface TokenStatus { account_id: string; state: string }
export interface CalendarStatus { account_id: string; calendar_id: string; last_sync: string; status: string }
export interface StatusResponse {
  daemon: DaemonInfo;
  // サーバーは常に配列を返す(appserver 側で []TokenStatus{} 初期化済み)が、
  // 型としては optional にして呼び出し側に ?? [] の防御を強制する。
  tokens?: TokenStatus[];
  calendars?: CalendarStatus[];
  db_note?: string;
}
export interface CalendarListEntry { id: string; summary: string; primary: boolean; access_role: string }
export interface AuthState { phase: "idle" | "running" | "done" | "error"; account_id?: string; error?: string; hint?: string }
// GET /api/maintenance/state(internal/appserver/maintenance.go の handleMaintenanceState)。
// サーバーは phase/log_path/error を常に文字列で返す(空文字はキー自体が無かったことを示さない)。
export interface MaintenanceState {
  phase: "idle" | "running" | "done" | "error";
  log_path: string;
  error: string;
}

// EventOut は GET /api/events の 1 件(internal/appserver/events.go の EventOut と
// json タグを完全一致させること。desktop calendar view design 2026-07-21 §4)。
export interface EventOut {
  account_id: string;
  account_ids: string[];
  title: string;
  start: string; // RFC3339
  end: string; // RFC3339
  all_day: boolean;
  all_day_start: string; // YYYY-MM-DD(all_day 時のみ)
  all_day_end: string; // 排他的終了日・YYYY-MM-DD(all_day 時、複数日イベントのみ非空)
  meeting_url: string;
  html_link: string;
}
export interface EventsResponse { events: EventOut[]; failed: string[] }
