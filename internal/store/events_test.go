package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
)

// timedEvent は時刻指定の busy イベントを作るヘルパ(Task 5 のテストも使用)。
func timedEvent(id, icalUID string, start time.Time, d time.Duration) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID:       id,
		ICalUID:  icalUID,
		StartUTC: start,
		EndUTC:   start.Add(d),
		IsBusy:   true,
	}
}

// allDayEvent は終日 busy イベントを作るヘルパ(end は排他的終了日)。
func allDayEvent(id, icalUID, start, end string) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID:          id,
		ICalUID:     icalUID,
		IsAllDay:    true,
		AllDayStart: start,
		AllDayEnd:   end,
		IsBusy:      true,
	}
}

func TestEvents_UpsertGetDelete(t *testing.T) {
	s := mustOpen(t)
	ref := model.CalendarRef{AccountID: "acct-a", CalendarID: "primary"}
	start := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	ev := timedEvent("ev1", "uid-1@example.com", start, time.Hour)

	// 未存在は (nil, nil)
	got, err := s.GetEvent(ref, "ev1")
	require.NoError(t, err)
	require.Nil(t, got)

	require.NoError(t, s.UpsertEvent(ref, ev))
	got, err = s.GetEvent(ref, "ev1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "ev1", got.ID)
	require.Equal(t, "uid-1@example.com", got.ICalUID)
	require.True(t, got.StartUTC.Equal(start))
	require.True(t, got.EndUTC.Equal(start.Add(time.Hour)))
	require.False(t, got.IsAllDay)
	require.True(t, got.IsBusy) // キャッシュには busy イベントのみ入る契約

	// upsert: 同一キーで時刻を更新できる
	moved := timedEvent("ev1", "uid-1@example.com", start.Add(2*time.Hour), time.Hour)
	require.NoError(t, s.UpsertEvent(ref, moved))
	got, err = s.GetEvent(ref, "ev1")
	require.NoError(t, err)
	require.True(t, got.StartUTC.Equal(start.Add(2*time.Hour)))

	ids, err := s.ListEventIDs(ref)
	require.NoError(t, err)
	require.Equal(t, []string{"ev1"}, ids)

	require.NoError(t, s.DeleteEvent(ref, "ev1"))
	got, err = s.GetEvent(ref, "ev1")
	require.NoError(t, err)
	require.Nil(t, got)

	// 存在しない ID の削除はエラーにならない(冪等)
	require.NoError(t, s.DeleteEvent(ref, "ev1"))
}

func TestEvents_AllDayRoundTrip(t *testing.T) {
	s := mustOpen(t)
	ref := model.CalendarRef{AccountID: "acct-a", CalendarID: "primary"}
	ev := allDayEvent("ev-allday", "uid-ad@example.com", "2026-07-20", "2026-07-21")

	require.NoError(t, s.UpsertEvent(ref, ev))
	got, err := s.GetEvent(ref, "ev-allday")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.IsAllDay)
	require.Equal(t, "2026-07-20", got.AllDayStart)
	require.Equal(t, "2026-07-21", got.AllDayEnd)
}

func TestEvents_ListIDsAndDeleteForAccount(t *testing.T) {
	s := mustOpen(t)
	refA1 := model.CalendarRef{AccountID: "acct-a", CalendarID: "primary"}
	refA2 := model.CalendarRef{AccountID: "acct-a", CalendarID: "team"}
	refB := model.CalendarRef{AccountID: "acct-b", CalendarID: "primary"}
	start := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

	require.NoError(t, s.UpsertEvent(refA1, timedEvent("a1-ev2", "u1", start, time.Hour)))
	require.NoError(t, s.UpsertEvent(refA1, timedEvent("a1-ev1", "u2", start, time.Hour)))
	require.NoError(t, s.UpsertEvent(refA2, timedEvent("a2-ev1", "u3", start, time.Hour)))
	require.NoError(t, s.UpsertEvent(refB, timedEvent("b-ev1", "u4", start, time.Hour)))

	// event_id 昇順、他カレンダー・他アカウントは混ざらない
	ids, err := s.ListEventIDs(refA1)
	require.NoError(t, err)
	require.Equal(t, []string{"a1-ev1", "a1-ev2"}, ids)

	ids, err = s.ListEventIDs(refB)
	require.NoError(t, err)
	require.Equal(t, []string{"b-ev1"}, ids)

	// アカウント単位削除は全カレンダーに及び、他アカウントに触れない
	require.NoError(t, s.DeleteEventsForAccount("acct-a"))
	ids, err = s.ListEventIDs(refA1)
	require.NoError(t, err)
	require.Empty(t, ids)
	ids, err = s.ListEventIDs(refA2)
	require.NoError(t, err)
	require.Empty(t, ids)
	ids, err = s.ListEventIDs(refB)
	require.NoError(t, err)
	require.Equal(t, []string{"b-ev1"}, ids)
}

func TestHasBusyEventByICalUID(t *testing.T) {
	s := mustOpen(t)
	refB := model.CalendarRef{AccountID: "acct-b", CalendarID: "primary"}
	refC := model.CalendarRef{AccountID: "acct-c", CalendarID: "primary"}
	start := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

	require.NoError(t, s.UpsertEvent(refB, timedEvent("b-timed", "meet-1", start, time.Hour)))
	require.NoError(t, s.UpsertEvent(refB, allDayEvent("b-allday", "ad-1", "2026-07-20", "2026-07-21")))
	require.NoError(t, s.UpsertEvent(refC, timedEvent("c-timed", "meet-2", start, time.Hour)))

	cases := []struct {
		name        string
		accountID   string
		icalUID     string
		startUTC    time.Time
		allDayStart string
		want        bool
	}{
		{"時刻指定: ical_uid+start_utc 一致でヒット", "acct-b", "meet-1", start, "", true},
		{"時刻指定: start_utc 不一致はヒットしない", "acct-b", "meet-1", start.Add(time.Hour), "", false},
		{"時刻指定: 未知の ical_uid はヒットしない", "acct-b", "no-such-uid", start, "", false},
		{"時刻指定: 他アカウントのイベントはヒットしない", "acct-b", "meet-2", start, "", false},
		{"終日: ical_uid+all_day_start 一致でヒット", "acct-b", "ad-1", time.Time{}, "2026-07-20", true},
		{"終日: 日付不一致はヒットしない", "acct-b", "ad-1", time.Time{}, "2026-07-21", false},
		{"終日: allDayStart 指定時は時刻指定イベントにヒットしない", "acct-b", "meet-1", start, "2026-07-10", false},
		{"空の ical_uid は常に false", "acct-b", "", start, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.HasBusyEventByICalUID(tc.accountID, tc.icalUID, tc.startUTC, tc.allDayStart)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
