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
