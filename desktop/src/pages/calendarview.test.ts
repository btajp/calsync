import { describe, expect, it } from "vitest";
import { colorForAccount, formatLocalRFC3339, isHttpsUrl, toFullCalendarEvents } from "./CalendarView";
import type { EventOut } from "../types";

/** テスト対象の Date に対して、実行環境の Date.getTimezoneOffset() から期待されるオフセット文字列を計算する。 */
function expectedOffsetSuffix(d: Date): string {
  const offsetMinutes = -d.getTimezoneOffset();
  const sign = offsetMinutes >= 0 ? "+" : "-";
  const offH = String(Math.floor(Math.abs(offsetMinutes) / 60)).padStart(2, "0");
  const offM = String(Math.abs(offsetMinutes) % 60).padStart(2, "0");
  return `${sign}${offH}:${offM}`;
}

describe("formatLocalRFC3339", () => {
  // オフセットをハードコードせず、実行環境の Date.getTimezoneOffset() と整合する文字列かを
  // 検証する(CI 等 JST 以外の TZ で動いても壊れないようにするため)。
  it("Date.getTimezoneOffset() と整合するオフセット付き RFC3339 文字列を返す", () => {
    const d = new Date(2026, 6, 21, 10, 30, 5); // 2026-07-21 10:30:05 (ローカル)
    expect(formatLocalRFC3339(d)).toBe(`2026-07-21T10:30:05${expectedOffsetSuffix(d)}`);
  });

  it("月/日/時/分/秒をゼロパディングする", () => {
    const d = new Date(2026, 0, 5, 3, 4, 5); // 2026-01-05 03:04:05
    expect(formatLocalRFC3339(d)).toBe(`2026-01-05T03:04:05${expectedOffsetSuffix(d)}`);
  });

  it("Date.toISOString()(常に UTC)とは異なり Z を含まない(UTC 送信禁止の回帰)", () => {
    const d = new Date(2026, 6, 21, 0, 0, 0);
    expect(formatLocalRFC3339(d)).not.toContain("Z");
  });
});

describe("colorForAccount", () => {
  it("orderedIds の定義順でパレットを巡回する", () => {
    const ids = ["personal", "work-ms", "work-a"];
    expect(colorForAccount("personal", ids)).toBe("#4285F4");
    expect(colorForAccount("work-ms", ids)).toBe("#0F9D58");
    expect(colorForAccount("work-a", ids)).toBe("#F4B400");
  });

  it("orderedIds に無いアカウントは未知色(#999999)にフォールバックする", () => {
    expect(colorForAccount("ghost", ["personal", "work-ms"])).toBe("#999999");
  });

  it("パレット8色を超えた分は先頭から再度巡回する(Slack ダイジェストの colorFor と同じ規則)", () => {
    const ids = Array.from({ length: 9 }, (_, i) => `acct-${i}`);
    expect(colorForAccount("acct-8", ids)).toBe(colorForAccount("acct-0", ids));
  });
});

function baseEvent(overrides: Partial<EventOut> = {}): EventOut {
  return {
    account_id: "personal",
    account_ids: ["personal"],
    title: "定例MTG",
    start: "2026-07-21T10:00:00+09:00",
    end: "2026-07-21T11:00:00+09:00",
    all_day: false,
    all_day_start: "",
    meeting_url: "",
    html_link: "",
    ...overrides,
  };
}

describe("isHttpsUrl", () => {
  it("https URL は true(既定ブラウザで開く対象)", () => {
    expect(isHttpsUrl("https://calendar.example.com/event")).toBe(true);
  });

  it("http・他スキーム・不正な文字列は false(shell:allow-open に scope 機能が無いための呼び出し側防御)", () => {
    expect(isHttpsUrl("http://calendar.example.com/event")).toBe(false);
    expect(isHttpsUrl("file:///etc/passwd")).toBe(false);
    expect(isHttpsUrl("javascript:alert(1)")).toBe(false);
    expect(isHttpsUrl("not a url")).toBe(false);
    expect(isHttpsUrl("")).toBe(false);
  });
});

describe("toFullCalendarEvents", () => {
  const colorOf = (id: string) => (id === "personal" ? "#4285F4" : "#999999");

  it("時刻ありイベントは start/end をそのまま使い allDay: false にする", () => {
    const [out] = toFullCalendarEvents([baseEvent()], colorOf);
    expect(out.start).toBe("2026-07-21T10:00:00+09:00");
    expect(out.end).toBe("2026-07-21T11:00:00+09:00");
    expect(out.allDay).toBe(false);
  });

  it("終日イベントは all_day_start を start にし allDay: true にする(EventOut に all_day_end は無いため end は指定しない)", () => {
    const ev = baseEvent({
      all_day: true,
      all_day_start: "2026-07-21",
      start: "2026-07-21T00:00:00+09:00",
      end: "2026-07-22T00:00:00+09:00",
    });
    const [out] = toFullCalendarEvents([ev], colorOf);
    expect(out.start).toBe("2026-07-21");
    expect(out.allDay).toBe(true);
    expect(out.end).toBeUndefined();
  });

  it("backgroundColor/borderColor を colorOf(代表アカウント = account_id) から設定する", () => {
    const [out] = toFullCalendarEvents([baseEvent({ account_id: "personal" })], colorOf);
    expect(out.backgroundColor).toBe("#4285F4");
    expect(out.borderColor).toBe("#4285F4");

    const [out2] = toFullCalendarEvents([baseEvent({ account_id: "work-ms" })], colorOf);
    expect(out2.backgroundColor).toBe("#999999");
    expect(out2.borderColor).toBe("#999999");
  });

  it("title が空文字なら「(無題)」にする", () => {
    const [out] = toFullCalendarEvents([baseEvent({ title: "" })], colorOf);
    expect(out.title).toBe("(無題)");
  });

  it("meeting_url/html_link/account_ids を extendedProps に引き継ぐ", () => {
    const ev = baseEvent({
      meeting_url: "https://zoom.us/my/example",
      html_link: "https://calendar.example.com/event",
      account_ids: ["personal", "work-ms"],
    });
    const [out] = toFullCalendarEvents([ev], colorOf);
    expect(out.extendedProps).toEqual({
      meetingUrl: "https://zoom.us/my/example",
      htmlLink: "https://calendar.example.com/event",
      accountIds: ["personal", "work-ms"],
    });
  });
});
