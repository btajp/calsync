import { describe, expect, it } from "vitest";
import { deriveMaintenanceBannerView } from "./MaintenanceBanner";
import type { MaintenanceState } from "../types";

describe("deriveMaintenanceBannerView", () => {
  it("state が null(未取得)なら none", () => {
    expect(deriveMaintenanceBannerView(null, false)).toEqual({ kind: "none" });
  });

  it("idle は none(バナーを出さない)", () => {
    expect(deriveMaintenanceBannerView({ phase: "idle", log_path: "", error: "" }, false)).toEqual({
      kind: "none",
    });
  });

  it("running は dismissed の値に関わらず常に表示する(誤って隠せない)", () => {
    const state: MaintenanceState = { phase: "running", log_path: "", error: "" };
    expect(deriveMaintenanceBannerView(state, false)).toEqual({ kind: "running" });
    expect(deriveMaintenanceBannerView(state, true)).toEqual({ kind: "running" });
  });

  it("done は未 dismiss なら結果表示、dismiss 済みなら none", () => {
    const state: MaintenanceState = { phase: "done", log_path: "", error: "" };
    expect(deriveMaintenanceBannerView(state, false)).toEqual({ kind: "done" });
    expect(deriveMaintenanceBannerView(state, true)).toEqual({ kind: "none" });
  });

  it("error は未 dismiss なら message/logPath 付きで表示、dismiss 済みなら none", () => {
    const state: MaintenanceState = { phase: "error", log_path: "/data/reconcile-x.log", error: "boom" };
    expect(deriveMaintenanceBannerView(state, false)).toEqual({
      kind: "error",
      message: "boom",
      logPath: "/data/reconcile-x.log",
    });
    expect(deriveMaintenanceBannerView(state, true)).toEqual({ kind: "none" });
  });
});
