import { describe, expect, it } from "vitest";
import { shouldSuppressShow } from "./panelWindow";

// トレイ再クリックのトグル閉じ判定(macOS の blur → hide → Click 到着順序への対応)。
describe("shouldSuppressShow", () => {
  it("hide 直後(閾値未満)のクリックは再表示を抑制する", () => {
    expect(shouldSuppressShow(1000, 1000, 350)).toBe(true);
    expect(shouldSuppressShow(1000, 1349, 350)).toBe(true);
  });

  it("閾値以上経過したクリックは通常どおり表示する", () => {
    expect(shouldSuppressShow(1000, 1350, 350)).toBe(false);
    expect(shouldSuppressShow(1000, 5000, 350)).toBe(false);
  });

  it("初期状態(hiddenAt=0)や時計の巻き戻しでは抑制しない", () => {
    expect(shouldSuppressShow(0, Date.now(), 350)).toBe(false);
    expect(shouldSuppressShow(2000, 1999, 350)).toBe(false);
  });
});
