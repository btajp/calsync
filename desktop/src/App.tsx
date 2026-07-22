import { useCallback, useEffect, useState } from "react";
import { open } from "@tauri-apps/plugin-dialog";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { ApiClient, ApiError } from "./api";
import { startSidecar } from "./sidecar";
import type { SidecarHandle } from "./sidecar";
import { useMaintenance } from "./maintenance";
import Dashboard from "./pages/Dashboard";
import ConfigForm from "./pages/ConfigForm";
import AccountAdd from "./pages/AccountAdd";
import CalendarView from "./pages/CalendarView";
import UpdateBanner from "./components/UpdateBanner";
import MaintenanceBanner from "./components/MaintenanceBanner";

export default function App() {
  const [api, setApi] = useState<ApiClient | null>(null);
  // F12: フォルダ選択直後、サイドカー起動(handshake)自体は calsync.yaml の有無に関わらず成功する
  // (appserver はリスナー起動時点で handshake を出し、設定ファイルは各 API 呼び出し時に読む)ため、
  // 「間違って親フォルダ等を選んだ」ケースをここで検出できない。起動直後に 1 回 GET /api/config を
  // 叩き、config_read(Stat/ReadFile 失敗。典型はファイル不在)のときだけ警告する。YAML パース
  // エラー等の別種の失敗はここでは判定せず通常フロー(各ページの読み込みエラー表示)に委ねる。
  const [configWarning, setConfigWarning] = useState<SidecarHandle | null>(null);
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
        void getCurrentWindow().onCloseRequested(() => s.kill());
        try {
          await s.api.getConfig();
          if (!cancelled) setApi(s.api);
        } catch (e) {
          if (cancelled) return;
          if (e instanceof ApiError && e.code === "config_read") {
            setConfigWarning({ api: s.api, kill: s.kill });
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
  if (!api) return <main>appserver に接続中…</main>;
  return <Shell api={api} onResetDataDir={resetDataDir} />;
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

function Shell({ api, onResetDataDir }: { api: ApiClient; onResetDataDir: () => void }) {
  const [tab, setTab] = useState<Tab>("dashboard");
  const [modeCheck, setModeCheck] = useState<ModeCheck>("checking");
  const [modeCheckError, setModeCheckError] = useState<string | null>(null);
  // maintenance state は App(Shell)レベルで一元管理し、各タブへ props で配る
  // (デスクトップ設計 2026-07-23 §4)。フックは早期 return より前で無条件に呼ぶ必要があるため
  // ここで生成する(container/error 分岐でも呼ばれるが、ポーリングは running 判明時のみ)。
  const maintenance = useMaintenance(api);

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
