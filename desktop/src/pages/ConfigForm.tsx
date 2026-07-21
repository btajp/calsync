import { useCallback, useEffect, useState } from "react";
import type { ApiClient } from "../api";
import { ApiError } from "../api";
import type { RawAccount, RawConfig, RawDetailSync, RawSlack } from "../types";

const BUSY_SHOW_AS_OPTIONS = ["free", "tentative", "busy", "oof", "workingElsewhere", "unknown"] as const;
const DETAIL_SYNC_FIELD_OPTIONS = ["title", "description"] as const;
const VISIBILITY_OPTIONS: { value: string; label: string }[] = [
  { value: "", label: "未指定(private 相当)" },
  { value: "private", label: "private" },
  { value: "default", label: "default" },
  { value: "public", label: "public" },
];

function strOrUndef(v: string | undefined): string | undefined {
  return v === undefined || v === "" ? undefined : v;
}

function arrOrUndef<T>(v: T[] | undefined): T[] | undefined {
  return v && v.length > 0 ? v : undefined;
}

/** カンマ区切りテキストを空要素を除いた文字列配列に変換する。 */
function csvToList(text: string): string[] {
  return text
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

// id/provider や show_origin_in_description(*bool 相当)は他フィールドの後ろで spread するので
// ここでは触らず素通しする(値も、キー自体の有無も変えない)。
function normalizeAccount(a: RawAccount): RawAccount {
  return {
    ...a,
    email: strOrUndef(a.email),
    calendars: arrOrUndef(a.calendars),
    digest_calendars: arrOrUndef(a.digest_calendars),
    blocker_calendar: strOrUndef(a.blocker_calendar),
  };
}

function normalizeDetailSync(d: RawDetailSync): RawDetailSync {
  return {
    ...d,
    from: strOrUndef(d.from),
    to: strOrUndef(d.to),
    fields: arrOrUndef(d.fields),
    visibility: strOrUndef(d.visibility),
  };
}

function normalizeSlack(s: RawSlack | undefined): RawSlack | undefined {
  if (!s) return undefined;
  const out: RawSlack = {
    ...s,
    bot_token_env: strOrUndef(s.bot_token_env),
    channel: strOrUndef(s.channel),
    morning_digest: strOrUndef(s.morning_digest),
    remind_before: strOrUndef(s.remind_before),
  };
  const hasValue =
    out.bot_token_env !== undefined ||
    out.channel !== undefined ||
    out.morning_digest !== undefined ||
    out.remind_before !== undefined;
  return hasValue ? out : undefined;
}

/**
 * フォーム値(RawConfig)を PUT 送信用に整形する純関数。
 * 「フォームが値を持たない = キーを出力しない」を徹底し、空文字/空配列/全フィールド未入力の
 * サブオブジェクトを undefined に落とす(YAML への空キー混入を防ぐ)。特に notifications.slack は
 * ポインタ型(*RawSlack 相当)なので、空オブジェクトのまま送ると「channel 未設定」の検証エラーに
 * なる。dedupe_same_meeting / show_origin_in_description は *bool 相当(undefined/true/false の
 * 3値)のため、他のフィールドと違い値をそのまま素通しする(false を undefined に潰さない)。
 */
export function normalizeRaw(raw: RawConfig): RawConfig {
  const slack = normalizeSlack(raw.notifications?.slack);
  const googleFile = strOrUndef(raw.providers?.google?.credentials_file);
  const msClientId = strOrUndef(raw.providers?.microsoft?.client_id);
  const hasProviders = googleFile !== undefined || msClientId !== undefined;

  return {
    // dedupe_same_meeting は上の spread でそのまま(undefined/true/false 3値)引き継がれる
    ...raw,
    poll_interval: strOrUndef(raw.poll_interval),
    sync_window: strOrUndef(raw.sync_window),
    blocker_title: strOrUndef(raw.blocker_title),
    reconcile_at: strOrUndef(raw.reconcile_at),
    busy_show_as: arrOrUndef(raw.busy_show_as),
    notifications: slack ? { slack } : undefined,
    providers: hasProviders
      ? {
          google: googleFile !== undefined ? { credentials_file: googleFile } : undefined,
          microsoft: msClientId !== undefined ? { client_id: msClientId } : undefined,
        }
      : undefined,
    accounts: arrOrUndef(raw.accounts?.map(normalizeAccount)),
    detail_sync: arrOrUndef(raw.detail_sync?.map(normalizeDetailSync)),
  };
}

function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    return e.hint ? `${e.message}(${e.hint})` : e.message;
  }
  return String(e);
}

function triStateValue(v: boolean | undefined): "" | "true" | "false" {
  return v === undefined ? "" : v ? "true" : "false";
}

function parseTriState(v: string): boolean | undefined {
  return v === "" ? undefined : v === "true";
}

function TriStateSelect({
  value,
  onChange,
  unsetLabel,
}: {
  value: boolean | undefined;
  onChange: (v: boolean | undefined) => void;
  unsetLabel: string;
}) {
  return (
    <select value={triStateValue(value)} onChange={(e) => onChange(parseTriState(e.target.value))}>
      <option value="">{unsetLabel}</option>
      <option value="true">true</option>
      <option value="false">false</option>
    </select>
  );
}

function GlobalSettingsSection({
  draft,
  onChange,
}: {
  draft: RawConfig;
  onChange: <K extends keyof RawConfig>(key: K, value: RawConfig[K]) => void;
}) {
  const busy = draft.busy_show_as ?? [];
  const toggleBusy = (opt: string, checked: boolean) => {
    onChange("busy_show_as", checked ? [...busy, opt] : busy.filter((v) => v !== opt));
  };
  return (
    <section className="card">
      <h2>グローバル設定</h2>
      <div className="field">
        <label>poll_interval</label>
        <input
          value={draft.poll_interval ?? ""}
          placeholder="1m"
          onChange={(e) => onChange("poll_interval", e.target.value)}
        />
      </div>
      <div className="field">
        <label>sync_window</label>
        <input
          value={draft.sync_window ?? ""}
          placeholder="3mo"
          onChange={(e) => onChange("sync_window", e.target.value)}
        />
      </div>
      <div className="field">
        <label>blocker_title</label>
        <input
          value={draft.blocker_title ?? ""}
          placeholder="予定あり"
          onChange={(e) => onChange("blocker_title", e.target.value)}
        />
      </div>
      <div className="field">
        <label>reconcile_at</label>
        <input
          value={draft.reconcile_at ?? ""}
          placeholder="04:00"
          onChange={(e) => onChange("reconcile_at", e.target.value)}
        />
      </div>
      <div className="field">
        <label>dedupe_same_meeting</label>
        <TriStateSelect
          value={draft.dedupe_same_meeting}
          onChange={(v) => onChange("dedupe_same_meeting", v)}
          unsetLabel="既定(true)"
        />
      </div>
      <div className="field">
        <label>busy_show_as</label>
        <div className="checkbox-row">
          {BUSY_SHOW_AS_OPTIONS.map((opt) => (
            <label key={opt} className="checkbox">
              <input type="checkbox" checked={busy.includes(opt)} onChange={(e) => toggleBusy(opt, e.target.checked)} />
              {opt}
            </label>
          ))}
        </div>
        <p className="hint">すべて外すと未指定(既定 busy, oof, tentative を使用)になります。</p>
      </div>
    </section>
  );
}

function SlackSection({ draft, onChange }: { draft: RawConfig; onChange: (patch: Partial<RawSlack>) => void }) {
  const slack = draft.notifications?.slack ?? {};
  return (
    <section className="card">
      <h2>Slack 通知</h2>
      <div className="field">
        <label>bot_token_env</label>
        <input
          value={slack.bot_token_env ?? ""}
          placeholder="SLACK_BOT_TOKEN"
          onChange={(e) => onChange({ bot_token_env: e.target.value })}
        />
      </div>
      <div className="field">
        <label>channel</label>
        <input
          value={slack.channel ?? ""}
          placeholder="C0123456789"
          onChange={(e) => onChange({ channel: e.target.value })}
        />
      </div>
      <div className="field">
        <label>morning_digest</label>
        <input
          value={slack.morning_digest ?? ""}
          placeholder="07:30"
          onChange={(e) => onChange({ morning_digest: e.target.value })}
        />
      </div>
      <div className="field">
        <label>remind_before</label>
        <input
          value={slack.remind_before ?? ""}
          placeholder="10m"
          onChange={(e) => onChange({ remind_before: e.target.value })}
        />
      </div>
      <p className="hint">すべて空欄のまま保存すると Slack 通知設定なしとして扱われます。</p>
    </section>
  );
}

function ProvidersSection({
  draft,
  onChange,
}: {
  draft: RawConfig;
  onChange: (patch: { google?: string; microsoft?: string }) => void;
}) {
  return (
    <section className="card">
      <h2>providers</h2>
      <div className="field">
        <label>google.credentials_file</label>
        <input
          value={draft.providers?.google?.credentials_file ?? ""}
          placeholder="/path/to/credentials.json"
          onChange={(e) => onChange({ google: e.target.value })}
        />
      </div>
      <div className="field">
        <label>microsoft.client_id</label>
        <input
          value={draft.providers?.microsoft?.client_id ?? ""}
          placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
          onChange={(e) => onChange({ microsoft: e.target.value })}
        />
      </div>
    </section>
  );
}

function AccountsSection({
  accounts,
  calendarsText,
  digestText,
  onCalendarsTextChange,
  onDigestTextChange,
  onUpdate,
  onGoToAccountAdd,
}: {
  accounts: RawAccount[];
  calendarsText: string[];
  digestText: string[];
  onCalendarsTextChange: (idx: number, text: string) => void;
  onDigestTextChange: (idx: number, text: string) => void;
  onUpdate: (idx: number, patch: Partial<RawAccount>) => void;
  onGoToAccountAdd?: () => void;
}) {
  return (
    <section className="card">
      <h2>アカウント</h2>
      {accounts.length === 0 && <p>アカウントが設定されていません。</p>}
      {accounts.map((a, i) => (
        <div className="account-row" key={a.id ?? i}>
          <p>
            <strong>{a.id}</strong>({a.provider})
          </p>
          <div className="field">
            <label>email</label>
            <input
              value={a.email ?? ""}
              placeholder="user@gmail.com"
              onChange={(e) => onUpdate(i, { email: e.target.value })}
            />
          </div>
          <div className="field">
            <label>calendars(カンマ区切り)</label>
            <input
              value={calendarsText[i] ?? ""}
              placeholder="primary, xxxxx@group.calendar.google.com"
              onChange={(e) => onCalendarsTextChange(i, e.target.value)}
            />
          </div>
          <div className="field">
            <label>digest_calendars(カンマ区切り)</label>
            <input
              value={digestText[i] ?? ""}
              placeholder="xxxxx@group.calendar.google.com"
              onChange={(e) => onDigestTextChange(i, e.target.value)}
            />
          </div>
          <div className="field">
            <label>blocker_calendar</label>
            <input
              value={a.blocker_calendar ?? ""}
              placeholder="primary"
              onChange={(e) => onUpdate(i, { blocker_calendar: e.target.value })}
            />
          </div>
          <div className="field">
            <label>show_origin_in_description</label>
            <TriStateSelect
              value={a.show_origin_in_description}
              onChange={(v) => onUpdate(i, { show_origin_in_description: v })}
              unsetLabel="既定(false)"
            />
          </div>
        </div>
      ))}
      <p className="hint">
        アカウントの追加は
        {onGoToAccountAdd ? (
          <button className="link-button" onClick={onGoToAccountAdd}>
            「アカウント追加」タブ
          </button>
        ) : (
          "「アカウント追加」タブ"
        )}
        から行ってください。
      </p>
    </section>
  );
}

function DetailSyncSection({
  list,
  accountIds,
  onUpdate,
  onAdd,
  onRemove,
}: {
  list: RawDetailSync[];
  accountIds: string[];
  onUpdate: (idx: number, patch: Partial<RawDetailSync>) => void;
  onAdd: () => void;
  onRemove: (idx: number) => void;
}) {
  const toggleField = (idx: number, field: string, checked: boolean, current: string[]) => {
    onUpdate(idx, { fields: checked ? [...current, field] : current.filter((f) => f !== field) });
  };
  return (
    <section className="card">
      <h2>detail_sync(タイトル/説明の転記)</h2>
      {list.length === 0 && <p>設定されていません。</p>}
      {list.map((d, i) => {
        const fields = d.fields ?? [];
        return (
          <div className="detail-sync-row" key={i}>
            <div className="field">
              <label>from</label>
              <select value={d.from ?? ""} onChange={(e) => onUpdate(i, { from: e.target.value })}>
                <option value="">未選択</option>
                {accountIds.map((id) => (
                  <option key={id} value={id}>
                    {id}
                  </option>
                ))}
              </select>
            </div>
            <div className="field">
              <label>to</label>
              <select value={d.to ?? ""} onChange={(e) => onUpdate(i, { to: e.target.value })}>
                <option value="">未選択</option>
                {accountIds.map((id) => (
                  <option key={id} value={id}>
                    {id}
                  </option>
                ))}
              </select>
            </div>
            <div className="field">
              <label>fields</label>
              <div className="checkbox-row">
                {DETAIL_SYNC_FIELD_OPTIONS.map((f) => (
                  <label key={f} className="checkbox">
                    <input
                      type="checkbox"
                      checked={fields.includes(f)}
                      onChange={(e) => toggleField(i, f, e.target.checked, fields)}
                    />
                    {f}
                  </label>
                ))}
              </div>
            </div>
            <div className="field">
              <label>visibility</label>
              <select value={d.visibility ?? ""} onChange={(e) => onUpdate(i, { visibility: e.target.value })}>
                {VISIBILITY_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </div>
            <button onClick={() => onRemove(i)}>この行を削除</button>
          </div>
        );
      })}
      <button onClick={onAdd}>行を追加</button>
    </section>
  );
}

export default function ConfigForm({ api, onGoToAccountAdd }: { api: ApiClient; onGoToAccountAdd?: () => void }) {
  const [draft, setDraft] = useState<RawConfig | null>(null);
  const [mtime, setMtime] = useState<string | null>(null);
  const [calendarsText, setCalendarsText] = useState<string[]>([]);
  const [digestText, setDigestText] = useState<string[]>([]);
  const [daemonMode, setDaemonMode] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [conflictMessage, setConflictMessage] = useState<string | null>(null);
  const [pendingRestart, setPendingRestart] = useState(false);
  const [restarting, setRestarting] = useState(false);

  const loadConfig = useCallback(() => {
    setLoadError(null);
    setConflictMessage(null);
    api
      .getConfig()
      .then((c) => {
        setDraft(c.raw);
        setMtime(c.mtime);
        setCalendarsText((c.raw.accounts ?? []).map((a) => (a.calendars ?? []).join(", ")));
        setDigestText((c.raw.accounts ?? []).map((a) => (a.digest_calendars ?? []).join(", ")));
      })
      .catch((e) => setLoadError(describeError(e)));
  }, [api]);

  useEffect(() => {
    loadConfig();
    api
      .status()
      .then((s) => setDaemonMode(s.daemon.mode))
      .catch(() => {
        /* デーモン状態が取れなくても設定編集自体は続行できる */
      });
  }, [api, loadConfig]);

  const updateField = <K extends keyof RawConfig>(key: K, value: RawConfig[K]) => {
    setDraft((d) => (d ? { ...d, [key]: value } : d));
  };

  const updateSlack = (patch: Partial<RawSlack>) => {
    setDraft((d) =>
      d
        ? { ...d, notifications: { ...d.notifications, slack: { ...(d.notifications?.slack ?? {}), ...patch } } }
        : d,
    );
  };

  const updateProviders = (patch: { google?: string; microsoft?: string }) => {
    setDraft((d) => {
      if (!d) return d;
      const providers = d.providers ?? {};
      return {
        ...d,
        providers: {
          google: patch.google !== undefined ? { credentials_file: patch.google } : providers.google,
          microsoft: patch.microsoft !== undefined ? { client_id: patch.microsoft } : providers.microsoft,
        },
      };
    });
  };

  const updateAccount = (idx: number, patch: Partial<RawAccount>) => {
    setDraft((d) => {
      if (!d) return d;
      return { ...d, accounts: (d.accounts ?? []).map((a, i) => (i === idx ? { ...a, ...patch } : a)) };
    });
  };

  const updateCalendarsText = (idx: number, text: string) => {
    setCalendarsText((prev) => prev.map((v, i) => (i === idx ? text : v)));
  };
  const updateDigestText = (idx: number, text: string) => {
    setDigestText((prev) => prev.map((v, i) => (i === idx ? text : v)));
  };

  const updateDetailSync = (idx: number, patch: Partial<RawDetailSync>) => {
    setDraft((d) => {
      if (!d) return d;
      return { ...d, detail_sync: (d.detail_sync ?? []).map((x, i) => (i === idx ? { ...x, ...patch } : x)) };
    });
  };

  const addDetailSync = () => {
    setDraft((d) => {
      if (!d) return d;
      const ids = (d.accounts ?? []).map((a) => a.id).filter((id): id is string => !!id);
      return {
        ...d,
        detail_sync: [...(d.detail_sync ?? []), { from: ids[0] ?? "", to: ids[1] ?? "", fields: [], visibility: "" }],
      };
    });
  };

  const removeDetailSync = (idx: number) => {
    setDraft((d) => (d ? { ...d, detail_sync: (d.detail_sync ?? []).filter((_, i) => i !== idx) } : d));
  };

  const handleSave = async () => {
    if (!draft || mtime === null) return;
    setSaving(true);
    setSaveError(null);
    setConflictMessage(null);
    const merged: RawConfig = {
      ...draft,
      accounts: (draft.accounts ?? []).map((a, i) => ({
        ...a,
        calendars: csvToList(calendarsText[i] ?? ""),
        digest_calendars: csvToList(digestText[i] ?? ""),
      })),
    };
    const payload = normalizeRaw(merged);
    try {
      const res = await api.putConfig(payload, mtime);
      setDraft(payload);
      setMtime(res.mtime);
      setPendingRestart(true);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setConflictMessage(e.hint || "外部で変更されています。再読み込みしてください");
      } else if (e instanceof ApiError && e.status === 400) {
        setSaveError(e.message);
      } else {
        setSaveError(describeError(e));
      }
    } finally {
      setSaving(false);
    }
  };

  const handleRestart = async () => {
    setRestarting(true);
    setSaveError(null);
    try {
      await api.daemon("restart");
      setPendingRestart(false);
    } catch (e) {
      setSaveError(describeError(e));
    } finally {
      setRestarting(false);
    }
  };

  if (loadError) {
    return <p className="error">{loadError}</p>;
  }
  if (!draft) {
    return <p>読み込み中…</p>;
  }

  const accountIds = (draft.accounts ?? []).map((a) => a.id).filter((id): id is string => !!id);

  return (
    <div className="config-form">
      <GlobalSettingsSection draft={draft} onChange={updateField} />
      <SlackSection draft={draft} onChange={updateSlack} />
      <ProvidersSection draft={draft} onChange={updateProviders} />
      <AccountsSection
        accounts={draft.accounts ?? []}
        calendarsText={calendarsText}
        digestText={digestText}
        onCalendarsTextChange={updateCalendarsText}
        onDigestTextChange={updateDigestText}
        onUpdate={updateAccount}
        onGoToAccountAdd={onGoToAccountAdd}
      />
      <DetailSyncSection
        list={draft.detail_sync ?? []}
        accountIds={accountIds}
        onUpdate={updateDetailSync}
        onAdd={addDetailSync}
        onRemove={removeDetailSync}
      />

      <section className="card">
        <div className="button-row">
          <button onClick={() => void handleSave()} disabled={saving}>
            {saving ? "保存中…" : "保存"}
          </button>
          {conflictMessage && <button onClick={loadConfig}>再読み込み</button>}
        </div>
        {conflictMessage && <p className="error">{conflictMessage}</p>}
        {saveError && <p className="error">{saveError}</p>}
      </section>

      {pendingRestart && (
        <section className="card">
          <p>デーモン未反映の変更があります。</p>
          {daemonMode === "launchd" && (
            <button onClick={() => void handleRestart()} disabled={restarting}>
              {restarting ? "再起動中…" : "再起動して適用"}
            </button>
          )}
          {daemonMode === "container" && (
            <p className="hint">
              コンテナ運用中のためこの画面から再起動できません。ホストで <code>docker compose restart</code>{" "}
              を実行してください。
            </p>
          )}
          {daemonMode !== null && daemonMode !== "launchd" && daemonMode !== "container" && (
            <p className="hint">launchd 管理外です。デーモンプロセスを手動で再起動して設定を反映してください。</p>
          )}
          {daemonMode === null && (
            <p className="hint">
              デーモンの状態を確認できませんでした。手動で再起動するか、アプリを再読み込みして状態を確認してください。
            </p>
          )}
        </section>
      )}
    </div>
  );
}
