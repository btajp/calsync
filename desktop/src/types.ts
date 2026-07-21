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
  tokens: TokenStatus[];
  calendars?: CalendarStatus[];
  db_note?: string;
}
export interface CalendarListEntry { id: string; summary: string; primary: boolean; access_role: string }
export interface AuthState { phase: "idle" | "running" | "done" | "error"; account_id?: string; error?: string; hint?: string }
