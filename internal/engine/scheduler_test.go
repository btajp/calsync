package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/work-a-co/calsync/internal/config"
	"github.com/work-a-co/calsync/internal/model"
	"github.com/work-a-co/calsync/internal/provider"
	"github.com/work-a-co/calsync/internal/provider/fake"
	"github.com/work-a-co/calsync/internal/store"
)

func TestNextReconcileAt(t *testing.T) {
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
			got := nextReconcileAt(tt.now, tt.hhmm)
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
	f.SetFullState(refB, []model.NormalizedEvent{schedEvent("evB", now.Add(2 * time.Hour))})

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
