package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/btajp/calsync/internal/model"
)

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UpsertEvent は busy イベントをキャッシュに upsert する。
// time_hash は model.TimeHash で導出して保存する。
func (s *Store) UpsertEvent(ref model.CalendarRef, ev model.NormalizedEvent) error {
	_, err := s.db.Exec(`
INSERT INTO events (account_id, calendar_id, event_id, ical_uid, start_utc, end_utc,
                    is_all_day, all_day_start, all_day_end, time_hash, title)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (account_id, calendar_id, event_id) DO UPDATE SET
  ical_uid      = excluded.ical_uid,
  start_utc     = excluded.start_utc,
  end_utc       = excluded.end_utc,
  is_all_day    = excluded.is_all_day,
  all_day_start = excluded.all_day_start,
  all_day_end   = excluded.all_day_end,
  time_hash     = excluded.time_hash,
  title         = excluded.title`,
		ref.AccountID, ref.CalendarID, ev.ID, ev.ICalUID,
		ev.StartUTC.UTC().Unix(), ev.EndUTC.UTC().Unix(),
		boolToInt(ev.IsAllDay), ev.AllDayStart, ev.AllDayEnd,
		model.TimeHash(ev), ev.Title)
	return err
}

// GetEvent はキャッシュ行を NormalizedEvent に復元して返す。未存在は (nil, nil)。
// キャッシュには busy イベントのみ入る契約のため IsBusy=true で復元する。
func (s *Store) GetEvent(ref model.CalendarRef, eventID string) (*model.NormalizedEvent, error) {
	row := s.db.QueryRow(`
SELECT ical_uid, start_utc, end_utc, is_all_day, all_day_start, all_day_end, title
FROM events WHERE account_id = ? AND calendar_id = ? AND event_id = ?`,
		ref.AccountID, ref.CalendarID, eventID)
	var (
		icalUID, adStart, adEnd, title sql.NullString
		startUTC, endUTC               sql.NullInt64
		isAllDay                       int
	)
	err := row.Scan(&icalUID, &startUTC, &endUTC, &isAllDay, &adStart, &adEnd, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ev := &model.NormalizedEvent{
		ID:          eventID,
		ICalUID:     icalUID.String,
		Title:       title.String,
		IsAllDay:    isAllDay != 0,
		AllDayStart: adStart.String,
		AllDayEnd:   adEnd.String,
		IsBusy:      true,
	}
	if startUTC.Valid {
		ev.StartUTC = time.Unix(startUTC.Int64, 0).UTC()
	}
	if endUTC.Valid {
		ev.EndUTC = time.Unix(endUTC.Int64, 0).UTC()
	}
	return ev, nil
}

// DeleteEvent はキャッシュ行を削除する。未存在でもエラーにしない(冪等)。
func (s *Store) DeleteEvent(ref model.CalendarRef, eventID string) error {
	_, err := s.db.Exec(`
DELETE FROM events WHERE account_id = ? AND calendar_id = ? AND event_id = ?`,
		ref.AccountID, ref.CalendarID, eventID)
	return err
}

// ListEventIDs はカレンダーのキャッシュ済みイベント ID を昇順で返す
// (リコンサイルの set-difference 用)。
func (s *Store) ListEventIDs(ref model.CalendarRef) ([]string, error) {
	rows, err := s.db.Query(`
SELECT event_id FROM events WHERE account_id = ? AND calendar_id = ? ORDER BY event_id`,
		ref.AccountID, ref.CalendarID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteEventsForAccount はアカウントの全キャッシュを削除する(accounts remove / フル再同期用)。
func (s *Store) DeleteEventsForAccount(accountID string) error {
	_, err := s.db.Exec(`DELETE FROM events WHERE account_id = ?`, accountID)
	return err
}

// HasBusyEventByICalUID はターゲットアカウントに「同一会議の実予定」があるかを返す(仕様書 6.5)。
// allDayStart が非空なら ical_uid + all_day_start 一致、そうでなければ ical_uid + start_utc 一致で判定する。
func (s *Store) HasBusyEventByICalUID(accountID, icalUID string, startUTC time.Time, allDayStart string) (bool, error) {
	if icalUID == "" {
		return false, nil
	}
	var (
		n   int
		err error
	)
	if allDayStart != "" {
		err = s.db.QueryRow(`
SELECT COUNT(1) FROM events
WHERE account_id = ? AND ical_uid = ? AND all_day_start = ?`,
			accountID, icalUID, allDayStart).Scan(&n)
	} else {
		err = s.db.QueryRow(`
SELECT COUNT(1) FROM events
WHERE account_id = ? AND ical_uid = ? AND start_utc = ?`,
			accountID, icalUID, startUTC.UTC().Unix()).Scan(&n)
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
