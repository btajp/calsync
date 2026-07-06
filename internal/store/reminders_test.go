package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
)

func remTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return st
}

var remRef = model.CalendarRef{AccountID: "a", CalendarID: "primary"}

func remEvent(id, ical, title string, start time.Time) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID: id, ICalUID: ical, Title: title,
		MeetingURL:  "https://zoom.us/j/" + id,
		Description: "desc-" + id,
		HTMLLink:    "https://cal.example.com/" + id,
		StartUTC:    start, EndUTC: start.Add(30 * time.Minute), IsBusy: true,
	}
}

// 抽出条件は now < start_utc <= now+lead。終日(is_all_day=1)は対象外(スペック 6 章)。
func TestListUpcomingEvents(t *testing.T) {
	st := remTestStore(t)
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	lead := 10 * time.Minute

	require.NoError(t, st.UpsertEvent(remRef, remEvent("past", "p@x", "過去", now.Add(-time.Minute))))
	require.NoError(t, st.UpsertEvent(remRef, remEvent("at-now", "n@x", "ちょうど今", now)))
	require.NoError(t, st.UpsertEvent(remRef, remEvent("in-window", "w@x", "10分以内", now.Add(5*time.Minute))))
	require.NoError(t, st.UpsertEvent(remRef, remEvent("at-edge", "e@x", "ちょうど境界", now.Add(lead))))
	require.NoError(t, st.UpsertEvent(remRef, remEvent("beyond", "b@x", "遠すぎ", now.Add(lead+time.Second))))
	require.NoError(t, st.UpsertEvent(remRef, model.NormalizedEvent{
		ID: "allday", ICalUID: "a@x", IsAllDay: true,
		AllDayStart: "2026-07-10", AllDayEnd: "2026-07-11", IsBusy: true,
	}))

	got, err := st.ListUpcomingEvents(now, lead)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "in-window", got[0].EventID) // start_utc 昇順
	require.Equal(t, "at-edge", got[1].EventID)
	require.Equal(t, "10分以内", got[0].Title)
	require.Equal(t, "w@x", got[0].ICalUID)
	require.Equal(t, remRef, got[0].Ref)
	require.Equal(t, "https://zoom.us/j/in-window", got[0].MeetingURL)
	require.Equal(t, "desc-in-window", got[0].Description)
	require.Equal(t, "https://cal.example.com/in-window", got[0].HTMLLink)
}

func TestReminderSentLifecycle(t *testing.T) {
	st := remTestStore(t)
	start := time.Date(2026, 7, 10, 9, 5, 0, 0, time.UTC)
	sentAt := time.Date(2026, 7, 10, 8, 55, 0, 0, time.UTC)

	sent, err := st.WasReminderSent(remRef, "ev1", start)
	require.NoError(t, err)
	require.False(t, sent)

	require.NoError(t, st.MarkReminderSent(remRef, "ev1", "u@x", start, sentAt))
	// 冪等(再記録でエラーにならない)
	require.NoError(t, st.MarkReminderSent(remRef, "ev1", "u@x", start, sentAt))

	sent, err = st.WasReminderSent(remRef, "ev1", start)
	require.NoError(t, err)
	require.True(t, sent)

	// iCalUID 照会(dedupe 用)。空 UID は常に false
	dup, err := st.HasReminderForICalUID("u@x", start)
	require.NoError(t, err)
	require.True(t, dup)
	dup, err = st.HasReminderForICalUID("", start)
	require.NoError(t, err)
	require.False(t, dup)

	// 時刻変更で再アーム(start_utc が主キーの一部。スペック 4.3)
	moved := start.Add(time.Hour)
	sent, err = st.WasReminderSent(remRef, "ev1", moved)
	require.NoError(t, err)
	require.False(t, sent)
}

func TestCleanupAndDeleteReminders(t *testing.T) {
	st := remTestStore(t)
	old := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	require.NoError(t, st.MarkReminderSent(remRef, "old", "o@x", old, old))
	require.NoError(t, st.MarkReminderSent(remRef, "recent", "r@x", recent, recent))
	require.NoError(t, st.MarkReminderSent(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, "other", "b@x", recent, recent))

	// start_utc < before の行だけ消える
	require.NoError(t, st.CleanupRemindersSent(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)))
	sent, err := st.WasReminderSent(remRef, "old", old)
	require.NoError(t, err)
	require.False(t, sent)
	sent, err = st.WasReminderSent(remRef, "recent", recent)
	require.NoError(t, err)
	require.True(t, sent)

	// アカウント削除
	require.NoError(t, st.DeleteRemindersForAccount("b"))
	sent, err = st.WasReminderSent(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, "other", recent)
	require.NoError(t, err)
	require.False(t, sent)
}
