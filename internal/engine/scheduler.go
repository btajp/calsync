package engine

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/work-a-co/calsync/internal/model"
	"github.com/work-a-co/calsync/internal/provider"
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
func (e *Engine) Run(ctx context.Context) error {
	reauth := make(map[string]bool) // account ID -> reauth_required
	next := nextReconcileAt(e.Now(), e.Cfg.ReconcileAt)
	ticker := time.NewTicker(e.Cfg.PollInterval)
	defer ticker.Stop()

	// 起動直後に 1 ティック実行(初回同期をポーリング間隔まで待たせない)
	e.tick(ctx, reauth)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if !e.Now().Before(next) {
			if err := e.Reconcile(ctx); err != nil {
				log.Printf("reconcile: %v", err)
			}
			next = nextReconcileAt(e.Now(), e.Cfg.ReconcileAt)
			// 日次リコンサイルのタイミングで reauth スキップを解除し再試行する
			// (再認証済みなら次のティックから自動バックフィルされる)
			reauth = make(map[string]bool)
		}
		e.tick(ctx, reauth)
	}
}

// tick は 1 ポーリングサイクル分の同期を行う。
func (e *Engine) tick(ctx context.Context, reauth map[string]bool) {
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
			if err == nil {
				continue
			}
			if errors.Is(err, provider.ErrAuthExpired) {
				reauth[acct.ID] = true
				msg := "reauth_required: run `calsync auth add " + acct.ID + "`"
				if serr := e.Store.SetCalendarError(ref, msg); serr != nil {
					log.Printf("set calendar error %s: %v", ref, serr)
				}
				log.Printf("account %s: authentication expired; pausing sync until reauth (%s)", acct.ID, msg)
				break // 同一アカウントの残りカレンダーもスキップ
			}
			log.Printf("sync %s: %v", ref, err)
			if serr := e.Store.SetCalendarError(ref, err.Error()); serr != nil {
				log.Printf("set calendar error %s: %v", ref, serr)
			}
		}
	}
}
