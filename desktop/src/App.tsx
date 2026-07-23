import { useCallback, useEffect, useState } from "react";
import { open } from "@tauri-apps/plugin-dialog";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { emitTo, listen } from "@tauri-apps/api/event";
import { exit } from "@tauri-apps/plugin-process";
import { ApiClient, ApiError } from "./api";
import { startSidecar } from "./sidecar";
import type { SidecarHandle } from "./sidecar";
import { useMaintenance } from "./maintenance";
import { initTray, scheduleFetchRange, setTrayEvents } from "./tray";
import { showPanelNearTray } from "./panelWindow";
import Dashboard from "./pages/Dashboard";
import ConfigForm from "./pages/ConfigForm";
import AccountAdd from "./pages/AccountAdd";
import CalendarView from "./pages/CalendarView";
import UpdateBanner from "./components/UpdateBanner";
import MaintenanceBanner from "./components/MaintenanceBanner";

// トレイの events 共有(5分ごと)の間隔(デスクトップトレイ設計 2026-07-23 §3.1)。
const TRAY_EVENTS_REFRESH_MS = 5 * 60_000;

export default function App() {
  const [api, setApi] = useState<ApiClient | null>(null);
  // F12: フォルダ選択直後、サイドカー起動(handshake)自体は calsync.yaml の有無に関わらず成功する
  // (appserver はリスナー起動時点で handshake を出し、設定ファイルは各 API 呼び出し時に読む)ため、
  // 「間違って親フォルダ等を選んだ」ケースをここで検出できない。起動直後に 1 回 GET /api/config を
  // 叩き、config_read(Stat/ReadFile 失敗。典型はファイル不在)のときだけ警告する。YAML パース
  // エラー等の別種の失敗はここでは判定せず通常フロー(各ページの読み込みエラー表示)に委ねる。
  const [configWarning, setConfigWarning] = useState<SidecarHandle | null>(null);
  // トレイ/ポップオーバーへ渡す接続情報(port/token)+サイドカー kill を Shell へ渡すために保持する
  // (デスクトップトレイ設計 2026-07-23 §3.2)。api が確定するのと同じ非同期フローの中で、
  // getConfig の成否に関わらず一度だけ設定する。
  const [sidecarHandle, setSidecarHandle] = useState<SidecarHandle | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [dataDir, setDataDir] = useState<string | null>(localStorage.getItem("calsync.dataDir"));
  const [retry, setRetry] = useState(0); // 起動エラー画面の再試行で effect を再発火させる

  useEffect(() => {
    if (!dataDir) return;
    let kill: (() => void) | undefined;
    let cancelled = false;
    startSidecar(dataDir)
      .then(async (s) => {
        kill = s.kill;
        // dev の StrictMode は同一 dataDir の effect を 2 回連続実行するが、sidecar.ts の
        // module スコープガード(F11)により両者は同じ Promise を共有する。そのため先に
        // クリーンアップ済みの(=もう使われない)側の .then も実行されてしまい、
        // onCloseRequested の二重登録や getConfig の二重呼び出しが起きていた。cancelled は
        // どちらの effect 呼び出しかを区別できるので、ここで早期リターンして防ぐ
        // (レビュー Minor 対応)。
        if (cancelled) return;
        setSidecarHandle(s);
        // メインウィンドウを閉じてもアプリは常駐継続する(hide に変更。デスクトップトレイ設計
        // 2026-07-23 §3.3)。既存のサイドカー kill 処理はここから外し、Shell の
        // "quit-app" イベント(ポップオーバーの「終了」ボタン起点)でのみ行う —
        // stdin EOF が最終防衛のため、hide 時にサイドカーを殺してはならない。
        void getCurrentWindow().onCloseRequested((event) => {
          event.preventDefault();
          void getCurrentWindow().hide();
        });
        try {
          await s.api.getConfig();
          if (!cancelled) setApi(s.api);
        } catch (e) {
          if (cancelled) return;
          if (e instanceof ApiError && e.code === "config_read") {
            setConfigWarning(s);
          } else {
            // calsync.yaml 不在以外の失敗(YAML 破損等)は通常フローに委ね、各ページのエラー
            // 表示・再試行導線(F8 等)に任せる。
            setApi(s.api);
          }
        }
      })
      .catch((e) => { if (!cancelled) setError(String(e)); });
    return () => { cancelled = true; kill?.(); };
  }, [dataDir, retry]);

  const resetDataDir = useCallback(() => {
    localStorage.removeItem("calsync.dataDir");
    setError(null);
    setConfigWarning(null);
    setApi(null);
    setDataDir(null);
  }, []);

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
  if (error) {
    return (
      <main className="setup">
        <h1>calsync</h1>
        <UpdateBanner />
        <p className="error">起動エラー: {error}</p>
        <div className="button-row">
          <button onClick={() => { setError(null); setRetry((r) => r + 1); }}>
            再試行
          </button>
          <button className="link-button" onClick={resetDataDir}>
            データフォルダ変更
          </button>
        </div>
      </main>
    );
  }
  if (configWarning) {
    return (
      <main className="setup">
        <h1>calsync</h1>
        <UpdateBanner />
        <p className="error">
          選択したフォルダに calsync.yaml がありません。data ディレクトリを選んでいますか?
        </p>
        <div className="button-row">
          <button onClick={resetDataDir}>選び直す</button>
          <button
            className="link-button"
            onClick={() => {
              setApi(configWarning.api);
              setConfigWarning(null);
            }}
          >
            このまま使う
          </button>
        </div>
      </main>
    );
  }
  // sidecarHandle は api と同じ非同期フローの中で先に設定されるため、api が確定した
  // 時点では常に非 null(型ガードとして両方をチェックする)。
  if (!api || !sidecarHandle) return <main>appserver に接続中…</main>;
  return <Shell api={api} sidecar={sidecarHandle} onResetDataDir={resetDataDir} />;
}

type Tab = "dashboard" | "calendar" | "config" | "account-add";

const TABS: { key: Tab; label: string }[] = [
  { key: "dashboard", label: "ダッシュボード" },
  { key: "calendar", label: "カレンダー" },
  { key: "config", label: "設定" },
  { key: "account-add", label: "アカウント追加" },
];

// ModeCheck は起動直後に 1 回だけ行うコンテナ検出の結果。仕様§9のコンテナ
// ガード(ホスト側からは DB 読み取りを含む全機能を停止して案内表示モードに
// 落とす)をフロント側でも反映する。ポーリングはしない(初回判定+再チェック
// ボタンで十分。サーバ側の各エンドポイントは常にガードされているため、稼働中
// にコンテナが起動する稀なケースでも書き込み系は 409 で守られる)。
type ModeCheck = "checking" | "container" | "ok" | "error";

function Shell({
  api,
  sidecar,
  onResetDataDir,
}: {
  api: ApiClient;
  sidecar: SidecarHandle;
  onResetDataDir: () => void;
}) {
  const [tab, setTab] = useState<Tab>("dashboard");
  const [modeCheck, setModeCheck] = useState<ModeCheck>("checking");
  const [modeCheckError, setModeCheckError] = useState<string | null>(null);
  // maintenance state は App(Shell)レベルで一元管理し、各タブへ props で配る
  // (デスクトップ設計 2026-07-23 §4)。フックは早期 return より前で無条件に呼ぶ必要があるため
  // ここで生成する(container/error 分岐でも呼ばれるが、ポーリングは running 判明時のみ)。
  const maintenance = useMaintenance(api);

  // トレイ・ポップオーバーの初期化(api 接続確立後・デスクトップトレイ設計 2026-07-23 §3)。
  // フックは早期 return より前で無条件に呼ぶ必要があるため maintenance と同じ位置に置く。
  useEffect(() => {
    let cancelled = false;

    void initTray((event) => {
      void showPanelNearTray(event);
    });

    const panelReadyPromise = listen("panel-ready", () => {
      void emitTo("panel", "api-info", { port: sidecar.port, token: sidecar.token });
    });
    // ポップオーバーの「終了」ボタン起点。ウィンドウを跨いだ JS モジュールスコープの共有が
    // 無いため、kill クロージャを持つこちら側で「kill してから exit」の順序を保証する
    // (デスクトップトレイ設計 2026-07-23 §3.3。stdin EOF は最終防衛であり、これが唯一の
    // 明示 kill 呼び出し箇所になる)。
    const quitAppPromise = listen("quit-app", () => {
      sidecar.kill();
      void exit(0);
    });

    const fetchTrayEvents = () => {
      const { from, to } = scheduleFetchRange(new Date());
      api
        .events(from, to)
        .then((res) => {
          if (!cancelled) setTrayEvents(res.events);
        })
        .catch(() => {
          // ベストエフォート。トレイタイトルは古いデータのまま次周期まで維持する
        });
    };
    fetchTrayEvents();
    const id = setInterval(fetchTrayEvents, TRAY_EVENTS_REFRESH_MS);

    return () => {
      cancelled = true;
      clearInterval(id);
      void panelReadyPromise.then((u) => u());
      void quitAppPromise.then((u) => u());
    };
  }, [api, sidecar]);

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
        {/* エラー時でも自動更新には到達できるようにする(0.1.0 でここが不具合で
            行き止まりになり、修正版を配れなかった教訓) */}
        <UpdateBanner />
        <p className="error">状態確認に失敗しました: {modeCheckError}</p>
        <div className="button-row">
          <button onClick={checkMode}>再試行</button>
          <button className="link-button" onClick={onResetDataDir}>
            データフォルダ変更
          </button>
        </div>
      </main>
    );
  }

  if (modeCheck === "container") {
    return (
      <main className="setup">
        <h1>calsync</h1>
        <UpdateBanner />
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
    <main className={tab === "calendar" ? "wide" : undefined}>
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

      <UpdateBanner />
      <MaintenanceBanner state={maintenance.state} />

      {tab === "dashboard" && <Dashboard api={api} maintenance={maintenance} />}
      {tab === "calendar" && <CalendarView api={api} />}
      {tab === "config" && (
        <ConfigForm api={api} maintenance={maintenance} onGoToAccountAdd={() => setTab("account-add")} />
      )}
      {tab === "account-add" && (
        <AccountAdd api={api} maintenance={maintenance} onGoToDashboard={() => setTab("dashboard")} />
      )}
    </main>
  );
}
