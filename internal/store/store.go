// Package store は calsync のステートストア(SQLite 1 ファイル + WAL + flock)を提供する。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite" // driver name "sqlite" を登録

	"github.com/btajp/calsync/internal/model"
)

// ErrLocked は別の calsync プロセスがデータディレクトリを掴んでいる場合に Open が返す。
var ErrLocked = errors.New("data directory is locked by another calsync process")

// Store は SQLite に裏付けられたステートストア。単一プロセス前提(flock で保証)。
type Store struct {
	db   *sql.DB
	lock *flock.Flock
}

// スキーマは仕様書 7 章 + events への all_day_start/all_day_end TEXT 追加(計画側補正)。
const schema = `
CREATE TABLE IF NOT EXISTS calendars (
  account_id     TEXT NOT NULL,
  calendar_id    TEXT NOT NULL,
  cursor         TEXT,
  window_start   INTEGER,
  window_end     INTEGER,
  timezone       TEXT,
  last_synced_at INTEGER,
  last_error     TEXT,
  PRIMARY KEY (account_id, calendar_id)
);

CREATE TABLE IF NOT EXISTS events (
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
  title         TEXT NOT NULL DEFAULT '',
  meeting_url   TEXT NOT NULL DEFAULT '',
  description   TEXT NOT NULL DEFAULT '',
  html_link     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (account_id, calendar_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_events_icaluid ON events (ical_uid, start_utc);

CREATE TABLE IF NOT EXISTS mappings (
  origin_account   TEXT NOT NULL,
  origin_calendar  TEXT NOT NULL,
  origin_event_id  TEXT NOT NULL,
  target_account   TEXT NOT NULL,
  target_calendar  TEXT NOT NULL,
  blocker_event_id TEXT,
  idempotency_key  TEXT NOT NULL,
  time_hash        TEXT NOT NULL,
  status           TEXT NOT NULL,
  PRIMARY KEY (origin_account, origin_calendar, origin_event_id, target_account)
);
CREATE INDEX IF NOT EXISTS idx_mappings_blocker ON mappings (target_account, blocker_event_id);

CREATE TABLE IF NOT EXISTS reminders_sent (
  account_id  TEXT NOT NULL,
  calendar_id TEXT NOT NULL,
  event_id    TEXT NOT NULL,
  start_utc   INTEGER NOT NULL,
  ical_uid    TEXT NOT NULL DEFAULT '',
  sent_at     INTEGER NOT NULL,
  PRIMARY KEY (account_id, calendar_id, event_id, start_utc)
);
CREATE INDEX IF NOT EXISTS idx_reminders_sent_icaluid ON reminders_sent (ical_uid, start_utc);
`

// migrate は既存 DB への後方互換の列追加を行う。方針(スペック 4.2):
// 新規 DB は const schema、既存 DB は Open 時の冪等 ALTER(duplicate column のみ無視)。
// schema version 管理は導入しない。
func migrate(db *sql.DB) error {
	alters := []string{
		`ALTER TABLE events ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN meeting_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN description TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN html_link TEXT NOT NULL DEFAULT ''`,
	}
	for _, q := range alters {
		if _, err := db.Exec(q); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("events schema migration: %w", err)
			}
		}
	}
	return nil
}

// Open はデータディレクトリの flock を取得し(失敗は ErrLocked)、
// WAL モードで SQLite を開いてスキーマを migrate する。
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	lock := flock.New(filepath.Join(dataDir, "calsync.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	if !locked {
		return nil, ErrLocked
	}
	// _pragma はコネクションごとに適用される(database/sql のプール対策)。
	dsn := "file:" + filepath.Join(dataDir, "calsync.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = lock.Unlock()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite は単一ライター。プール多重化による SQLITE_BUSY を避ける。
	db.SetMaxOpenConns(1)

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		_ = db.Close()
		_ = lock.Unlock()
		return nil, fmt.Errorf("check journal mode: %w", err)
	}
	if mode != "wal" {
		_ = db.Close()
		_ = lock.Unlock()
		return nil, fmt.Errorf("journal_mode is %q, want wal", mode)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		_ = lock.Unlock()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		_ = lock.Unlock()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db, lock: lock}, nil
}

// OpenReadOnly は flock を取らずに読み取り専用で SQLite を開く。
// status / doctor 用: WAL は並行リーダーを許すため、稼働中のデーモンと共存できる。
func OpenReadOnly(dataDir string) (*Store, error) {
	dsn := "file:" + filepath.Join(dataDir, "calsync.db") +
		"?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	// sql.Open は遅延接続のため、DB ファイル欠如をここで検知する。
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	return &Store{db: db}, nil
}

// Close は DB を閉じ、flock を保持していれば解放する(読み取り専用時は lock が nil)。
func (s *Store) Close() error {
	dbErr := s.db.Close()
	var unlockErr error
	if s.lock != nil {
		unlockErr = s.lock.Unlock()
	}
	if dbErr != nil {
		return dbErr
	}
	return unlockErr
}

// CalendarState は calendars テーブルの 1 行。
type CalendarState struct {
	Ref          model.CalendarRef
	Cursor       string
	Window       model.Window
	Timezone     string
	LastSyncedAt time.Time
	LastError    string
}

// UpsertCalendar は行が無ければ作る(あれば何もしない)。冪等。
func (s *Store) UpsertCalendar(ref model.CalendarRef) error {
	_, err := s.db.Exec(`
INSERT INTO calendars (account_id, calendar_id) VALUES (?, ?)
ON CONFLICT (account_id, calendar_id) DO NOTHING`,
		ref.AccountID, ref.CalendarID)
	return err
}

// GetCalendar は 1 行を返す。未存在は (nil, nil)。
func (s *Store) GetCalendar(ref model.CalendarRef) (*CalendarState, error) {
	row := s.db.QueryRow(`
SELECT cursor, window_start, window_end, timezone, last_synced_at, last_error
FROM calendars WHERE account_id = ? AND calendar_id = ?`,
		ref.AccountID, ref.CalendarID)
	var (
		cursor, tz, lastError    sql.NullString
		wStart, wEnd, lastSynced sql.NullInt64
	)
	err := row.Scan(&cursor, &wStart, &wEnd, &tz, &lastSynced, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st := &CalendarState{
		Ref:       ref,
		Cursor:    cursor.String,
		Timezone:  tz.String,
		LastError: lastError.String,
	}
	if wStart.Valid {
		st.Window.Start = time.Unix(wStart.Int64, 0).UTC()
	}
	if wEnd.Valid {
		st.Window.End = time.Unix(wEnd.Int64, 0).UTC()
	}
	if lastSynced.Valid {
		st.LastSyncedAt = time.Unix(lastSynced.Int64, 0).UTC()
	}
	return st, nil
}

// ListCalendars は全行を (account_id, calendar_id) 昇順で返す。
func (s *Store) ListCalendars() ([]CalendarState, error) {
	rows, err := s.db.Query(`
SELECT account_id, calendar_id, cursor, window_start, window_end, timezone, last_synced_at, last_error
FROM calendars ORDER BY account_id, calendar_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CalendarState
	for rows.Next() {
		var (
			acct, cal                string
			cursor, tz, lastError    sql.NullString
			wStart, wEnd, lastSynced sql.NullInt64
		)
		if err := rows.Scan(&acct, &cal, &cursor, &wStart, &wEnd, &tz, &lastSynced, &lastError); err != nil {
			return nil, err
		}
		st := CalendarState{
			Ref:       model.CalendarRef{AccountID: acct, CalendarID: cal},
			Cursor:    cursor.String,
			Timezone:  tz.String,
			LastError: lastError.String,
		}
		if wStart.Valid {
			st.Window.Start = time.Unix(wStart.Int64, 0).UTC()
		}
		if wEnd.Valid {
			st.Window.End = time.Unix(wEnd.Int64, 0).UTC()
		}
		if lastSynced.Valid {
			st.LastSyncedAt = time.Unix(lastSynced.Int64, 0).UTC()
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// SetCursor はカーソルとカーソル確立時のウィンドウを保存する(行が無ければ作る)。
func (s *Store) SetCursor(ref model.CalendarRef, cursor string, w model.Window) error {
	_, err := s.db.Exec(`
INSERT INTO calendars (account_id, calendar_id, cursor, window_start, window_end)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (account_id, calendar_id) DO UPDATE SET
  cursor       = excluded.cursor,
  window_start = excluded.window_start,
  window_end   = excluded.window_end`,
		ref.AccountID, ref.CalendarID, cursor, w.Start.Unix(), w.End.Unix())
	return err
}

// ClearCursor はカーソルとウィンドウを NULL に戻す(フル再同期のトリガー)。
func (s *Store) ClearCursor(ref model.CalendarRef) error {
	_, err := s.db.Exec(`
UPDATE calendars SET cursor = NULL, window_start = NULL, window_end = NULL
WHERE account_id = ? AND calendar_id = ?`,
		ref.AccountID, ref.CalendarID)
	return err
}

// SetCalendarTimezone は終日ブロッカー作成用のタイムゾーンをキャッシュする(行が無ければ作る)。
func (s *Store) SetCalendarTimezone(ref model.CalendarRef, tz string) error {
	_, err := s.db.Exec(`
INSERT INTO calendars (account_id, calendar_id, timezone) VALUES (?, ?, ?)
ON CONFLICT (account_id, calendar_id) DO UPDATE SET timezone = excluded.timezone`,
		ref.AccountID, ref.CalendarID, tz)
	return err
}

// SetCalendarError は last_error を設定する(msg=="" で NULL クリア)。
// 同期試行があった記録として last_synced_at も現在時刻で更新する。
func (s *Store) SetCalendarError(ref model.CalendarRef, msg string) error {
	var v any
	if msg != "" {
		v = msg
	}
	_, err := s.db.Exec(`
UPDATE calendars SET last_error = ?, last_synced_at = ?
WHERE account_id = ? AND calendar_id = ?`,
		v, time.Now().Unix(), ref.AccountID, ref.CalendarID)
	return err
}

// TouchSynced は last_synced_at を指定時刻に更新する。
func (s *Store) TouchSynced(ref model.CalendarRef, at time.Time) error {
	_, err := s.db.Exec(`
UPDATE calendars SET last_synced_at = ? WHERE account_id = ? AND calendar_id = ?`,
		at.Unix(), ref.AccountID, ref.CalendarID)
	return err
}

// DeleteCalendarsForAccount はアカウントの全カレンダー行を削除する(accounts remove 用)。
func (s *Store) DeleteCalendarsForAccount(accountID string) error {
	_, err := s.db.Exec(`DELETE FROM calendars WHERE account_id = ?`, accountID)
	return err
}
