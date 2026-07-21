import { useCallback, useEffect, useState } from "react";
import type { ApiClient } from "../api";
import { ApiError } from "../api";
import type { CalendarStatus, DaemonInfo, RawConfig, StatusResponse } from "../types";

export interface OverviewRow {
  accountId: string;
  provider: string;
  watched: string[];
  blocker: string;
  digest: string[];
  tokenState: string;
  lastSync: string;
  syncStatus: string;
}

/** status.calendars の中から、時刻として解釈できて最も新しいものを 1 件選ぶ。 */
function pickLatestCalendar(calendars: CalendarStatus[]): CalendarStatus | undefined {
  let latest: CalendarStatus | undefined;
  let latestTime = -Infinity;
  for (const c of calendars) {
    if (!c.last_sync) continue;
    const t = Date.parse(c.last_sync);
    if (Number.isNaN(t) || t < latestTime) continue;
    latest = c;
    latestTime = t;
  }
  return latest;
}

/**
 * status/config を突合し、ダッシュボードの俯瞰テーブル用の行データに整形する純関数。
 * カレンダー単位の最終同期は status.calendars から accountId で突合し、最も新しいものを表示する。
 */
export function buildOverview(raw: RawConfig, status: StatusResponse): OverviewRow[] {
  const calendarsByAccount = new Map<string, CalendarStatus[]>();
  for (const c of status.calendars ?? []) {
    const list = calendarsByAccount.get(c.account_id);
    if (list) {
      list.push(c);
    } else {
      calendarsByAccount.set(c.account_id, [c]);
    }
  }
  const tokenStateByAccount = new Map((status.tokens ?? []).map((t) => [t.account_id, t.state]));

  return (raw.accounts ?? []).map((account) => {
    const accountId = account.id ?? "";
    const watched = account.calendars && account.calendars.length > 0 ? account.calendars : ["primary"];
    const latest = pickLatestCalendar(calendarsByAccount.get(accountId) ?? []);
    return {
      accountId,
      provider: account.provider ?? "",
      watched,
      blocker: account.blocker_calendar || "primary",
      digest: account.digest_calendars ?? [],
      tokenState: tokenStateByAccount.get(accountId) ?? "unknown",
      lastSync: latest?.last_sync || "-",
      syncStatus: latest?.status || "-",
    };
  });
}

const TOKEN_STATE_LABELS: Record<string, string> = {
  ok: "OK",
  missing: "未認可",
  no_refresh_token: "再認可が必要",
  unknown: "不明",
};

function tokenStateLabel(state: string): string {
  return TOKEN_STATE_LABELS[state] ?? state;
}

function formatLastSync(value: string): string {
  if (!value || value === "-") return "-";
  const t = Date.parse(value);
  if (Number.isNaN(t)) return value;
  return new Date(t).toLocaleString("ja-JP");
}

function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    return e.hint ? `${e.message}(${e.hint})` : e.message;
  }
  return String(e);
}

type DaemonAction = "start" | "stop" | "restart";

const DAEMON_ACTIONS: { key: DaemonAction; label: string }[] = [
  { key: "stop", label: "停止" },
  { key: "start", label: "起動" },
  { key: "restart", label: "再起動" },
];

export default function Dashboard({ api }: { api: ApiClient }) {
  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [rawConfig, setRawConfig] = useState<RawConfig | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [doctorText, setDoctorText] = useState<string | null>(null);
  const [doctorRunning, setDoctorRunning] = useState(false);
  const [daemonAction, setDaemonAction] = useState<DaemonAction | null>(null);

  const refreshStatus = useCallback(() => {
    api
      .status()
      .then(setStatus)
      .catch((e) => setError(describeError(e)));
  }, [api]);

  useEffect(() => {
    refreshStatus();
    api
      .getConfig()
      .then((c) => setRawConfig(c.raw))
      .catch((e) => setError(describeError(e)));
  }, [api, refreshStatus]);

  // 5 秒ポーリング。バックグラウンドタブでは appserver への無駄な問い合わせを避ける。
  useEffect(() => {
    const id = setInterval(() => {
      if (document.visibilityState === "visible") {
        refreshStatus();
      }
    }, 5000);
    return () => clearInterval(id);
  }, [refreshStatus]);

  const runDaemonAction = async (action: DaemonAction) => {
    setDaemonAction(action);
    setError(null);
    try {
      await api.daemon(action);
      refreshStatus();
    } catch (e) {
      setError(describeError(e));
    } finally {
      setDaemonAction(null);
    }
  };

  const runDoctor = async () => {
    setDoctorRunning(true);
    setError(null);
    try {
      const res = await api.doctor();
      setDoctorText(res.text);
    } catch (e) {
      setDoctorText(describeError(e));
    } finally {
      setDoctorRunning(false);
    }
  };

  const rows = rawConfig && status ? buildOverview(rawConfig, status) : [];

  return (
    <div className="dashboard">
      {error && <p className="error">{error}</p>}

      <section className="card">
        <h2>デーモン状態</h2>
        {status ? (
          <DaemonCard daemon={status.daemon} action={daemonAction} onAction={runDaemonAction} />
        ) : (
          <p>読み込み中…</p>
        )}
      </section>

      <section className="card">
        <h2>アカウント構成</h2>
        {rawConfig && status ? <OverviewTable rows={rows} /> : <p>読み込み中…</p>}
      </section>

      <section className="card">
        <h2>doctor 診断</h2>
        <button onClick={() => void runDoctor()} disabled={doctorRunning}>
          {doctorRunning ? "実行中…" : "doctor を実行"}
        </button>
        {doctorText !== null && <pre>{doctorText}</pre>}
      </section>
    </div>
  );
}

function DaemonCard({
  daemon,
  action,
  onAction,
}: {
  daemon: DaemonInfo;
  action: DaemonAction | null;
  onAction: (action: DaemonAction) => void;
}) {
  return (
    <div>
      <p>
        モード: {daemon.mode} / 状態: {daemon.running ? "稼働中" : "停止中"}
      </p>
      {daemon.detail && <p className="hint">{daemon.detail}</p>}
      {daemon.mode === "launchd" && (
        <div className="button-row">
          {DAEMON_ACTIONS.map(({ key, label }) => (
            <button key={key} disabled={action !== null} onClick={() => onAction(key)}>
              {action === key ? `${label}中…` : label}
            </button>
          ))}
        </div>
      )}
      {daemon.mode === "container" && (
        <p className="hint">
          コンテナ運用中のためホストからの操作・読み取りはできません。状態確認は{" "}
          <code>docker compose logs</code> と{" "}
          <code>docker exec calsync /calsync status --config /data/calsync.yaml --data /data</code>{" "}
          を使用してください。
        </p>
      )}
      {daemon.mode === "manual" && (
        <p className="hint">
          launchd 管理外です。<code>./scripts/macos/install-launchd.sh</code>{" "}
          を実行するとネイティブ常駐(停止/起動/再起動ボタンの利用)に切り替えられます。
        </p>
      )}
    </div>
  );
}

function OverviewTable({ rows }: { rows: OverviewRow[] }) {
  if (rows.length === 0) {
    return <p>アカウントが設定されていません。</p>;
  }
  return (
    <table>
      <thead>
        <tr>
          <th>アカウント</th>
          <th>プロバイダ</th>
          <th>監視カレンダー</th>
          <th>ブロッカー先</th>
          <th>digest</th>
          <th>トークン状態</th>
          <th>最終同期</th>
          <th>状態</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => (
          <tr key={r.accountId}>
            <td>{r.accountId}</td>
            <td>{r.provider}</td>
            <td>{r.watched.join(", ")}</td>
            <td>{r.blocker}</td>
            <td>{r.digest.length > 0 ? r.digest.join(", ") : "-"}</td>
            <td>{tokenStateLabel(r.tokenState)}</td>
            <td>{formatLastSync(r.lastSync)}</td>
            <td>{r.syncStatus}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
