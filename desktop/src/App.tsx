import { useEffect, useState } from "react";
import { open } from "@tauri-apps/plugin-dialog";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { ApiClient } from "./api";
import { startSidecar } from "./sidecar";

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

function Shell({ api }: { api: ApiClient; onResetDataDir: () => void }) {
  const [ping, setPing] = useState<string>("...");
  useEffect(() => {
    api.status().then((s) => setPing(`daemon: ${s.daemon.mode}`)).catch((e) => setPing(String(e)));
  }, [api]);
  return <main><h1>calsync</h1><p>{ping}</p></main>;
}
