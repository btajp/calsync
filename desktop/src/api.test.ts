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
});
