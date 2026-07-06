package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
)

// 旧スキーマ(title 列なし)の DB を Open すると冪等 ALTER で title 列が追加される
// (スペック 4.2: 本リポジトリのマイグレーション方針の最初の適用例)。
func TestOpenMigratesEventsTitleColumn(t *testing.T) {
	dir := t.TempDir()

	// 旧スキーマの DB を直接作る(store.Open を経由しない)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "calsync.db"))
	require.NoError(t, err)
	_, err = db.Exec(`
CREATE TABLE events (
  account_id    TEXT NOT NULL,
  calendar_id   TEXT NOT NULL,
  event_id      TEXT NOT NULL,
  ical_uid      TEXT,
  start_utc     INTEGER,
  end_utc       INTEGER,
  is_all_day    INTEGER NOT NULL DEFAULT 0,
  all_day_start TEXT,
  all_day_end   TEXT,
  time_hash     TEXT NOT NULL,
  PRIMARY KEY (account_id, calendar_id, event_id)
);
INSERT INTO events (account_id, calendar_id, event_id, ical_uid, start_utc, end_utc, time_hash)
VALUES ('a', 'primary', 'ev-old', 'old@example.com', 100, 200, 'h');`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	ref := model.CalendarRef{AccountID: "a", CalendarID: "primary"}

	// 1回目の Open: ALTER が走り、旧行は空タイトルで読める
	st, err := Open(dir)
	require.NoError(t, err)
	ev, err := st.GetEvent(ref, "ev-old")
	require.NoError(t, err)
	require.NotNil(t, ev)
	require.Equal(t, "", ev.Title)

	// title 付きで upsert → 復元できる
	require.NoError(t, st.UpsertEvent(ref, model.NormalizedEvent{
		ID:       "ev-new",
		ICalUID:  "new@example.com",
		Title:    "設計レビュー",
		StartUTC: time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
		IsBusy:   true,
	}))
	got, err := st.GetEvent(ref, "ev-new")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "設計レビュー", got.Title)
	require.NoError(t, st.Close())

	// 2回目の Open: duplicate column が無視され、壊れない(冪等性)
	st2, err := Open(dir)
	require.NoError(t, err)
	got2, err := st2.GetEvent(ref, "ev-new")
	require.NoError(t, err)
	require.Equal(t, "設計レビュー", got2.Title)

	// v2: 3 列も冪等 ALTER で追加され、ラウンドトリップする(v2 スペック 3.3)
	require.NoError(t, st2.UpsertEvent(ref, model.NormalizedEvent{
		ID: "ev-v2", ICalUID: "v2@example.com", Title: "件名",
		MeetingURL:  "https://zoom.us/j/123",
		Description: "本文テキスト",
		HTMLLink:    "https://calendar.google.com/event?eid=x",
		StartUTC:    time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC),
		EndUTC:      time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
		IsBusy:      true,
	}))
	got3, err := st2.GetEvent(ref, "ev-v2")
	require.NoError(t, err)
	require.Equal(t, "https://zoom.us/j/123", got3.MeetingURL)
	require.Equal(t, "本文テキスト", got3.Description)
	require.Equal(t, "https://calendar.google.com/event?eid=x", got3.HTMLLink)
	require.NoError(t, st2.Close())
}
