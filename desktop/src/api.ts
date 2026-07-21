import type { AuthState, CalendarListEntry, RawConfig, StatusResponse } from "./types";

export class ApiError extends Error {
  constructor(
    public code: string,
    message: string,
    public hint?: string,
    public status?: number,
  ) {
    super(message);
  }
}

export class ApiClient {
  private fetchFn: typeof fetch;

  constructor(
    private baseUrl: string,
    private token: string,
    fetchFn?: typeof fetch,
  ) {
    // 既定はグローバル fetch を呼び出し時に解決するラッパーにする。`fetch` を
    // そのまま既定値にすると this.fetchFn(...) 呼び出しで this が失われ、WebKit は
    // "Can only call Window.fetch on instances of Window" で全リクエストが失敗する
    // (desktop-v0.1.0 の実障害)。
    this.fetchFn = fetchFn ?? ((input, init) => fetch(input, init));
  }

  private async req<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await this.fetchFn(`${this.baseUrl}${path}`, {
      method,
      headers: {
        Authorization: `Bearer ${this.token}`,
        ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
      },
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
    const text = await res.text();
    const data = text ? JSON.parse(text) : {};
    if (!res.ok) {
      throw new ApiError(data.code ?? "unknown", data.message ?? res.statusText, data.hint, res.status);
    }
    return data as T;
  }

  status() { return this.req<StatusResponse>("GET", "/api/status"); }
  getConfig() { return this.req<{ raw: RawConfig; mtime: string }>("GET", "/api/config"); }
  putConfig(raw: RawConfig, baseMtime: string) {
    return this.req<{ ok: boolean; mtime: string }>("PUT", "/api/config", { raw, base_mtime: baseMtime });
  }
  listCalendars(id: string) {
    return this.req<{ calendars: CalendarListEntry[] }>("GET", `/api/accounts/${encodeURIComponent(id)}/calendars?provider=google`);
  }
  authStart(accountId: string, provider: string) {
    return this.req<{ ok: boolean }>("POST", "/api/auth/start", { account_id: accountId, provider });
  }
  authState() { return this.req<AuthState>("GET", "/api/auth/state"); }
  authCancel() { return this.req<{ ok: boolean }>("POST", "/api/auth/cancel"); }
  daemon(action: "start" | "stop" | "restart") {
    return this.req<{ ok: boolean }>("POST", `/api/daemon/${action}`);
  }
  doctor() { return this.req<{ ok: boolean; text: string }>("GET", "/api/doctor"); }
}
