package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/work-a-co/calsync/internal/model"
	"github.com/work-a-co/calsync/internal/provider"
	"github.com/work-a-co/calsync/internal/store"
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
	require.Equal(t, model.TimeHash(ev), m.TimeHash)
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
