package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/notify"
	"github.com/btajp/calsync/internal/provider/fake"
	"github.com/btajp/calsync/internal/store"
)

// fakeNotifier は Notifier のテストダブル。failWith は次の Send を 1 回失敗させる。
type fakeNotifier struct {
	mu        sync.Mutex
	digests   []fakeDigest
	reminders []fakeReminder
	failWith  error
}

type fakeDigest struct {
	day     time.Time
	entries []DigestEntry
	failed  []string
}

type fakeReminder struct {
	entry DigestEntry
	lead  time.Duration
}

func (f *fakeNotifier) SendDigest(ctx context.Context, day time.Time, entries []DigestEntry, failedAccounts []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		err := f.failWith
		f.failWith = nil
		return err
	}
	f.digests = append(f.digests, fakeDigest{day: day, entries: entries, failed: failedAccounts})
	return nil
}

func (f *fakeNotifier) SendReminder(ctx context.Context, e DigestEntry, lead time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		err := f.failWith
		f.failWith = nil
		return err
	}
	f.reminders = append(f.reminders, fakeReminder{entry: e, lead: lead})
	return nil
}

var jstLoc = time.FixedZone("JST", 9*3600)

// digestEngine は JST の 2026-07-05 07:30 を now とするエンジンを組み立てる。
func digestEngine(t *testing.T) (*Engine, *fake.Fake, *fakeNotifier) {
	t.Helper()
	e, f := newTestEngine(t)
	fn := &fakeNotifier{}
	e.Notifier = fn
	e.Cfg.Notifications.Slack = &config.SlackConfig{
		Channel: "C1", MorningDigest: "07:30", RemindBefore: 10 * time.Minute,
	}
	e.Now = func() time.Time { return time.Date(2026, 7, 5, 7, 30, 0, 0, jstLoc) }
	return e, f, fn
}

func timedEvent(id, ical, title string, startJST time.Time, busy bool) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID: id, ICalUID: ical, Title: title,
		StartUTC: startJST.UTC(), EndUTC: startJST.Add(time.Hour).UTC(), IsBusy: busy,
	}
}

func TestCollectDigestFiltersAndSorts(t *testing.T) {
	e, f, _ := digestEngine(t)
	ctx := context.Background()
	day := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)

	// アカウント a のライブ取得結果(fake は cursor=="" で full を返す)
	f.SetFullState(refA, []model.NormalizedEvent{
		timedEvent("late", "late@x", "午後の予定", time.Date(2026, 7, 5, 14, 0, 0, 0, jstLoc), true),
		timedEvent("early", "early@x", "朝会", time.Date(2026, 7, 5, 9, 0, 0, 0, jstLoc), true),
		timedEvent("free", "free@x", "free も含む", time.Date(2026, 7, 5, 11, 0, 0, 0, jstLoc), false),
		func() model.NormalizedEvent {
			ev := timedEvent("declined", "d@x", "辞退済み", time.Date(2026, 7, 5, 12, 0, 0, 0, jstLoc), true)
			ev.IsDeclined = true
			return ev
		}(),
		func() model.NormalizedEvent {
			ev := timedEvent("tagged", "t@x", "タグ二次判定", time.Date(2026, 7, 5, 13, 0, 0, 0, jstLoc), true)
			ev.OriginTag = "x:orig1"
			return ev
		}(),
		timedEvent("tomorrow", "tm@x", "翌日", time.Date(2026, 7, 6, 9, 0, 0, 0, jstLoc), true),
		// 前日(7/4)の終日予定: Window.Contains の UTC 近似だと JST で混入する(除外されること)
		{ID: "ad-prev", ICalUID: "ap@x", Title: "前日の終日", IsAllDay: true, AllDayStart: "2026-07-04", AllDayEnd: "2026-07-05", IsBusy: true},
		{ID: "ad-today", ICalUID: "at@x", Title: "当日の終日", IsAllDay: true, AllDayStart: "2026-07-05", AllDayEnd: "2026-07-06", IsBusy: true},
		timedEvent("blocker", "b@x", "受領ブロッカー", time.Date(2026, 7, 5, 10, 0, 0, 0, jstLoc), true),
	})
	// mappings 一次判定: アカウント a 上の "blocker" は受領ブロッカー
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "b", OriginCalendar: "primary", OriginEventID: "src1",
		TargetAccount: "a", TargetCalendar: "primary", BlockerEventID: "blocker",
		IdempotencyKey: "k1", TimeHash: "h1", Status: store.StatusActive,
	}))

	entries, failed := e.collectDigest(ctx, day)
	require.Empty(t, failed)

	var titles []string
	for _, en := range entries {
		titles = append(titles, en.Title)
	}
	// 終日が先頭、以降開始時刻順。free は含む。前日終日・辞退・タグ付き・ブロッカー・翌日は除外
	require.Equal(t, []string{"当日の終日", "朝会", "free も含む", "午後の予定"}, titles)
}

func TestCollectDigestDedupesAcrossAccounts(t *testing.T) {
	e, f, _ := digestEngine(t)
	day := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)
	start := time.Date(2026, 7, 5, 10, 0, 0, 0, jstLoc)

	f.SetFullState(refA, []model.NormalizedEvent{timedEvent("ev-a", "same@x", "設計レビュー", start, true)})
	f.SetFullState(model.CalendarRef{AccountID: "b", CalendarID: "primary"},
		[]model.NormalizedEvent{timedEvent("ev-b", "same@x", "", start, true)}) // b 側は無題
	f.SetFullState(model.CalendarRef{AccountID: "c", CalendarID: "primary"}, nil)

	entries, failed := e.collectDigest(context.Background(), day)
	require.Empty(t, failed)
	require.Len(t, entries, 1)
	require.Equal(t, "設計レビュー", entries[0].Title) // 設定順で最初の非空 Title
	require.Equal(t, []string{"a", "b"}, entries[0].AccountIDs)
}

func TestCollectDigestReportsFailedAccounts(t *testing.T) {
	e, f, _ := digestEngine(t)
	day := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)
	f.SetFullState(refA, []model.NormalizedEvent{timedEvent("ok", "ok@x", "生きてる", time.Date(2026, 7, 5, 9, 0, 0, 0, jstLoc), true)})
	f.FailNext(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, errors.New("boom"))

	entries, failed := e.collectDigest(context.Background(), day)
	require.Equal(t, []string{"b"}, failed)
	require.Len(t, entries, 1)
}

func TestRunDigest(t *testing.T) {
	e, f, fn := digestEngine(t)
	f.SetFullState(refA, nil)
	scheduled := time.Date(2026, 7, 5, 7, 30, 0, 0, jstLoc)

	// 成功 → 翌日 07:30 を返す
	next := e.runDigest(context.Background(), scheduled)
	require.Len(t, fn.digests, 1)
	require.Equal(t, time.Date(2026, 7, 6, 7, 30, 0, 0, jstLoc).Unix(), next.Unix())

	// リトライ可能エラー → scheduled 据え置き
	fn.failWith = errors.New("network down")
	next = e.runDigest(context.Background(), scheduled)
	require.Equal(t, scheduled.Unix(), next.Unix())
	require.Len(t, fn.digests, 1)

	// リトライ不能エラー → 翌日へ進める
	fn.failWith = fmt.Errorf("invalid_auth: %w", notify.ErrNonRetryable)
	next = e.runDigest(context.Background(), scheduled)
	require.Equal(t, time.Date(2026, 7, 6, 7, 30, 0, 0, jstLoc).Unix(), next.Unix())
	require.Len(t, fn.digests, 1)

	// 対象日が過去日(跨日遅延)→ 送らず放棄して翌日へ(スペック 9 章)
	stale := time.Date(2026, 7, 4, 7, 30, 0, 0, jstLoc)
	next = e.runDigest(context.Background(), stale)
	require.Len(t, fn.digests, 1)
	require.Equal(t, time.Date(2026, 7, 6, 7, 30, 0, 0, jstLoc).Unix(), next.Unix())
}
