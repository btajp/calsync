package engine

import (
	"context"
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
