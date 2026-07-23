import { WebviewWindow } from "@tauri-apps/api/webviewWindow";
import { LogicalPosition } from "@tauri-apps/api/dpi";
import { TauriEvent } from "@tauri-apps/api/event";
import { monitorFromPoint } from "@tauri-apps/api/window";
import type { BackgroundThrottlingPolicy } from "@tauri-apps/api/window";
import type { TrayIconEvent } from "@tauri-apps/api/tray";

const PANEL_LABEL = "panel";
const PANEL_WIDTH = 380;
const PANEL_HEIGHT = 540;
// macOS 14+ は非表示/隠れたウェブビューを約5分で suspend し、タイマー・イベント処理を止める
// (レビュー Critical 対応)。パネルは隠れている時間が大半のため無効化する。
// `BackgroundThrottlingPolicy` は @tauri-apps/api/window から型としてのみ export されている
// (window.js の実行時 export リストに値としては含まれないことを確認済み)ため、実体は
// リテラル文字列 "disabled" をアサーションで渡す。tauri.conf.json 側(main ウィンドウ)の
// 静的設定と同じ値。macOS 14 未満・Linux/Windows/Android では no-op(公式型定義の JSDoc に明記)。
const BACKGROUND_THROTTLING_DISABLED = "disabled" as BackgroundThrottlingPolicy;

let panelPromise: Promise<WebviewWindow> | null = null;

// トレイ再クリックでのトグル閉じ対応。macOS ではパネル表示中にトレイをクリックすると
// (1) パネルの blur → hide が先に走り (2) その後に Click イベントが届くため、素朴な
// 「表示中なら閉じる」判定だけでは (2) で再表示されてしまう。hide 直後の短時間は
// Click を「閉じる操作の後半」とみなして再表示を抑制する(ポップオーバーの定石)。
const DISMISS_SUPPRESS_MS = 350;
// show 直後にトグル閉じを受け付けない猶予(マウスチャタリング対策)。
const SHOW_GRACE_MS = 300;
let panelShown = false;
let panelHiddenAt = 0;
let panelShownAt = 0;

function markPanelHidden(): void {
  panelShown = false;
  panelHiddenAt = Date.now();
}

/**
 * hide 直後のトレイクリックを「トグルで閉じた」として扱うべきかの純粋判定。
 * now が hiddenAt より過去(時計巻き戻し等)の場合は抑制しない。
 */
export function shouldSuppressShow(
  hiddenAt: number,
  now: number,
  thresholdMs: number = DISMISS_SUPPRESS_MS,
): boolean {
  const elapsed = now - hiddenAt;
  return elapsed >= 0 && elapsed < thresholdMs;
}

async function createPanel(): Promise<WebviewWindow> {
  // dev のホットリロード等で JS 側の状態は失われても OS 側にウィンドウが残っている
  // ケースへの備え。既存があれば新規生成せず再利用する。
  const existing = await WebviewWindow.getByLabel(PANEL_LABEL);
  if (existing) return existing;

  const panel = new WebviewWindow(PANEL_LABEL, {
    url: "index.html?panel=1",
    decorations: false,
    alwaysOnTop: true,
    skipTaskbar: true,
    visible: false,
    width: PANEL_WIDTH,
    height: PANEL_HEIGHT,
    resizable: false,
    backgroundThrottling: BACKGROUND_THROTTLING_DISABLED,
  });
  await new Promise<void>((resolve, reject) => {
    void panel.once("tauri://created", () => resolve());
    void panel.once("tauri://error", (event) => reject(new Error(String(event.payload))));
  });
  // フォーカスが外れたら隠す(デスクトップトレイ設計 2026-07-23 §3.2)。
  // markPanelHidden の記録が、直後に届くトレイ Click のトグル判定(shouldSuppressShow)に使われる。
  void panel.listen(TauriEvent.WINDOW_BLUR, () => {
    markPanelHidden();
    void panel.hide();
  });
  return panel;
}

/**
 * ポップオーバーウィンドウ(label "panel")を初回クリック時にだけ生成し、以後は同じ
 * インスタンスを show/hide で再利用する。dev の React.StrictMode 二重実行対策として、
 * 進行中/完了済みの Promise をモジュールスコープで共有する(tray.ts の initTray と同じ
 * ガード方式)。失敗時は次回呼び出しで再試行できるようガードを解除する。
 */
async function ensurePanel(): Promise<WebviewWindow> {
  if (panelPromise) return panelPromise;
  panelPromise = createPanel().catch((e: unknown) => {
    panelPromise = null;
    throw e;
  });
  return panelPromise;
}

/**
 * トレイのクリックイベントを受けてポップオーバーを表示する(デスクトップトレイ設計
 * 2026-07-23 §3.2)。位置はトレイアイコンの矩形(event.rect)基準で、x はアイコン中心 -
 * パネル幅の半分、y はアイコン矩形の下端(メニューバー下)にする。
 *
 * event.rect は物理ピクセル(PhysicalPosition/PhysicalSize)だが、WebviewWindow の
 * width/height オプション(PANEL_WIDTH/PANEL_HEIGHT)は論理ピクセルであるため、物理値の
 * まま PANEL_WIDTH を差し引くと Retina(scaleFactor 2 以上)のディスプレイで位置がずれる
 * (最終ホールレビュー Fix 3)。トレイクリック座標が属するモニタの scaleFactor で
 * event.rect を論理ピクセルに変換してから PANEL_WIDTH と同じ座標系で計算する。
 * monitorFromPoint が null を返す(モニタ境界の特定失敗)場合のみ、パネル自身の
 * scaleFactor にフォールバックする。
 */
export async function showPanelNearTray(event: TrayIconEvent): Promise<void> {
  // Click は 1 回の物理クリックで buttonState "Down" と "Up" の 2 イベントが届く
  // (@tauri-apps/api/tray.d.ts の MouseButtonState)。両方に反応すると Down で開いた
  // 直後に Up がトグル判定に入り「一回のクリックで開いて閉じる」ため、Up だけ扱う。
  if (event.type !== "Click" || event.buttonState !== "Up") return;
  const panel = await ensurePanel();
  // トグル閉じ: 表示中(blur が発火しない経路)ならこのクリックで閉じる。
  // ただし show 直後(チャタリング・連打)は閉じ操作とみなさない。
  if (panelShown) {
    if (shouldSuppressShow(panelShownAt, Date.now(), SHOW_GRACE_MS)) return;
    markPanelHidden();
    await panel.hide();
    return;
  }
  // blur → hide 直後のクリックは「閉じる操作」の後半なので再表示しない。
  if (shouldSuppressShow(panelHiddenAt, Date.now())) return;
  const monitor = await monitorFromPoint(event.rect.position.x, event.rect.position.y);
  const scaleFactor = monitor?.scaleFactor ?? (await panel.scaleFactor());
  const logicalRectPos = event.rect.position.toLogical(scaleFactor);
  const logicalRectSize = event.rect.size.toLogical(scaleFactor);
  const centerX = logicalRectPos.x + logicalRectSize.width / 2;
  const x = Math.round(centerX - PANEL_WIDTH / 2);
  const y = Math.round(logicalRectPos.y + logicalRectSize.height);
  await panel.setPosition(new LogicalPosition(x, y));
  await panel.show();
  await panel.setFocus();
  panelShown = true;
  panelShownAt = Date.now();
}
