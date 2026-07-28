import { siGooglemeet, siWebex, siZoom } from "simple-icons";
import type { MeetingService } from "../urlSafety";

// 会議サービスのロゴアイコン(2026-07-28 実機フィードバック: 単色ドットではなく
// ロゴにしたい)。パスデータは simple-icons(CC0-1.0)を使う。色は simple-icons の
// ブランド色を基本に、ダークモードでも見えるよう Webex のみ Cisco のティール系に
// 差し替える(simple-icons の webex は #000000 で黒背景に沈む)。
// Microsoft Teams は Microsoft の要請により simple-icons から削除されており
// (サードパーティのロゴ再現を認めないブランドガイドライン)、商標ロゴの再現を
// 避けて頭文字バッジ(.service-badge-teams)で代替する。
const ICON_PATHS: Partial<Record<MeetingService["key"], { path: string; color: string }>> = {
  zoom: { path: siZoom.path, color: "#0b5cff" },
  meet: { path: siGooglemeet.path, color: "#00897b" },
  webex: { path: siWebex.path, color: "#00bceb" },
};

export default function ServiceIcon({ service, size = 14 }: { service: MeetingService; size?: number }) {
  const icon = ICON_PATHS[service.key];
  if (!icon) {
    return (
      <span className="service-badge-teams" style={{ width: size, height: size }} aria-hidden>
        T
      </span>
    );
  }
  return (
    <svg viewBox="0 0 24 24" width={size} height={size} aria-hidden>
      <path d={icon.path} fill={icon.color} />
    </svg>
  );
}
