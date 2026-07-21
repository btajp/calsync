import { describe, expect, it, vi } from "vitest";
import {
  bannerReducer,
  initialBannerState,
  runCheck,
  runDownloadAndInstall,
  type BannerAction,
} from "./UpdateBanner";
import type { UpdaterApi } from "../updater";

const info = { version: "1.2.3", currentVersion: "1.0.0" };

function collector() {
  const actions: BannerAction[] = [];
  return { actions, dispatch: (a: BannerAction) => actions.push(a) };
}

describe("bannerReducer", () => {
  it("check-started は idle から checking へ遷移する", () => {
    expect(bannerReducer(initialBannerState, { type: "check-started" })).toEqual({ phase: "checking" });
  });

  it("check-succeeded に info があれば available へ遷移する", () => {
    const state = bannerReducer({ phase: "checking" }, { type: "check-succeeded", info });
    expect(state).toEqual({ phase: "available", info });
  });

  it("check-succeeded の info が null なら up-to-date へ遷移する", () => {
    const state = bannerReducer({ phase: "checking" }, { type: "check-succeeded", info: null });
    expect(state).toEqual({ phase: "up-to-date" });
  });

  it("check-failed(silent: true) は idle に戻り、エラーを表面化しない(起動時チェックの握りつぶし)", () => {
    const state = bannerReducer({ phase: "checking" }, { type: "check-failed", message: "offline", silent: true });
    expect(state).toEqual({ phase: "idle" });
  });

  it("check-failed(silent: false) は error へ遷移する(手動確認の失敗)", () => {
    const state = bannerReducer({ phase: "checking" }, { type: "check-failed", message: "offline", silent: false });
    expect(state).toEqual({ phase: "error", message: "offline" });
  });

  it("download-started は available から downloading へ遷移し、進捗を0で初期化する", () => {
    const state = bannerReducer({ phase: "available", info }, { type: "download-started", info });
    expect(state).toEqual({ phase: "downloading", info, progress: { downloaded: 0, contentLength: null } });
  });

  it("download-progress は downloading フェーズでのみ進捗を更新する", () => {
    const downloading = { phase: "downloading" as const, info, progress: { downloaded: 0, contentLength: null } };
    const progress = { downloaded: 50, contentLength: 100 };
    expect(bannerReducer(downloading, { type: "download-progress", progress })).toEqual({
      phase: "downloading",
      info,
      progress,
    });
  });

  it("download-progress は downloading 以外のフェーズでは無視する", () => {
    const state = bannerReducer(initialBannerState, {
      type: "download-progress",
      progress: { downloaded: 1, contentLength: 2 },
    });
    expect(state).toEqual(initialBannerState);
  });

  it("download-failed は error へ遷移する", () => {
    const downloading = { phase: "downloading" as const, info, progress: { downloaded: 0, contentLength: null } };
    expect(bannerReducer(downloading, { type: "download-failed", message: "network error" })).toEqual({
      phase: "error",
      message: "network error",
    });
  });
});

function fakeApi(overrides: Partial<UpdaterApi> = {}): UpdaterApi {
  return {
    check: vi.fn().mockResolvedValue(null),
    downloadAndInstall: vi.fn().mockResolvedValue(undefined),
    relaunch: vi.fn().mockResolvedValue(undefined),
    ...overrides,
  };
}

describe("runCheck", () => {
  it("更新が見つかったら check-started → check-succeeded(info) を dispatch する", async () => {
    const api = fakeApi({ check: vi.fn().mockResolvedValue(info) });
    const { actions, dispatch } = collector();
    await runCheck(api, dispatch, { silent: false });
    expect(actions).toEqual([{ type: "check-started" }, { type: "check-succeeded", info }]);
  });

  it("更新が無ければ check-succeeded(null) を dispatch する", async () => {
    const api = fakeApi();
    const { actions, dispatch } = collector();
    await runCheck(api, dispatch, { silent: false });
    expect(actions).toEqual([{ type: "check-started" }, { type: "check-succeeded", info: null }]);
  });

  it("check() が失敗したら silent フラグをそのまま check-failed へ渡す", async () => {
    const api = fakeApi({ check: vi.fn().mockRejectedValue(new Error("offline")) });
    const { actions, dispatch } = collector();
    await runCheck(api, dispatch, { silent: true });
    expect(actions).toEqual([
      { type: "check-started" },
      { type: "check-failed", message: "Error: offline", silent: true },
    ]);
  });
});

describe("runDownloadAndInstall", () => {
  it("download-started → 進捗 → relaunch の順で実行する", async () => {
    const api = fakeApi({
      downloadAndInstall: vi.fn(async (onProgress?: (p: { downloaded: number; contentLength: number | null }) => void) => {
        onProgress?.({ downloaded: 100, contentLength: 100 });
      }),
    });
    const { actions, dispatch } = collector();
    await runDownloadAndInstall(api, info, dispatch);
    expect(actions).toEqual([
      { type: "download-started", info },
      { type: "download-progress", progress: { downloaded: 100, contentLength: 100 } },
    ]);
    expect(api.relaunch).toHaveBeenCalledOnce();
  });

  it("ダウンロードに失敗したら download-failed を dispatch し relaunch しない", async () => {
    const api = fakeApi({ downloadAndInstall: vi.fn().mockRejectedValue(new Error("write failed")) });
    const { actions, dispatch } = collector();
    await runDownloadAndInstall(api, info, dispatch);
    expect(actions).toEqual([
      { type: "download-started", info },
      { type: "download-failed", message: "Error: write failed" },
    ]);
    expect(api.relaunch).not.toHaveBeenCalled();
  });
});
