// Package fake はエンジンテスト用のインメモリ Provider 実装。
// 実 HTTP を使わず、実プロバイダと同じ契約(冪等作成・404削除成功扱い・
// 完走時のみ非空カーソル)を再現する。
package fake

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/work-a-co/calsync/internal/model"
	"github.com/work-a-co/calsync/internal/provider"
)

// コンパイル時に provider.Provider を満たすことを保証する。
var _ provider.Provider = (*Fake)(nil)

type storedBlocker struct {
	rec  model.BlockerRecord
	body model.Blocker
}

type calState struct {
	full      []model.NormalizedEvent
	queue     [][]model.NormalizedEvent
	failNext  error
	blockers  map[string]*storedBlocker // key: eventID
	byIdemKey map[string]string         // idemKey → eventID
	timezone  string
	cursorSeq int
	idSeq     int
}

type Fake struct {
	mu   sync.Mutex
	cals map[model.CalendarRef]*calState
}

func New() *Fake {
	return &Fake{cals: make(map[model.CalendarRef]*calState)}
}

// state は cal の状態を返す(なければ初期化)。呼び出し側が f.mu を保持していること。
func (f *Fake) state(cal model.CalendarRef) *calState {
	st, ok := f.cals[cal]
	if !ok {
		st = &calState{
			blockers:  make(map[string]*storedBlocker),
			byIdemKey: make(map[string]string),
			timezone:  "UTC",
		}
		f.cals[cal] = st
	}
	return st
}

// SetFullState は cursor=="" の Changes が返す全量を設定する(フル同期シミュレーション)。
func (f *Fake) SetFullState(cal model.CalendarRef, evs []model.NormalizedEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state(cal).full = append([]model.NormalizedEvent(nil), evs...)
}

// QueueChanges は次の増分 Changes(cursor!="")が返すイベント列を積む。
// 呼ばれるたびにキューを1要素消費し、newCursor は "c1","c2",... と増加する。
func (f *Fake) QueueChanges(cal model.CalendarRef, evs []model.NormalizedEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state(cal)
	st.queue = append(st.queue, append([]model.NormalizedEvent(nil), evs...))
}

// FailNext は次の Changes 呼び出しに err を返させる(1回で消費)。
// ErrCursorInvalid / ErrAuthExpired のテスト用。
func (f *Fake) FailNext(cal model.CalendarRef, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state(cal).failNext = err
}

// Blockers は cal 上のブロッカーを EventID 昇順で返す(観測用)。
func (f *Fake) Blockers(cal model.CalendarRef) []model.BlockerRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state(cal)
	recs := make([]model.BlockerRecord, 0, len(st.blockers))
	for _, b := range st.blockers {
		recs = append(recs, b.rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].EventID < recs[j].EventID })
	return recs
}

// StoredBlocker は eventID のブロッカー本体(Title / TargetTimezone 等)を返す(観測用)。
// NOTE: コントラクトの公開 API への追加。Task 8 が Blocker の内容を検証するために使う。
func (f *Fake) StoredBlocker(cal model.CalendarRef, eventID string) (model.Blocker, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.state(cal).blockers[eventID]
	if !ok {
		return model.Blocker{}, false
	}
	return b.body, true
}

// SeedBlocker はタグ付きブロッカーを事前配置する(adoption テスト用)。
// EventID で upsert する(同じ EventID への再 Seed は上書き)。
func (f *Fake) SeedBlocker(cal model.CalendarRef, rec model.BlockerRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state(cal).blockers[rec.EventID] = &storedBlocker{rec: rec}
}

// SetTimezone は GetCalendarTimezone が返す値を設定する(既定 "UTC")。
func (f *Fake) SetTimezone(cal model.CalendarRef, tz string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state(cal).timezone = tz
}

// --- provider.Provider 実装 ---

func (f *Fake) Changes(ctx context.Context, cal model.CalendarRef, cursor string, window model.Window) ([]model.NormalizedEvent, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state(cal)
	if st.failNext != nil {
		err := st.failNext
		st.failNext = nil
		return nil, "", err
	}
	var evs []model.NormalizedEvent
	if cursor == "" {
		evs = append([]model.NormalizedEvent(nil), st.full...)
	} else if len(st.queue) > 0 {
		evs = st.queue[0]
		st.queue = st.queue[1:]
	}
	// ウィンドウではフィルタしない(実プロバイダ同様、ウィンドウ外も返りうる契約)
	st.cursorSeq++
	return evs, fmt.Sprintf("c%d", st.cursorSeq), nil
}

// blockerTimeHash は Blocker の時刻から BlockerRecord.TimeHash を導出する
// (model.TimeHash と同一のハッシュ規則)。
func blockerTimeHash(b model.Blocker) string {
	return model.TimeHash(model.NormalizedEvent{
		StartUTC:    b.StartUTC,
		EndUTC:      b.EndUTC,
		IsAllDay:    b.IsAllDay,
		AllDayStart: b.AllDayStart,
		AllDayEnd:   b.AllDayEnd,
	})
}

func (f *Fake) CreateBlocker(ctx context.Context, cal model.CalendarRef, b model.Blocker, idemKey string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state(cal)
	if id, ok := st.byIdemKey[idemKey]; ok {
		return id, nil // 同一冪等キーの再作成は既存 ID を返す(実プロバイダと同じ契約)
	}
	st.idSeq++
	id := fmt.Sprintf("fake-%s-%d", cal.AccountID, st.idSeq)
	st.blockers[id] = &storedBlocker{
		rec:  model.BlockerRecord{EventID: id, OriginTag: b.OriginTag, TimeHash: blockerTimeHash(b)},
		body: b,
	}
	st.byIdemKey[idemKey] = id
	return id, nil
}

func (f *Fake) UpdateBlocker(ctx context.Context, cal model.CalendarRef, eventID string, b model.Blocker) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.state(cal).blockers[eventID]
	if !ok {
		return fmt.Errorf("update blocker %s on %s: %w", eventID, cal, provider.ErrNotFound)
	}
	sb.rec.OriginTag = b.OriginTag
	sb.rec.TimeHash = blockerTimeHash(b)
	sb.body = b
	return nil
}

func (f *Fake) DeleteBlocker(ctx context.Context, cal model.CalendarRef, eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state(cal)
	delete(st.blockers, eventID) // 未存在(404 相当)でも成功扱い

	// byIdemKey から該当するエントリを削除(削除後の同キー再作成は新規扱い)
	for key, id := range st.byIdemKey {
		if id == eventID {
			delete(st.byIdemKey, key)
			break
		}
	}
	return nil
}

func (f *Fake) ListBlockers(ctx context.Context, cal model.CalendarRef, window model.Window) ([]model.BlockerRecord, error) {
	// fake はウィンドウでフィルタしない(リコンサイルテストを単純に保つ)
	return f.Blockers(cal), nil
}

func (f *Fake) GetCalendarTimezone(ctx context.Context, cal model.CalendarRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state(cal).timezone, nil
}
