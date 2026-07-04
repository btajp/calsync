package engine

import (
	"context"
	"log"
	"time"

	"github.com/btajp/calsync/internal/model"
)

// nextReconcileAt は now と同じロケーションで、hhmm("15:04" 形式)が指す直近の
// 未来時刻を返す。当日の hhmm が now より未来ならその日、now 以前なら翌日。
// hhmm が不正な場合は既定の "04:00" にフォールバックする(config 側の検証が正、
// ここは防御的フォールバック)。
func nextReconcileAt(now time.Time, hhmm string) time.Time {
	parsed, err := time.Parse("15:04", hhmm)
	if err != nil {
		parsed, _ = time.Parse("15:04", "04:00")
	}
	candidate := time.Date(now.Year(), now.Month(), now.Day(),
		parsed.Hour(), parsed.Minute(), 0, 0, now.Location())
	if candidate.After(now) {
		return candidate
	}
	return candidate.AddDate(0, 0, 1)
}

// Run はデーモン本体。
//   - Cfg.PollInterval ごとに全アカウント・全カレンダーを SyncCalendar する
//   - SyncCalendar が provider.ErrAuthExpired を返したアカウントは reauth_required
//     として以後のティックでスキップし、SetCalendarError に記録する(仕様 9.3)。
//     他アカウントの同期は継続する
//   - Cfg.ReconcileAt(コンテナのローカル TZ)を過ぎたら Reconcile を実行する。
//     Reconcile 後は reauth スキップを解除して再試行の機会を与える
//   - ctx キャンセルで nil を返して終了する
//
// consecutiveFailureResyncThreshold: 同一カレンダーの同期がこの回数連続で失敗したら、
// カーソルが毒された(Graph が 410/syncStateNotFound ではなく持続的な 5xx を返し続けた
// 実測ケース・2026-07-04)とみなし、FullResync でカーソルを再初期化して自己回復する。
const consecutiveFailureResyncThreshold = 5

func (e *Engine) Run(ctx context.Context) error {
	reauth := make(map[string]bool)             // account ID -> reauth_required
	failures := make(map[model.CalendarRef]int) // 連続同期失敗回数
	next := nextReconcileAt(e.now(), e.Cfg.ReconcileAt)
	ticker := time.NewTicker(e.Cfg.PollInterval)
	defer ticker.Stop()

	// 起動直後に 1 ティック実行(初回同期をポーリング間隔まで待たせない)
	e.tick(ctx, reauth, failures)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if !e.now().Before(next) {
			if err := e.Reconcile(ctx); err != nil {
				log.Printf("reconcile: %v", err)
			}
			next = nextReconcileAt(e.now(), e.Cfg.ReconcileAt)
			// 日次リコンサイルのタイミングで reauth スキップを解除し再試行する
			// (再認証済みなら次のティックから自動バックフィルされる)
			reauth = make(map[string]bool)
			failures = make(map[model.CalendarRef]int)
		}
		e.tick(ctx, reauth, failures)
	}
}

// tick は 1 ポーリングサイクル分の同期を行う。
func (e *Engine) tick(ctx context.Context, reauth map[string]bool, failures map[model.CalendarRef]int) {
	for _, acct := range e.Cfg.Accounts {
		if reauth[acct.ID] {
			continue
		}
		for _, calID := range acct.Calendars {
			if ctx.Err() != nil {
				return
			}
			ref := model.CalendarRef{AccountID: acct.ID, CalendarID: calID}
			err := e.SyncCalendar(ctx, ref)

			// ターゲットアカウントの認証失効(TargetAuthError)は origin ではなく
			// 該当ターゲットに帰属させる(仕様9.3)。origin の同期は継続する
			for _, tae := range collectTargetAuthErrors(err) {
				if reauth[tae.AccountID] {
					continue
				}
				reauth[tae.AccountID] = true
				msg := "reauth_required: run `calsync auth add " + tae.AccountID + "`"
				if target := e.Cfg.AccountByID(tae.AccountID); target != nil {
					for _, tcalID := range target.Calendars {
						tref := model.CalendarRef{AccountID: tae.AccountID, CalendarID: tcalID}
						if serr := e.Store.SetCalendarError(tref, msg); serr != nil {
							log.Printf("set calendar error %s: %v", tref, serr)
						}
					}
				}
				log.Printf("account %s: authentication expired (detected while writing blockers); pausing sync until reauth (%s)", tae.AccountID, msg)
			}

			if err == nil || onlyTargetAuthErrors(err) {
				// 成功した同期を記録する: 過去の last_error(reauth_required 等)を
				// クリアし、last_synced_at を更新する(仕様11章の calsync status が
				// 参照する)。SetCalendarError は "" 指定でエラークリア + 時刻更新の
				// 両方を担う(store 契約)。TargetAuthError のみの場合、origin の
				// 同期自体は完走している(カーソルも前進済み)ため成功扱いにする
				delete(failures, ref)
				if serr := e.Store.SetCalendarError(ref, ""); serr != nil {
					log.Printf("clear calendar error %s: %v", ref, serr)
				}
				continue
			}
			// origin 自身の失効のみ reauth 扱いにする(errors.Is だと TargetAuthError
			// 配下の ErrAuthExpired まで origin に誤帰属するため使わない)
			if originAuthExpired(err) {
				reauth[acct.ID] = true
				msg := "reauth_required: run `calsync auth add " + acct.ID + "`"
				if serr := e.Store.SetCalendarError(ref, msg); serr != nil {
					log.Printf("set calendar error %s: %v", ref, serr)
				}
				log.Printf("account %s: authentication expired; pausing sync until reauth (%s)", acct.ID, msg)
				delete(failures, ref)
				break // 同一アカウントの残りカレンダーもスキップ
			}
			failures[ref]++
			if failures[ref] >= consecutiveFailureResyncThreshold {
				log.Printf("sync %s: %v (%d consecutive failures; forcing full resync to re-establish the cursor)", ref, err, failures[ref])
				delete(failures, ref)
				if rerr := e.FullResync(ctx, ref); rerr != nil {
					log.Printf("forced full resync %s: %v", ref, rerr)
					if serr := e.Store.SetCalendarError(ref, rerr.Error()); serr != nil {
						log.Printf("set calendar error %s: %v", ref, serr)
					}
				} else if serr := e.Store.SetCalendarError(ref, ""); serr != nil {
					log.Printf("clear calendar error %s: %v", ref, serr)
				}
				continue
			}
			log.Printf("sync %s: %v", ref, err)
			if serr := e.Store.SetCalendarError(ref, err.Error()); serr != nil {
				log.Printf("set calendar error %s: %v", ref, serr)
			}
		}
	}
}
