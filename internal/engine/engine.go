package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/work-a-co/calsync/internal/config"
	"github.com/work-a-co/calsync/internal/model"
	"github.com/work-a-co/calsync/internal/provider"
	"github.com/work-a-co/calsync/internal/store"
)

// Engine は同期エンジン本体。プロバイダ非依存(仕様2章)。
type Engine struct {
	Store     *store.Store
	Providers map[string]provider.Provider // key: account ID
	Cfg       *config.Config
	Now       func() time.Time // テストで固定する。nil なら time.Now
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) currentWindow() model.Window { return e.Cfg.WindowFrom(e.now()) }

func (e *Engine) providerFor(accountID string) (provider.Provider, error) {
	p, ok := e.Providers[accountID]
	if !ok {
		return nil, fmt.Errorf("no provider registered for account %q", accountID)
	}
	return p, nil
}

// idemKeyFor はターゲットの provider 種別から決定的な冪等キーを導出する(仕様6.4)。
// Google はクライアント生成イベントID、Microsoft は transactionId に使う。
func idemKeyFor(providerName, originTag, targetAccountID string) string {
	if providerName == "microsoft" {
		return model.MSTransactionID(originTag, targetAccountID)
	}
	return model.GoogleBlockerID(originTag, targetAccountID)
}

// SyncCalendar は 1 カレンダー分の差分を取得し、各イベントを processEvent へ流す。
// カーソルは Changes が完走した(newCursor 非空)場合のみ永続化する(仕様5.1)。
// 途中失敗時は旧カーソルのまま再実行される(全処理が冪等なので安全)。
// カーソル失効(Google 410 / Graph 410・syncStateNotFound 系)は provider が
// ErrCursorInvalid に正規化して返し、ここで即時 FullResync に切り替えて自己修復する(仕様8章)。
func (e *Engine) SyncCalendar(ctx context.Context, ref model.CalendarRef) error {
	if err := e.Store.UpsertCalendar(ref); err != nil {
		return err
	}
	st, err := e.Store.GetCalendar(ref)
	if err != nil {
		return err
	}
	cursor := ""
	if st != nil {
		cursor = st.Cursor
	}
	w := e.currentWindow()
	p, err := e.providerFor(ref.AccountID)
	if err != nil {
		return err
	}
	events, newCursor, err := p.Changes(ctx, ref, cursor, w)
	if errors.Is(err, provider.ErrCursorInvalid) {
		return e.FullResync(ctx, ref)
	}
	if err != nil {
		return err
	}
	for _, ev := range events {
		if err := e.processEvent(ctx, ref, ev); err != nil {
			return err
		}
	}
	if newCursor != "" {
		if err := e.Store.SetCursor(ref, newCursor, w); err != nil {
			return err
		}
	}
	return nil
}

// processEvent は仕様6.1のフローチャート(コントラクトの決定則1〜5)を実装する。
func (e *Engine) processEvent(ctx context.Context, ref model.CalendarRef, ev model.NormalizedEvent) error {
	// 決定則1: 削除通知。id しか含まれない前提のため、判定はタグではなく mappings で行う
	if ev.Deleted {
		isBlocker, err := e.Store.IsBlocker(ref.AccountID, ev.ID)
		if err != nil {
			return err
		}
		if isBlocker {
			return nil // 自作ブロッカーの削除通知。復元はリコンサイルの責務
		}
		maps, err := e.Store.ListMappingsForOrigin(ref.AccountID, ref.CalendarID, ev.ID)
		if err != nil {
			return err
		}
		if len(maps) == 0 {
			return nil // 未知ID(Graph の @removed ウィンドウ外ノイズ含む)
		}
		// 削除通知には iCalUID が無いので、suppressed 昇格用にキャッシュから引く
		cached, err := e.Store.GetEvent(ref, ev.ID)
		if err != nil {
			return err
		}
		if err := e.deleteBlockersForOrigin(ctx, ref, ev.ID); err != nil {
			return err
		}
		if err := e.Store.DeleteEvent(ref, ev.ID); err != nil {
			return err
		}
		if cached != nil && cached.ICalUID != "" {
			return e.promoteSuppressed(ctx, ref.AccountID, cached.ICalUID)
		}
		return nil
	}

	// 決定則2: ループ遮断(mappings 一次判定 / タグ二次判定)
	isBlocker, err := e.Store.IsBlocker(ref.AccountID, ev.ID)
	if err != nil {
		return err
	}
	if isBlocker || ev.OriginTag != "" {
		return nil
	}

	// 決定則3: ウィンドウ外(クライアント側判定が正。仕様5.3)
	if !InWindow(e.currentWindow(), ev) {
		return e.deleteBlockersForOrigin(ctx, ref, ev.ID)
	}

	// 決定則4: busy でない・辞退済み
	if !ShouldBlock(ev) {
		if err := e.deleteBlockersForOrigin(ctx, ref, ev.ID); err != nil {
			return err
		}
		if err := e.Store.DeleteEvent(ref, ev.ID); err != nil {
			return err
		}
		if ev.ICalUID != "" {
			return e.promoteSuppressed(ctx, ref.AccountID, ev.ICalUID)
		}
		return nil
	}

	// 決定則5: busy かつウィンドウ内 → イベントキャッシュ更新 + 全ターゲットへ upsert
	if err := e.Store.UpsertEvent(ref, ev); err != nil {
		return err
	}
	return e.upsertBlockers(ctx, ref, ev)
}

// upsertBlockers は origin イベントを全ターゲットへ配布する。
// mapping なし → 重複抑止判定の上、作成(intent-first + 決定的冪等キー)
// mapping あり・time_hash 不一致 → UpdateBlocker(patch)
// mapping あり・一致 → 何もしない
func (e *Engine) upsertBlockers(ctx context.Context, ref model.CalendarRef, ev model.NormalizedEvent) error {
	originTag := model.OriginTagOf(ref.AccountID, ev.ID)
	timeHash := model.TimeHash(ev)
	for _, target := range e.Cfg.TargetsOf(ref.AccountID) {
		m, err := e.Store.GetMapping(ref.AccountID, ref.CalendarID, ev.ID, target.ID)
		if err != nil {
			return err
		}
		targetCal := model.CalendarRef{AccountID: target.ID, CalendarID: target.BlockerCalendar}
		p, err := e.providerFor(target.ID)
		if err != nil {
			return err
		}
		switch {
		case m == nil || (m.Status == store.StatusPending && m.BlockerEventID == ""):
			// 新規作成、または pending のまま残った行(クラッシュ跡)の再実行。
			// 冪等キーは決定的なので再送しても二重作成にならない(仕様6.4)
			dup, err := e.isDuplicateOnTarget(target, ev)
			if err != nil {
				return err
			}
			pm := store.Mapping{
				OriginAccount:  ref.AccountID,
				OriginCalendar: ref.CalendarID,
				OriginEventID:  ev.ID,
				TargetAccount:  target.ID,
				TargetCalendar: target.BlockerCalendar,
				IdempotencyKey: idemKeyFor(target.Provider, originTag, target.ID),
				TimeHash:       timeHash,
				Status:         store.StatusSuppressed,
			}
			if dup {
				// 同一会議の実予定がターゲットに存在 → 作成せず suppressed 記録(仕様6.5)
				if err := e.Store.PutMapping(pm); err != nil {
					return err
				}
				continue
			}
			pm.Status = store.StatusPending
			if err := e.Store.PutMapping(pm); err != nil {
				return err
			}
			b, err := e.blockerFor(ctx, ev, originTag, targetCal, p)
			if err != nil {
				return err
			}
			eventID, err := p.CreateBlocker(ctx, targetCal, b, pm.IdempotencyKey)
			if err != nil {
				return err
			}
			pm.BlockerEventID = eventID
			pm.Status = store.StatusActive
			if err := e.Store.PutMapping(pm); err != nil {
				return err
			}
		case m.Status == store.StatusSuppressed:
			// 昇格は Task 9(promoteSuppressed / 再評価)の責務。時刻変更のみ追従する
			if m.TimeHash != timeHash {
				m.TimeHash = timeHash
				if err := e.Store.PutMapping(*m); err != nil {
					return err
				}
			}
		case m.TimeHash != timeHash:
			b, err := e.blockerFor(ctx, ev, originTag, targetCal, p)
			if err != nil {
				return err
			}
			if err := p.UpdateBlocker(ctx, targetCal, m.BlockerEventID, b); err != nil {
				return err
			}
			m.TimeHash = timeHash
			if err := e.Store.PutMapping(*m); err != nil {
				return err
			}
		default:
			// time_hash 一致 → プロバイダ呼び出しなし
		}
	}
	return nil
}

// deleteBlockersForOrigin は origin イベントに紐づく全ターゲットのブロッカーと
// mappings を削除する。mapping が無ければ何もしない。
// suppressed / pending(BlockerEventID 空)の行はプロバイダ呼び出しなしで mapping だけ消す。
func (e *Engine) deleteBlockersForOrigin(ctx context.Context, ref model.CalendarRef, originEventID string) error {
	maps, err := e.Store.ListMappingsForOrigin(ref.AccountID, ref.CalendarID, originEventID)
	if err != nil {
		return err
	}
	for _, m := range maps {
		if m.BlockerEventID != "" {
			p, err := e.providerFor(m.TargetAccount)
			if err != nil {
				return err
			}
			targetCal := model.CalendarRef{AccountID: m.TargetAccount, CalendarID: m.TargetCalendar}
			if err := p.DeleteBlocker(ctx, targetCal, m.BlockerEventID); err != nil {
				return err
			}
		}
		if err := e.Store.DeleteMapping(m.OriginAccount, m.OriginCalendar, m.OriginEventID, m.TargetAccount); err != nil {
			return err
		}
	}
	return nil
}

// blockerFor は origin イベントからターゲット用の Blocker を組み立てる。
// TargetTimezone は calendars.timezone のキャッシュを優先し、無ければ
// provider から取得して calendars テーブルへキャッシュする(仕様6.6)。
func (e *Engine) blockerFor(ctx context.Context, ev model.NormalizedEvent, originTag string, targetCal model.CalendarRef, p provider.Provider) (model.Blocker, error) {
	tz, err := e.targetTimezone(ctx, targetCal, p)
	if err != nil {
		return model.Blocker{}, err
	}
	return model.Blocker{
		Title:          e.Cfg.BlockerTitle,
		StartUTC:       ev.StartUTC,
		EndUTC:         ev.EndUTC,
		IsAllDay:       ev.IsAllDay,
		AllDayStart:    ev.AllDayStart,
		AllDayEnd:      ev.AllDayEnd,
		TargetTimezone: tz,
		OriginTag:      originTag,
	}, nil
}

func (e *Engine) targetTimezone(ctx context.Context, cal model.CalendarRef, p provider.Provider) (string, error) {
	st, err := e.Store.GetCalendar(cal)
	if err != nil {
		return "", err
	}
	if st != nil && st.Timezone != "" {
		return st.Timezone, nil
	}
	tz, err := p.GetCalendarTimezone(ctx, cal)
	if err != nil {
		return "", err
	}
	if err := e.Store.UpsertCalendar(cal); err != nil {
		return "", err
	}
	if err := e.Store.SetCalendarTimezone(cal, tz); err != nil {
		return "", err
	}
	return tz, nil
}

// TokenDeleter は RemoveAccount がトークンファイルを消すための最小インター
// フェース。auth.TokenStore が満たす(engine→auth の依存を作らないためにここで定義)。
type TokenDeleter interface {
	Delete(accountID string) error
}

// RemoveAccount はアカウントを完全に削除する(仕様 11 章 accounts remove)。処理順:
//  1. このアカウント発のブロッカーを全ターゲットのカレンダーから削除(+mappings 行削除)
//  2. このアカウントに置かれた受領ブロッカーを削除(+mappings 行削除)
//  3. events / calendars / トークンのローカル状態を削除
//
// force=true の場合、プロバイダ不在(認証切れで構築不能)や DeleteBlocker の失敗を
// スキップして続行する(リモートにブロッカーが残りうることは呼び出し側が警告する)。
// force=false でエラー中断しても、mappings 行は API 削除成功後にのみ消しているため
// 再実行すれば続きから冪等に完了できる。
func RemoveAccount(ctx context.Context, e *Engine, tokens TokenDeleter, accountID string, force bool) error {
	deleteRemote := func(targetAccount, targetCalendar, eventID string) error {
		if eventID == "" {
			return nil // pending / suppressed はリモートに実体がない
		}
		p, ok := e.Providers[targetAccount]
		if !ok {
			if force {
				return nil
			}
			return fmt.Errorf("account %s: provider unavailable; re-authenticate it or pass --force to skip remote deletion", targetAccount)
		}
		cal := model.CalendarRef{AccountID: targetAccount, CalendarID: targetCalendar}
		if err := p.DeleteBlocker(ctx, cal, eventID); err != nil {
			if force {
				return nil
			}
			return fmt.Errorf("delete blocker %s on %s (pass --force to skip): %w", eventID, cal, err)
		}
		return nil
	}

	// (1) 配布済みブロッカー(このアカウントが origin)
	origins, err := e.Store.ListMappingsWhereOriginAccount(accountID)
	if err != nil {
		return err
	}
	for _, m := range origins {
		if err := deleteRemote(m.TargetAccount, m.TargetCalendar, m.BlockerEventID); err != nil {
			return err
		}
		if err := e.Store.DeleteMapping(m.OriginAccount, m.OriginCalendar, m.OriginEventID, m.TargetAccount); err != nil {
			return err
		}
	}

	// (2) 受領ブロッカー(このアカウントが target。m.TargetAccount == accountID)
	received, err := e.Store.ListMappingsWhereTargetAccount(accountID)
	if err != nil {
		return err
	}
	for _, m := range received {
		if err := deleteRemote(m.TargetAccount, m.TargetCalendar, m.BlockerEventID); err != nil {
			return err
		}
		if err := e.Store.DeleteMapping(m.OriginAccount, m.OriginCalendar, m.OriginEventID, m.TargetAccount); err != nil {
			return err
		}
	}

	// (3) ローカル状態
	if err := e.Store.DeleteEventsForAccount(accountID); err != nil {
		return err
	}
	if err := e.Store.DeleteCalendarsForAccount(accountID); err != nil {
		return err
	}
	return tokens.Delete(accountID)
}
