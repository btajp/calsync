/**
 * capabilities の shell:allow-open にはコマンド単位の scope(URL allowlist)機能が
 * 無い(tauri-plugin-shell 2.3.5 の gen/schemas/acl-manifests.json で確認済み:
 * "Enables the open command without any pre-configured scope")。ただし
 * tauri-plugin-shell の Rust 実装側(open_scope() in lib.rs)には、
 * `tauri.conf.json` に `plugins.shell.open` を明示しない限り既定の検証 regex
 * `^((mailto:\w+)|(tel:\w+)|(https?://\w+)).+` が常に適用される(この設定は
 * 本アプリでは未指定のため既定が有効)。つまり file:/javascript: 等は Rust 側の
 * 既定 regex で既に弾かれるが、この既定は http: にも一致してしまうため、
 * TypeScript 側でさらに https のみへ明示的に絞り込む(html_link/meeting_url は
 * Google/Microsoft のカレンダー API 由来で通常 https。多層防御であり Rust 側の
 * 検証を代替するものではない)。
 *
 * CalendarView(イベントクリック→モーダル)と EventDetail(会議参加・カレンダーで
 * 開く・説明文中のリンク)の両方から使うため独立モジュールに切り出している
 * (デスクトップ予定詳細設計 2026-07-24 §3.1: 1 コンポーネントをカレンダービューと
 * ポップオーバーで共用する方針と同じ理由。EventDetail から CalendarView を
 * 直接 import すると相互 import になるため避けた)。
 */
export function isHttpsUrl(value: string): boolean {
  try {
    return new URL(value).protocol === "https:";
  } catch {
    return false;
  }
}

/**
 * Zoom の会議 URL(https://<host>/j/<confno>?pwd=...)を Zoom デスクトップアプリの
 * URL スキーム(zoommtg://<host>/join?action=join&confno=...&pwd=...)へ変換する
 * 純関数(2026-07-28 実機フィードバック: ブラウザを経由せず直接アプリで参加したい)。
 * 変換できない URL(Zoom 以外のホスト・/my/ 等のパーソナルリンク・confno が数値で
 * 読めない形)は null を返し、呼び出し側は従来どおりブラウザで開く。ホストを保持する
 * のは vanity URL(company.zoom.us)や zoomgov.com に対応するため。zoommtg:// は
 * Rust 側 shell open スコープ(tauri.conf.json の plugins.shell.open)でも許可して
 * いる(既定スコープは https/http/mailto/tel のみで zoommtg を拒否する)。
 */
export interface MeetingService {
  key: "zoom" | "meet" | "teams" | "webex";
  label: string; // 表示名(「Zoom で参加」等に使う)
}

/**
 * 会議 URL のホストからサービス種別を判定する純関数(2026-07-28 実機フィードバック:
 * 参加ボタンにどのサービスかを表示したい)。判定できない場合は null(汎用表示)。
 * アイコンの実体(ロゴパス・色)は components/ServiceIcon.tsx が持つ。
 */
export function meetingService(httpsUrl: string): MeetingService | null {
  let host: string;
  try {
    host = new URL(httpsUrl).hostname;
  } catch {
    return null;
  }
  const is = (base: string) => host === base || host.endsWith(`.${base}`);
  if (is("zoom.us") || is("zoomgov.com")) {
    return { key: "zoom", label: "Zoom" };
  }
  if (host === "meet.google.com") {
    return { key: "meet", label: "Meet" };
  }
  if (is("teams.microsoft.com") || is("teams.live.com")) {
    return { key: "teams", label: "Teams" };
  }
  if (is("webex.com")) {
    return { key: "webex", label: "Webex" };
  }
  return null;
}

export function zoomAppUrl(httpsUrl: string): string | null {
  let u: URL;
  try {
    u = new URL(httpsUrl);
  } catch {
    return null;
  }
  if (u.protocol !== "https:") return null;
  const host = u.hostname;
  const isZoomHost =
    host === "zoom.us" ||
    host.endsWith(".zoom.us") ||
    host === "zoomgov.com" ||
    host.endsWith(".zoomgov.com");
  if (!isZoomHost) return null;
  const m = u.pathname.match(/^\/j\/(\d{6,15})$/);
  if (!m) return null;
  const pwd = u.searchParams.get("pwd");
  return `zoommtg://${host}/join?action=join&confno=${m[1]}${pwd ? `&pwd=${encodeURIComponent(pwd)}` : ""}`;
}
