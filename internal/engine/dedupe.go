package engine

import (
	"context"

	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/store"
)

// isDuplicateOnTarget は「ターゲットの events キャッシュに同一 iCalUID・同一開始の
// busy 実予定が存在するか」を判定する(仕様6.5)。真の場合、upsertBlockers は
// ブロッカーを作らず status=suppressed の mapping を記録する。
// 判定単位: 時刻指定は iCalUID + StartUTC の完全一致、終日は iCalUID + AllDayStart の一致。
// Google は繰り返し全回が同一 iCalUID、Graph は回ごとに異なるため、単発は確実・
// 繰り返しはベストエフォート(抑止漏れはブロッカーが重なるだけで安全側)。
// Cfg.DedupeSameMeeting=false なら常に false(抑止しない)。
func (e *Engine) isDuplicateOnTarget(target config.Account, ev model.NormalizedEvent) (bool, error) {
	if !e.Cfg.DedupeSameMeeting || ev.ICalUID == "" {
		return false, nil
	}
	allDayStart := ""
	if ev.IsAllDay {
		allDayStart = ev.AllDayStart
	}
	return e.Store.HasBusyEventByICalUID(target.ID, ev.ICalUID, ev.StartUTC, allDayStart)
}

// promoteSuppressed はターゲット上の実予定(icalUID)が消えたときに呼ばれ、
// その実予定を理由に抑止されていた suppressed mapping をブロッカー作成に昇格する
// (仕様6.5「削除処理時に iCalUID で suppressed を逆引き」)。
// 呼び出し元: processEvent の削除通知処理・非 busy 化処理(Task 8 で配線済み)、
// FullResync の set-difference(Task 10)。
func (e *Engine) promoteSuppressed(ctx context.Context, targetAccountID, icalUID string) error {
	if icalUID == "" {
		return nil
	}
	maps, err := e.Store.ListSuppressedByOriginICalUID(targetAccountID, icalUID)
	if err != nil {
		return err
	}
	for _, m := range maps {
		originRef := model.CalendarRef{AccountID: m.OriginAccount, CalendarID: m.OriginCalendar}
		ev, err := e.Store.GetEvent(originRef, m.OriginEventID)
		if err != nil {
			return err
		}
		if ev == nil {
			// ListSuppressedByOriginICalUID は events キャッシュと JOIN 済みのため
			// 通常到達しない(防御)。掃除はリコンサイルの責務
			continue
		}
		target := e.Cfg.AccountByID(m.TargetAccount)
		if target == nil {
			continue // 設定から消えたアカウント。掃除は accounts remove の責務
		}
		// 同一会議の別の実予定がまだ残っていれば昇格しない
		dup, err := e.isDuplicateOnTarget(*target, *ev)
		if err != nil {
			return err
		}
		if dup {
			continue
		}
		if err := e.createFromMapping(ctx, m, *ev); err != nil {
			return err
		}
	}
	return nil
}

// createFromMapping は既存 mapping(suppressed / pending)に記録済みの冪等キーを
// 使ってブロッカーを作成し、active へ遷移させる(intent-first。仕様6.4)。
// 同一キーの再実行は既存 ID が返るため二重作成にならない。
// Task 11 の pending 解決・suppressed 再評価もこのヘルパを使う。
// ターゲットの認証失効は TargetAuthError に包んで返す(仕様9.3。呼び出し元が
// 失効アカウントに帰属させられるようにする)。
func (e *Engine) createFromMapping(ctx context.Context, m store.Mapping, ev model.NormalizedEvent) error {
	p, err := e.providerFor(m.TargetAccount)
	if err != nil {
		return err
	}
	targetCal := model.CalendarRef{AccountID: m.TargetAccount, CalendarID: m.TargetCalendar}
	originTag := model.OriginTagOf(m.OriginAccount, m.OriginEventID)
	b, err := e.blockerFor(ctx, ev, originTag, targetCal, p)
	if err != nil {
		return wrapTargetAuth(m.TargetAccount, err)
	}
	m.TimeHash = e.policyHashFor(model.TimeHash(ev), m.TargetAccount)
	m.Status = store.StatusPending
	m.BlockerEventID = ""
	if err := e.Store.PutMapping(m); err != nil {
		return err
	}
	eventID, err := p.CreateBlocker(ctx, targetCal, b, m.IdempotencyKey)
	if err != nil {
		return wrapTargetAuth(m.TargetAccount, err)
	}
	m.BlockerEventID = eventID
	m.Status = store.StatusActive
	return e.Store.PutMapping(m)
}
