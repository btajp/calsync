import { useEffect, useState } from "react";
import type { MaintenanceState } from "../types";

export type MaintenanceBannerView =
  | { kind: "none" }
  | { kind: "running" }
  | { kind: "done" }
  | { kind: "error"; message: string; logPath: string };

/**
 * maintenance state(+ 結果表示を閉じたかどうか)からバナーの表示内容を決める純関数。
 * running は常に最優先で表示する(閉じるボタンが無く、誤って隠せない)。done/error はユーザーが
 * 「閉じる」を押すまで表示し続け、次に running へ戻ったら自動的に再表示できるよう
 * dismissed をリセットする(呼び出し側の useEffect が担当)。
 */
export function deriveMaintenanceBannerView(
  state: MaintenanceState | null,
  dismissed: boolean,
): MaintenanceBannerView {
  if (!state) return { kind: "none" };
  if (state.phase === "running") return { kind: "running" };
  if (dismissed) return { kind: "none" };
  if (state.phase === "done") return { kind: "done" };
  if (state.phase === "error") return { kind: "error", message: state.error, logPath: state.log_path };
  return { kind: "none" };
}

/**
 * 全タブ共通のメンテナンスバナー(デスクトップ設計 2026-07-23 §4)。running 中は保存・デーモン操作を
 * 阻んでいる旨を案内し、done/error に遷移したら結果を表示する(error はログファイルの案内付き)。
 */
export default function MaintenanceBanner({ state }: { state: MaintenanceState | null }) {
  const [dismissed, setDismissed] = useState(false);

  useEffect(() => {
    if (state?.phase === "running") setDismissed(false);
  }, [state?.phase]);

  const view = deriveMaintenanceBannerView(state, dismissed);

  if (view.kind === "none") return null;

  if (view.kind === "running") {
    return (
      <div className="banner banner-warning">
        <p>リコンサイル実行中(数分かかります)。この間、設定の保存とデーモン操作はできません。</p>
      </div>
    );
  }

  if (view.kind === "done") {
    return (
      <div className="banner banner-success">
        <p>リコンサイルが完了しました。</p>
        <button className="link-button" onClick={() => setDismissed(true)}>
          閉じる
        </button>
      </div>
    );
  }

  return (
    <div className="banner">
      <p>
        リコンサイルに失敗しました: {view.message || "不明なエラー"}
        {view.logPath && <> (ログ: {view.logPath})</>}
      </p>
      <button className="link-button" onClick={() => setDismissed(true)}>
        閉じる
      </button>
    </div>
  );
}
