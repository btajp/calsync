package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/store"
)

// dupPair は「a の origin 予定」と「b 上の同一会議の実予定」の組を返す。
// 同一 iCalUID で、時刻指定なら同一開始時刻、終日なら同一開始日(仕様6.5の判定単位)。
// Task 11 の suppressed 再評価テストからも再利用される。
func dupPair(allDay bool) (origin, real model.NormalizedEvent) {
	if allDay {
		origin = model.NormalizedEvent{
			ID: "a-ev", ICalUID: "meet-1@example.com", IsAllDay: true,
			AllDayStart: "2026-07-15", AllDayEnd: "2026-07-16", IsBusy: true,
		}
		real = model.NormalizedEvent{
			ID: "b-ev", ICalUID: "meet-1@example.com", IsAllDay: true,
			AllDayStart: "2026-07-15", AllDayEnd: "2026-07-16", IsBusy: true,
		}
		return origin, real
	}
	start := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	origin = model.NormalizedEvent{
		ID: "a-ev", ICalUID: "meet-1@example.com",
		StartUTC: start, EndUTC: start.Add(time.Hour), IsBusy: true,
	}
	real = model.NormalizedEvent{
		ID: "b-ev", ICalUID: "meet-1@example.com",
		// 終了時刻は判定に使わない(開始一致のみ)ことを検証するため意図的にズラす
		StartUTC: start, EndUTC: start.Add(30 * time.Minute), IsBusy: true,
	}
	return origin, real
}

// 抑止: ターゲット b のキャッシュに同一会議の busy 実予定がある場合、
// b にはブロッカーを作らず status=suppressed の mapping を記録する。
// 時刻指定と終日の両方を検証する。
func TestDedupe_SuppressesSameMeetingOnTarget(t *testing.T) {
	cases := []struct {
		name   string
		allDay bool
	}{
		{"時刻指定: 同一 iCalUID + 同一開始時刻", false},
		{"終日: 同一 iCalUID + 同一開始日", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, f := newTestEngine(t)
			ctx := context.Background()
			origin, real := dupPair(tc.allDay)

			// b のイベントキャッシュに同一会議の busy 実予定を置く
			require.NoError(t, e.Store.UpsertEvent(calBv, real))

			require.NoError(t, e.processEvent(ctx, refA, origin))

			// b にはブロッカーを作らない
			require.Empty(t, f.Blockers(calBv))
			m, err := e.Store.GetMapping("a", "primary", "a-ev", "b")
			require.NoError(t, err)
			require.NotNil(t, m)
			require.Equal(t, store.StatusSuppressed, m.Status)
			require.Empty(t, m.BlockerEventID)
			require.Equal(t, model.TimeHash(origin), m.TimeHash)
			// 冪等キーは suppressed の時点で確定している(昇格時にそのまま使う)
			require.Equal(t, model.MSTransactionID(model.OriginTagOf("a", "a-ev"), "b"), m.IdempotencyKey)

			// 重複のない c には通常どおり作成される
			require.Len(t, f.Blockers(calCv), 1)
			mc, err := e.Store.GetMapping("a", "primary", "a-ev", "c")
			require.NoError(t, err)
			require.NotNil(t, mc)
			require.Equal(t, store.StatusActive, mc.Status)
		})
	}
}

// 「同一会議」でなければ抑止しない(iCalUID 不一致・開始不一致・終日/時刻指定の食い違い)
func TestDedupe_NoSuppressionWhenNotSameMeeting(t *testing.T) {
	start := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		real model.NormalizedEvent
	}{
		{
			name: "iCalUID が異なる",
			real: model.NormalizedEvent{ID: "b-ev", ICalUID: "other@example.com",
				StartUTC: start, EndUTC: start.Add(time.Hour), IsBusy: true},
		},
		{
			name: "開始時刻が異なる",
			real: model.NormalizedEvent{ID: "b-ev", ICalUID: "meet-1@example.com",
				StartUTC: start.Add(time.Hour), EndUTC: start.Add(2 * time.Hour), IsBusy: true},
		},
		{
			name: "origin は時刻指定・ターゲットは終日",
			real: model.NormalizedEvent{ID: "b-ev", ICalUID: "meet-1@example.com", IsAllDay: true,
				AllDayStart: "2026-07-10", AllDayEnd: "2026-07-11", IsBusy: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, f := newTestEngine(t)
			ctx := context.Background()
			origin, _ := dupPair(false)
			require.NoError(t, e.Store.UpsertEvent(calBv, tc.real))

			require.NoError(t, e.processEvent(ctx, refA, origin))

			require.Len(t, f.Blockers(calBv), 1) // 抑止されず通常作成
			m, err := e.Store.GetMapping("a", "primary", "a-ev", "b")
			require.NoError(t, err)
			require.NotNil(t, m)
			require.Equal(t, store.StatusActive, m.Status)
		})
	}
}

// dedupe_same_meeting: false なら重複していても抑止しない(設定でオフ可。仕様3章)
func TestDedupe_DisabledByConfig(t *testing.T) {
	e, f := newTestEngine(t)
	e.Cfg.DedupeSameMeeting = false
	ctx := context.Background()
	origin, real := dupPair(false)
	require.NoError(t, e.Store.UpsertEvent(calBv, real))

	require.NoError(t, e.processEvent(ctx, refA, origin))

	require.Len(t, f.Blockers(calBv), 1)
	m, err := e.Store.GetMapping("a", "primary", "a-ev", "b")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, store.StatusActive, m.Status)
}

// 昇格: ターゲット b の実予定の削除通知を processEvent が処理すると
// promoteSuppressed(b, iCalUID) が呼ばれ、suppressed が作成実行され active になる。
// 時刻指定と終日の両方を検証する。
func TestPromoteSuppressed_OnTargetRealEventDeletion(t *testing.T) {
	cases := []struct {
		name   string
		allDay bool
	}{
		{"時刻指定", false},
		{"終日", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, f := newTestEngine(t)
			ctx := context.Background()
			origin, real := dupPair(tc.allDay)
			tag := model.OriginTagOf("a", "a-ev")

			// b の実予定を b 自身の origin として通常処理(キャッシュ登録 + a/c へ配布)
			require.NoError(t, e.processEvent(ctx, calBv, real))
			require.Len(t, f.Blockers(refA), 1)  // b:b-ev のブロッカー
			require.Len(t, f.Blockers(calCv), 1) // b:b-ev のブロッカー

			// a の origin は b では抑止される
			require.NoError(t, e.processEvent(ctx, refA, origin))
			require.Empty(t, f.Blockers(calBv))
			require.Len(t, f.Blockers(calCv), 2) // b:b-ev + a:a-ev

			// b の実予定の削除通知(id のみ。仕様6.1)
			require.NoError(t, e.processEvent(ctx, calBv, model.NormalizedEvent{ID: "b-ev", Deleted: true}))

			// suppressed が昇格し、b にブロッカーが立つ
			blks := f.Blockers(calBv)
			require.Len(t, blks, 1)
			require.Equal(t, tag, blks[0].OriginTag)
			require.Equal(t, model.TimeHash(origin), blks[0].TimeHash)
			m, err := e.Store.GetMapping("a", "primary", "a-ev", "b")
			require.NoError(t, err)
			require.NotNil(t, m)
			require.Equal(t, store.StatusActive, m.Status)
			require.Equal(t, blks[0].EventID, m.BlockerEventID)
			require.Equal(t, model.MSTransactionID(tag, "b"), m.IdempotencyKey)

			// 終日は終日ブロッカーとして昇格される
			if tc.allDay {
				body, ok := f.StoredBlocker(calBv, blks[0].EventID)
				require.True(t, ok)
				require.True(t, body.IsAllDay)
				require.Equal(t, "2026-07-15", body.AllDayStart)
				require.Equal(t, "2026-07-16", body.AllDayEnd)
			}

			// b:b-ev が a/c に配っていたブロッカーは削除通知処理で消えている
			require.Empty(t, f.Blockers(refA))
			require.Len(t, f.Blockers(calCv), 1)
			require.Equal(t, tag, f.Blockers(calCv)[0].OriginTag)
		})
	}
}

// 昇格の再評価: 同一会議の実予定がまだ別に残っている場合は昇格しない
func TestPromoteSuppressed_KeepsSuppressedWhileAnotherDuplicateAlive(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	origin, real := dupPair(false)

	// b のキャッシュに同一会議の実予定が 2 本ある(例: 主催者コピーと招待コピー)
	real2 := real
	real2.ID = "b-ev2"
	require.NoError(t, e.processEvent(ctx, calBv, real))
	require.NoError(t, e.Store.UpsertEvent(calBv, real2)) // 2本目はキャッシュのみ
	require.NoError(t, e.processEvent(ctx, refA, origin))
	require.Empty(t, f.Blockers(calBv)) // 抑止中

	// 1本目の削除通知 → 2本目が残っているので昇格しない
	require.NoError(t, e.processEvent(ctx, calBv, model.NormalizedEvent{ID: "b-ev", Deleted: true}))

	require.Empty(t, f.Blockers(calBv))
	m, err := e.Store.GetMapping("a", "primary", "a-ev", "b")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, store.StatusSuppressed, m.Status)
}
