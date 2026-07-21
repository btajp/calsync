import { describe, expect, it } from "vitest";
import { validateNewAccountId } from "./AccountAdd";

describe("validateNewAccountId", () => {
  it("空文字は必須エラーを返す", () => {
    const err = validateNewAccountId("", []);
    expect(err).not.toBeNull();
  });

  it("許容文字([A-Za-z0-9._-])以外を含む id は形式エラーを返す", () => {
    const err = validateNewAccountId("bad/id", []);
    expect(err).not.toBeNull();
  });

  it("既存 id と重複する場合は重複エラーを返す", () => {
    const err = validateNewAccountId("personal", ["personal"]);
    expect(err).not.toBeNull();
  });

  it("英数字と . _ - のみで構成された未使用の id は null(OK)を返す", () => {
    expect(validateNewAccountId("ok-id.1", [])).toBeNull();
  });

  it('"." はファイルパスとして特別な意味を持つため拒否する(internal/auth の validateAccountID と同様)', () => {
    expect(validateNewAccountId(".", [])).not.toBeNull();
  });

  it('".." はファイルパスとして特別な意味を持つため拒否する(internal/auth の validateAccountID と同様)', () => {
    expect(validateNewAccountId("..", [])).not.toBeNull();
  });

  it("空文字エラーと形式エラーは異なるメッセージになる(利用者が原因を判別できる)", () => {
    const emptyErr = validateNewAccountId("", []);
    const formatErr = validateNewAccountId("bad/id", []);
    expect(emptyErr).not.toEqual(formatErr);
  });
});
