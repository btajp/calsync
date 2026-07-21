import { useEffect, useState } from "react";
import { open } from "@tauri-apps/plugin-dialog";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { ApiClient } from "./api";
import { startSidecar } from "./sidecar";
import Dashboard from "./pages/Dashboard";

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

function Shell({ api, onResetDataDir }: { api: ApiClient; onResetDataDir: () => void }) {
  const [tab, setTab] = useState<Tab>("dashboard");

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
      {tab === "config" && <p>設定画面は Task 13 で実装します。</p>}
      {tab === "account-add" && <p>アカウント追加画面は Task 14 で実装します。</p>}
    </main>
  );
}
