import { describe, expect, it } from "vitest";
import { nextEventLabel, scheduleFetchRange } from "./tray";
import type { EventOut } from "./types";

function baseEvent(overrides: Partial<EventOut> = {}): EventOut {
  return {
    account_id: "personal",
    account_ids: ["personal"],
    title: "定例MTG",
    start: "2026-07-21T10:00:00+09:00",
    end: "2026-07-21T11:00:00+09:00",
    all_day: false,
    all_day_start: "",
    all_day_end: "",
    meeting_url: "",
    html_link: "",
    ...overrides,
  };
}

// now は常にローカル(実行環境の)時刻として構築する。CalendarView のテストと同様、
// TZ に依存しない絶対 UTC 文字列は使わず new Date(year, month, day, ...) で組み立てる。
function local(y: number, m: number, d: number, h = 0, min = 0): Date {
  return new Date(y, m - 1, d, h, min, 0, 0);
}

describe("nextEventLabel", () => {
  it("events が空なら \"\"", () => {
    expect(nextEventLabel([], local(2026, 7, 21, 10, 0))).toBe("");
  });

  it("終日イベントのみなら対象外で \"\"", () => {
    const ev = baseEvent({ all_day: true, all_day_start: "2026-07-21", start: "2026-07-21T00:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 9, 0))).toBe("");
  });

  it("過去のイベントのみなら \"\"", () => {
    const ev = baseEvent({ start: "2026-07-21T09:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 10, 0))).toBe("");
  });

  it("60分未満: 「N分後 タイトル」", () => {
    const ev = baseEvent({ title: "週次定例", start: "2026-07-21T10:15:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 10, 0))).toBe("15分後 週次定例");
  });

  it("59分後は「59分後」のまま(60分未満の境界)", () => {
    const ev = baseEvent({ title: "週次定例", start: "2026-07-21T10:59:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 10, 0))).toBe("59分後 週次定例");
  });

  it("ちょうど60分後は「HH:MM」表示に切り替わる(60分以上の境界)", () => {
    const ev = baseEvent({ title: "週次定例", start: "2026-07-21T11:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 10, 0))).toBe("11:00 週次定例");
  });

  it("開始が now と同時刻(diff=0)は「0分後」", () => {
    const ev = baseEvent({ title: "週次定例", start: "2026-07-21T10:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 10, 0))).toBe("0分後 週次定例");
  });

  it("60分以上・当日: 「HH:MM タイトル」", () => {
    const ev = baseEvent({ title: "週次定例", start: "2026-07-21T14:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 8, 0))).toBe("14:00 週次定例");
  });

  it("翌日: 「明日HH:MM タイトル」", () => {
    const ev = baseEvent({ title: "1on1", start: "2026-07-22T09:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 23, 0))).toBe("明日09:00 1on1");
  });

  it("日付は翌日でも差分60分未満なら「N分後」が優先される", () => {
    const ev = baseEvent({ title: "深夜MTG", start: "2026-07-22T00:10:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 23, 55))).toBe("15分後 深夜MTG");
  });

  it("2〜7日以内: 「M/D タイトル」(時刻は含めない)", () => {
    const ev = baseEvent({ title: "四半期レビュー", start: "2026-07-25T13:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 9, 0))).toBe("7/25 四半期レビュー");
  });

  it("ちょうど7日後は表示される(7日以内の境界)", () => {
    const ev = baseEvent({ title: "月次MTG", start: "2026-07-28T10:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 9, 0))).toBe("7/28 月次MTG");
  });

  it("8日後は表示されない(7日超の境界)", () => {
    const ev = baseEvent({ title: "月次MTG", start: "2026-07-29T10:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 9, 0))).toBe("");
  });

  it("複数イベントから最も早い未来のイベントを選ぶ(順不同・過去/終日混在)", () => {
    const events = [
      baseEvent({ title: "遠い予定", start: "2026-07-25T10:00:00+09:00" }),
      baseEvent({ title: "過去の予定", start: "2026-07-21T08:00:00+09:00" }),
      baseEvent({ title: "終日イベント", all_day: true, all_day_start: "2026-07-21" }),
      baseEvent({ title: "直近の予定", start: "2026-07-21T14:00:00+09:00" }),
    ];
    expect(nextEventLabel(events, local(2026, 7, 21, 9, 0))).toBe("14:00 直近の予定");
  });

  it("タイトルが空文字なら「(無題)」", () => {
    const ev = baseEvent({ title: "", start: "2026-07-21T14:00:00+09:00" });
    expect(nextEventLabel([ev], local(2026, 7, 21, 9, 0))).toBe("14:00 (無題)");
  });

  it("全体で24文字を超える場合は23文字+「…」に切り詰める", () => {
    const longTitle = "とても長い会議タイトルであることのテストケースです";
    const ev = baseEvent({ title: longTitle, start: "2026-07-21T14:00:00+09:00" });
    const label = nextEventLabel([ev], local(2026, 7, 21, 9, 0));
    expect(label.length).toBe(24);
    expect(label.endsWith("…")).toBe(true);
    expect(label).toBe(`14:00 ${longTitle}`.slice(0, 23) + "…");
  });

  it("24文字ちょうどなら切り詰めない", () => {
    // "14:00 " (6文字) + 18文字 = 24文字ちょうど
    const title = "abcdefghijklmnopqr";
    expect(title.length).toBe(18);
    const ev = baseEvent({ title, start: "2026-07-21T14:00:00+09:00" });
    const label = nextEventLabel([ev], local(2026, 7, 21, 9, 0));
    expect(label).toBe(`14:00 ${title}`);
    expect(label.length).toBe(24);
    expect(label.endsWith("…")).toBe(false);
  });
});

describe("scheduleFetchRange", () => {
  it("from はローカル当日0時、to は当日+8日0時(当日+7日を含む排他的終了)", () => {
    const now = local(2026, 7, 21, 15, 30);
    const { from, to } = scheduleFetchRange(now);
    expect(from.startsWith("2026-07-21T00:00:00")).toBe(true);
    expect(to.startsWith("2026-07-29T00:00:00")).toBe(true);
  });

  it("月をまたぐ場合も正しく計算する", () => {
    const now = local(2026, 7, 28, 0, 0);
    const { from, to } = scheduleFetchRange(now);
    expect(from.startsWith("2026-07-28T00:00:00")).toBe(true);
    expect(to.startsWith("2026-08-05T00:00:00")).toBe(true);
  });
});
