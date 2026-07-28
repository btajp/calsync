import { siGooglemeet, siWebex } from "simple-icons";
import zoomIcon from "../assets/zoom-icon.png";
import type { MeetingService } from "../urlSafety";

// 会議サービスのロゴアイコン(2026-07-28 実機フィードバック: 単色ドットではなく
// ロゴにしたい)。
// - Zoom: 公式ブランドアセットのアプリタイル PNG(48x48)を同梱して表示する
//   (ユーザー提供の公式 CDN 配布アセット。CSP が外部画像を禁止するため同梱必須)
// - Meet / Webex: simple-icons(CC0-1.0)のパスデータ。色はブランド色を基本に、
//   ダークモードでも見えるよう Webex のみ Cisco のティール系に差し替える
//   (simple-icons の webex は #000000 で黒背景に沈む)
// - Microsoft Teams: Microsoft の要請により simple-icons から削除されており
//   (サードパーティのロゴ再現を認めないブランドガイドライン)、商標ロゴの再現を
//   避けて頭文字バッジ(.service-badge-teams)で代替する
const ICON_PATHS: Partial<Record<MeetingService["key"], { path: string; color: string }>> = {
  meet: { path: siGooglemeet.path, color: "#00897b" },
  webex: { path: siWebex.path, color: "#00bceb" },
};

export default function ServiceIcon({ service, size = 14 }: { service: MeetingService; size?: number }) {
  if (service.key === "zoom") {
    return <img src={zoomIcon} width={size} height={size} alt="" aria-hidden />;
  }
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
