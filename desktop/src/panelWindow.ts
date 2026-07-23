import { WebviewWindow } from "@tauri-apps/api/webviewWindow";
import { PhysicalPosition } from "@tauri-apps/api/dpi";
import { TauriEvent } from "@tauri-apps/api/event";
import type { TrayIconEvent } from "@tauri-apps/api/tray";

const PANEL_LABEL = "panel";
const PANEL_WIDTH = 380;
const PANEL_HEIGHT = 540;

let panelPromise: Promise<WebviewWindow> | null = null;

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
  });
  await new Promise<void>((resolve, reject) => {
    void panel.once("tauri://created", () => resolve());
    void panel.once("tauri://error", (event) => reject(new Error(String(event.payload))));
  });
  // フォーカスが外れたら隠す(デスクトップトレイ設計 2026-07-23 §3.2)。
  void panel.listen(TauriEvent.WINDOW_BLUR, () => { void panel.hide(); });
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
 */
export async function showPanelNearTray(event: TrayIconEvent): Promise<void> {
  if (event.type !== "Click") return;
  const panel = await ensurePanel();
  const centerX = event.rect.position.x + event.rect.size.width / 2;
  const x = Math.round(centerX - PANEL_WIDTH / 2);
  const y = Math.round(event.rect.position.y + event.rect.size.height);
  await panel.setPosition(new PhysicalPosition(x, y));
  await panel.show();
  await panel.setFocus();
}
