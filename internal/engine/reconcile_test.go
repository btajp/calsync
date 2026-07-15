package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
	"github.com/btajp/calsync/internal/store"
)

// FullResync の set-difference: 1件消滅・1件時刻変更・1件新規・1件生存。
// 生存分の mappings が保持される(全ワイプしない)ことを明示的にアサートする。
func TestFullResync_SetDifference(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()

	evKeep := busyEvent("ev-keep")
	evGone := busyEvent("ev-gone")
	evMoved := busyEvent("ev-moved")

	// 事前状態: 3件を通常同期済み(events キャッシュ + active mappings + ブロッカー)
	f.SetFullState(refA, []model.NormalizedEvent{evKeep, evGone, evMoved})
	require.NoError(t, e.SyncCalendar(ctx, refA)) // cursor "c1"
	require.Len(t, f.Blockers(calBv), 3)
	require.Len(t, f.Blockers(calCv), 3)

	keepBefore, err := e.Store.GetMapping("a", "primary", "ev-keep", "b")
	require.NoError(t, err)
	require.NotNil(t, keepBefore)
	movedBefore, err := e.Store.GetMapping("a", "primary", "ev-moved", "b")
	require.NoError(t, err)
	require.NotNil(t, movedBefore)

	// 実カレンダーの現状: ev-gone 消滅・ev-moved 時刻変更・ev-new 新規
	moved := evMoved
	moved.EndUTC = evMoved.EndUTC.Add(30 * time.Minute)
	evNew := busyEvent("ev-new")
	f.SetFullState(refA, []model.NormalizedEvent{evKeep, moved, evNew})

	// リコンサイル時点の now は 1 日進んでいる(ウィンドウのスライドを検証)
	now2 := testNow.AddDate(0, 0, 1)
	e.Now = func() time.Time { return now2 }

	require.NoError(t, e.FullResync(ctx, refA))

	// 消滅分: ブロッカー削除 + mapping 削除 + キャッシュ削除
	for _, acct := range []string{"b", "c"} {
		m, err := e.Store.GetMapping("a", "primary", "ev-gone", acct)
		require.NoError(t, err)
		require.Nil(t, m)
	}
	cached, err := e.Store.GetEvent(refA, "ev-gone")
	require.NoError(t, err)
	require.Nil(t, cached)

	// 生存分の mapping は保持される(全ワイプしない): resync 前と同じ行・同じブロッカーID
	keepAfter, err := e.Store.GetMapping("a", "primary", "ev-keep", "b")
	require.NoError(t, err)
	require.NotNil(t, keepAfter)
	require.Equal(t, keepBefore.BlockerEventID, keepAfter.BlockerEventID)
	require.Equal(t, store.StatusActive, keepAfter.Status)

	// 変更分: UpdateBlocker(同一イベントIDのまま time_hash 更新。再作成ではない)
	movedAfter, err := e.Store.GetMapping("a", "primary", "ev-moved", "b")
	require.NoError(t, err)
	require.NotNil(t, movedAfter)
	require.Equal(t, movedBefore.BlockerEventID, movedAfter.BlockerEventID)
	require.Equal(t, model.TimeHash(moved), movedAfter.TimeHash)

	// 新規分: 作成
	newMap, err := e.Store.GetMapping("a", "primary", "ev-new", "b")
	require.NoError(t, err)
	require.NotNil(t, newMap)
	require.Equal(t, store.StatusActive, newMap.Status)
	newCached, err := e.Store.GetEvent(refA, "ev-new")
	require.NoError(t, err)
	require.NotNil(t, newCached)

	// ブロッカーの最終状態: keep / moved(更新済) / new の3本
	for _, cal := range []model.CalendarRef{calBv, calCv} {
		blks := f.Blockers(cal)
		require.Len(t, blks, 3, "calendar %s", cal)
		byTag := map[string]model.BlockerRecord{}
		for _, b := range blks {
			byTag[b.OriginTag] = b
		}
		require.Contains(t, byTag, "a:ev-keep")
		require.Contains(t, byTag, "a:ev-new")
		require.Equal(t, model.TimeHash(moved), byTag["a:ev-moved"].TimeHash)
	}

	// カーソルは張り直され、新しい Window(now2 起点)が保存される
	st, err := e.Store.GetCalendar(refA)
	require.NoError(t, err)
	require.NotNil(t, st)
	require.Equal(t, "c2", st.Cursor) // SyncCalendar で c1 → FullResync 完走で c2
	w := e.Cfg.WindowFrom(now2)
	require.True(t, st.Window.Start.Equal(w.Start), "window start: got %v want %v", st.Window.Start, w.Start)
	require.True(t, st.Window.End.Equal(w.End), "window end: got %v want %v", st.Window.End, w.End)
}

// ウィンドウ外へ移動した元予定: ブロッカー・mapping・キャッシュがすべて掃除される
// (processEvent の決定則3はキャッシュを消さないため、set-difference 側の責務)
func TestFullResync_RemovesOutOfWindowEventFromCache(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	f.SetFullState(refA, []model.NormalizedEvent{ev})
	require.NoError(t, e.SyncCalendar(ctx, refA))
	require.Len(t, f.Blockers(calBv), 1)

	// 実カレンダー上でウィンドウ外(now+4ヶ月)へ移動した
	movedOut := ev
	movedOut.StartUTC = testNow.AddDate(0, 4, 0)
	movedOut.EndUTC = movedOut.StartUTC.Add(time.Hour)
	f.SetFullState(refA, []model.NormalizedEvent{movedOut})

	require.NoError(t, e.FullResync(ctx, refA))

	require.Empty(t, f.Blockers(calBv))
	require.Empty(t, f.Blockers(calCv))
	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Nil(t, m)
	cached, err := e.Store.GetEvent(refA, "ev1")
	require.NoError(t, err)
	require.Nil(t, cached)
}

// FullResync が途中失敗しても旧カーソル・ブロッカー・mappings は無傷
// (完走時のみ SetCursor。冪等なので再実行で復旧する)
func TestFullResync_FailureKeepsCursorAndState(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	f.SetFullState(refA, []model.NormalizedEvent{busyEvent("ev1")})
	require.NoError(t, e.SyncCalendar(ctx, refA)) // cursor "c1"

	f.FailNext(refA, provider.ErrAuthExpired)
	err := e.FullResync(ctx, refA)
	require.ErrorIs(t, err, provider.ErrAuthExpired)

	st, gerr := e.Store.GetCalendar(refA)
	require.NoError(t, gerr)
	require.Equal(t, "c1", st.Cursor)
	require.Len(t, f.Blockers(calBv), 1)
	m, gerr := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, gerr)
	require.NotNil(t, m)
}

// SyncCalendar が provider から ErrCursorInvalid を受けたとき、
// 自動で FullResync に切り替わり、呼び出し元にはエラーを返さない(仕様8章)
func TestSyncCalendar_CursorInvalidTriggersFullResync(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	f.SetFullState(refA, []model.NormalizedEvent{busyEvent("ev1")})
	require.NoError(t, e.SyncCalendar(ctx, refA)) // cursor "c1"

	// カーソル失効。実カレンダーは ev1 消滅・ev2 出現に変わっている
	f.SetFullState(refA, []model.NormalizedEvent{busyEvent("ev2")})
	f.FailNext(refA, provider.ErrCursorInvalid)

	require.NoError(t, e.SyncCalendar(ctx, refA)) // エラーにならず自己修復

	// set-difference が適用されている: ev1 のブロッカーは消え、ev2 が立つ
	for _, cal := range []model.CalendarRef{calBv, calCv} {
		blks := f.Blockers(cal)
		require.Len(t, blks, 1, "calendar %s", cal)
		require.Equal(t, "a:ev2", blks[0].OriginTag)
	}
	m1, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Nil(t, m1)
	m2, err := e.Store.GetMapping("a", "primary", "ev2", "b")
	require.NoError(t, err)
	require.NotNil(t, m2)
	require.Equal(t, store.StatusActive, m2.Status)

	// FailNext はカーソル連番を消費しないため、FullResync 完走で "c2"
	st, err := e.Store.GetCalendar(refA)
	require.NoError(t, err)
	require.Equal(t, "c2", st.Cursor)
}

// 複数カレンダーで新ウィンドウのカーソルが前進する
func TestReconcile_AdvancesCursorsForAllCalendars(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	f.SetFullState(refA, []model.NormalizedEvent{busyEvent("ev1")})
	// b・c のフル状態は空

	require.NoError(t, e.Reconcile(ctx))

	w := e.Cfg.WindowFrom(testNow)
	for _, ref := range []model.CalendarRef{refA, calBv, calCv} {
		st, err := e.Store.GetCalendar(ref)
		require.NoError(t, err)
		require.NotNil(t, st, "calendar %s", ref)
		require.Equal(t, "c1", st.Cursor, "calendar %s(fake はカレンダーごとに独立の連番)", ref)
		require.True(t, st.Window.Start.Equal(w.Start), "calendar %s", ref)
		require.True(t, st.Window.End.Equal(w.End), "calendar %s", ref)
	}
	// 通常配布も行われている
	require.Len(t, f.Blockers(calBv), 1)
	require.Len(t, f.Blockers(calCv), 1)
}

// adoption: mappings 未登録のタグ付きブロッカー(DB 消失起因の孤児)は、
// origin イベントが生存していれば PutMapping で active として収容される。
// origin アカウントの同期障害(reauth_required 相当)が他の処理を止めないことも兼ねる。
func TestReconcile_AdoptsOrphanWhenOriginAlive(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	tag := model.OriginTagOf("a", "ev1")

	// origin アカウント a は今回の同期に失敗するが、
	// 過去の同期で events キャッシュには ev1 が残っている
	require.NoError(t, e.Store.UpsertCalendar(refA))
	require.NoError(t, e.Store.UpsertEvent(refA, ev))
	f.FailNext(refA, provider.ErrAuthExpired)

	// b 上に mappings 未登録のタグ付きブロッカーを事前配置
	f.SeedBlocker(calBv, model.BlockerRecord{
		EventID:   "orphan-b1",
		OriginTag: tag,
		TimeHash:  model.TimeHash(ev),
	})

	err := e.Reconcile(ctx)
	require.ErrorIs(t, err, provider.ErrAuthExpired) // a の失敗は集約されて返る

	// 孤児は削除・再作成ではなく、そのまま active として収容される
	m, gerr := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, gerr)
	require.NotNil(t, m)
	require.Equal(t, store.StatusActive, m.Status)
	require.Equal(t, "orphan-b1", m.BlockerEventID)
	// 収容時は内容成分を再現できないため sentinel 付きで保存される(スペック 2026-07-15 §6)。
	// origin のフル同期が失敗しているので修復 patch は次回に持ち越される
	require.Equal(t, model.TimeHash(ev)+"+detail:unknown", m.TimeHash)
	require.Equal(t, model.MSTransactionID(tag, "b"), m.IdempotencyKey)
	blks := f.Blockers(calBv)
	require.Len(t, blks, 1)
	require.Equal(t, "orphan-b1", blks[0].EventID)

	// a の障害は b・c のリコンサイルを止めない
	stB, gerr := e.Store.GetCalendar(calBv)
	require.NoError(t, gerr)
	require.Equal(t, "c1", stB.Cursor)
	stC, gerr := e.Store.GetCalendar(calCv)
	require.NoError(t, gerr)
	require.Equal(t, "c1", stC.Cursor)
}

// adoption: origin イベントが消滅している孤児は DeleteBlocker で掃除される
func TestReconcile_CleansOrphanWhenOriginGone(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()

	f.SeedBlocker(calBv, model.BlockerRecord{
		EventID:   "orphan-b2",
		OriginTag: model.OriginTagOf("a", "ghost"),
		TimeHash:  "deadbeef00000000",
	})

	require.NoError(t, e.Reconcile(ctx))

	require.Empty(t, f.Blockers(calBv))
	m, err := e.Store.GetMapping("a", "primary", "ghost", "b")
	require.NoError(t, err)
	require.Nil(t, m) // mapping も作られない
}

// adoption: タグ形式が不正なイベントは calsync 製と断定できないため触らない
func TestReconcile_IgnoresBlockerWithMalformedTag(t *testing.T) {
	e, f := newTestEngine(t)
	f.SeedBlocker(calBv, model.BlockerRecord{EventID: "weird", OriginTag: "no-separator", TimeHash: "h"})

	require.NoError(t, e.Reconcile(context.Background()))

	require.Len(t, f.Blockers(calBv), 1) // 残る
}

// pending 解決: CreateBlocker 成功と active 更新の間のクラッシュ跡。
// fake に同一 idemKey のブロッカーが既存 → CreateBlocker 再実行で既存 ID が返り active 化。
func TestReconcile_ResolvesPendingWithExistingBlocker(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	tag := model.OriginTagOf("a", "ev1")
	idem := model.MSTransactionID(tag, "b")

	// クラッシュ跡の再現: ブロッカー作成は成功済みだが mapping は pending のまま
	preID, err := f.CreateBlocker(ctx, calBv, model.Blocker{
		Title: "予定あり", StartUTC: ev.StartUTC, EndUTC: ev.EndUTC,
		TargetTimezone: "Asia/Tokyo", OriginTag: tag,
	}, idem)
	require.NoError(t, err)
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: "ev1",
		TargetAccount: "b", TargetCalendar: "primary",
		IdempotencyKey: idem, TimeHash: model.TimeHash(ev),
		Status: store.StatusPending, // BlockerEventID は空のまま
	}))
	// origin は生きている(今回のフル同期にも現れる)
	f.SetFullState(refA, []model.NormalizedEvent{ev})

	require.NoError(t, e.Reconcile(ctx))

	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, store.StatusActive, m.Status)
	require.Equal(t, preID, m.BlockerEventID) // 既存 ID で収容
	require.Len(t, f.Blockers(calBv), 1)      // 二重作成なし・adoption にも掃除されない
}

// pending 解決: origin が消滅している pending 行は intent ごと破棄される
func TestReconcile_DropsPendingWhenOriginGone(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: "ghost",
		TargetAccount: "b", TargetCalendar: "primary",
		IdempotencyKey: "k", TimeHash: "h",
		Status: store.StatusPending,
	}))

	require.NoError(t, e.Reconcile(ctx))

	m, err := e.Store.GetMapping("a", "primary", "ghost", "b")
	require.NoError(t, err)
	require.Nil(t, m)
	require.Empty(t, f.Blockers(calBv)) // 作成もされない
}

// suppressed 再評価: 抑止理由だった重複実予定が(削除通知を取り逃がすなどで)
// いつの間にか消えていた場合、リコンサイルが昇格する
func TestReconcile_PromotesSuppressedWhenDuplicateGone(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	origin, _ := dupPair(false)
	tag := model.OriginTagOf("a", origin.ID)

	// 過去に重複抑止された suppressed 行。重複実予定は b のどこにも存在しない
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: origin.ID,
		TargetAccount: "b", TargetCalendar: "primary",
		IdempotencyKey: model.MSTransactionID(tag, "b"),
		TimeHash:       model.TimeHash(origin),
		Status:         store.StatusSuppressed,
	}))
	// origin は生きている
	f.SetFullState(refA, []model.NormalizedEvent{origin})

	require.NoError(t, e.Reconcile(ctx))

	m, err := e.Store.GetMapping("a", "primary", origin.ID, "b")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, store.StatusActive, m.Status)
	require.NotEmpty(t, m.BlockerEventID)
	blks := f.Blockers(calBv)
	require.Len(t, blks, 1)
	require.Equal(t, tag, blks[0].OriginTag)
}

// suppressed 再評価: 重複実予定がまだ生きていれば suppressed のまま
func TestReconcile_KeepsSuppressedWhileDuplicateAlive(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	origin, real := dupPair(false)
	tag := model.OriginTagOf("a", origin.ID)

	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: origin.ID,
		TargetAccount: "b", TargetCalendar: "primary",
		IdempotencyKey: model.MSTransactionID(tag, "b"),
		TimeHash:       model.TimeHash(origin),
		Status:         store.StatusSuppressed,
	}))
	f.SetFullState(refA, []model.NormalizedEvent{origin})
	f.SetFullState(calBv, []model.NormalizedEvent{real}) // 重複実予定は健在

	require.NoError(t, e.Reconcile(ctx))

	m, err := e.Store.GetMapping("a", "primary", origin.ID, "b")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, store.StatusSuppressed, m.Status)
	require.Empty(t, m.BlockerEventID)
	require.Empty(t, f.Blockers(calBv)) // b 自身にはブロッカーが立たない
}

// suppressed 再評価: origin が消滅した suppressed 行は掃除される
func TestReconcile_DropsSuppressedWhenOriginGone(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: "ghost",
		TargetAccount: "b", TargetCalendar: "primary",
		IdempotencyKey: "k", TimeHash: "h",
		Status: store.StatusSuppressed,
	}))

	require.NoError(t, e.Reconcile(ctx))

	m, err := e.Store.GetMapping("a", "primary", "ghost", "b")
	require.NoError(t, err)
	require.Nil(t, m)
	require.Empty(t, f.Blockers(calBv))
}

// 手で消されたブロッカーの再作成(仕様8章4: 元予定が生きている限りブロッカーは
// 維持する、確定仕様)。active mapping はそのままに、ブロッカー本体だけを
// DeleteBlocker で直接消す(手動削除の再現)。processEvent の time_hash 一致判定は
// プロバイダを一切呼ばないため(engine.go upsertBlockers の default ケース)、この
// 消失は restoreMissingBlockers でしか検知できない(最終ホールブランチレビュー所見2)。
func TestReconcile_RestoresManuallyDeletedBlocker(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")

	f.SetFullState(refA, []model.NormalizedEvent{ev})
	require.NoError(t, e.SyncCalendar(ctx, refA))

	before, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.Equal(t, store.StatusActive, before.Status)
	require.NotEmpty(t, before.BlockerEventID)

	// 手動削除の再現: mapping には触れず、ブロッカー本体だけを消す
	require.NoError(t, f.DeleteBlocker(ctx, calBv, before.BlockerEventID))
	require.Empty(t, f.Blockers(calBv))

	require.NoError(t, e.Reconcile(ctx))

	after, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, store.StatusActive, after.Status, "再作成後も active")
	require.NotEmpty(t, after.BlockerEventID, "新しいブロッカー ID が記録される")

	blks := f.Blockers(calBv)
	require.Len(t, blks, 1)
	require.Equal(t, "a:ev1", blks[0].OriginTag)
	require.Equal(t, after.BlockerEventID, blks[0].EventID)
}

// UpdateBlocker が provider.ErrNotFound を返した場合(ブロッカー手動削除後に
// 元予定の時刻が変わったケース)、upsertBlockers は mapping を pending 化して
// createFromMapping で再作成する(リコンサイルを待たない即時フォールバック)。
// fake の UpdateBlocker は未存在 ID に対して自然に ErrNotFound を返す。
func TestUpsertBlockers_UpdateNotFoundRecreatesBlocker(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")

	f.SetFullState(refA, []model.NormalizedEvent{ev})
	require.NoError(t, e.SyncCalendar(ctx, refA))

	before, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, before)

	// 手動削除の再現: mapping には触れず、b 上のブロッカー本体だけを消す
	require.NoError(t, f.DeleteBlocker(ctx, calBv, before.BlockerEventID))
	require.Empty(t, f.Blockers(calBv))

	// 元予定の時刻変更 → time_hash 不一致で UpdateBlocker 分岐に入る
	moved := ev
	moved.EndUTC = ev.EndUTC.Add(30 * time.Minute)
	require.NoError(t, e.processEvent(ctx, refA, moved))

	after, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, store.StatusActive, after.Status)
	require.NotEmpty(t, after.BlockerEventID)
	require.Equal(t, model.TimeHash(moved), after.TimeHash)

	blks := f.Blockers(calBv)
	require.Len(t, blks, 1, "ブロッカーが再作成される")
	require.Equal(t, after.BlockerEventID, blks[0].EventID)
	require.Equal(t, model.TimeHash(moved), blks[0].TimeHash, "再作成は変更後の時刻で行われる")

	// c 側のブロッカーは通常の Update 経路で更新されている(巻き添えなし)
	blksC := f.Blockers(calCv)
	require.Len(t, blksC, 1)
	require.Equal(t, model.TimeHash(moved), blksC[0].TimeHash)
}

// 実在するブロッカーは再作成の対象にならない(余計な CreateBlocker が起きない)。
func TestReconcile_DoesNotRecreateBlockerThatStillExists(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")

	f.SetFullState(refA, []model.NormalizedEvent{ev})
	require.NoError(t, e.SyncCalendar(ctx, refA))

	before, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.Len(t, f.Blockers(calBv), 1)

	require.NoError(t, e.Reconcile(ctx))

	after, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, before.BlockerEventID, after.BlockerEventID, "既存ブロッカーの ID は変わらない")
	require.Len(t, f.Blockers(calBv), 1, "余計な CreateBlocker は起きない")
}

// errListBlockersBoom は failingListProvider が注入する ListBlockers の失敗。
var errListBlockersBoom = errors.New("boom: list blockers failed")

// failingListProvider は ListBlockers のみを常に失敗させるラッパー
// (リコンサイル後半フェーズのエラー集約・続行の検証用)。
type failingListProvider struct {
	provider.Provider
}

func (p *failingListProvider) ListBlockers(ctx context.Context, cal model.CalendarRef, window model.Window) ([]model.BlockerRecord, error) {
	return nil, errListBlockersBoom
}

// Reconcile 後半フェーズは 1 アカウントの障害で全体を中断しない(仕様10章。
// 最終ホールブランチレビュー追補 Issue 6)。アカウント a の ListBlockers が
// 失敗しても、b 上の孤児 adoption は実行され、エラーは集約されて返る。
// 修正前は adoptOrphans が a のエラーで即 return し、b の孤児が収容されなかった。
func TestReconcile_ListBlockersFailureDoesNotStopOtherAdoptions(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	tag := model.OriginTagOf("a", "ev1")

	// origin イベントは(過去の同期で)キャッシュに生存。a の FullResync 自体は
	// 失敗させ、upsertBlockers が孤児より先に正規ブロッカーを作らないようにする
	require.NoError(t, e.Store.UpsertCalendar(refA))
	require.NoError(t, e.Store.UpsertEvent(refA, ev))
	f.FailNext(refA, provider.ErrAuthExpired)

	// b 上に mappings 未登録の孤児ブロッカー
	f.SeedBlocker(calBv, model.BlockerRecord{
		EventID:   "orphan-b1",
		OriginTag: tag,
		TimeHash:  model.TimeHash(ev),
	})

	// アカウント a(Cfg.Accounts の先頭)の ListBlockers を失敗させる
	e.Providers["a"] = &failingListProvider{Provider: f}

	err := e.Reconcile(ctx)
	require.ErrorIs(t, err, errListBlockersBoom, "a の adoption 障害は集約されて返る")
	require.ErrorIs(t, err, provider.ErrAuthExpired, "a の resync 失敗も併せて集約される")

	// b の adoption は実行されている
	m, gerr := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, gerr)
	require.NotNil(t, m, "b の孤児は a の障害に関係なく収容される")
	require.Equal(t, store.StatusActive, m.Status)
	require.Equal(t, "orphan-b1", m.BlockerEventID)
}

// errCreateBlockerBoom は onceFailingCreateProvider が注入する CreateBlocker の失敗。
var errCreateBlockerBoom = errors.New("boom: create blocker failed")

// onceFailingCreateProvider は provider.Provider をラップし、CreateBlocker のみを
// fail フラグが立っている間失敗させる(他メソッドは埋め込みの fake にそのまま委譲)。
// fake パッケージ自体には CreateBlocker の失敗注入手段が無く、それを追加すると
// 他のテストへの影響範囲が広がるため、このテストファイル内だけで完結するラッパーで代替する。
type onceFailingCreateProvider struct {
	provider.Provider
	fail bool
}

func (p *onceFailingCreateProvider) CreateBlocker(ctx context.Context, cal model.CalendarRef, b model.Blocker, idemKey string) (string, error) {
	if p.fail {
		return "", errCreateBlockerBoom
	}
	return p.Provider.CreateBlocker(ctx, cal, b, idemKey)
}

// pending 解決: CreateBlocker が失敗した場合、mapping は pending のまま残り
// (active に遷移しない)、ブロッカーも作られない。障害が解消すれば次回 Reconcile の
// resolvePending で再解決される(intent-first の再実行安全性。仕様6.4)。
// a の FullResync は両呼び出しとも FailNext で失敗させ、upsertBlockers 側が
// 同じ mapping を先取りして処理しないようにする(origin イベントは直接キャッシュに
// 置く)ことで、CreateBlocker の成否が resolvePending 単独の効果として観測できるようにする。
func TestReconcile_PendingStaysPendingWhenCreateBlockerFails(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	tag := model.OriginTagOf("a", "ev1")
	idem := model.MSTransactionID(tag, "b")

	// クラッシュ跡の再現: pending 行のみ存在し、ブロッカーはまだ作られていない。
	// origin イベントは(過去の同期で)events キャッシュに残っている
	require.NoError(t, e.Store.UpsertCalendar(refA))
	require.NoError(t, e.Store.UpsertEvent(refA, ev))
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: "ev1",
		TargetAccount: "b", TargetCalendar: "primary",
		IdempotencyKey: idem, TimeHash: model.TimeHash(ev),
		Status: store.StatusPending, // BlockerEventID は空のまま
	}))

	// b への CreateBlocker だけを失敗させる
	fp := &onceFailingCreateProvider{Provider: f, fail: true}
	e.Providers["b"] = fp
	// a の FullResync 自体は失敗させ、upsertBlockers がこの pending 行を
	// 先取り解決しないようにする
	f.FailNext(refA, provider.ErrAuthExpired)

	err := e.Reconcile(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, provider.ErrAuthExpired)
	require.ErrorIs(t, err, errCreateBlockerBoom)

	m, gerr := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, gerr)
	require.NotNil(t, m)
	require.Equal(t, store.StatusPending, m.Status) // active に遷移しない
	require.Empty(t, m.BlockerEventID)
	require.Empty(t, f.Blockers(calBv))

	// 障害解消後: 次回 Reconcile の resolvePending が同一冪等キーで再解決する
	// (a の FullResync 自体は今回も失敗させ、resolvePending 単独の効果を見る)
	fp.fail = false
	f.FailNext(refA, provider.ErrAuthExpired)
	err = e.Reconcile(ctx)
	require.ErrorIs(t, err, provider.ErrAuthExpired) // a の resync 失敗は残る

	m2, gerr := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, gerr)
	require.NotNil(t, m2)
	require.Equal(t, store.StatusActive, m2.Status) // resolvePending は解決している
	require.NotEmpty(t, m2.BlockerEventID)
	require.Len(t, f.Blockers(calBv), 1)
}

// TestReconcile_RebuildsLoopPreventionBeforeDistribution は DB 全損からの再構築時の
// ループ遮断を検証する。実障害(2026-07-04): DB 再構築で mappings が空の状態のまま配布が
// 先に走り、Graph delta がタグを返せない制約と重なって、Microsoft カレンダー上の受領
// ブロッカー(タグ不可視の busy イベントとして届く)が実予定と誤認され全カレンダーへ
// 再ミラーされた(複製957件)。フェーズ0(タグからの mappings 先行再構築)がこれを防ぐ。
func TestReconcile_RebuildsLoopPreventionBeforeDistribution(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()

	// 実 origin: a の実予定 ev1(過去の稼働で b へ配布済みという状況を再現)
	f.SetFullState(refA, []model.NormalizedEvent{busyEvent("ev1")})

	// b 上に既存ブロッカー blk1(タグは ListBlockers でのみ見える = Graph の実挙動)
	f.SeedBlocker(calBv, model.BlockerRecord{
		EventID:   "blk1",
		OriginTag: model.OriginTagOf("a", "ev1"),
		TimeHash:  model.TimeHash(busyEvent("ev1")),
	})
	// b の差分/フル同期はそのブロッカーを「タグなしの busy イベント」として返す
	// (Graph delta は拡張プロパティを返せない公式制約の再現)
	tagless := busyEvent("blk1")
	tagless.ICalUID = "blk1@fake"
	f.SetFullState(calBv, []model.NormalizedEvent{tagless})

	// 既存汚染の再現: 過去の事故で blk1 が b のイベントキャッシュに実予定として
	// 誤キャッシュされ、b:blk1 を origin とする複製が c に配布済み(mapping+実体)
	require.NoError(t, e.Store.UpsertEvent(calBv, tagless))
	f.SeedBlocker(calCv, model.BlockerRecord{
		EventID:   "dupOnC",
		OriginTag: model.OriginTagOf("b", "blk1"),
		TimeHash:  model.TimeHash(tagless),
	})
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "b", OriginCalendar: "primary", OriginEventID: "blk1",
		TargetAccount: "c", TargetCalendar: "primary",
		BlockerEventID: "dupOnC", IdempotencyKey: "k-dup", TimeHash: model.TimeHash(tagless),
		Status: store.StatusActive,
	}))

	require.NoError(t, e.Reconcile(ctx))

	// (0) 既存汚染が完全に除去されている: 誤キャッシュ行・複製 mapping・複製実体
	ids, err := e.Store.ListEventIDs(calBv)
	require.NoError(t, err)
	require.NotContains(t, ids, "blk1", "自作ブロッカーの誤キャッシュ行は set-difference で掃除される")
	for _, b := range f.Blockers(calCv) {
		require.NotEqual(t, "dupOnC", b.EventID, "既存の複製ブロッカーは物理削除される")
	}
	dm, err := e.Store.GetMapping("b", "primary", "blk1", "c")
	require.NoError(t, err)
	require.Nil(t, dm, "複製 mapping は削除される")

	// (1) b 上のブロッカーが実予定と誤認されて配布されていないこと(ループ遮断)
	bOrigin, err := e.Store.ListMappingsWhereOriginAccount("b")
	require.NoError(t, err)
	require.Empty(t, bOrigin, "b 上の受領ブロッカーを origin として再ミラーしてはならない")
	require.Empty(t, f.Blockers(refA), "origin a のカレンダーにブロッカーが逆流してはならない")
	require.Len(t, f.Blockers(calCv), 1, "c には a:ev1 由来の1件だけが立つ(blk1 の複製が立ってはならない)")

	// (2) 既存ブロッカー blk1 はタグから mappings に再収容されている(重複作成なし)
	require.Len(t, f.Blockers(calBv), 1, "b のブロッカーは既存 blk1 の1件のまま")
	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, "blk1", m.BlockerEventID)
	require.Equal(t, store.StatusActive, m.Status)
}

// TestReconcile_CleansActiveMappingsWithDeadOrigins は「origin がイベントキャッシュに
// 存在しない active mapping」の自動掃除を検証する。汚染された mapping(実在しない origin を
// 指す)が残ると restoreMissingBlockers が複製を再作成し続けるため、フル同期が成功した
// カレンダーの origin についてはキャッシュ非存在 = origin 消滅として blocker ごと掃除する。
func TestReconcile_CleansActiveMappingsWithDeadOrigins(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()

	// a のフル同期結果に evGone は存在しない(実予定ではない)
	f.SetFullState(refA, []model.NormalizedEvent{busyEvent("ev1")})

	// 汚染: 実在しない origin a:evGone を指す active mapping と、その blocker が b 上にある
	f.SeedBlocker(calBv, model.BlockerRecord{
		EventID:   "blkGone",
		OriginTag: model.OriginTagOf("a", "evGone"),
		TimeHash:  "deadbeef00000000",
	})
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: "evGone",
		TargetAccount: "b", TargetCalendar: "primary",
		BlockerEventID: "blkGone", IdempotencyKey: "k-evGone", TimeHash: "deadbeef00000000",
		Status: store.StatusActive,
	}))

	require.NoError(t, e.Reconcile(ctx))

	// 汚染ブロッカーは物理削除され、mapping も消え、復元もされない
	for _, b := range f.Blockers(calBv) {
		require.NotEqual(t, "blkGone", b.EventID, "origin 消滅ブロッカーは掃除される")
	}
	m, err := e.Store.GetMapping("a", "primary", "evGone", "b")
	require.NoError(t, err)
	require.Nil(t, m, "origin 消滅の mapping は削除される")
}

// 日次リコンサイルが reminders_sent の古い行(start_utc < now-48h)を掃除する(スペック 4.3)。
func TestReconcileCleansOldReminderRecords(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()
	now := e.now()
	old := now.Add(-72 * time.Hour)
	recent := now.Add(-time.Hour)
	require.NoError(t, e.Store.MarkReminderSent(refA, "old", "o@x", old, old))
	require.NoError(t, e.Store.MarkReminderSent(refA, "recent", "r@x", recent, recent))

	require.NoError(t, e.Reconcile(ctx))

	sent, err := e.Store.WasReminderSent(refA, "old", old)
	require.NoError(t, err)
	require.False(t, sent)
	sent, err = e.Store.WasReminderSent(refA, "recent", recent)
	require.NoError(t, err)
	require.True(t, sent)
}

// ---- detail_sync: 収容・再構築経路の sentinel(スペック 2026-07-15 §6) ----

// タグ再構築(フェーズ0)は sentinel 付きで収容し、同一リコンサイル内の FullResync
// 再処理で 1 回だけ patch されて正しい内容+正規ハッシュに自己修復する。
// sentinel はペア設定の有無に関わらず付与される(ペア解除 → DB 全損 → 再構築の
// 経路で転記内容が残留するプライバシー穴を塞ぐ)。
func TestReconcile_RebuildSelfHealsBlockerContent(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	ev.Title = "経営会議"
	tag := model.OriginTagOf("a", "ev1")

	// DB 全損相当: mappings は空だが、b 上に「転記タイトル付き」ブロッカーが残っている
	// (ペア設定があった時期に作られた想定)。origin はフル同期で生きている
	f.SetFullState(refA, []model.NormalizedEvent{ev})
	f.SetFullState(calBv, nil)
	f.SetFullState(calCv, nil)
	blkID, err := f.CreateBlocker(ctx, calBv, model.Blocker{
		Title: "経営会議", StartUTC: ev.StartUTC, EndUTC: ev.EndUTC, OriginTag: tag,
	}, model.MSTransactionID(tag, "b"))
	require.NoError(t, err)

	// ペアは現在「未設定」= 復帰の期待値は既定内容
	require.NoError(t, e.Reconcile(ctx))

	// 転記タイトルが既定の「予定あり」へ復帰し、ハッシュも正規値(素の TimeHash)になる
	body, ok := f.StoredBlocker(calBv, blkID)
	require.True(t, ok)
	require.Equal(t, "予定あり", body.Title)
	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Equal(t, store.StatusActive, m.Status)
	require.Equal(t, model.TimeHash(ev), m.TimeHash, "修復後は sentinel が消えて正規ハッシュ")

	// 2 回目のリコンサイルでは patch が走らない(1 回限りの自己修復)
	f.SeedBlocker(calBv, model.BlockerRecord{EventID: blkID, OriginTag: tag, TimeHash: "tampered"})
	require.NoError(t, e.Reconcile(ctx))
	require.Equal(t, "tampered", f.Blockers(calBv)[0].TimeHash, "2 回目は呼び出しなし")
}

// ペア設定ありでも同様: 再構築 → FullResync で転記内容+正規ハッシュに収斂する
func TestReconcile_RebuildSelfHealsWithDetailSyncPair(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	enableDetailSync(e, "a", "b", true, false)
	ev := busyEvent("ev1")
	ev.Title = "経営会議"
	tag := model.OriginTagOf("a", "ev1")

	f.SetFullState(refA, []model.NormalizedEvent{ev})
	f.SetFullState(calBv, nil)
	f.SetFullState(calCv, nil)
	// 古い既定タイトルのままのブロッカーが残っている(ペア設定前に作られた想定)
	blkID, err := f.CreateBlocker(ctx, calBv, model.Blocker{
		Title: "予定あり", StartUTC: ev.StartUTC, EndUTC: ev.EndUTC, OriginTag: tag,
	}, model.MSTransactionID(tag, "b"))
	require.NoError(t, err)

	require.NoError(t, e.Reconcile(ctx))

	body, ok := f.StoredBlocker(calBv, blkID)
	require.True(t, ok)
	require.Equal(t, "経営会議", body.Title, "再構築後の修復 patch で転記タイトルになる")
	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Equal(t,
		model.TimeHash(ev)+"+detail:"+model.DetailHash(true, false, ev.Title, ev.Description),
		m.TimeHash)
}
