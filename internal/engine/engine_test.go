package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/work-a-co/calsync/internal/config"
	"github.com/work-a-co/calsync/internal/model"
	"github.com/work-a-co/calsync/internal/provider"
	"github.com/work-a-co/calsync/internal/provider/fake"
	"github.com/work-a-co/calsync/internal/store"
)

var (
	testNow = time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	refA    = model.CalendarRef{AccountID: "a", CalendarID: "primary"} // origin(google)
	calBv   = model.CalendarRef{AccountID: "b", CalendarID: "primary"} // target(microsoft)
	calCv   = model.CalendarRef{AccountID: "c", CalendarID: "primary"} // target(google)
)

func testConfig() *config.Config {
	return &config.Config{
		PollInterval:      time.Minute,
		SyncWindowMonths:  3,
		BlockerTitle:      "予定あり",
		DedupeSameMeeting: true,
		BusyShowAs:        []string{"busy", "oof", "tentative"},
		Accounts: []config.Account{
			{ID: "a", Provider: "google", Email: "a@example.com", Calendars: []string{"primary"}, BlockerCalendar: "primary"},
			{ID: "b", Provider: "microsoft", Email: "b@example.com", Calendars: []string{"primary"}, BlockerCalendar: "primary"},
			{ID: "c", Provider: "google", Email: "c@example.com", Calendars: []string{"primary"}, BlockerCalendar: "primary"},
		},
	}
}

// newTestEngine は 実SQLite(t.TempDir)+ fake プロバイダのエンジンを組み立てる。
// ターゲット b/c のタイムゾーンは calendars テーブルに事前キャッシュしておく。
func newTestEngine(t *testing.T) (*Engine, *fake.Fake) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	f := fake.New()
	e := &Engine{
		Store:     st,
		Providers: map[string]provider.Provider{"a": f, "b": f, "c": f},
		Cfg:       testConfig(),
		Now:       func() time.Time { return testNow },
	}
	for acct, tz := range map[string]string{"b": "Asia/Tokyo", "c": "America/New_York"} {
		ref := model.CalendarRef{AccountID: acct, CalendarID: "primary"}
		require.NoError(t, st.UpsertCalendar(ref))
		require.NoError(t, st.SetCalendarTimezone(ref, tz))
	}
	return e, f
}

// busyEvent はウィンドウ内(2026-07-10)の busy イベントを返す。
func busyEvent(id string) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID:       id,
		ICalUID:  id + "@example.com",
		StartUTC: time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
		IsBusy:   true,
	}
}

// ---- SyncCalendar ----

func TestSyncCalendar_SetsCursorOnlyOnCompletion(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	f.SetFullState(refA, []model.NormalizedEvent{busyEvent("ev1")})

	// フル同期(cursor=="")完走 → "c1" を永続化し、イベントも処理される
	require.NoError(t, e.SyncCalendar(ctx, refA))
	st, err := e.Store.GetCalendar(refA)
	require.NoError(t, err)
	require.NotNil(t, st)
	require.Equal(t, "c1", st.Cursor)
	require.Len(t, f.Blockers(calBv), 1)
	require.Len(t, f.Blockers(calCv), 1)

	// 増分完走 → "c2" に進む
	f.QueueChanges(refA, []model.NormalizedEvent{busyEvent("ev2")})
	require.NoError(t, e.SyncCalendar(ctx, refA))
	st, err = e.Store.GetCalendar(refA)
	require.NoError(t, err)
	require.Equal(t, "c2", st.Cursor)
	require.Len(t, f.Blockers(calBv), 2)
}

func TestSyncCalendar_AuthErrorPropagatesAndKeepsCursor(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	f.SetFullState(refA, nil)
	require.NoError(t, e.SyncCalendar(ctx, refA)) // cursor "c1" を確立

	f.FailNext(refA, provider.ErrAuthExpired)
	err := e.SyncCalendar(ctx, refA)
	require.ErrorIs(t, err, provider.ErrAuthExpired) // そのまま返る

	// 失敗時はカーソルを進めない(Changes 完走時のみ SetCursor)
	st, gerr := e.Store.GetCalendar(refA)
	require.NoError(t, gerr)
	require.Equal(t, "c1", st.Cursor)
}

// ---- processEvent (g): 新規 busy → 全ターゲットに作成 ----

func TestProcessEvent_CreateOnAllTargets(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	require.NoError(t, e.processEvent(ctx, refA, ev))

	wantTag := model.OriginTagOf("a", "ev1")
	wantHash := model.TimeHash(ev)

	cases := []struct {
		cal     model.CalendarRef
		acct    string
		idemKey string
	}{
		// b は microsoft → transactionId / c は google → クライアント生成イベントID
		{calBv, "b", model.MSTransactionID(wantTag, "b")},
		{calCv, "c", model.GoogleBlockerID(wantTag, "c")},
	}
	for _, tc := range cases {
		blks := f.Blockers(tc.cal)
		require.Len(t, blks, 1, "target %s", tc.acct)
		require.Equal(t, wantTag, blks[0].OriginTag)
		require.Equal(t, wantHash, blks[0].TimeHash)

		body, ok := f.StoredBlocker(tc.cal, blks[0].EventID)
		require.True(t, ok)
		require.Equal(t, "予定あり", body.Title)  // Cfg.BlockerTitle
		require.Empty(t, body.TargetTimezone) // 時刻指定は UTC 固定で送るため TZ 不要(仕様6.6)
		require.Equal(t, wantTag, body.OriginTag)

		m, err := e.Store.GetMapping("a", "primary", "ev1", tc.acct)
		require.NoError(t, err)
		require.NotNil(t, m)
		require.Equal(t, store.StatusActive, m.Status)
		require.Equal(t, blks[0].EventID, m.BlockerEventID)
		require.Equal(t, tc.idemKey, m.IdempotencyKey) // provider 種別ごとの決定的冪等キー
		require.Equal(t, wantHash, m.TimeHash)
		require.Equal(t, "primary", m.TargetCalendar)
	}

	// origin 側の events キャッシュにも登録される
	cached, err := e.Store.GetEvent(refA, "ev1")
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.Equal(t, wantHash, model.TimeHash(*cached))

	// origin 自身にはブロッカーを作らない
	require.Empty(t, f.Blockers(refA))
}

// (g) intent-first の回復性 1: pending 登録後・作成前にクラッシュした状態からの再処理
func TestProcessEvent_PendingRecovery(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	tag := model.OriginTagOf("a", "ev1")
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: "ev1",
		TargetAccount: "b", TargetCalendar: "primary",
		IdempotencyKey: model.MSTransactionID(tag, "b"),
		TimeHash:       model.TimeHash(ev),
		Status:         store.StatusPending, // BlockerEventID は空のまま
	}))

	require.NoError(t, e.processEvent(ctx, refA, ev))

	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, store.StatusActive, m.Status) // pending → active に解決
	require.NotEmpty(t, m.BlockerEventID)
	require.Len(t, f.Blockers(calBv), 1)
	require.Len(t, f.Blockers(calCv), 1) // 他ターゲットは通常作成
}

// (g) intent-first の回復性 2: CreateBlocker 成功後・active 更新前にクラッシュ。
// 同一冪等キーの再作成は既存 ID が返るため二重作成にならない(仕様6.4)
func TestProcessEvent_CreateAfterCrashBeforeActive(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	tag := model.OriginTagOf("a", "ev1")
	idem := model.MSTransactionID(tag, "b")

	preID, err := f.CreateBlocker(ctx, calBv, model.Blocker{
		Title: "予定あり", StartUTC: ev.StartUTC, EndUTC: ev.EndUTC,
		TargetTimezone: "Asia/Tokyo", OriginTag: tag,
	}, idem)
	require.NoError(t, err)
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: "ev1",
		TargetAccount: "b", TargetCalendar: "primary",
		IdempotencyKey: idem, TimeHash: model.TimeHash(ev),
		Status: store.StatusPending,
	}))

	require.NoError(t, e.processEvent(ctx, refA, ev))

	require.Len(t, f.Blockers(calBv), 1) // 二重作成なし
	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Equal(t, store.StatusActive, m.Status)
	require.Equal(t, preID, m.BlockerEventID) // 既存 ID で収容
}

// (g) 終日イベントは現地日付のまま配布され、TargetTimezone が付く
func TestProcessEvent_AllDayBlockerKeepsLocalDates(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := model.NormalizedEvent{
		ID:          "allday1",
		ICalUID:     "allday1@example.com",
		IsAllDay:    true,
		AllDayStart: "2026-07-15",
		AllDayEnd:   "2026-07-16",
		IsBusy:      true,
	}
	require.NoError(t, e.processEvent(ctx, refA, ev))

	blks := f.Blockers(calBv)
	require.Len(t, blks, 1)
	body, ok := f.StoredBlocker(calBv, blks[0].EventID)
	require.True(t, ok)
	require.True(t, body.IsAllDay)
	require.Equal(t, "2026-07-15", body.AllDayStart)
	require.Equal(t, "2026-07-16", body.AllDayEnd)
	require.Equal(t, "Asia/Tokyo", body.TargetTimezone) // Graph はこの TZ の midnight 境界で作る
}

// calendars.timezone が未キャッシュなら(終日イベントの配布時に)provider から
// 取得して保存する
func TestUpsertBlockers_FetchesTimezoneWhenNotCached(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	require.NoError(t, e.Store.DeleteCalendarsForAccount("c")) // c のキャッシュを消す
	f.SetTimezone(calCv, "Europe/Berlin")

	allDay := model.NormalizedEvent{
		ID: "allday1", ICalUID: "allday1@example.com", IsBusy: true,
		IsAllDay: true, AllDayStart: "2026-07-15", AllDayEnd: "2026-07-16",
	}
	require.NoError(t, e.processEvent(ctx, refA, allDay))

	blks := f.Blockers(calCv)
	require.Len(t, blks, 1)
	body, ok := f.StoredBlocker(calCv, blks[0].EventID)
	require.True(t, ok)
	require.Equal(t, "Europe/Berlin", body.TargetTimezone)

	st, err := e.Store.GetCalendar(calCv)
	require.NoError(t, err)
	require.NotNil(t, st)
	require.Equal(t, "Europe/Berlin", st.Timezone) // calendars.timezone にキャッシュされる
}

// tzCountingProvider は GetCalendarTimezone の呼び出し回数を数えるラッパー
// (タイムゾーン取得の遅延化=時刻指定イベントでは呼ばれないことの検証用)。
type tzCountingProvider struct {
	provider.Provider
	mu    sync.Mutex
	calls int
}

func (p *tzCountingProvider) GetCalendarTimezone(ctx context.Context, cal model.CalendarRef) (string, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return p.Provider.GetCalendarTimezone(ctx, cal)
}

func (p *tzCountingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// 時刻指定イベントの配布では GetCalendarTimezone を呼ばない(仕様6.6:
// タイムゾーンは終日ブロッカーにしか使われない。毎イベントの TZ 取得は
// スコープ要求と API コールの両面で無駄。最終ホールブランチレビュー追補 Issue 1)。
func TestUpsertBlockers_TimedEventDoesNotFetchTimezone(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	// TZ キャッシュを消し、「呼ばれるなら必ず provider に到達する」状態にする
	require.NoError(t, e.Store.DeleteCalendarsForAccount("b"))
	require.NoError(t, e.Store.DeleteCalendarsForAccount("c"))
	cp := &tzCountingProvider{Provider: f}
	e.Providers["b"] = cp
	e.Providers["c"] = cp

	require.NoError(t, e.processEvent(ctx, refA, busyEvent("ev1")))

	require.Len(t, f.Blockers(calBv), 1)
	require.Len(t, f.Blockers(calCv), 1)
	require.Zero(t, cp.count(), "時刻指定イベントでは GetCalendarTimezone を呼ばない")

	// 終日イベントでは取得される(カウンタが機能していることの対照)
	allDay := model.NormalizedEvent{
		ID: "allday1", ICalUID: "allday1@example.com", IsBusy: true,
		IsAllDay: true, AllDayStart: "2026-07-15", AllDayEnd: "2026-07-16",
	}
	require.NoError(t, e.processEvent(ctx, refA, allDay))
	require.Positive(t, cp.count(), "終日イベントでは GetCalendarTimezone が呼ばれる")
}

// ---- (a) mappings の blocker_event_id 一致で無視(ループ遮断・一次判定) ----

func TestProcessEvent_IgnoreOwnBlocker(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	// b 発のブロッカー blk1 が a 上に置かれている状態
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "b", OriginCalendar: "primary", OriginEventID: "ev-b",
		TargetAccount: "a", TargetCalendar: "primary",
		BlockerEventID: "blk1", IdempotencyKey: "k", TimeHash: "h",
		Status: store.StatusActive,
	}))

	ev := busyEvent("blk1") // a のフィードに自作ブロッカーが busy として現れた
	require.NoError(t, e.processEvent(ctx, refA, ev))

	require.Empty(t, f.Blockers(calBv))
	require.Empty(t, f.Blockers(calCv))
	cached, err := e.Store.GetEvent(refA, "blk1")
	require.NoError(t, err)
	require.Nil(t, cached) // キャッシュにも入れない
	m, err := e.Store.GetMapping("a", "primary", "blk1", "b")
	require.NoError(t, err)
	require.Nil(t, m) // 新たな mapping も作らない
}

// ---- (b) OriginTag 非空で無視(タグ二次判定) ----

func TestProcessEvent_IgnoreTaggedEvent(t *testing.T) {
	e, f := newTestEngine(t)
	ev := busyEvent("ev1")
	ev.OriginTag = "b:ev-b" // mappings に無くてもタグが読めれば遮断(DB 消失後の孤児等)
	require.NoError(t, e.processEvent(context.Background(), refA, ev))

	require.Empty(t, f.Blockers(calBv))
	require.Empty(t, f.Blockers(calCv))
	cached, err := e.Store.GetEvent(refA, "ev1")
	require.NoError(t, err)
	require.Nil(t, cached)
}

// ---- (c) 削除通知 + origin 登録あり ----

func TestProcessEvent_DeleteNotification(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	require.NoError(t, e.processEvent(ctx, refA, busyEvent("ev1")))
	require.Len(t, f.Blockers(calBv), 1)
	require.Len(t, f.Blockers(calCv), 1)

	// 削除通知は id しか含まれない前提(仕様6.1)
	del := model.NormalizedEvent{ID: "ev1", Deleted: true}
	require.NoError(t, e.processEvent(ctx, refA, del))

	require.Empty(t, f.Blockers(calBv)) // 全ターゲットのブロッカー削除
	require.Empty(t, f.Blockers(calCv))
	for _, acct := range []string{"b", "c"} {
		m, err := e.Store.GetMapping("a", "primary", "ev1", acct)
		require.NoError(t, err)
		require.Nil(t, m) // mappings 削除
	}
	cached, err := e.Store.GetEvent(refA, "ev1")
	require.NoError(t, err)
	require.Nil(t, cached) // events キャッシュ削除
}

// (c) 自作ブロッカーの削除通知は無視(復元はリコンサイルの責務)
func TestProcessEvent_DeleteOwnBlockerIsIgnored(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "b", OriginCalendar: "primary", OriginEventID: "ev-b",
		TargetAccount: "a", TargetCalendar: "primary",
		BlockerEventID: "blk1", IdempotencyKey: "k", TimeHash: "h",
		Status: store.StatusActive,
	}))

	del := model.NormalizedEvent{ID: "blk1", Deleted: true}
	require.NoError(t, e.processEvent(ctx, refA, del))

	// mapping は残る(リコンサイルが再作成できるように消さない)
	m, err := e.Store.GetMapping("b", "primary", "ev-b", "a")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Empty(t, f.Blockers(calBv))
	require.Empty(t, f.Blockers(calCv))
}

// ---- (d) 未知 ID の削除通知 → 無視(Graph の @removed ウィンドウ外ノイズ耐性) ----

func TestProcessEvent_DeleteUnknownIDIsNoop(t *testing.T) {
	e, f := newTestEngine(t)
	del := model.NormalizedEvent{ID: "never-seen", Deleted: true}
	require.NoError(t, e.processEvent(context.Background(), refA, del))
	require.Empty(t, f.Blockers(calBv))
	require.Empty(t, f.Blockers(calCv))
}

// ---- (e) ウィンドウ外 → 既存 mapping の削除 ----

func TestProcessEvent_OutOfWindowDeletesExistingMapping(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	require.NoError(t, e.processEvent(ctx, refA, ev))
	require.Len(t, f.Blockers(calBv), 1)

	// 予定がウィンドウ外(now+4ヶ月)へ移動した
	moved := ev
	moved.StartUTC = testNow.AddDate(0, 4, 0)
	moved.EndUTC = moved.StartUTC.Add(time.Hour)
	require.NoError(t, e.processEvent(ctx, refA, moved))

	require.Empty(t, f.Blockers(calBv))
	require.Empty(t, f.Blockers(calCv))
	for _, acct := range []string{"b", "c"} {
		m, err := e.Store.GetMapping("a", "primary", "ev1", acct)
		require.NoError(t, err)
		require.Nil(t, m)
	}
	// events キャッシュの掃除はリコンサイルの set-difference の責務(決定則3ではブロッカーのみ)
}

func TestProcessEvent_OutOfWindowWithoutMappingIsNoop(t *testing.T) {
	e, f := newTestEngine(t)
	ev := busyEvent("ev1")
	ev.StartUTC = testNow.AddDate(0, 4, 0)
	ev.EndUTC = ev.StartUTC.Add(time.Hour)
	require.NoError(t, e.processEvent(context.Background(), refA, ev))

	require.Empty(t, f.Blockers(calBv))
	require.Empty(t, f.Blockers(calCv))
	cached, err := e.Store.GetEvent(refA, "ev1")
	require.NoError(t, err)
	require.Nil(t, cached) // ウィンドウ外はキャッシュにも入れない
}

// ---- (f) 非 busy / 辞退 → 既存ブロッカー削除 + キャッシュ削除 ----

func TestProcessEvent_NotBlockableDeletesExisting(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(ev *model.NormalizedEvent)
	}{
		{"busyでなくなった", func(ev *model.NormalizedEvent) { ev.IsBusy = false }},
		{"辞退した", func(ev *model.NormalizedEvent) { ev.IsDeclined = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, f := newTestEngine(t)
			ctx := context.Background()
			ev := busyEvent("ev1")
			require.NoError(t, e.processEvent(ctx, refA, ev))
			require.Len(t, f.Blockers(calBv), 1)

			changed := ev
			tc.mutate(&changed)
			require.NoError(t, e.processEvent(ctx, refA, changed))

			require.Empty(t, f.Blockers(calBv))
			require.Empty(t, f.Blockers(calCv))
			for _, acct := range []string{"b", "c"} {
				m, err := e.Store.GetMapping("a", "primary", "ev1", acct)
				require.NoError(t, err)
				require.Nil(t, m)
			}
			cached, err := e.Store.GetEvent(refA, "ev1")
			require.NoError(t, err)
			require.Nil(t, cached) // 決定則4はキャッシュも削除する
		})
	}
}

// ---- (h) time_hash 変化 → UpdateBlocker(patch であって再作成ではない) ----

func TestProcessEvent_TimeChangeUpdatesBlocker(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	require.NoError(t, e.processEvent(ctx, refA, ev))
	origID := f.Blockers(calBv)[0].EventID

	moved := ev
	moved.EndUTC = ev.EndUTC.Add(30 * time.Minute)
	require.NoError(t, e.processEvent(ctx, refA, moved))

	wantHash := model.TimeHash(moved)
	blks := f.Blockers(calBv)
	require.Len(t, blks, 1)
	require.Equal(t, origID, blks[0].EventID) // 同一イベントの patch
	require.Equal(t, wantHash, blks[0].TimeHash)
	require.Len(t, f.Blockers(calCv), 1)
	require.Equal(t, wantHash, f.Blockers(calCv)[0].TimeHash)

	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Equal(t, wantHash, m.TimeHash) // mapping の time_hash も更新
	cached, err := e.Store.GetEvent(refA, "ev1")
	require.NoError(t, err)
	require.Equal(t, wantHash, model.TimeHash(*cached)) // events キャッシュも更新
}

// ---- (i) time_hash 一致 → プロバイダ呼び出しなし ----

func TestProcessEvent_UnchangedMakesNoProviderCalls(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	require.NoError(t, e.processEvent(ctx, refA, ev))
	blks := f.Blockers(calBv)
	require.Len(t, blks, 1)

	// fake 上の記録を改竄しておく(SeedBlocker は EventID で upsert)。
	// エンジンが Update/Delete を呼べば改竄値は上書き・消滅するので、
	// 残っていれば「プロバイダ呼び出しなし」を証明できる。
	f.SeedBlocker(calBv, model.BlockerRecord{
		EventID: blks[0].EventID, OriginTag: blks[0].OriginTag, TimeHash: "tampered",
	})

	require.NoError(t, e.processEvent(ctx, refA, ev)) // 同一内容の再受信

	after := f.Blockers(calBv)
	require.Len(t, after, 1)
	require.Equal(t, "tampered", after[0].TimeHash) // 改竄が残っている = 未呼び出し
	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Equal(t, store.StatusActive, m.Status)
}
