import { useCallback, useEffect, useState } from "react";
import type { ApiClient } from "./api";
import { ApiError } from "./api";
import type { MaintenanceState } from "./types";

// running 中のみ 3 秒間隔でポーリングする(デスクトップ設計 2026-07-23 §4)。done/error に遷移したら
// 自動的に止まる(依存する useEffect が phase !== "running" で早期リターンするため)。
const POLL_INTERVAL_MS = 3000;

/**
 * 保存・デーモン操作・リコンサイル実行を無効化すべきかどうかを判定する純関数。
 * state が null(未取得)の間は「実行中でない」扱いにする(初回ロード前に全操作をブロックしない)。
 */
export function isMaintenanceBlocking(state: MaintenanceState | null): boolean {
  return state?.phase === "running";
}

/**
 * ダッシュボードのデーモン状態カードの表示ラベルを決める純関数。
 * メンテナンス実行中は appserver が launchctl bootout でデーモンを止めるため daemon.running が
 * false になり、通常のクラッシュ・停止と区別が付かない(デスクトップ設計 2026-07-23 §4 の
 * 統合事実)。そのため maintenance state を daemon.running より優先して判定する。
 */
export function daemonRunningLabel(daemonRunning: boolean, maintenance: MaintenanceState | null): string {
  if (isMaintenanceBlocking(maintenance)) return "メンテナンス中";
  return daemonRunning ? "稼働中" : "停止中";
}

function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    return e.hint ? `${e.message}(${e.hint})` : e.message;
  }
  return String(e);
}

export interface UseMaintenanceResult {
  state: MaintenanceState | null;
  blocking: boolean;
  triggerError: string | null;
  /** POST /api/maintenance/reconcile を呼び、直後に state を再取得する。失敗時は triggerError に反映した上で再スローする。 */
  trigger: () => Promise<void>;
}

/**
 * maintenance state の唯一の取得元。App.tsx(Shell)で 1 回だけ生成し、Dashboard/ConfigForm/
 * AccountAdd へ props で配る(デスクトップ設計 2026-07-23 §4: 「App.tsx レベルで maintenance
 * state を管理し、各ページに渡す」)。
 */
export function useMaintenance(api: ApiClient): UseMaintenanceResult {
  const [state, setState] = useState<MaintenanceState | null>(null);
  const [triggerError, setTriggerError] = useState<string | null>(null);

  const refresh = useCallback(() => api.maintenanceState().then(setState), [api]);

  // 初回取得。ここで running が判明すればすぐ下のポーリング effect が引き継ぐ。
  useEffect(() => {
    refresh().catch(() => {
      /* ベストエフォート。取得できなくても running 中でなければ通常操作は継続できる */
    });
  }, [refresh]);

  useEffect(() => {
    if (state?.phase !== "running") return;
    const id = setInterval(() => {
      refresh().catch(() => {
        /* ポーリング失敗はベストエフォート。次周期に再試行する */
      });
    }, POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [state?.phase, refresh]);

  const trigger = useCallback(async () => {
    setTriggerError(null);
    try {
      await api.maintenanceReconcile();
      await refresh();
    } catch (e) {
      setTriggerError(describeError(e));
      throw e;
    }
  }, [api, refresh]);

  return { state, blocking: isMaintenanceBlocking(state), triggerError, trigger };
}
