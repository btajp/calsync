package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/work-a-co/calsync/internal/model"
)

func mkMapping(originAcct, originEventID, targetAcct, status, blockerID string) Mapping {
	return Mapping{
		OriginAccount:  originAcct,
		OriginCalendar: "primary",
		OriginEventID:  originEventID,
		TargetAccount:  targetAcct,
		TargetCalendar: "primary",
		BlockerEventID: blockerID,
		IdempotencyKey: "idem-" + originAcct + "-" + originEventID + "-" + targetAcct,
		TimeHash:       "1111222233334444",
		Status:         status,
	}
}

func TestMappings_PutGetDelete(t *testing.T) {
	s := mustOpen(t)

	// 未存在は (nil, nil)
	got, err := s.GetMapping("acct-a", "primary", "ev1", "acct-b")
	require.NoError(t, err)
	require.Nil(t, got)

	m := mkMapping("acct-a", "ev1", "acct-b", StatusPending, "")
	require.NoError(t, s.PutMapping(m))

	got, err = s.GetMapping("acct-a", "primary", "ev1", "acct-b")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, m, *got) // BlockerEventID は "" のまま往復する

	// upsert: pending → active + blocker_event_id 記録(仕様書 6.4 の状態遷移)
	m.BlockerEventID = "blocker-1"
	m.Status = StatusActive
	require.NoError(t, s.PutMapping(m))
	got, err = s.GetMapping("acct-a", "primary", "ev1", "acct-b")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "blocker-1", got.BlockerEventID)
	require.Equal(t, StatusActive, got.Status)

	require.NoError(t, s.DeleteMapping("acct-a", "primary", "ev1", "acct-b"))
	got, err = s.GetMapping("acct-a", "primary", "ev1", "acct-b")
	require.NoError(t, err)
	require.Nil(t, got)

	// 存在しない行の削除はエラーにならない(冪等)
	require.NoError(t, s.DeleteMapping("acct-a", "primary", "ev1", "acct-b"))
}

func TestMappings_ListQueries(t *testing.T) {
	s := mustOpen(t)
	m1 := mkMapping("acct-a", "ev1", "acct-b", StatusActive, "blk-ab-1")
	m2 := mkMapping("acct-a", "ev1", "acct-c", StatusActive, "blk-ac-1")
	m3 := mkMapping("acct-a", "ev2", "acct-b", StatusPending, "")
	m4 := mkMapping("acct-b", "ev9", "acct-a", StatusActive, "blk-ba-9")
	for _, m := range []Mapping{m1, m2, m3, m4} {
		require.NoError(t, s.PutMapping(m))
	}

	cases := []struct {
		name string
		list func() ([]Mapping, error)
		want []Mapping
	}{
		{
			name: "ListMappingsForOrigin は 1 origin イベントの全ターゲット行を返す",
			list: func() ([]Mapping, error) { return s.ListMappingsForOrigin("acct-a", "primary", "ev1") },
			want: []Mapping{m1, m2}, // target_account 昇順
		},
		{
			name: "ListMappingsWhereOriginAccount は origin がそのアカウントの全行を返す",
			list: func() ([]Mapping, error) { return s.ListMappingsWhereOriginAccount("acct-a") },
			want: []Mapping{m1, m2, m3}, // origin_calendar, origin_event_id, target_account 昇順
		},
		{
			name: "ListMappingsWhereTargetAccount は target がそのアカウントの全行を返す",
			list: func() ([]Mapping, error) { return s.ListMappingsWhereTargetAccount("acct-b") },
			want: []Mapping{m1, m3}, // origin_account, origin_calendar, origin_event_id 昇順
		},
		{
			name: "ListPendingMappings は pending 行のみ返す",
			list: func() ([]Mapping, error) { return s.ListPendingMappings() },
			want: []Mapping{m3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.list()
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestIsBlocker(t *testing.T) {
	s := mustOpen(t)
	require.NoError(t, s.PutMapping(mkMapping("acct-a", "ev1", "acct-b", StatusActive, "blk-1")))
	require.NoError(t, s.PutMapping(mkMapping("acct-a", "ev2", "acct-b", StatusPending, "")))

	cases := []struct {
		name       string
		targetAcct string
		eventID    string
		want       bool
	}{
		{"登録済みブロッカー ID はヒット", "acct-b", "blk-1", true},
		{"別アカウントの検索ではヒットしない", "acct-c", "blk-1", false},
		{"未知の ID はヒットしない", "acct-b", "no-such", false},
		{"空文字は blocker_event_id が空(NULL)の行にヒットしない", "acct-b", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.IsBlocker(tc.targetAcct, tc.eventID)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestListSuppressedByOriginICalUID(t *testing.T) {
	s := mustOpen(t)
	origin := model.CalendarRef{AccountID: "acct-a", CalendarID: "primary"}
	start := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

	// origin 側イベントキャッシュ(ev-s2 は意図的にキャッシュしない)
	require.NoError(t, s.UpsertEvent(origin, timedEvent("ev-s1", "meet-1", start, time.Hour)))
	require.NoError(t, s.UpsertEvent(origin, timedEvent("ev-s3", "other-uid", start, time.Hour)))
	require.NoError(t, s.UpsertEvent(origin, timedEvent("ev-s4", "meet-1", start, time.Hour)))

	suppressed := mkMapping("acct-a", "ev-s1", "acct-b", StatusSuppressed, "")
	require.NoError(t, s.PutMapping(suppressed))
	// origin キャッシュに無い suppressed → 返らない
	require.NoError(t, s.PutMapping(mkMapping("acct-a", "ev-s2", "acct-b", StatusSuppressed, "")))
	// iCalUID 不一致の suppressed → 返らない
	require.NoError(t, s.PutMapping(mkMapping("acct-a", "ev-s3", "acct-b", StatusSuppressed, "")))
	// 別ターゲットの suppressed → 返らない
	require.NoError(t, s.PutMapping(mkMapping("acct-a", "ev-s1", "acct-c", StatusSuppressed, "")))
	// iCalUID は一致するが active → 返らない
	require.NoError(t, s.PutMapping(mkMapping("acct-a", "ev-s4", "acct-b", StatusActive, "blk-4")))

	got, err := s.ListSuppressedByOriginICalUID("acct-b", "meet-1")
	require.NoError(t, err)
	require.Equal(t, []Mapping{suppressed}, got)

	// 該当なしは空
	got, err = s.ListSuppressedByOriginICalUID("acct-b", "no-such-uid")
	require.NoError(t, err)
	require.Empty(t, got)
}
