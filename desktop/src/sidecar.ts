import { Command } from "@tauri-apps/plugin-shell";
import { ApiClient } from "./api";

export function parseHandshake(line: string): { port: number; token: string } {
  const parsed = JSON.parse(line) as { port?: number; token?: string };
  if (typeof parsed.port !== "number" || typeof parsed.token !== "string") {
    throw new Error(`invalid handshake: ${line}`);
  }
  return { port: parsed.port, token: parsed.token };
}

export interface SidecarHandle {
  api: ApiClient;
  kill: () => void;
  // トレイのポップオーバー(別ウィンドウ)へ ApiClient 相当の接続情報を Tauri イベントで
  // 引き渡すために port/token を公開する(デスクトップトレイ設計 2026-07-23 §3.2:
  // 「API 接続情報の受け渡し」。localStorage には書かず、emitTo("panel", "api-info", ...) で渡す)。
  port: number;
  token: string;
}

// dev の React.StrictMode は effect を「マウント→クリーンアップ→再マウント」で 2 回連続実行する。
// startSidecar は非同期(handshake 待ち)のため、1 回目の呼び出しの Promise が解決する前に
// クリーンアップが走り(この時点では kill 未確定のため何も起きない)、2 回目の呼び出しが素通しで
// 来ると sidecar プロセスが二重に spawn され、1 つ目が孤児化してしまう(F11)。同一 dataDir に対する
// 呼び出しが進行中(未解決)であれば新規 spawn せず進行中の Promise を再利用する。挙動が変わるのは
// dev の StrictMode 二重実行時のみで、本番(単発呼び出し)は従来どおり毎回新規に spawn する。
let pending: { dataDir: string; promise: Promise<SidecarHandle> } | null = null;

export function startSidecar(dataDir: string): Promise<SidecarHandle> {
  if (pending && pending.dataDir === dataDir) {
    return pending.promise;
  }
  const promise = spawnSidecar(dataDir).finally(() => {
    if (pending?.promise === promise) pending = null;
  });
  pending = { dataDir, promise };
  return promise;
}

async function spawnSidecar(dataDir: string): Promise<SidecarHandle> {
  const cmd = Command.sidecar("binaries/calsync", [
    "appserver",
    "--config", `${dataDir}/calsync.yaml`,
    "--data", dataDir,
  ]);
  // spawn は 1 回だけ。stdout リスナは spawn 前に登録し、handshake 行が来たら resolve する
  return new Promise((resolve, reject) => {
    let child: Awaited<ReturnType<typeof cmd.spawn>> | undefined;
    // handshake タイムアウト後に cmd.spawn() が遅れて解決するケース(OS 負荷等)への対策(F10)。
    // タイムアウト時点で child が未確定だと `child?.kill()` は何もできず、後から代入される
    // child を誰も kill しないまま孤児プロセスとして残ってしまう。timedOut を憶えておき、
    // spawn 解決時にタイムアウト済みなら直ちに kill する。
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
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
          port: hs.port,
          token: hs.token,
        });
      } catch {
        // 起動ノイズ行は無視して次の行を待つ
      }
    });
    cmd.on("error", (e: string) => { clearTimeout(timer); reject(new Error(e)); });
    void cmd
      .spawn()
      .then((c) => {
        child = c;
        if (timedOut) void c.kill();
      })
      .catch((e) => { clearTimeout(timer); reject(e); });
  });
}
