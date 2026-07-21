import { describe, expect, it } from "vitest";
import { buildOverview } from "./Dashboard";
import type { RawConfig, StatusResponse } from "../types";

const raw: RawConfig = {
  accounts: [
    {
      id: "personal",
      provider: "google",
      calendars: ["primary", "xxxxx@group.calendar.google.com"],
      digest_calendars: ["yyyyy@group.calendar.google.com"],
    },
    {
      id: "work-ms",
      provider: "microsoft",
      calendars: ["primary"],
    },
  ],
};

const status: StatusResponse = {
  daemon: { mode: "launchd", running: true },
  tokens: [
    { account_id: "personal", state: "ok" },
    { account_id: "work-ms", state: "missing" },
  ],
  calendars: [
    { account_id: "personal", calendar_id: "primary", last_sync: "2026-07-20T01:00:00Z", status: "ok" },
    {
      account_id: "personal",
      calendar_id: "xxxxx@group.calendar.google.com",
      last_sync: "2026-07-21T03:00:00Z",
      status: "ok",
    },
  ],
};

describe("buildOverview", () => {
  it("google account with 2 watched calendars + 1 digest calendar: watched/digest/blocker(既定 primary) が期待通り", () => {
    const rows = buildOverview(raw, status);
    const personal = rows.find((r) => r.accountId === "personal");
    expect(personal).toBeDefined();
    expect(personal?.provider).toBe("google");
    expect(personal?.watched).toEqual(["primary", "xxxxx@group.calendar.google.com"]);
    expect(personal?.digest).toEqual(["yyyyy@group.calendar.google.com"]);
    expect(personal?.blocker).toBe("primary");
  });

  it("最終同期は status.calendars を accountId で突合し、最も新しいものを表示する", () => {
    const rows = buildOverview(raw, status);
    const personal = rows.find((r) => r.accountId === "personal");
    expect(personal?.lastSync).toBe("2026-07-21T03:00:00Z");
    expect(personal?.syncStatus).toBe("ok");
  });

  it("microsoft account with a missing token reports tokenState missing / watched は既定 primary / digest は空", () => {
    const rows = buildOverview(raw, status);
    const workMs = rows.find((r) => r.accountId === "work-ms");
    expect(workMs).toBeDefined();
    expect(workMs?.provider).toBe("microsoft");
    expect(workMs?.watched).toEqual(["primary"]);
    expect(workMs?.blocker).toBe("primary");
    expect(workMs?.digest).toEqual([]);
    expect(workMs?.tokenState).toBe("missing");
  });

  it("status.calendars に突合するエントリが無いアカウントは lastSync/syncStatus が '-' になる", () => {
    const rows = buildOverview(raw, status);
    const workMs = rows.find((r) => r.accountId === "work-ms");
    expect(workMs?.lastSync).toBe("-");
    expect(workMs?.syncStatus).toBe("-");
  });

  it("tokens にエントリが無いアカウントは tokenState が 'unknown' になる", () => {
    const noToken: StatusResponse = { ...status, tokens: [] };
    const rows = buildOverview(raw, noToken);
    expect(rows.every((r) => r.tokenState === "unknown")).toBe(true);
  });

  it("accounts が無い設定は空配列を返す", () => {
    expect(buildOverview({}, status)).toEqual([]);
  });
});
