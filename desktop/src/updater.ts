import { check } from "@tauri-apps/plugin-updater";
import type { Update } from "@tauri-apps/plugin-updater";
import { relaunch } from "@tauri-apps/plugin-process";

export interface UpdateInfo {
  version: string;
  currentVersion: string;
}

export interface DownloadProgress {
  downloaded: number;
  contentLength: number | null;
}

/** UpdateBanner から呼び出す薄いラッパー。テストでは fake 実装に差し替える。 */
export interface UpdaterApi {
  /** 更新の有無を確認する。無ければ null。 */
  check(): Promise<UpdateInfo | null>;
  /** 直前の check() で見つかった更新をダウンロード+インストールする。 */
  downloadAndInstall(onProgress?: (progress: DownloadProgress) => void): Promise<void>;
  /** インストール後にアプリを再起動する。 */
  relaunch(): Promise<void>;
}

/** @tauri-apps/plugin-updater / plugin-process を実際に呼び出す実装。 */
export function createUpdaterApi(): UpdaterApi {
  // downloadAndInstall は直前の check() が返した Update オブジェクトに対して呼ぶ必要がある
  // (再度 check() すると別バージョンを引く可能性がある)ため、ここに保持する。
  let pending: Update | null = null;
  return {
    async check() {
      if (pending) {
        // 前回の check() が確保した Rust 側リソースを解放する。close() は Promise を返す
        // (失敗しうる)ため await した上で、失敗しても新しい check() の続行は妨げない。
        try {
          await pending.close();
        } catch {
          // 握りつぶす(上記コメント参照)
        }
        pending = null;
      }
      const update = await check();
      pending = update;
      if (!update) return null;
      return { version: update.version, currentVersion: update.currentVersion };
    },
    async downloadAndInstall(onProgress) {
      if (!pending) {
        throw new Error("check() で更新が見つかっていません");
      }
      let downloaded = 0;
      let contentLength: number | null = null;
      await pending.downloadAndInstall((event) => {
        if (event.event === "Started") {
          contentLength = event.data.contentLength ?? null;
        } else if (event.event === "Progress") {
          downloaded += event.data.chunkLength;
        }
        onProgress?.({ downloaded, contentLength });
      });
    },
    async relaunch() {
      await relaunch();
    },
  };
}

export const updaterApi: UpdaterApi = createUpdaterApi();
