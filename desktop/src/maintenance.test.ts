import { describe, expect, it } from "vitest";
import { daemonRunningLabel, isMaintenanceBlocking } from "./maintenance";
import type { MaintenanceState } from "./types";

function state(phase: MaintenanceState["phase"]): MaintenanceState {
  return { phase, log_path: "", error: "" };
}

describe("isMaintenanceBlocking", () => {
  it("state が null(未取得)なら false", () => {
    expect(isMaintenanceBlocking(null)).toBe(false);
  });

  it("phase が running のときだけ true", () => {
    expect(isMaintenanceBlocking(state("running"))).toBe(true);
    expect(isMaintenanceBlocking(state("idle"))).toBe(false);
    expect(isMaintenanceBlocking(state("done"))).toBe(false);
    expect(isMaintenanceBlocking(state("error"))).toBe(false);
  });
});

describe("daemonRunningLabel", () => {
  it("メンテナンス実行中は daemon.running の値に関わらず「メンテナンス中」", () => {
    expect(daemonRunningLabel(true, state("running"))).toBe("メンテナンス中");
    expect(daemonRunningLabel(false, state("running"))).toBe("メンテナンス中");
  });

  it("メンテナンス実行中でなければ daemon.running をそのまま反映する", () => {
    expect(daemonRunningLabel(true, state("idle"))).toBe("稼働中");
    expect(daemonRunningLabel(false, state("idle"))).toBe("停止中");
    expect(daemonRunningLabel(true, null)).toBe("稼働中");
    expect(daemonRunningLabel(false, null)).toBe("停止中");
  });

  it("done/error に遷移した後は稼働中/停止中の通常表示に戻る(バナー解消と同じ判定基準)", () => {
    expect(daemonRunningLabel(true, state("done"))).toBe("稼働中");
    expect(daemonRunningLabel(false, state("error"))).toBe("停止中");
  });
});
