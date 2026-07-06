package fake_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
	"github.com/btajp/calsync/internal/provider/fake"
)

var (
	calA = model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	calB = model.CalendarRef{AccountID: "b", CalendarID: "primary"}
)

func ev(id string, start time.Time) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID:       id,
		ICalUID:  id + "@test",
		StartUTC: start,
		EndUTC:   start.Add(time.Hour),
		IsBusy:   true,
	}
}

func blocker(tag string, start time.Time) model.Blocker {
	return model.Blocker{
		Title:          "予定あり",
		StartUTC:       start,
		EndUTC:         start.Add(time.Hour),
		TargetTimezone: "UTC",
		OriginTag:      tag,
	}
}

func TestChanges_FullThenIncrementalCursorSequence(t *testing.T) {
	f := fake.New()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	w := model.Window{Start: base.AddDate(0, 0, -7), End: base.AddDate(0, 3, 0)}

	f.SetFullState(calA, []model.NormalizedEvent{ev("e1", base), ev("e2", base.Add(2*time.Hour))})
	f.QueueChanges(calA, []model.NormalizedEvent{ev("e3", base.Add(4*time.Hour))})
	f.QueueChanges(calA, nil)

	// cursor=="" → フル同期(SetFullState の全量)
	evs, cur, err := f.Changes(ctx, calA, "", w)
	require.NoError(t, err)
	require.Len(t, evs, 2)
	require.Equal(t, "e1", evs[0].ID)
	require.Equal(t, "e2", evs[1].ID)
	require.Equal(t, "c1", cur)

	// 増分1回目: キュー先頭を消費
	evs, cur, err = f.Changes(ctx, calA, cur, w)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "e3", evs[0].ID)
	require.Equal(t, "c2", cur)

	// 増分2回目: 空のキュー要素も1要素として消費される
	evs, cur, err = f.Changes(ctx, calA, cur, w)
	require.NoError(t, err)
	require.Empty(t, evs)
	require.Equal(t, "c3", cur)

	// キュー枯渇後は空イベント + 新カーソル
	evs, cur, err = f.Changes(ctx, calA, cur, w)
	require.NoError(t, err)
	require.Empty(t, evs)
	require.Equal(t, "c4", cur)
}

func TestChanges_FailNext(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"cursor invalid", provider.ErrCursorInvalid},
		{"auth expired", provider.ErrAuthExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fake.New()
			ctx := context.Background()
			f.FailNext(calA, tc.err)

			_, cur, err := f.Changes(ctx, calA, "", model.Window{})
			require.ErrorIs(t, err, tc.err)
			require.Empty(t, cur) // 失敗時は newCursor を返さない(完走時のみ非空の契約)

			// エラーは1回で消費され、次の呼び出しは成功する
			_, cur, err = f.Changes(ctx, calA, "", model.Window{})
			require.NoError(t, err)
			require.Equal(t, "c1", cur)
		})
	}
}

func TestChanges_CalendarsAreIsolated(t *testing.T) {
	f := fake.New()
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)

	f.SetFullState(calA, []model.NormalizedEvent{ev("e1", base)})

	// calB には何も設定していない → 空 + calB 自身の連番カーソル
	evs, cur, err := f.Changes(ctx, calB, "", model.Window{})
	require.NoError(t, err)
	require.Empty(t, evs)
	require.Equal(t, "c1", cur)

	// calA は独立に自分の全量を返す
	evs, cur, err = f.Changes(ctx, calA, "", model.Window{})
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "c1", cur)
}

func TestCreateBlocker_IdempotentOnSameKey(t *testing.T) {
	f := fake.New()
	ctx := context.Background()
	start := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)

	id1, err := f.CreateBlocker(ctx, calB, blocker("a:e1", start), "key-1")
	require.NoError(t, err)
	require.NotEmpty(t, id1)

	// 同一 idemKey の再作成 → 既存 ID を返し、二重作成しない(実プロバイダと同じ契約)
	id2, err := f.CreateBlocker(ctx, calB, blocker("a:e1", start), "key-1")
	require.NoError(t, err)
	require.Equal(t, id1, id2)
	require.Len(t, f.Blockers(calB), 1)

	// 別キーは別ブロッカー
	id3, err := f.CreateBlocker(ctx, calB, blocker("a:e2", start), "key-2")
	require.NoError(t, err)
	require.NotEqual(t, id1, id3)
	require.Len(t, f.Blockers(calB), 2)
}

func TestCreateBlocker_RecordsTagTimeHashAndBody(t *testing.T) {
	f := fake.New()
	ctx := context.Background()
	start := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	b := blocker("a:e1", start)

	id, err := f.CreateBlocker(ctx, calB, b, "key-1")
	require.NoError(t, err)

	recs := f.Blockers(calB)
	require.Len(t, recs, 1)
	require.Equal(t, id, recs[0].EventID)
	require.Equal(t, "a:e1", recs[0].OriginTag)
	wantHash := model.TimeHash(model.NormalizedEvent{StartUTC: b.StartUTC, EndUTC: b.EndUTC})
	require.Equal(t, wantHash, recs[0].TimeHash)

	body, ok := f.StoredBlocker(calB, id)
	require.True(t, ok)
	require.Equal(t, "予定あり", body.Title)
	require.Equal(t, "UTC", body.TargetTimezone)
	require.Equal(t, "a:e1", body.OriginTag)

	_, ok = f.StoredBlocker(calB, "no-such-id")
	require.False(t, ok)
}

func TestUpdateBlocker(t *testing.T) {
	f := fake.New()
	ctx := context.Background()
	start := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	id, err := f.CreateBlocker(ctx, calB, blocker("a:e1", start), "key-1")
	require.NoError(t, err)

	updated := blocker("a:e1", start.Add(30*time.Minute))
	require.NoError(t, f.UpdateBlocker(ctx, calB, id, updated))

	recs := f.Blockers(calB)
	require.Len(t, recs, 1)
	wantHash := model.TimeHash(model.NormalizedEvent{StartUTC: updated.StartUTC, EndUTC: updated.EndUTC})
	require.Equal(t, wantHash, recs[0].TimeHash)
	body, ok := f.StoredBlocker(calB, id)
	require.True(t, ok)
	require.Equal(t, updated.StartUTC, body.StartUTC)

	// 未存在 ID は ErrNotFound
	err = f.UpdateBlocker(ctx, calB, "no-such-id", updated)
	require.ErrorIs(t, err, provider.ErrNotFound)
}

func TestDeleteBlocker(t *testing.T) {
	f := fake.New()
	ctx := context.Background()
	start := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	id, err := f.CreateBlocker(ctx, calB, blocker("a:e1", start), "key-1")
	require.NoError(t, err)

	require.NoError(t, f.DeleteBlocker(ctx, calB, id))
	require.Empty(t, f.Blockers(calB))

	// 未存在(404 相当)でも nil(コントラクト: 404 は成功扱い)
	require.NoError(t, f.DeleteBlocker(ctx, calB, id))
	require.NoError(t, f.DeleteBlocker(ctx, calB, "never-existed"))
}

func TestDeleteBlocker_ReleasesIdemKey(t *testing.T) {
	f := fake.New()
	ctx := context.Background()
	start := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)

	// Create first blocker with idemKey "k1"
	id1, err := f.CreateBlocker(ctx, calB, blocker("a:e1", start), "k1")
	require.NoError(t, err)
	require.NotEmpty(t, id1)

	// Delete the blocker
	require.NoError(t, f.DeleteBlocker(ctx, calB, id1))
	require.Empty(t, f.Blockers(calB))

	// Create a new blocker with the same idemKey but different content
	id2, err := f.CreateBlocker(ctx, calB, blocker("a:e2", start.Add(time.Hour)), "k1")
	require.NoError(t, err)

	// Should get a new ID (not the old deleted one)
	require.NotEqual(t, id1, id2)

	// Should have exactly one blocker with the new content
	recs := f.Blockers(calB)
	require.Len(t, recs, 1)
	require.Equal(t, id2, recs[0].EventID)
	require.Equal(t, "a:e2", recs[0].OriginTag)

	// StoredBlocker should return the new content
	body, ok := f.StoredBlocker(calB, id2)
	require.True(t, ok)
	require.Equal(t, "a:e2", body.OriginTag)
	require.Equal(t, start.Add(time.Hour), body.StartUTC)
}

func TestSeedBlockerAndListBlockers(t *testing.T) {
	f := fake.New()
	ctx := context.Background()
	seed := model.BlockerRecord{EventID: "orphan-1", OriginTag: "x:gone", TimeHash: "h1"}
	f.SeedBlocker(calB, seed)

	require.Equal(t, []model.BlockerRecord{seed}, f.Blockers(calB))
	listed, err := f.ListBlockers(ctx, calB, model.Window{})
	require.NoError(t, err)
	require.Equal(t, []model.BlockerRecord{seed}, listed)

	// SeedBlocker は EventID で upsert(2度目は上書き)。
	// Task 8 の「プロバイダ呼び出しなし」検証(改竄トリック)がこの性質に依存する
	f.SeedBlocker(calB, model.BlockerRecord{EventID: "orphan-1", OriginTag: "x:gone", TimeHash: "h2"})
	recs := f.Blockers(calB)
	require.Len(t, recs, 1)
	require.Equal(t, "h2", recs[0].TimeHash)
}

func TestBlockers_SortedByEventID(t *testing.T) {
	f := fake.New()
	ctx := context.Background()
	start := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)

	f.SeedBlocker(calB, model.BlockerRecord{EventID: "zzz", OriginTag: "x:1", TimeHash: "h"})
	_, err := f.CreateBlocker(ctx, calB, blocker("a:e1", start), "key-1")
	require.NoError(t, err)
	f.SeedBlocker(calB, model.BlockerRecord{EventID: "aaa", OriginTag: "x:2", TimeHash: "h"})

	recs := f.Blockers(calB)
	require.Len(t, recs, 3)
	require.True(t, sort.SliceIsSorted(recs, func(i, j int) bool {
		return recs[i].EventID < recs[j].EventID
	}))
}

func TestGetCalendarTimezone(t *testing.T) {
	f := fake.New()
	ctx := context.Background()

	tz, err := f.GetCalendarTimezone(ctx, calB)
	require.NoError(t, err)
	require.Equal(t, "UTC", tz) // 既定値

	f.SetTimezone(calB, "Asia/Tokyo")
	tz, err = f.GetCalendarTimezone(ctx, calB)
	require.NoError(t, err)
	require.Equal(t, "Asia/Tokyo", tz)
}

// fake は実プロバイダと同じ契約で Title を保持・返却する(スペック 4.1)。
func TestChangesPreservesTitle(t *testing.T) {
	f := fake.New()
	cal := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	f.SetFullState(cal, []model.NormalizedEvent{{ID: "ev1", Title: "設計レビュー", IsBusy: true}})
	evs, _, err := f.Changes(context.Background(), cal, "", model.Window{})
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "設計レビュー", evs[0].Title)
}

// fake は v2 の表示フィールドも素通しする(実プロバイダと同じ契約)。
func TestChangesPreservesDisplayFields(t *testing.T) {
	f := fake.New()
	cal := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	f.SetFullState(cal, []model.NormalizedEvent{{
		ID: "ev1", MeetingURL: "https://zoom.us/j/1", Description: "d", HTMLLink: "https://cal/x", IsBusy: true,
	}})
	evs, _, err := f.Changes(context.Background(), cal, "", model.Window{})
	require.NoError(t, err)
	require.Equal(t, "https://zoom.us/j/1", evs[0].MeetingURL)
	require.Equal(t, "d", evs[0].Description)
	require.Equal(t, "https://cal/x", evs[0].HTMLLink)
}

func TestFakeImplementsProvider(t *testing.T) {
	var p provider.Provider = fake.New()
	require.NotNil(t, p)
}
