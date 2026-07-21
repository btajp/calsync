import { useEffect, useReducer } from "react";
import type { DownloadProgress, UpdateInfo, UpdaterApi } from "../updater";
import { updaterApi } from "../updater";

export type BannerState =
  | { phase: "idle" }
  | { phase: "checking" }
  | { phase: "up-to-date" }
  | { phase: "available"; info: UpdateInfo }
  | { phase: "downloading"; info: UpdateInfo; progress: DownloadProgress }
  | { phase: "error"; message: string };

export type BannerAction =
  | { type: "check-started" }
  | { type: "check-succeeded"; info: UpdateInfo | null }
  | { type: "check-failed"; message: string; silent: boolean }
  | { type: "download-started"; info: UpdateInfo }
  | { type: "download-progress"; progress: DownloadProgress }
  | { type: "download-failed"; message: string };

export const initialBannerState: BannerState = { phase: "idle" };

/** 純関数の状態遷移。起動時の自動チェック失敗(silent: true)は idle に戻して握りつぶす。 */
export function bannerReducer(state: BannerState, action: BannerAction): BannerState {
  switch (action.type) {
    case "check-started":
      return { phase: "checking" };
    case "check-succeeded":
      return action.info ? { phase: "available", info: action.info } : { phase: "up-to-date" };
    case "check-failed":
      return action.silent ? { phase: "idle" } : { phase: "error", message: action.message };
    case "download-started":
      return { phase: "downloading", info: action.info, progress: { downloaded: 0, contentLength: null } };
    case "download-progress":
      return state.phase === "downloading" ? { ...state, progress: action.progress } : state;
    case "download-failed":
      return { phase: "error", message: action.message };
    default:
      return state;
  }
}

/** check() を実行し、結果を dispatch する。silent: true は起動時の自動チェック用(失敗を表示しない)。 */
export async function runCheck(
  api: UpdaterApi,
  dispatch: (action: BannerAction) => void,
  opts: { silent: boolean },
): Promise<void> {
  dispatch({ type: "check-started" });
  try {
    const info = await api.check();
    dispatch({ type: "check-succeeded", info });
  } catch (e) {
    dispatch({ type: "check-failed", message: String(e), silent: opts.silent });
  }
}

/** ダウンロード+インストール+再起動を実行し、進捗を dispatch する。 */
export async function runDownloadAndInstall(
  api: UpdaterApi,
  info: UpdateInfo,
  dispatch: (action: BannerAction) => void,
): Promise<void> {
  dispatch({ type: "download-started", info });
  try {
    await api.downloadAndInstall((progress) => dispatch({ type: "download-progress", progress }));
    await api.relaunch();
  } catch (e) {
    dispatch({ type: "download-failed", message: String(e) });
  }
}

function progressPercent(progress: DownloadProgress): string | null {
  if (!progress.contentLength || progress.contentLength <= 0) return null;
  return `${Math.min(100, Math.floor((progress.downloaded / progress.contentLength) * 100))}%`;
}

export default function UpdateBanner({ api = updaterApi }: { api?: UpdaterApi }) {
  const [state, dispatch] = useReducer(bannerReducer, initialBannerState);

  useEffect(() => {
    void runCheck(api, dispatch, { silent: true });
    // 起動時に一度だけ実行する(api は起動後不変の前提)。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const busy = state.phase === "checking" || state.phase === "downloading";

  return (
    <div className="update-banner-row">
      <button className="link-button" disabled={busy} onClick={() => void runCheck(api, dispatch, { silent: false })}>
        更新確認
      </button>
      {state.phase === "up-to-date" && <span className="hint">最新です</span>}
      {state.phase === "error" && <span className="hint error">確認に失敗しました: {state.message}</span>}
      {(state.phase === "available" || state.phase === "downloading") && (
        <span className="hint">v{state.info.version} が利用可能</span>
      )}

      {state.phase === "available" && (
        <div className="update-banner">
          <p>新しいバージョン v{state.info.version} があります。</p>
          <button onClick={() => void runDownloadAndInstall(api, state.info, dispatch)}>更新して再起動</button>
        </div>
      )}
      {state.phase === "downloading" && (
        <div className="update-banner">
          <p>ダウンロード中…{(() => {
            const pct = progressPercent(state.progress);
            return pct ? ` ${pct}` : "";
          })()}</p>
          <button disabled>更新して再起動</button>
        </div>
      )}
    </div>
  );
}
