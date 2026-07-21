import { describe, expect, it } from "vitest";
import { ApiClient } from "./api";
import { parseHandshake } from "./sidecar";

describe("parseHandshake", () => {
  it("parses the sidecar handshake line", () => {
    expect(parseHandshake('{"port":12345,"token":"abc"}')).toEqual({ port: 12345, token: "abc" });
  });
  it("throws on garbage", () => {
    expect(() => parseHandshake("boot noise")).toThrow();
  });
});

describe("ApiClient", () => {
  it("sends bearer token and parses errors", async () => {
    const calls: { url: string; init: RequestInit }[] = [];
    const fakeFetch = (async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return new Response(JSON.stringify({ code: "conflict", message: "changed", hint: "reload" }), {
        status: 409,
      });
    }) as unknown as typeof fetch;
    const api = new ApiClient("http://127.0.0.1:1", "tok", fakeFetch);
    await expect(api.getConfig()).rejects.toMatchObject({ code: "conflict" });
    expect(calls[0].url).toBe("http://127.0.0.1:1/api/config");
    expect((calls[0].init.headers as Record<string, string>)["Authorization"]).toBe("Bearer tok");
  });

  it("default fetch keeps a safe this binding (WebKit regression, desktop-v0.1.0)", async () => {
    // WebKit は fetch を Window 以外の this で呼ぶと TypeError を投げる。
    // this に敏感なフェイクをグローバルに挿して、既定経路(第3引数なし)が
    // 呼び出し時に this を持ち込まないことを検証する。
    const orig = globalThis.fetch;
    function thisSensitiveFetch(this: unknown): Promise<Response> {
      if (this !== undefined && this !== globalThis) {
        throw new TypeError("Can only call Window.fetch on instances of Window");
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }
    globalThis.fetch = thisSensitiveFetch as unknown as typeof fetch;
    try {
      const api = new ApiClient("http://127.0.0.1:1", "tok");
      await expect(api.status()).resolves.toBeTruthy();
    } finally {
      globalThis.fetch = orig;
    }
  });
});
