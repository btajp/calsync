import { useCallback, useEffect, useState } from "react";
import { open } from "@tauri-apps/plugin-dialog";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { ApiClient } from "./api";
import { startSidecar } from "./sidecar";
import Dashboard from "./pages/Dashboard";
import ConfigForm from "./pages/ConfigForm";
import AccountAdd from "./pages/AccountAdd";

export default function App() {
  const [api, setApi] = useState<ApiClient | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [dataDir, setDataDir] = useState<string | null>(localStorage.getItem("calsync.dataDir"));

  useEffect(() => {
    if (!dataDir) return;
    let kill: (() => void) | undefined;
    startSidecar(dataDir)
      .then((s) => {
        setApi(s.api);
        kill = s.kill;
        void getCurrentWindow().onCloseRequested(() => s.kill());
      })
      .catch((e) => setError(String(e)));
    return () => kill?.();
  }, [dataDir]);

  if (!dataDir) {
    return (
      <main className="setup">
        <h1>calsync</h1>
        <p>calsync のデータディレクトリ(calsync.yaml があるフォルダ)を選択してください。</p>
        <button
          onClick={async () => {
            const dir = await open({ directory: true });
            if (typeof dir === "string") {
              localStorage.setItem("calsync.dataDir", dir);
              setDataDir(dir);
            }
          }}
        >
          フォルダを選択
        </button>
      </main>
    );
  }
  if (error) return <main className="error">起動エラー: {error}</main>;
  if (!api) return <main>appserver に接続中…</main>;
  return <Shell api={api} onResetDataDir={() => { localStorage.removeItem("calsync.dataDir"); setDataDir(null); }} />;
}

type Tab = "dashboard" | "config" | "account-add";

const TABS: { key: Tab; label: string }[] = [
  { key: "dashboard", label: "ダッシュボード" },
  { key: "config", label: "設定" },
  { key: "account-add", label: "アカウント追加" },
];

// ModeCheck は起動直後に 1 回だけ行うコンテナ検出の結果。仕様§9のコンテナ
// ガード(ホスト側からは DB 読み取りを含む全機能を停止して案内表示モードに
// 落とす)をフロント側でも反映する。ポーリングはしない(初回判定+再チェック
// ボタンで十分。サーバ側の各エンドポイントは常にガードされているため、稼働中
// にコンテナが起動する稀なケースでも書き込み系は 409 で守られる)。
type ModeCheck = "checking" | "container" | "ok" | "error";

function Shell({ api, onResetDataDir }: { api: ApiClient; onResetDataDir: () => void }) {
  const [tab, setTab] = useState<Tab>("dashboard");
  const [modeCheck, setModeCheck] = useState<ModeCheck>("checking");
  const [modeCheckError, setModeCheckError] = useState<string | null>(null);

  const checkMode = useCallback(() => {
    setModeCheck("checking");
    setModeCheckError(null);
    api
      .status()
      .then((s) => setModeCheck(s.daemon.mode === "container" ? "container" : "ok"))
      .catch((e) => {
        setModeCheckError(String(e));
        setModeCheck("error");
      });
  }, [api]);

  useEffect(() => {
    checkMode();
  }, [checkMode]);

  if (modeCheck === "checking") {
    return <main>状態を確認中…</main>;
  }

  if (modeCheck === "error") {
    return (
      <main className="setup">
        <h1>calsync</h1>
        <p className="error">状態確認に失敗しました: {modeCheckError}</p>
        <button onClick={checkMode}>再試行</button>
      </main>
    );
  }

  if (modeCheck === "container") {
    return (
      <main className="setup">
        <h1>calsync</h1>
        <p>
          コンテナ運用が検出されました。ホスト側アプリからの閲覧・変更はできません。
        </p>
        <p className="hint">
          docker compose logs / docker exec での確認手順は README を参照してください。
        </p>
        <div className="button-row">
          <button onClick={checkMode}>再チェック</button>
          <button className="link-button" onClick={onResetDataDir}>
            データフォルダ変更
          </button>
        </div>
      </main>
    );
  }

  return (
    <main>
      <header className="app-header">
        <h1>calsync</h1>
        <nav className="tabs">
          {TABS.map((t) => (
            <button
              key={t.key}
              className={t.key === tab ? "tab active" : "tab"}
              onClick={() => setTab(t.key)}
            >
              {t.label}
            </button>
          ))}
        </nav>
        <button className="link-button" onClick={onResetDataDir}>
          データフォルダ変更
        </button>
      </header>

      {tab === "dashboard" && <Dashboard api={api} />}
      {tab === "config" && <ConfigForm api={api} onGoToAccountAdd={() => setTab("account-add")} />}
      {tab === "account-add" && <AccountAdd api={api} onGoToDashboard={() => setTab("dashboard")} />}
    </main>
  );
}
