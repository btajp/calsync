package engine

import (
	"context"

	"github.com/work-a-co/calsync/internal/model"
)

// FullResync は 1 カレンダーに対するカーソル張り直し + set-difference リコンサイル
// (仕様8章 1〜2)。cursor="" のウィンドウ付きフル同期で新カーソルを確立し
// (= ウィンドウのスライド)、フル同期結果と events キャッシュを突合する。
//
// mappings は全ワイプしない: Google の「410 時は全ワイプ」公式手順を mappings に
// 適用するとループ防止(一次判定)と origin→blocker の対応関係が失われるため、
// 破棄するのはカーソルと、フル結果に現れなくなったキャッシュ行のみ(仕様8章の確定判断)。
// カーソル失効時(SyncCalendar からの委譲)と日次リコンサイル(Task 11)の両方で使う。
func (e *Engine) FullResync(ctx context.Context, ref model.CalendarRef) error {
	if err := e.Store.UpsertCalendar(ref); err != nil {
		return err
	}
	p, err := e.providerFor(ref.AccountID)
	if err != nil {
		return err
	}
	w := e.currentWindow()
	events, newCursor, err := p.Changes(ctx, ref, "", w)
	if err != nil {
		return err
	}

	// フル結果のうち「キャッシュに残るべきイベント」(busy・未辞退・ウィンドウ内)の ID 集合。
	// ウィンドウ外・非 busy 化・削除済みはここに入らず、下の消滅処理で掃除される。
	alive := make(map[string]bool, len(events))
	for _, ev := range events {
		if !ev.Deleted && ShouldBlock(ev) && InWindow(w, ev) {
			alive[ev.ID] = true
		}
	}

	// set-difference: キャッシュにあるがフル結果で生きていない → 消滅扱い。
	// ブロッカー削除 + mapping 削除 + キャッシュ削除 + suppressed 昇格
	// (processEvent の削除通知処理と同じ手順。キャッシュ由来の ID なので未知IDガードは不要)
	cachedIDs, err := e.Store.ListEventIDs(ref)
	if err != nil {
		return err
	}
	for _, id := range cachedIDs {
		if alive[id] {
			continue
		}
		cached, err := e.Store.GetEvent(ref, id)
		if err != nil {
			return err
		}
		if err := e.deleteBlockersForOrigin(ctx, ref, id); err != nil {
			return err
		}
		if err := e.Store.DeleteEvent(ref, id); err != nil {
			return err
		}
		if cached != nil && cached.ICalUID != "" {
			if err := e.promoteSuppressed(ctx, ref.AccountID, cached.ICalUID); err != nil {
				return err
			}
		}
	}

	// フル結果を通常の決定則で処理する(新規範囲入りの作成・time_hash 不一致の patch・
	// pending 行の解決は processEvent / upsertBlockers 側の既存ロジックが行う)
	for _, ev := range events {
		if err := e.processEvent(ctx, ref, ev); err != nil {
			return err
		}
	}

	// 完走した時だけ新カーソルと新ウィンドウを永続化する(仕様5.1のカーソル規律と同じ)
	if newCursor != "" {
		if err := e.Store.SetCursor(ref, newCursor, w); err != nil {
			return err
		}
	}
	return nil
}
