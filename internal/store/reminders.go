package store

import (
	"database/sql"
	"time"

	"github.com/btajp/calsync/internal/model"
)

// UpcomingEvent は開始前リマインドの対象候補(events キャッシュの時刻指定イベント)。
// キャッシュには busy・未辞退・ウィンドウ内の実予定しか入らない契約のため、
// 抽出側での追加フィルタは不要(スペック 6 章)。
type UpcomingEvent struct {
	Ref      model.CalendarRef
	EventID  string
	ICalUID  string
	Title    string
	StartUTC time.Time
	EndUTC   time.Time
}

// ListUpcomingEvents は「now < start_utc <= now+lead」の時刻指定イベントを
// start_utc 昇順で返す(スペック 6 章の抽出条件。終日は対象外)。
func (s *Store) ListUpcomingEvents(now time.Time, lead time.Duration) ([]UpcomingEvent, error) {
	rows, err := s.db.Query(`
SELECT account_id, calendar_id, event_id, ical_uid, title, start_utc, end_utc
FROM events
WHERE is_all_day = 0 AND start_utc > ? AND start_utc <= ?
ORDER BY start_utc, account_id, calendar_id, event_id`,
		now.UTC().Unix(), now.Add(lead).UTC().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpcomingEvent
	for rows.Next() {
		var (
			u                  UpcomingEvent
			icalUID, title     sql.NullString
			startUnix, endUnix sql.NullInt64
		)
		if err := rows.Scan(&u.Ref.AccountID, &u.Ref.CalendarID, &u.EventID, &icalUID, &title, &startUnix, &endUnix); err != nil {
			return nil, err
		}
		u.ICalUID, u.Title = icalUID.String, title.String
		if startUnix.Valid {
			u.StartUTC = time.Unix(startUnix.Int64, 0).UTC()
		}
		if endUnix.Valid {
			u.EndUTC = time.Unix(endUnix.Int64, 0).UTC()
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// WasReminderSent は自分自身の送信記録があるかを返す。
func (s *Store) WasReminderSent(ref model.CalendarRef, eventID string, startUTC time.Time) (bool, error) {
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(1) FROM reminders_sent
WHERE account_id = ? AND calendar_id = ? AND event_id = ? AND start_utc = ?`,
		ref.AccountID, ref.CalendarID, eventID, startUTC.UTC().Unix()).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// HasReminderForICalUID は同一 iCalUID+開始時刻の送信記録があるかを返す
// (dedupe_same_meeting 用。スペック 6 章)。icalUID=="" は常に false(dedupe 対象外)。
func (s *Store) HasReminderForICalUID(icalUID string, startUTC time.Time) (bool, error) {
	if icalUID == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(1) FROM reminders_sent WHERE ical_uid = ? AND start_utc = ?`,
		icalUID, startUTC.UTC().Unix()).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkReminderSent は送信記録を upsert する(冪等)。記録条件は
// (a) 送信成功時 (b) dedupe により送信不要と確定した時 の 2 つ(スペック 6 章)。
func (s *Store) MarkReminderSent(ref model.CalendarRef, eventID, icalUID string, startUTC, sentAt time.Time) error {
	_, err := s.db.Exec(`
INSERT INTO reminders_sent (account_id, calendar_id, event_id, start_utc, ical_uid, sent_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (account_id, calendar_id, event_id, start_utc) DO UPDATE SET
  ical_uid = excluded.ical_uid,
  sent_at  = excluded.sent_at`,
		ref.AccountID, ref.CalendarID, eventID, startUTC.UTC().Unix(), icalUID, sentAt.UTC().Unix())
	return err
}

// CleanupRemindersSent は start_utc < before の行を削除する(日次リコンサイル相乗り。
// スペック 4.3: before = now-48h)。
func (s *Store) CleanupRemindersSent(before time.Time) error {
	_, err := s.db.Exec(`DELETE FROM reminders_sent WHERE start_utc < ?`, before.UTC().Unix())
	return err
}

// DeleteRemindersForAccount はアカウントの送信記録を削除する(accounts remove 用)。
func (s *Store) DeleteRemindersForAccount(accountID string) error {
	_, err := s.db.Exec(`DELETE FROM reminders_sent WHERE account_id = ?`, accountID)
	return err
}
