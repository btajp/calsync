package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
	"github.com/btajp/calsync/internal/provider/fake"
	"github.com/btajp/calsync/internal/store"
)

// recordingTokenDeleter は TokenDeleter のテストダブル。
type recordingTokenDeleter struct{ deleted []string }

func (r *recordingTokenDeleter) Delete(accountID string) error {
	r.deleted = append(r.deleted, accountID)
	return nil
}

// setupRemoveFixture は
//   - a 発の予定 evA -> b 上のブロッカー blk-on-b(mapping active)
//   - b 発の予定 evB -> a 上の受領ブロッカー blk-on-a(mapping active)
//   - a のイベントキャッシュ・カレンダー行
//
// を持つエンジンを作る。
func setupRemoveFixture(t *testing.T) (*Engine, *fake.Fake, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	f := fake.New()
	cfg := &config.Config{
		BlockerTitle: "予定あり",
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

	calA := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	calB := model.CalendarRef{AccountID: "b", CalendarID: "primary"}

	// (1) 用: a 発の配布済みブロッカー(b 上)
	f.SeedBlocker(calB, model.BlockerRecord{EventID: "blk-on-b", OriginTag: model.OriginTagOf("a", "evA"), TimeHash: "h1"})
	require.NoError(t, st.PutMapping(store.Mapping{
		OriginAccount: "a", OriginCalendar: "primary", OriginEventID: "evA",
		TargetAccount: "b", TargetCalendar: "primary",
		BlockerEventID: "blk-on-b", IdempotencyKey: "k1", TimeHash: "h1", Status: store.StatusActive,
	}))

	// (2) 用: a が受領しているブロッカー(a 上、origin は b)
	f.SeedBlocker(calA, model.BlockerRecord{EventID: "blk-on-a", OriginTag: model.OriginTagOf("b", "evB"), TimeHash: "h2"})
	require.NoError(t, st.PutMapping(store.Mapping{
		OriginAccount: "b", OriginCalendar: "primary", OriginEventID: "evB",
		TargetAccount: "a", TargetCalendar: "primary",
		BlockerEventID: "blk-on-a", IdempotencyKey: "k2", TimeHash: "h2", Status: store.StatusActive,
	}))

	// (3) 用: a のローカル状態
	require.NoError(t, st.UpsertCalendar(calA))
	require.NoError(t, st.UpsertCalendar(calB))
	require.NoError(t, st.UpsertEvent(calA, model.NormalizedEvent{
		ID: "evA", ICalUID: "evA@example.com",
		StartUTC: time.Now().UTC(), EndUTC: time.Now().Add(time.Hour).UTC(),
		IsBusy: true,
	}))
	return e, f, st
}

func TestRemoveAccountDeletesDistributedReceivedAndLocalState(t *testing.T) {
	e, f, st := setupRemoveFixture(t)
	tokens := &recordingTokenDeleter{}

	require.NoError(t, RemoveAccount(context.Background(), e, tokens, "a", false))

	calA := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	calB := model.CalendarRef{AccountID: "b", CalendarID: "primary"}

	// (1) b 上の配布済みブロッカーと mapping が消えている
	require.Empty(t, f.Blockers(calB))
	m, err := st.GetMapping("a", "primary", "evA", "b")
	require.NoError(t, err)
	require.Nil(t, m)

	// (2) a 上の受領ブロッカーと mapping が消えている
	require.Empty(t, f.Blockers(calA))
	m, err = st.GetMapping("b", "primary", "evB", "a")
	require.NoError(t, err)
	require.Nil(t, m)

	// (3) イベントキャッシュ・カレンダー行・トークンが消えている
	ids, err := st.ListEventIDs(calA)
	require.NoError(t, err)
	require.Empty(t, ids)
	cs, err := st.GetCalendar(calA)
	require.NoError(t, err)
	require.Nil(t, cs)
	require.Equal(t, []string{"a"}, tokens.deleted)

	// b のカレンダー行は残る
	csB, err := st.GetCalendar(calB)
	require.NoError(t, err)
	require.NotNil(t, csB)
}

func TestRemoveAccountProviderUnavailable(t *testing.T) {
	t.Run("force=false はエラーで中断し受領側を残す", func(t *testing.T) {
		e, _, st := setupRemoveFixture(t)
		delete(e.Providers, "a") // a の provider 構築不能(認証切れ等)を再現
		tokens := &recordingTokenDeleter{}

		err := RemoveAccount(context.Background(), e, tokens, "a", false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "--force")

		// (1) は b の provider が生きているので完了済み(部分進行。再実行で冪等に続行できる)
		m1, gerr := st.GetMapping("a", "primary", "evA", "b")
		require.NoError(t, gerr)
		require.Nil(t, m1)

		// (2) の受領 mapping は残り、ローカル状態・トークンも消えていない
		m2, gerr := st.GetMapping("b", "primary", "evB", "a")
		require.NoError(t, gerr)
		require.NotNil(t, m2)
		cs, gerr := st.GetCalendar(model.CalendarRef{AccountID: "a", CalendarID: "primary"})
		require.NoError(t, gerr)
		require.NotNil(t, cs)
		require.Empty(t, tokens.deleted)
	})

	t.Run("force=true はリモート削除をスキップして完走する", func(t *testing.T) {
		e, f, st := setupRemoveFixture(t)
		delete(e.Providers, "a")
		tokens := &recordingTokenDeleter{}

		require.NoError(t, RemoveAccount(context.Background(), e, tokens, "a", true))

		calA := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
		calB := model.CalendarRef{AccountID: "b", CalendarID: "primary"}

		// b 上の配布済みブロッカーは b の provider が生きているので消える
		require.Empty(t, f.Blockers(calB))
		// a 上の受領ブロッカーはリモートに残る(スキップされた)
		require.Len(t, f.Blockers(calA), 1)

		// ローカル状態は全て消える
		m, err := st.GetMapping("b", "primary", "evB", "a")
		require.NoError(t, err)
		require.Nil(t, m)
		cs, err := st.GetCalendar(calA)
		require.NoError(t, err)
		require.Nil(t, cs)
		require.Equal(t, []string{"a"}, tokens.deleted)
	})
}

// accounts remove がそのアカウントの reminders_sent も削除する(スペック 4.3)。
func TestRemoveAccountDeletesReminderRecords(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()
	start := e.now()
	require.NoError(t, e.Store.MarkReminderSent(refA, "ev1", "u@x", start, start))

	require.NoError(t, RemoveAccount(ctx, e, &recordingTokenDeleter{}, "a", true))

	sent, err := e.Store.WasReminderSent(refA, "ev1", start)
	require.NoError(t, err)
	require.False(t, sent)
}
