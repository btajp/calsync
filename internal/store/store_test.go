package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/work-a-co/calsync/internal/model"
)

// mustOpen は t.TempDir にストアを開き、テスト終了時に閉じる。
// Task 4 / Task 5 のテストからも再利用する共有ヘルパ。
func mustOpen(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_CreatesSchemaAndWAL(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// DB ファイルが作成されている
	_, statErr := os.Stat(filepath.Join(dir, "calsync.db"))
	require.NoError(t, statErr)

	// WAL モードが有効
	var mode string
	require.NoError(t, s.db.QueryRow("PRAGMA journal_mode").Scan(&mode))
	require.Equal(t, "wal", mode)

	// スキーマ: 3 テーブルが存在する
	for _, tbl := range []string{"calendars", "events", "mappings"} {
		var name string
		err := s.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name)
		require.NoError(t, err, "table %s must exist", tbl)
	}
}

func TestOpen_SecondOpenReturnsErrLocked(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	require.NoError(t, err)

	// 同一ディレクトリの二重 Open は flock で弾かれる
	s2, err := Open(dir)
	require.Nil(t, s2)
	require.ErrorIs(t, err, ErrLocked)

	// Close で解放すれば再度開ける
	require.NoError(t, s1.Close())
	s3, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, s3.Close())
}

func TestOpenReadOnly_CoexistsWithRunningDaemon(t *testing.T) {
	dir := t.TempDir()
	writer, err := Open(dir) // デーモン相当(flock 保持)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	ref := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	require.NoError(t, writer.UpsertCalendar(ref))

	// flock 保持中でも読み取り専用オープンは成功し、読める
	ro, err := OpenReadOnly(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	states, err := ro.ListCalendars()
	require.NoError(t, err)
	require.Len(t, states, 1)

	// 書き込みは拒否される(mode=ro)
	require.Error(t, ro.UpsertCalendar(model.CalendarRef{AccountID: "b", CalendarID: "primary"}))
}

func TestOpenReadOnly_FailsWhenDBMissing(t *testing.T) {
	_, err := OpenReadOnly(t.TempDir())
	require.Error(t, err)
}

func TestCalendars_UpsertAndGet(t *testing.T) {
	s := mustOpen(t)
	ref := model.CalendarRef{AccountID: "acct-a", CalendarID: "primary"}

	// 未存在は (nil, nil)
	got, err := s.GetCalendar(ref)
	require.NoError(t, err)
	require.Nil(t, got)

	// Upsert は冪等(2 回呼んでもエラーにならず 1 行のまま)
	require.NoError(t, s.UpsertCalendar(ref))
	require.NoError(t, s.UpsertCalendar(ref))

	got, err = s.GetCalendar(ref)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, ref, got.Ref)
	require.Empty(t, got.Cursor)
	require.Empty(t, got.Timezone)
	require.Empty(t, got.LastError)
	require.True(t, got.Window.Start.IsZero())
	require.True(t, got.Window.End.IsZero())
	require.True(t, got.LastSyncedAt.IsZero())

	list, err := s.ListCalendars()
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestCalendars_Mutations(t *testing.T) {
	window := model.Window{
		Start: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
	}
	syncedAt := time.Date(2026, 7, 3, 12, 34, 56, 0, time.UTC)

	cases := []struct {
		name   string
		mutate func(s *Store, ref model.CalendarRef) error
		check  func(t *testing.T, st *CalendarState)
	}{
		{
			name: "SetCursor はカーソルとウィンドウを保存する",
			mutate: func(s *Store, ref model.CalendarRef) error {
				return s.SetCursor(ref, "sync-token-1", window)
			},
			check: func(t *testing.T, st *CalendarState) {
				require.Equal(t, "sync-token-1", st.Cursor)
				require.True(t, st.Window.Start.Equal(window.Start))
				require.True(t, st.Window.End.Equal(window.End))
			},
		},
		{
			name: "ClearCursor はカーソルとウィンドウを消す",
			mutate: func(s *Store, ref model.CalendarRef) error {
				if err := s.SetCursor(ref, "sync-token-1", window); err != nil {
					return err
				}
				return s.ClearCursor(ref)
			},
			check: func(t *testing.T, st *CalendarState) {
				require.Empty(t, st.Cursor)
				require.True(t, st.Window.Start.IsZero())
				require.True(t, st.Window.End.IsZero())
			},
		},
		{
			name: "SetCalendarTimezone はタイムゾーンを保存する",
			mutate: func(s *Store, ref model.CalendarRef) error {
				return s.SetCalendarTimezone(ref, "Asia/Tokyo")
			},
			check: func(t *testing.T, st *CalendarState) {
				require.Equal(t, "Asia/Tokyo", st.Timezone)
			},
		},
		{
			name: "SetCalendarError はメッセージを記録し last_synced_at も更新する",
			mutate: func(s *Store, ref model.CalendarRef) error {
				return s.SetCalendarError(ref, "boom")
			},
			check: func(t *testing.T, st *CalendarState) {
				require.Equal(t, "boom", st.LastError)
				require.False(t, st.LastSyncedAt.IsZero())
			},
		},
		{
			name: "SetCalendarError の空文字はエラーをクリアする",
			mutate: func(s *Store, ref model.CalendarRef) error {
				if err := s.SetCalendarError(ref, "boom"); err != nil {
					return err
				}
				return s.SetCalendarError(ref, "")
			},
			check: func(t *testing.T, st *CalendarState) {
				require.Empty(t, st.LastError)
				require.False(t, st.LastSyncedAt.IsZero())
			},
		},
		{
			name: "TouchSynced は指定時刻を last_synced_at に保存する",
			mutate: func(s *Store, ref model.CalendarRef) error {
				return s.TouchSynced(ref, syncedAt)
			},
			check: func(t *testing.T, st *CalendarState) {
				require.True(t, st.LastSyncedAt.Equal(syncedAt))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustOpen(t)
			ref := model.CalendarRef{AccountID: "acct-a", CalendarID: "primary"}
			require.NoError(t, s.UpsertCalendar(ref))
			require.NoError(t, tc.mutate(s, ref))
			st, err := s.GetCalendar(ref)
			require.NoError(t, err)
			require.NotNil(t, st)
			tc.check(t, st)
		})
	}
}

func TestCalendars_ListAndDeleteForAccount(t *testing.T) {
	s := mustOpen(t)
	refs := []model.CalendarRef{
		{AccountID: "acct-a", CalendarID: "primary"},
		{AccountID: "acct-a", CalendarID: "team"},
		{AccountID: "acct-b", CalendarID: "primary"},
	}
	for _, ref := range refs {
		require.NoError(t, s.UpsertCalendar(ref))
	}

	list, err := s.ListCalendars()
	require.NoError(t, err)
	require.Len(t, list, 3)
	gotRefs := make([]model.CalendarRef, 0, len(list))
	for _, st := range list {
		gotRefs = append(gotRefs, st.Ref)
	}
	require.Equal(t, refs, gotRefs) // account_id, calendar_id 昇順

	require.NoError(t, s.DeleteCalendarsForAccount("acct-a"))
	list, err = s.ListCalendars()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, refs[2], list[0].Ref)
}
