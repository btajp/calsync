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
	"github.com/btajp/calsync/internal/provider"
	"github.com/btajp/calsync/internal/provider/fake"
	"github.com/btajp/calsync/internal/store"
)

func TestNextDailyAt(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	tests := []struct {
		name string
		now  time.Time
		hhmm string
		want time.Time
	}{
		{
			name: "future same day",
			now:  time.Date(2026, 7, 3, 10, 0, 0, 0, jst),
			hhmm: "14:30",
			want: time.Date(2026, 7, 3, 14, 30, 0, 0, jst),
		},
		{
			name: "past time rolls to next day",
			now:  time.Date(2026, 7, 3, 10, 0, 0, 0, jst),
			hhmm: "04:00",
			want: time.Date(2026, 7, 4, 4, 0, 0, 0, jst),
		},
		{
			name: "exactly now rolls to next day",
			now:  time.Date(2026, 7, 3, 4, 0, 0, 0, jst),
			hhmm: "04:00",
			want: time.Date(2026, 7, 4, 4, 0, 0, 0, jst),
		},
		{
			name: "one minute before stays same day",
			now:  time.Date(2026, 7, 3, 3, 59, 0, 0, jst),
			hhmm: "04:00",
			want: time.Date(2026, 7, 3, 4, 0, 0, 0, jst),
		},
		{
			name: "invalid hhmm falls back to 04:00",
			now:  time.Date(2026, 7, 3, 10, 0, 0, 0, jst),
			hhmm: "bogus",
			want: time.Date(2026, 7, 4, 4, 0, 0, 0, jst),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextDailyAt(tt.now, tt.hhmm)
			require.True(t, got.Equal(tt.want), "got %v want %v", got, tt.want)
		})
	}
}

// schedEvent は scheduler テスト専用の busy イベント生成ヘルパー
// (engine_test.go 等の既存ヘルパーと名前が衝突しないよう sched プレフィクスを付ける)。
func schedEvent(id string, start time.Time) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID:       id,
		ICalUID:  id + "@test",
		StartUTC: start,
		EndUTC:   start.Add(time.Hour),
		IsBusy:   true,
	}
}

func TestRunSkipsReauthAccountAndContinuesOthers(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	defer st.Close()

	refA := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	refB := model.CalendarRef{AccountID: "b", CalendarID: "primary"}
	require.NoError(t, st.UpsertCalendar(refA))
	require.NoError(t, st.UpsertCalendar(refB))

	f := fake.New()
	f.SetTimezone(refA, "UTC")
	f.SetTimezone(refB, "UTC")

	now := time.Now().UTC()
	// a: 初回 Changes が ErrAuthExpired を返す。a は以後スキップされるため、
	// この full state(evA)が b へ配布されてはならない。
	f.SetFullState(refA, []model.NormalizedEvent{schedEvent("evA", now.Add(time.Hour))})
	f.FailNext(refA, provider.ErrAuthExpired)
	// b: busy 予定 evB → ターゲット a にブロッカーが立つはず(b の同期は継続する証拠)。
	f.SetFullState(refB, []model.NormalizedEvent{schedEvent("evB", now.Add(2*time.Hour))})

	cfg := &config.Config{
		PollInterval:      10 * time.Millisecond,
		SyncWindowMonths:  3,
		BlockerTitle:      "予定あり",
		ReconcileAt:       time.Now().Add(12 * time.Hour).Format("15:04"), // テスト中に発火しない
		DedupeSameMeeting: true,
		Accounts: []config.Account{
			{ID: "a", Provider: "google", Calendars: []string{"primary"}, BlockerCalendar: "primary"},
			{ID: "b", Provider: "google", Calendars: []string{"primary"}, BlockerCalendar: "primary"},
		},
	}
	e := &Engine{
		Store:     st,
		Providers: map[string]provider.Provider{"a": f, "b": f},
		Cfg:       cfg,
		Now:       time.Now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	// b の予定 evB が a にブロッカーとして配布されるまで待つ
	require.Eventually(t, func() bool {
		return len(f.Blockers(refA)) == 1
	}, 3*time.Second, 10*time.Millisecond)

	// 数ティック余分に回し、a がスキップされ続けることを確認する猶予を与える
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err) // ctx キャンセルは nil 終了
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	// a は reauth_required でスキップされた: もしスキップされていなければ
	// 2 ティック目以降で a の full state(evA)が同期され b にブロッカーが立つ
	require.Empty(t, f.Blockers(refB), "account a must be skipped after ErrAuthExpired")

	// b → a のブロッカーは 1 件で OriginTag は b:evB
	blockers := f.Blockers(refA)
	require.Len(t, blockers, 1)
	require.Equal(t, "b:evB", blockers[0].OriginTag)

	// SetCalendarError で reauth_required が記録されている
	calA, err := st.GetCalendar(refA)
	require.NoError(t, err)
	require.NotNil(t, calA)
	require.Contains(t, calA.LastError, "reauth_required")
}

// authFailingProvider はブロッカー書き込み系呼び出しを fail フラグが立っている間
// provider.ErrAuthExpired で失敗させるラッパー(「ターゲットとしての認証失効」を
// 再現する。Changes は素通しなので、失効検出が origin の同期経由であることを分離できる)。
type authFailingProvider struct {
	provider.Provider
	mu   sync.Mutex
	fail bool
}

func (p *authFailingProvider) setFail(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = v
}

func (p *authFailingProvider) failing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fail
}

func (p *authFailingProvider) authErr() error {
	return fmt.Errorf("status 401: %w", provider.ErrAuthExpired)
}

func (p *authFailingProvider) CreateBlocker(ctx context.Context, cal model.CalendarRef, b model.Blocker, idemKey string) (string, error) {
	if p.failing() {
		return "", p.authErr()
	}
	return p.Provider.CreateBlocker(ctx, cal, b, idemKey)
}

func (p *authFailingProvider) UpdateBlocker(ctx context.Context, cal model.CalendarRef, eventID string, b model.Blocker) error {
	if p.failing() {
		return p.authErr()
	}
	return p.Provider.UpdateBlocker(ctx, cal, eventID, b)
}

func (p *authFailingProvider) DeleteBlocker(ctx context.Context, cal model.CalendarRef, eventID string) error {
	if p.failing() {
		return p.authErr()
	}
	return p.Provider.DeleteBlocker(ctx, cal, eventID)
}

func (p *authFailingProvider) GetCalendarTimezone(ctx context.Context, cal model.CalendarRef) (string, error) {
	if p.failing() {
		return "", p.authErr()
	}
	return p.Provider.GetCalendarTimezone(ctx, cal)
}

// TestTickAttributesTargetAuthExpiryToTargetAccount は仕様9.3の帰属を検証する:
// origin A の同期中にターゲット B への書き込みが認証失効で失敗しても、
// (a) 他ターゲット C への配布は継続し、(b) reauth_required は B に帰属して
// A には帰属せず、(c) A のカーソルは前進し(同期成功扱い)、
// (d) B の復帰後は Reconcile でバックフィルされる。
// 修正前は B 由来の ErrAuthExpired が errors.Is で A に誤帰属し、A の同期が
// 全体停止していた(最終ホールブランチレビュー追補 Issue 3)。
func TestTickAttributesTargetAuthExpiryToTargetAccount(t *testing.T) {
	e, f := newTestEngine(t) // a(origin)/ b・c(target)。TZ はキャッシュ済み
	ctx := context.Background()

	bp := &authFailingProvider{Provider: f, fail: true}
	e.Providers["b"] = bp

	f.SetFullState(refA, []model.NormalizedEvent{busyEvent("ev1")})

	reauth := map[string]bool{}
	e.tick(ctx, reauth, map[model.CalendarRef]int{})

	// (a) 正常ターゲット C にはブロッカーが作成される(B のみスキップ)
	require.Len(t, f.Blockers(calCv), 1, "healthy target c must still receive the blocker")
	require.Empty(t, f.Blockers(calBv))

	// (b) reauth_required は B に帰属し、A には帰属しない
	require.True(t, reauth["b"], "expired target b must enter the reauth set")
	require.False(t, reauth["a"], "origin a must not be blamed for b's expiry")
	stB, err := e.Store.GetCalendar(calBv)
	require.NoError(t, err)
	require.Contains(t, stB.LastError, "reauth_required")
	require.Contains(t, stB.LastError, "calsync auth add b")
	stA, err := e.Store.GetCalendar(refA)
	require.NoError(t, err)
	require.Empty(t, stA.LastError, "origin a counts as a successful sync")

	// (c) A のカーソルは前進する(ターゲット失効だけでは同期を失敗扱いにしない)
	require.Equal(t, "c1", stA.Cursor)
	require.False(t, stA.LastSyncedAt.IsZero())

	// B への intent は pending として残る(復帰後のバックフィルの足がかり)
	mB, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, mB)
	require.Equal(t, store.StatusPending, mB.Status)

	// (d) B の復帰後、Reconcile がバックフィルする
	bp.setFail(false)
	require.NoError(t, e.Reconcile(ctx))

	blks := f.Blockers(calBv)
	require.Len(t, blks, 1, "b must be backfilled after recovery")
	require.Equal(t, "a:ev1", blks[0].OriginTag)
	mB, err = e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.NotNil(t, mB)
	require.Equal(t, store.StatusActive, mB.Status)
	require.Equal(t, blks[0].EventID, mB.BlockerEventID)
}

// TestTickPersistsSuccessOnSyncCalendar は成功ティック後に last_error が空で
// last_synced_at が更新されていることを確認する(`calsync status` が参照する状態)。
func TestTickPersistsSuccessOnSyncCalendar(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	defer st.Close()

	ref := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	require.NoError(t, st.UpsertCalendar(ref))

	f := fake.New()
	f.SetTimezone(ref, "UTC")
	f.SetFullState(ref, nil) // イベントなし → 同期は成功で完走する

	cfg := &config.Config{
		SyncWindowMonths: 3,
		BlockerTitle:     "予定あり",
		Accounts: []config.Account{
			{ID: "a", Provider: "google", Calendars: []string{"primary"}, BlockerCalendar: "primary"},
		},
	}
	e := &Engine{
		Store:     st,
		Providers: map[string]provider.Provider{"a": f},
		Cfg:       cfg,
		Now:       time.Now,
	}

	e.tick(context.Background(), map[string]bool{}, map[model.CalendarRef]int{})

	got, err := st.GetCalendar(ref)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got.LastError)
	require.False(t, got.LastSyncedAt.IsZero())
}

// TestTickClearsPriorReauthErrorOnSuccess は reauth_required で汚れた last_error が
// 再認証成功後の成功ティックでクリアされることを確認する(復旧後も
// `calsync status` が古いエラーを表示し続ける問題の回帰テスト)。
func TestTickClearsPriorReauthErrorOnSuccess(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	defer st.Close()

	ref := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	require.NoError(t, st.UpsertCalendar(ref))
	require.NoError(t, st.SetCalendarError(ref, "reauth_required: run `calsync auth add a`"))

	f := fake.New()
	f.SetTimezone(ref, "UTC")
	f.SetFullState(ref, nil)

	cfg := &config.Config{
		SyncWindowMonths: 3,
		BlockerTitle:     "予定あり",
		Accounts: []config.Account{
			{ID: "a", Provider: "google", Calendars: []string{"primary"}, BlockerCalendar: "primary"},
		},
	}
	e := &Engine{
		Store:     st,
		Providers: map[string]provider.Provider{"a": f},
		Cfg:       cfg,
		Now:       time.Now,
	}

	e.tick(context.Background(), map[string]bool{}, map[model.CalendarRef]int{})

	got, err := st.GetCalendar(ref)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got.LastError, "reauth_required は再認証成功後にクリアされるべき")
}

// TestRunWithoutNowDoesNotPanic は Engine.Now を設定しないまま Run を実行しても
// nil パニックしないことを確認する(nil セーフな e.now() 経由であることの回帰テスト)。
func TestRunWithoutNowDoesNotPanic(t *testing.T) {
	cfg := &config.Config{
		PollInterval: 10 * time.Millisecond,
		ReconcileAt:  "04:00",
	}
	e := &Engine{Cfg: cfg} // Now は意図的に未設定(nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 即キャンセルして短時間で Run を終了させる

	var runErr error
	require.NotPanics(t, func() {
		runErr = e.Run(ctx)
	})
	require.NoError(t, runErr)
}

// TestTickForcesFullResyncAfterConsecutiveFailures は「毒されたカーソル」への防御を検証する。
// 実測(2026-07-04): Graph が壊れた deltaLink に対し 410/syncStateNotFound ではなく持続的な
// 504 を返し続け、カーソル失効処理が発火せず同じリンクを永遠にリトライした。同一カレンダーの
// 同期が閾値回数連続で失敗したらカーソル毒化とみなし、FullResync(新規カーソル確立)で自己回復する。
func TestTickForcesFullResyncAfterConsecutiveFailures(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()

	f.SetFullState(refA, []model.NormalizedEvent{busyEvent("ev1")})
	reauth := map[string]bool{}
	failures := map[model.CalendarRef]int{}

	// 1回目のティックで正常同期しカーソルを確立する
	e.tick(ctx, reauth, failures)
	before, err := e.Store.GetCalendar(refA)
	require.NoError(t, err)
	require.NotEmpty(t, before.Cursor)

	// 閾値未満の連続失敗ではエラー記録のみで再初期化しない
	transient := errors.New("graph delta: status 504")
	for i := 0; i < consecutiveFailureResyncThreshold-1; i++ {
		f.FailNext(refA, transient)
		e.tick(ctx, reauth, failures)
	}
	st, err := e.Store.GetCalendar(refA)
	require.NoError(t, err)
	require.Contains(t, st.LastError, "504", "below the threshold the error must only be recorded")
	require.Equal(t, before.Cursor, st.Cursor, "cursor must not change while below the threshold")

	// 閾値到達で FullResync が強制され、新規カーソルで自己回復する
	f.FailNext(refA, transient)
	e.tick(ctx, reauth, failures)
	after, err := e.Store.GetCalendar(refA)
	require.NoError(t, err)
	require.Empty(t, after.LastError, "forced full resync must clear the error state")
	require.NotEqual(t, before.Cursor, after.Cursor, "cursor must be re-established")
	require.Zero(t, failures[refA], "failure counter must reset after the forced resync")
}
