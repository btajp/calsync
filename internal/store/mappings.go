package store

import (
	"database/sql"
	"errors"
)

// Mapping は origin 予定 1 件 → ターゲットアカウント 1 件のブロッカー対応(仕様書 7 章)。
type Mapping struct {
	OriginAccount, OriginCalendar, OriginEventID string
	TargetAccount, TargetCalendar                string
	BlockerEventID                               string // pending/suppressed 中は ""
	IdempotencyKey                               string
	TimeHash                                     string
	Status                                       string // StatusPending / StatusActive / StatusSuppressed
}

const (
	StatusPending    = "pending"
	StatusActive     = "active"
	StatusSuppressed = "suppressed"
)

// SELECT 列の共通定義。blocker_event_id は NULL を "" に正規化して読む。
const mappingColumns = `origin_account, origin_calendar, origin_event_id,
target_account, target_calendar, COALESCE(blocker_event_id, ''), idempotency_key, time_hash, status`

func scanMapping(sc interface{ Scan(...any) error }) (Mapping, error) {
	var m Mapping
	err := sc.Scan(&m.OriginAccount, &m.OriginCalendar, &m.OriginEventID,
		&m.TargetAccount, &m.TargetCalendar, &m.BlockerEventID,
		&m.IdempotencyKey, &m.TimeHash, &m.Status)
	return m, err
}

func (s *Store) queryMappings(query string, args ...any) ([]Mapping, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PutMapping は mapping を upsert する。BlockerEventID=="" は NULL として保存する
// (IsBlocker が空 ID にヒットしないことの担保)。
func (s *Store) PutMapping(m Mapping) error {
	var blockerID any
	if m.BlockerEventID != "" {
		blockerID = m.BlockerEventID
	}
	_, err := s.db.Exec(`
INSERT INTO mappings (origin_account, origin_calendar, origin_event_id,
                      target_account, target_calendar, blocker_event_id,
                      idempotency_key, time_hash, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (origin_account, origin_calendar, origin_event_id, target_account) DO UPDATE SET
  target_calendar  = excluded.target_calendar,
  blocker_event_id = excluded.blocker_event_id,
  idempotency_key  = excluded.idempotency_key,
  time_hash        = excluded.time_hash,
  status           = excluded.status`,
		m.OriginAccount, m.OriginCalendar, m.OriginEventID,
		m.TargetAccount, m.TargetCalendar, blockerID,
		m.IdempotencyKey, m.TimeHash, m.Status)
	return err
}

// GetMapping は主キー一致の 1 行を返す。未存在は (nil, nil)。
func (s *Store) GetMapping(originAcct, originCal, originEventID, targetAcct string) (*Mapping, error) {
	row := s.db.QueryRow(`
SELECT `+mappingColumns+`
FROM mappings
WHERE origin_account = ? AND origin_calendar = ? AND origin_event_id = ? AND target_account = ?`,
		originAcct, originCal, originEventID, targetAcct)
	m, err := scanMapping(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// DeleteMapping は主キー一致の 1 行を削除する。未存在でもエラーにしない(冪等)。
func (s *Store) DeleteMapping(originAcct, originCal, originEventID, targetAcct string) error {
	_, err := s.db.Exec(`
DELETE FROM mappings
WHERE origin_account = ? AND origin_calendar = ? AND origin_event_id = ? AND target_account = ?`,
		originAcct, originCal, originEventID, targetAcct)
	return err
}

// ListMappingsForOrigin は 1 origin イベントの全ターゲット行を返す(ブロッカー一斉削除用)。
func (s *Store) ListMappingsForOrigin(originAcct, originCal, originEventID string) ([]Mapping, error) {
	return s.queryMappings(`
SELECT `+mappingColumns+`
FROM mappings
WHERE origin_account = ? AND origin_calendar = ? AND origin_event_id = ?
ORDER BY target_account`, originAcct, originCal, originEventID)
}

// ListMappingsWhereOriginAccount は origin がそのアカウントの全行を返す(accounts remove: 配布済み削除用)。
func (s *Store) ListMappingsWhereOriginAccount(accountID string) ([]Mapping, error) {
	return s.queryMappings(`
SELECT `+mappingColumns+`
FROM mappings WHERE origin_account = ?
ORDER BY origin_calendar, origin_event_id, target_account`, accountID)
}

// ListMappingsWhereTargetAccount は target がそのアカウントの全行を返す(accounts remove: 受領削除用)。
func (s *Store) ListMappingsWhereTargetAccount(accountID string) ([]Mapping, error) {
	return s.queryMappings(`
SELECT `+mappingColumns+`
FROM mappings WHERE target_account = ?
ORDER BY origin_account, origin_calendar, origin_event_id`, accountID)
}

// IsBlocker はループ防止の一次判定(仕様書 6.3): 受信イベント ID が自作ブロッカーとして
// 登録済みかを返す。blocker_event_id が空(NULL)の行にはヒットしない。
func (s *Store) IsBlocker(targetAcct, eventID string) (bool, error) {
	if eventID == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(1) FROM mappings WHERE target_account = ? AND blocker_event_id = ?`,
		targetAcct, eventID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListPendingMappings は pending のまま残った行を返す(リコンサイルの解決対象。仕様書 6.4)。
func (s *Store) ListPendingMappings() ([]Mapping, error) {
	return s.queryMappings(`
SELECT `+mappingColumns+`
FROM mappings WHERE status = ?
ORDER BY origin_account, origin_calendar, origin_event_id, target_account`, StatusPending)
}

// ListSuppressedByOriginICalUID は suppressed 昇格用(仕様書 6.5): ターゲットの実予定が
// 消えたとき、その iCalUID を origin 側イベントキャッシュと JOIN して逆引きする。
// origin キャッシュに ical_uid 一致のイベントが実在する suppressed 行だけを返す。
func (s *Store) ListSuppressedByOriginICalUID(targetAcct, icalUID string) ([]Mapping, error) {
	return s.queryMappings(`
SELECT m.origin_account, m.origin_calendar, m.origin_event_id,
       m.target_account, m.target_calendar, COALESCE(m.blocker_event_id, ''),
       m.idempotency_key, m.time_hash, m.status
FROM mappings m
JOIN events e
  ON e.account_id = m.origin_account
 AND e.calendar_id = m.origin_calendar
 AND e.event_id = m.origin_event_id
WHERE m.target_account = ? AND m.status = ? AND e.ical_uid = ?
ORDER BY m.origin_account, m.origin_calendar, m.origin_event_id`,
		targetAcct, StatusSuppressed, icalUID)
}
