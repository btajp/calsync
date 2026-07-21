import { useEffect, useRef, useState } from "react";
import type { ApiClient } from "../api";
import { ApiError } from "../api";
import type { CalendarListEntry, RawAccount, RawConfig } from "../types";

const ACCOUNT_ID_RE = /^[A-Za-z0-9._-]+$/;

/**
 * 新規アカウント id の即時検証を行う純関数。
 * 許容文字集合は internal/auth の validateAccountID(パストラバーサル防止)と同じ
 * [A-Za-z0-9._-]+ に揃え、同関数が個別に拒否する "." / ".." も追加で弾く。
 * null = OK、それ以外はエラーメッセージ文字列を返す。
 */
export function validateNewAccountId(id: string, existing: string[]): string | null {
  if (id === "") return "id を入力してください";
  // "." / ".." はファイルパスとして特別な意味を持つため、internal/auth の validateAccountID と
  // 同様に明示的に拒否する(charset だけでは弾けない。トークン保存先のパストラバーサル防止)。
  if (id === "." || id === "..") return "id に \".\" / \"..\" は使用できません";
  if (!ACCOUNT_ID_RE.test(id)) return "id に使用できるのは英数字と . _ - のみです";
  if (existing.includes(id)) return `id "${id}" は既に使用されています`;
  return null;
}

type Provider = "google" | "microsoft";

type Step =
  | { kind: "loading" }
  | { kind: "blocked"; reason: string }
  | { kind: "input" }
  | { kind: "auth" }
  | { kind: "calendars" }
  | { kind: "review" }
  | { kind: "done" };

type AuthPhase = "starting" | "running" | "error";

type CalendarsState =
  | { status: "loading" }
  | { status: "ok"; list: CalendarListEntry[] }
  | { status: "error"; message: string; hint?: string };

function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    return e.hint ? `${e.message}(${e.hint})` : e.message;
  }
  return String(e);
}

/** カンマ区切りテキストを空要素を除いた文字列配列に変換する(ConfigForm.csvToList と同趣旨。依存はさせない)。 */
function csvToList(text: string): string[] {
  return text
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

/** カレンダーリストの1件をチェックボックス/select の値へ変換する。primary は "primary" 固定値に丸める。 */
function calendarValue(entry: CalendarListEntry): string {
  return entry.primary ? "primary" : entry.id;
}

/**
 * 追加するアカウント行を組み立てる純関数。normalizeRaw(ConfigForm.tsx)と同じ思想で、
 * 既定値(primary のみ/未入力)ならキー自体を出さない。microsoft は v1 制約(primary 固定)
 * のため calendars/blocker_calendar を常に省略する。
 */
function buildNewAccount(input: {
  id: string;
  provider: Provider;
  email: string;
  watched: string[];
  blocker: string;
}): RawAccount {
  const account: RawAccount = { id: input.id, provider: input.provider };
  if (input.email !== "") account.email = input.email;
  if (input.provider === "google") {
    const watched = input.watched;
    const isDefaultWatched = watched.length === 1 && watched[0] === "primary";
    if (watched.length > 0 && !isDefaultWatched) account.calendars = watched;
    if (input.blocker !== "" && input.blocker !== "primary") account.blocker_calendar = input.blocker;
  }
  return account;
}

export default function AccountAdd({ api, onGoToDashboard }: { api: ApiClient; onGoToDashboard?: () => void }) {
  const [step, setStep] = useState<Step>({ kind: "loading" });
  const [existingIds, setExistingIds] = useState<string[]>([]);
  const [hasGoogle, setHasGoogle] = useState(false);
  const [hasMicrosoft, setHasMicrosoft] = useState(false);
  const [daemonMode, setDaemonMode] = useState<string | null>(null);

  // 入力
  const [id, setId] = useState("");
  const [provider, setProvider] = useState<Provider | "">("");
  const [email, setEmail] = useState("");

  // 認可
  const [authPhase, setAuthPhase] = useState<AuthPhase>("starting");
  const [authError, setAuthError] = useState<string | null>(null);
  const [authHint, setAuthHint] = useState<string | undefined>(undefined);
  const [cancelling, setCancelling] = useState(false);
  // 「再試行」ボタンで同じ id/provider のまま authStart をもう一度呼ぶためだけのトリガー値。
  const [authRetryToken, setAuthRetryToken] = useState(0);
  // タブ切り替え等でアンマウントされた時、進行中の認可(phase=running)が残っていれば
  // サーバー側もキャンセルする(放置すると 5 分のタイムアウトまで auth_in_progress で
  // 再試行がブロックされ続ける)。ポーリング用 effect の外から参照するため ref で持つ。
  const runningFlowRef = useRef(false);

  // カレンダー選択(google のみ)
  const [calendarsState, setCalendarsState] = useState<CalendarsState | null>(null);
  const [watchedIds, setWatchedIds] = useState<string[]>(["primary"]);
  const [blockerId, setBlockerId] = useState("primary");
  const [manualWatchedText, setManualWatchedText] = useState("");
  const [manualBlockerText, setManualBlockerText] = useState("");

  // 保存・反映
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [conflictMessage, setConflictMessage] = useState<string | null>(null);
  const [pendingRestart, setPendingRestart] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [restartError, setRestartError] = useState<string | null>(null);

  const loadPrecheck = () => {
    setStep({ kind: "loading" });
    Promise.all([api.getConfig(), api.status().catch(() => null)])
      .then(([c, s]) => {
        const raw: RawConfig = c.raw;
        const google = !!raw.providers?.google?.credentials_file;
        const microsoft = !!raw.providers?.microsoft?.client_id;
        setExistingIds((raw.accounts ?? []).map((a) => a.id).filter((v): v is string => !!v));
        setHasGoogle(google);
        setHasMicrosoft(microsoft);
        if (s) setDaemonMode(s.daemon.mode);
        if (!google && !microsoft) {
          setStep({
            kind: "blocked",
            reason: "google/microsoft のいずれも providers 設定(credentials_file / client_id)がありません。",
          });
          return;
        }
        setProvider(google ? "google" : "microsoft");
        setStep({ kind: "input" });
      })
      .catch((e) => setStep({ kind: "blocked", reason: describeError(e) }));
  };

  useEffect(loadPrecheck, [api]);

  // 認可: authStart 呼び出し + phase running 中は 1 秒間隔で authState をポーリングする。
  // ステップ遷移・アンマウントで確実に片付くよう、cleanup は useEffect の戻り値に一本化する。
  useEffect(() => {
    if (step.kind !== "auth" || provider === "") return;
    let cancelled = false;
    setAuthPhase("starting");
    setAuthError(null);
    setAuthHint(undefined);
    // handleAuthStart はレスポンスを返す前に phase を "running" にする(authflow.go)。
    // つまりリクエスト送出からレスポンス到達までの間に既にサーバー側は running になり得る。
    // .then() 内で立てると、その窓でアンマウントされた場合に runningFlowRef が true にならず、
    // アンマウント時の authCancel が発火しない(サーバーが auth_in_progress に固着する)。
    // そのため送出時点で先に立て、この呼び出しが running を作らなかったと判明した場合(catch)
    // のみ倒す。
    runningFlowRef.current = true;
    api
      .authStart(id, provider)
      .then(() => {
        if (!cancelled) setAuthPhase("running");
      })
      .catch((e) => {
        runningFlowRef.current = false;
        if (!cancelled) {
          setAuthPhase("error");
          setAuthError(describeError(e));
        }
      });
    return () => {
      cancelled = true;
    };
    // authRetryToken は「再試行」ボタンで同じ id/provider のまま依存配列を変化させ、再実行させるためだけの値。
  }, [step.kind, authRetryToken, api, id, provider]);

  useEffect(() => {
    if (step.kind !== "auth" || authPhase !== "running") return;
    const timer = setInterval(() => {
      api
        .authState()
        .then((s) => {
          if (s.phase === "done") {
            runningFlowRef.current = false;
            setStep(provider === "google" ? { kind: "calendars" } : { kind: "review" });
          } else if (s.phase === "error") {
            runningFlowRef.current = false;
            setAuthPhase("error");
            setAuthError(cancelling ? "認可をキャンセルしました" : s.error || "認可に失敗しました");
            setAuthHint(cancelling ? undefined : s.hint);
            setCancelling(false);
          }
        })
        .catch((e) => {
          setAuthPhase("error");
          setAuthError(describeError(e));
        });
    }, 1000);
    return () => clearInterval(timer);
  }, [step.kind, authPhase, api, provider, cancelling]);

  // コンポーネントのアンマウント時、進行中の認可が残っていればサーバー側もキャンセルする。
  useEffect(() => {
    return () => {
      if (runningFlowRef.current) {
        api.authCancel().catch(() => {
          /* アンマウント後なので UI には反映できない。ベストエフォート */
        });
      }
    };
  }, [api]);

  // カレンダー選択: step 突入時に一度だけ listCalendars を呼ぶ(calendarsState !== null なら再取得しない)。
  useEffect(() => {
    if (step.kind !== "calendars" || calendarsState !== null) return;
    setCalendarsState({ status: "loading" });
    api
      .listCalendars(id)
      .then((res) => {
        setCalendarsState({ status: "ok", list: res.calendars });
        const primaryValues = res.calendars.filter((c) => c.primary).map(calendarValue);
        setWatchedIds(primaryValues.length > 0 ? primaryValues : []);
        setBlockerId(primaryValues[0] ?? res.calendars[0]?.id ?? "primary");
      })
      .catch((e) => {
        const hint = e instanceof ApiError ? e.hint : undefined;
        setCalendarsState({ status: "error", message: describeError(e), hint });
      });
  }, [step.kind, calendarsState, api, id]);

  const handleCancelAuth = () => {
    setCancelling(true);
    api.authCancel().catch((e) => {
      // キャンセル自体が失敗した場合、実際にはキャンセルされていないため
      // 以降のエラー表示を「キャンセルしました」に誤表示しないよう戻す。
      setCancelling(false);
      setAuthError(describeError(e));
    });
  };

  const handleRetryAuth = () => {
    setCancelling(false);
    setAuthRetryToken((n) => n + 1);
  };

  const handleSave = () => {
    setSaving(true);
    setSaveError(null);
    setConflictMessage(null);
    const usingManualCalendars = calendarsState?.status !== "ok";
    const watched = provider === "google" ? (usingManualCalendars ? csvToList(manualWatchedText) : watchedIds) : [];
    const blocker = provider === "google" ? (usingManualCalendars ? manualBlockerText.trim() : blockerId) : "";
    api
      .getConfig()
      .then((fresh) => {
        if (provider === "") throw new Error("provider is empty");
        const newAccount = buildNewAccount({ id, provider, email, watched, blocker });
        const merged: RawConfig = { ...fresh.raw, accounts: [...(fresh.raw.accounts ?? []), newAccount] };
        return api.putConfig(merged, fresh.mtime);
      })
      .then(() => {
        setPendingRestart(true);
        setStep({ kind: "done" });
      })
      .catch((e) => {
        if (e instanceof ApiError && e.status === 409) {
          setConflictMessage(e.hint || "設定が外部で変更されています。「再試行」で最新設定を取得して追記し直します");
        } else {
          setSaveError(describeError(e));
        }
      })
      .finally(() => setSaving(false));
  };

  // review ステップで保存に失敗した場合の避難ハッチ。認可済みトークンは id 単位でファイル保存
  // されているため、ここで input に戻っても失われない(同じ id で戻れば再利用され、別 id にすれば
  // 使われないまま残るだけで無害)。
  const resetToInput = () => {
    setCalendarsState(null);
    setWatchedIds(["primary"]);
    setBlockerId("primary");
    setManualWatchedText("");
    setManualBlockerText("");
    setSaveError(null);
    setConflictMessage(null);
    setStep({ kind: "input" });
  };

  const handleRestart = () => {
    setRestarting(true);
    setRestartError(null);
    api
      .daemon("restart")
      .then(() => setPendingRestart(false))
      .catch((e) => setRestartError(describeError(e)))
      .finally(() => setRestarting(false));
  };

  if (step.kind === "loading") {
    return <p>読み込み中…</p>;
  }

  if (step.kind === "blocked") {
    return (
      <div className="account-add">
        <section className="card">
          <h2>アカウント追加の前提が未設定です</h2>
          <p className="error">{step.reason}</p>
          <p className="hint">
            README の「セットアップ 1: Google(GCP)」または「セットアップ 2: Microsoft(Entra ID)」を参照して
            providers 設定(credentials_file / client_id)を「設定」タブから登録してください。手順に迷う場合は
            calsync-setup スキルも参照できます。
          </p>
          <button onClick={loadPrecheck}>再読み込み</button>
        </section>
      </div>
    );
  }

  const idError = step.kind === "input" ? validateNewAccountId(id, existingIds) : null;

  return (
    <div className="account-add">
      {step.kind === "input" && (
        <section className="card">
          <h2>1. アカウント情報の入力</h2>
          <div className="field">
            <label>id</label>
            <input value={id} placeholder="work-a" onChange={(e) => setId(e.target.value)} />
            {id !== "" && idError && <p className="error">{idError}</p>}
          </div>
          <div className="field">
            <label>provider</label>
            <select value={provider} onChange={(e) => setProvider(e.target.value as Provider)}>
              {hasGoogle && <option value="google">google</option>}
              {hasMicrosoft && <option value="microsoft">microsoft</option>}
            </select>
            <p className="hint">
              providers 設定がある方のみ選択できます(google: {hasGoogle ? "設定済み" : "未設定"} / microsoft:{" "}
              {hasMicrosoft ? "設定済み" : "未設定"})。
            </p>
          </div>
          <div className="field">
            <label>email(表示用ラベル・任意)</label>
            <input value={email} placeholder="user@gmail.com" onChange={(e) => setEmail(e.target.value)} />
          </div>
          <button disabled={idError !== null || provider === ""} onClick={() => setStep({ kind: "auth" })}>
            認可へ進む
          </button>
        </section>
      )}

      {step.kind === "auth" && (
        <section className="card">
          <h2>2. OAuth 認可</h2>
          <p>
            id: <strong>{id}</strong> / provider: <strong>{provider}</strong>
          </p>
          {(authPhase === "starting" || authPhase === "running") && (
            <>
              <p>ブラウザで認可を行ってください。認可が完了するとこの画面が自動的に進みます。</p>
              <button onClick={handleCancelAuth} disabled={authPhase === "starting"}>
                キャンセル
              </button>
            </>
          )}
          {authPhase === "error" && (
            <>
              <p className="error">{authError}</p>
              {authHint && <p className="hint">{authHint}</p>}
              <div className="button-row">
                <button onClick={handleRetryAuth}>再試行</button>
                <button className="link-button" onClick={() => setStep({ kind: "input" })}>
                  id/provider を変更する
                </button>
              </div>
            </>
          )}
        </section>
      )}

      {step.kind === "calendars" && (
        <section className="card">
          <h2>3. カレンダー選択</h2>
          {calendarsState === null || calendarsState.status === "loading" ? (
            <p>カレンダー一覧を取得中…</p>
          ) : calendarsState.status === "ok" ? (
            <>
              <div className="field">
                <label>監視対象(既定 primary のみ)</label>
                <div className="checkbox-row">
                  {calendarsState.list.map((c) => {
                    const v = calendarValue(c);
                    return (
                      <label key={c.id} className="checkbox">
                        <input
                          type="checkbox"
                          checked={watchedIds.includes(v)}
                          onChange={(e) =>
                            setWatchedIds((prev) =>
                              e.target.checked ? [...prev, v] : prev.filter((x) => x !== v),
                            )
                          }
                        />
                        {c.summary}
                        {c.primary ? "(primary)" : ""}
                      </label>
                    );
                  })}
                </div>
              </div>
              <div className="field">
                <label>ブロッカー先(既定 primary)</label>
                <select value={blockerId} onChange={(e) => setBlockerId(e.target.value)}>
                  {calendarsState.list.map((c) => (
                    <option key={c.id} value={calendarValue(c)}>
                      {c.summary}
                      {c.primary ? "(primary)" : ""}
                    </option>
                  ))}
                </select>
              </div>
            </>
          ) : (
            <>
              <p className="error">{calendarsState.message}</p>
              {calendarsState.hint && <p className="hint">{calendarsState.hint}</p>}
              <div className="field">
                <label>監視対象(カンマ区切り。空欄なら既定 primary)</label>
                <input
                  value={manualWatchedText}
                  placeholder="primary, xxxxx@group.calendar.google.com"
                  onChange={(e) => setManualWatchedText(e.target.value)}
                />
              </div>
              <div className="field">
                <label>ブロッカー先(空欄なら既定 primary)</label>
                <input
                  value={manualBlockerText}
                  placeholder="primary"
                  onChange={(e) => setManualBlockerText(e.target.value)}
                />
              </div>
              <button onClick={() => setCalendarsState(null)}>再取得</button>
            </>
          )}
          <div className="button-row">
            <button
              onClick={() => setStep({ kind: "review" })}
              disabled={calendarsState === null || calendarsState.status === "loading"}
            >
              次へ
            </button>
          </div>
        </section>
      )}

      {step.kind === "review" && (
        <section className="card">
          <h2>{provider === "google" ? "4" : "3"}. 設定への追記</h2>
          <p>
            id: <strong>{id}</strong> / provider: <strong>{provider}</strong>
            {email !== "" && <> / email: {email}</>}
          </p>
          {provider === "microsoft" && <p className="hint">microsoft は v1 制約により primary 固定です。</p>}
          {provider === "google" && calendarsState?.status === "ok" && (
            <p className="hint">
              監視対象: {watchedIds.length > 0 ? watchedIds.join(", ") : "primary(既定)"} / ブロッカー先:{" "}
              {blockerId || "primary(既定)"}
            </p>
          )}
          <div className="button-row">
            <button onClick={handleSave} disabled={saving}>
              {saving ? "保存中…" : conflictMessage ? "再試行" : "設定に追記する"}
            </button>
            <button className="link-button" onClick={resetToInput} disabled={saving}>
              id/provider からやり直す
            </button>
          </div>
          {conflictMessage && <p className="error">{conflictMessage}</p>}
          {saveError && <p className="error">{saveError}</p>}
        </section>
      )}

      {step.kind === "done" && (
        <section className="card">
          <h2>{provider === "google" ? "5" : "4"}. 完了・反映</h2>
          <p>
            アカウント <strong>{id}</strong> を設定に追記しました。
          </p>
          {pendingRestart && (
            <>
              <p>デーモン未反映の変更があります。</p>
              {daemonMode === "launchd" && (
                <button onClick={handleRestart} disabled={restarting}>
                  {restarting ? "再起動中…" : "デーモンを再起動して反映"}
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
                <p className="hint">デーモンの状態を確認できませんでした。手動で再起動して設定を反映してください。</p>
              )}
              {restartError && <p className="error">{restartError}</p>}
            </>
          )}
          {onGoToDashboard && (
            <button className="link-button" onClick={onGoToDashboard}>
              ダッシュボードへ戻る
            </button>
          )}
        </section>
      )}
    </div>
  );
}
