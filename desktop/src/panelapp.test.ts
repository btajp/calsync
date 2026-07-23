import { describe, expect, it } from "vitest";
import { buildScheduleList } from "./PanelApp";
import type { EventOut } from "./types";

// 既存フィクスチャ(2026-07-21)がすべて未来になる固定基準時刻
const NOW = new Date("2026-07-21T00:00:00+09:00");

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

describe("buildScheduleList", () => {
  it("events が空なら空配列", () => {
    expect(buildScheduleList([], NOW)).toEqual([]);
  });

  it("時刻ありイベントを日付見出しでグループ化し、時刻を HH:MM にする", () => {
    const ev = baseEvent({ title: "週次定例", start: "2026-07-21T14:00:00+09:00" });
    const days = buildScheduleList([ev], NOW);
    expect(days).toHaveLength(1);
    expect(days[0].dateKey).toBe("2026-07-21");
    expect(days[0].dateLabel).toBe("7/21(火)");
    expect(days[0].items).toEqual([{ time: "14:00", title: "週次定例", accountId: "personal" }]);
  });

  it("終日イベントは all_day_start の日付でグループ化し time を「終日」にする", () => {
    const ev = baseEvent({
      title: "祝日",
      all_day: true,
      all_day_start: "2026-07-23",
      start: "2026-07-23T00:00:00+09:00",
    });
    const days = buildScheduleList([ev], NOW);
    expect(days[0].dateKey).toBe("2026-07-23");
    expect(days[0].items).toEqual([{ time: "終日", title: "祝日", accountId: "personal" }]);
  });

  it("同じ日の終日イベントは時刻ありイベントより先頭に来る", () => {
    const events = [
      baseEvent({ title: "定例", start: "2026-07-21T09:00:00+09:00" }),
      baseEvent({
        title: "祝日",
        all_day: true,
        all_day_start: "2026-07-21",
        start: "2026-07-21T00:00:00+09:00",
      }),
    ];
    const days = buildScheduleList(events, NOW);
    expect(days[0].items.map((i) => i.title)).toEqual(["祝日", "定例"]);
  });

  it("同じ日の時刻ありイベントは開始時刻の昇順に並ぶ", () => {
    const events = [
      baseEvent({ title: "午後", start: "2026-07-21T14:00:00+09:00" }),
      baseEvent({ title: "午前", start: "2026-07-21T09:00:00+09:00" }),
    ];
    const days = buildScheduleList(events, NOW);
    expect(days[0].items.map((i) => i.title)).toEqual(["午前", "午後"]);
  });

  it("日付は昇順に並ぶ(入力順に依存しない)", () => {
    const events = [
      baseEvent({ title: "後日", start: "2026-07-25T10:00:00+09:00" }),
      baseEvent({ title: "当日", start: "2026-07-21T10:00:00+09:00" }),
    ];
    const days = buildScheduleList(events, NOW);
    expect(days.map((d) => d.dateKey)).toEqual(["2026-07-21", "2026-07-25"]);
  });

  it("イベントが無い日付は見出しを出さない(日付の間が飛ぶ)", () => {
    const events = [
      baseEvent({ title: "月曜", start: "2026-07-20T10:00:00+09:00" }),
      baseEvent({ title: "木曜", start: "2026-07-23T10:00:00+09:00" }),
    ];
    const days = buildScheduleList(events, NOW);
    expect(days).toHaveLength(2);
  });

  it("複数アカウントの accountId をそのまま引き継ぐ(色分けは呼び出し側の責務)", () => {
    const events = [
      baseEvent({ title: "A", account_id: "personal", start: "2026-07-21T09:00:00+09:00" }),
      baseEvent({ title: "B", account_id: "work-ms", start: "2026-07-21T10:00:00+09:00" }),
    ];
    const days = buildScheduleList(events, NOW);
    expect(days[0].items.map((i) => i.accountId)).toEqual(["personal", "work-ms"]);
  });

  it("タイトルが空文字なら「(無題)」", () => {
    const ev = baseEvent({ title: "", start: "2026-07-21T14:00:00+09:00" });
    const days = buildScheduleList([ev], NOW);
    expect(days[0].items[0].title).toBe("(無題)");
  });

  it("終わった時刻あり予定は除外し、開催中と未来は残す", () => {
    const now = new Date("2026-07-21T12:00:00+09:00");
    const events = [
      baseEvent({ title: "終了済み", start: "2026-07-21T10:00:00+09:00", end: "2026-07-21T11:00:00+09:00" }),
      baseEvent({ title: "開催中", start: "2026-07-21T11:30:00+09:00", end: "2026-07-21T12:30:00+09:00" }),
      baseEvent({ title: "これから", start: "2026-07-21T15:00:00+09:00", end: "2026-07-21T16:00:00+09:00" }),
    ];
    const days = buildScheduleList(events, now);
    const titles = days.flatMap((d) => d.items.map((i) => i.title));
    expect(titles).toEqual(["開催中", "これから"]);
  });

  it("当日の終日予定は時間帯に関わらず残る", () => {
    const now = new Date("2026-07-21T23:00:00+09:00");
    const ev = baseEvent({ title: "終日イベント", all_day: true, all_day_start: "2026-07-21", start: "", end: "" });
    const days = buildScheduleList([ev], now);
    expect(days.flatMap((d) => d.items.map((i) => i.title))).toEqual(["終日イベント"]);
  });

  it("end が不正な時刻あり予定は安全側で表示する", () => {
    const now = new Date("2026-07-21T12:00:00+09:00");
    const ev = baseEvent({ title: "end不明", start: "2026-07-21T09:00:00+09:00", end: "invalid" });
    const days = buildScheduleList([ev], now);
    expect(days.flatMap((d) => d.items.map((i) => i.title))).toEqual(["end不明"]);
  });
});
