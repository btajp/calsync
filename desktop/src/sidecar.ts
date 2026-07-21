import { Command } from "@tauri-apps/plugin-shell";
import { ApiClient } from "./api";

export function parseHandshake(line: string): { port: number; token: string } {
  const parsed = JSON.parse(line) as { port?: number; token?: string };
  if (typeof parsed.port !== "number" || typeof parsed.token !== "string") {
    throw new Error(`invalid handshake: ${line}`);
  }
  return { port: parsed.port, token: parsed.token };
}

export async function startSidecar(dataDir: string): Promise<{ api: ApiClient; kill: () => void }> {
  const cmd = Command.sidecar("binaries/calsync", [
    "appserver",
    "--config", `${dataDir}/calsync.yaml`,
    "--data", dataDir,
  ]);
  // spawn は 1 回だけ。stdout リスナは spawn 前に登録し、handshake 行が来たら resolve する
  return new Promise((resolve, reject) => {
    let child: Awaited<ReturnType<typeof cmd.spawn>> | undefined;
    const timer = setTimeout(() => {
      void child?.kill();
      reject(new Error("sidecar handshake timeout"));
    }, 10_000);
    cmd.stdout.on("data", (line: string) => {
      try {
        const hs = parseHandshake(line.trim());
        clearTimeout(timer);
        resolve({
          api: new ApiClient(`http://127.0.0.1:${hs.port}`, hs.token),
          kill: () => { void child?.kill(); },
        });
      } catch {
        // 起動ノイズ行は無視して次の行を待つ
      }
    });
    cmd.on("error", (e: string) => { clearTimeout(timer); reject(new Error(e)); });
    void cmd.spawn().then((c) => { child = c; }).catch((e) => { clearTimeout(timer); reject(e); });
  });
}
