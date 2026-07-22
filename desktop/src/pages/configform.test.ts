import { describe, expect, it } from "vitest";
import { normalizeRaw } from "./ConfigForm";
import type { RawConfig } from "../types";

describe("normalizeRaw", () => {
  it("空文字の poll_interval と空配列の calendars を undefined に落とす(brief step1 の例)", () => {
    const input: RawConfig = {
      poll_interval: "",
      accounts: [{ id: "personal", provider: "google", calendars: [] }],
    };
    const out = normalizeRaw(input);
    expect(out.poll_interval).toBeUndefined();
    expect(out.accounts?.[0].calendars).toBeUndefined();
    expect(out.accounts?.[0].id).toBe("personal");
    expect(out.accounts?.[0].provider).toBe("google");
  });

  it("値があるトップレベル文字列フィールドは素通しする", () => {
    const input: RawConfig = {
      poll_interval: "5m",
      sync_window: "3mo",
      blocker_title: "予定あり",
      reconcile_at: "04:00",
    };
    expect(normalizeRaw(input)).toEqual(input);
  });

  it("busy_show_as の空配列は undefined、値があれば素通しする", () => {
    expect(normalizeRaw({ busy_show_as: [] }).busy_show_as).toBeUndefined();
    expect(normalizeRaw({ busy_show_as: ["busy", "oof"] }).busy_show_as).toEqual(["busy", "oof"]);
  });

  it("dedupe_same_meeting は undefined/true/false を区別して保つ(既定/true/false の3択)", () => {
    expect(normalizeRaw({}).dedupe_same_meeting).toBeUndefined();
    expect(normalizeRaw({ dedupe_same_meeting: true }).dedupe_same_meeting).toBe(true);
    expect(normalizeRaw({ dedupe_same_meeting: false }).dedupe_same_meeting).toBe(false);
  });

  it("account.show_origin_in_description も undefined/true/false を区別して保つ", () => {
    const base = { id: "personal", provider: "google" };
    expect(normalizeRaw({ accounts: [{ ...base }] }).accounts?.[0].show_origin_in_description).toBeUndefined();
    expect(
      normalizeRaw({ accounts: [{ ...base, show_origin_in_description: true }] }).accounts?.[0]
        .show_origin_in_description,
    ).toBe(true);
    expect(
      normalizeRaw({ accounts: [{ ...base, show_origin_in_description: false }] }).accounts?.[0]
        .show_origin_in_description,
    ).toBe(false);
  });

  it("account の email/blocker_calendar の空文字と digest_calendars の空配列を undefined に落とす", () => {
    const out = normalizeRaw({
      accounts: [{ id: "personal", provider: "google", email: "", blocker_calendar: "", digest_calendars: [] }],
    });
    const a = out.accounts?.[0];
    expect(a?.email).toBeUndefined();
    expect(a?.blocker_calendar).toBeUndefined();
    expect(a?.digest_calendars).toBeUndefined();
  });

  it("accounts が空配列なら undefined に落とす", () => {
    expect(normalizeRaw({ accounts: [] }).accounts).toBeUndefined();
  });

  it("detail_sync: visibility の空文字は undefined、fields/from/to は値があれば素通し", () => {
    const out = normalizeRaw({
      detail_sync: [{ from: "personal", to: "work-ms", fields: ["title", "description"], visibility: "" }],
    });
    const d = out.detail_sync?.[0];
    expect(d?.from).toBe("personal");
    expect(d?.to).toBe("work-ms");
    expect(d?.fields).toEqual(["title", "description"]);
    expect(d?.visibility).toBeUndefined();
  });

  it("detail_sync が空配列なら undefined に落とす", () => {
    expect(normalizeRaw({ detail_sync: [] }).detail_sync).toBeUndefined();
  });

  it("notifications.slack は全フィールド空なら slack ごと undefined にする(未設定の空キーを YAML に出さないため)", () => {
    const out = normalizeRaw({
      notifications: { slack: { bot_token_env: "", channel: "", morning_digest: "", remind_before: "" } },
    });
    expect(out.notifications?.slack).toBeUndefined();
  });

  it("notifications.slack は値のあるフィールドだけ残す", () => {
    const out = normalizeRaw({
      notifications: { slack: { bot_token_env: "", channel: "C123", morning_digest: "", remind_before: "" } },
    });
    expect(out.notifications?.slack).toEqual({ channel: "C123" });
  });

  it("providers は google/microsoft とも空文字なら providers ごと undefined にする", () => {
    const out = normalizeRaw({ providers: { google: { credentials_file: "" }, microsoft: { client_id: "" } } });
    expect(out.providers).toBeUndefined();
  });

  it("providers は値がある方だけ残す", () => {
    const out = normalizeRaw({ providers: { google: { credentials_file: "/path/to/creds.json" } } });
    expect(out.providers).toEqual({ google: { credentials_file: "/path/to/creds.json" } });
  });

  it("providers は spread-then-override(F9): raw.providers の未知フィールドを保ったまま google/microsoft だけ正規化する", () => {
    // RawConfig 型に無い将来フィールドが providers 直下に来ても消さないことを確認する
    // (account/detail_sync の正規化と同じ spread-then-override へ統一した回帰テスト)。
    const input = {
      providers: { google: { credentials_file: "/path/to/creds.json" }, futureField: "keep-me" },
    } as unknown as RawConfig;
    const out = normalizeRaw(input);
    expect(out.providers).toMatchObject({ google: { credentials_file: "/path/to/creds.json" }, futureField: "keep-me" });
  });
});
