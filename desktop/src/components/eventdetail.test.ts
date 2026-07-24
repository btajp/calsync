import { describe, expect, it } from "vitest";
import { deriveEventDetailView, formatEventDateTime, linkifyDescription } from "./EventDetail";
import type { EventOut } from "../types";

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

describe("linkifyDescription", () => {
  it("空文字は空配列", () => {
    expect(linkifyDescription("")).toEqual([]);
  });

  it("URL を含まない文字列は 1 個の text セグメントにする", () => {
    expect(linkifyDescription("ただのメモ")).toEqual([{ type: "text", value: "ただのメモ" }]);
  });

  it("https URL 単体は 1 個の link セグメントにする", () => {
    expect(linkifyDescription("https://example.com/path")).toEqual([
      { type: "link", value: "https://example.com/path" },
    ]);
  });

  it("http URL も link セグメントとして抽出する(開けるかどうかは別判定)", () => {
    expect(linkifyDescription("http://example.com/path")).toEqual([
      { type: "link", value: "http://example.com/path" },
    ]);
  });

  it("http/https 以外のスキーム(mailto:)はリンク化しない", () => {
    expect(linkifyDescription("mailto:user@example.com")).toEqual([
      { type: "text", value: "mailto:user@example.com" },
    ]);
  });

  it("地の文と URL が混在する場合、順序を保ったまま分割する", () => {
    const text = "資料: https://example.com/doc 参照のこと";
    expect(linkifyDescription(text)).toEqual([
      { type: "text", value: "資料: " },
      { type: "link", value: "https://example.com/doc" },
      { type: "text", value: " 参照のこと" },
    ]);
  });

  it("複数の URL をすべて link セグメントとして抽出する", () => {
    const text = "https://a.example.com https://b.example.com";
    expect(linkifyDescription(text)).toEqual([
      { type: "link", value: "https://a.example.com" },
      { type: "text", value: " " },
      { type: "link", value: "https://b.example.com" },
    ]);
  });

  it("改行は text セグメントの value にそのまま残す(表示側の pre-wrap で改行を保つため)", () => {
    const text = "1行目\n2行目 https://example.com\n3行目";
    expect(linkifyDescription(text)).toEqual([
      { type: "text", value: "1行目\n2行目 " },
      { type: "link", value: "https://example.com" },
      { type: "text", value: "\n3行目" },
    ]);
  });
});

describe("formatEventDateTime", () => {
  it("単日の終日イベントは「YYYY/M/D(終日)」", () => {
    const ev = baseEvent({ all_day: true, all_day_start: "2026-07-21", all_day_end: "" });
    expect(formatEventDateTime(ev)).toBe("2026/7/21(終日)");
  });

  it("複数日の終日イベントは範囲表示にする(all_day_end は排他的終了日なので表示は前日まで)", () => {
    const ev = baseEvent({ all_day: true, all_day_start: "2026-07-21", all_day_end: "2026-07-24" });
    expect(formatEventDateTime(ev)).toBe("2026/7/21 〜 2026/7/23(終日)");
  });

  it("同日の時刻ありイベントは「YYYY/M/D HH:MM 〜 HH:MM」", () => {
    const ev = baseEvent({ start: "2026-07-21T10:00:00+09:00", end: "2026-07-21T11:30:00+09:00" });
    expect(formatEventDateTime(ev)).toBe("2026/7/21 10:00 〜 11:30");
  });

  it("複数日にまたがる時刻ありイベントは両端の日付を表示する", () => {
    const ev = baseEvent({ start: "2026-07-21T23:00:00+09:00", end: "2026-07-22T00:30:00+09:00" });
    expect(formatEventDateTime(ev)).toBe("2026/7/21 23:00 〜 2026/7/22 00:30");
  });
});

describe("deriveEventDetailView", () => {
  it("meeting_url が https のときだけ会議に参加ボタンを表示する", () => {
    expect(deriveEventDetailView(baseEvent({ meeting_url: "https://zoom.us/j/1" })).showJoinButton).toBe(true);
    expect(deriveEventDetailView(baseEvent({ meeting_url: "http://zoom.us/j/1" })).showJoinButton).toBe(false);
    expect(deriveEventDetailView(baseEvent({ meeting_url: "" })).showJoinButton).toBe(false);
  });

  it("html_link が https のときだけカレンダーで開くリンクを表示する", () => {
    expect(
      deriveEventDetailView(baseEvent({ html_link: "https://calendar.example.com/event" })).showCalendarLink,
    ).toBe(true);
    expect(deriveEventDetailView(baseEvent({ html_link: "" })).showCalendarLink).toBe(false);
  });

  it("account_ids が空のときは account_id にフォールバックする(理論上の安全弁)", () => {
    const ev = baseEvent({ account_id: "personal", account_ids: [] });
    expect(deriveEventDetailView(ev).accountIds).toEqual(["personal"]);
  });

  it("dedupe 統合された予定は account_ids 全件を表示する", () => {
    const ev = baseEvent({ account_ids: ["personal", "work-ms"] });
    expect(deriveEventDetailView(ev).accountIds).toEqual(["personal", "work-ms"]);
  });

  it("title が空文字なら「(無題)」", () => {
    expect(deriveEventDetailView(baseEvent({ title: "" })).title).toBe("(無題)");
  });

  it("終日イベントの dateTimeLabel は formatEventDateTime と一致する", () => {
    const ev = baseEvent({ all_day: true, all_day_start: "2026-07-21", all_day_end: "" });
    expect(deriveEventDetailView(ev).dateTimeLabel).toBe("2026/7/21(終日)");
  });

  it("description が空文字/未指定なら descriptionSegments は空配列", () => {
    expect(deriveEventDetailView(baseEvent({ description: "" })).descriptionSegments).toEqual([]);
    expect(deriveEventDetailView(baseEvent()).descriptionSegments).toEqual([]);
  });

  it("description のリンクを descriptionSegments に反映する", () => {
    const ev = baseEvent({ description: "議事録: https://example.com/notes" });
    expect(deriveEventDetailView(ev).descriptionSegments).toEqual([
      { type: "text", value: "議事録: " },
      { type: "link", value: "https://example.com/notes" },
    ]);
  });
});
