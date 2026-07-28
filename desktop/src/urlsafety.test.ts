import { describe, expect, it } from "vitest";
import { isHttpsUrl, meetingService, zoomAppUrl } from "./urlSafety";

describe("isHttpsUrl", () => {
  it("https のみ true", () => {
    expect(isHttpsUrl("https://example.com/x")).toBe(true);
    expect(isHttpsUrl("http://example.com/x")).toBe(false);
    expect(isHttpsUrl("javascript:alert(1)")).toBe(false);
    expect(isHttpsUrl("")).toBe(false);
  });
});

describe("zoomAppUrl", () => {
  it("標準の /j/ 会議 URL を zoommtg スキームへ変換する(pwd 保持)", () => {
    expect(zoomAppUrl("https://zoom.us/j/88630764097?pwd=abcDEF.1")).toBe(
      "zoommtg://zoom.us/join?action=join&confno=88630764097&pwd=abcDEF.1",
    );
  });

  it("vanity ホスト(company.zoom.us)を保持する", () => {
    expect(zoomAppUrl("https://example.zoom.us/j/123456789")).toBe(
      "zoommtg://example.zoom.us/join?action=join&confno=123456789",
    );
  });

  it("zoomgov.com も変換対象", () => {
    expect(zoomAppUrl("https://example.zoomgov.com/j/123456789")).toBe(
      "zoommtg://example.zoomgov.com/join?action=join&confno=123456789",
    );
  });

  it("パーソナルリンク(/my/)は変換しない(confno が無い)", () => {
    expect(zoomAppUrl("https://zoom.us/my/example")).toBeNull();
  });

  it("Zoom 以外のホストは変換しない(suffix 偽装 host も含む)", () => {
    expect(zoomAppUrl("https://meet.google.com/abc-defg-hij")).toBeNull();
    expect(zoomAppUrl("https://evilzoom.us/j/123456789")).toBeNull();
    expect(zoomAppUrl("https://zoom.us.evil.example/j/123456789")).toBeNull();
  });

  it("https 以外・パース不能は変換しない", () => {
    expect(zoomAppUrl("http://zoom.us/j/123456789")).toBeNull();
    expect(zoomAppUrl("not a url")).toBeNull();
  });

  it("confno が数値でない・桁が異常なパスは変換しない", () => {
    expect(zoomAppUrl("https://zoom.us/j/abc")).toBeNull();
    expect(zoomAppUrl("https://zoom.us/j/123")).toBeNull();
    expect(zoomAppUrl("https://zoom.us/j/123456789/extra")).toBeNull();
  });

  it("pwd は URL エンコードして引き継ぐ", () => {
    expect(zoomAppUrl("https://zoom.us/j/123456789?pwd=a%2Bb")).toBe(
      "zoommtg://zoom.us/join?action=join&confno=123456789&pwd=a%2Bb",
    );
  });
});

describe("meetingService", () => {
  it("Zoom(zoom.us / vanity / zoomgov)を判定する", () => {
    expect(meetingService("https://zoom.us/j/123456789")?.key).toBe("zoom");
    expect(meetingService("https://example.zoom.us/j/123456789")?.key).toBe("zoom");
    expect(meetingService("https://example.zoomgov.com/j/123456789")?.key).toBe("zoom");
  });

  it("Google Meet を判定する", () => {
    expect(meetingService("https://meet.google.com/abc-defg-hij")?.key).toBe("meet");
  });

  it("Microsoft Teams(teams.microsoft.com / teams.live.com)を判定する", () => {
    expect(meetingService("https://teams.microsoft.com/l/meetup-join/xxx")?.key).toBe("teams");
    expect(meetingService("https://teams.live.com/meet/12345")?.key).toBe("teams");
  });

  it("Webex(*.webex.com)を判定する", () => {
    expect(meetingService("https://example.webex.com/example/j.php?MTID=x")?.key).toBe("webex");
  });

  it("不明なホスト・suffix 偽装・パース不能は null", () => {
    expect(meetingService("https://example.com/meet")).toBeNull();
    expect(meetingService("https://evilzoom.us/j/1")).toBeNull();
    expect(meetingService("https://meet.google.com.evil.example/x")).toBeNull();
    expect(meetingService("not a url")).toBeNull();
  });
});
