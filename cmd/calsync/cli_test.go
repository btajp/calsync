package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/store"
)

func TestFindOrphanAccounts(t *testing.T) {
	tests := []struct {
		name   string
		cfgIDs []string
		dbIDs  []string
		want   []string
	}{
		{"no orphans", []string{"a", "b"}, []string{"a", "b"}, nil},
		{"one orphan", []string{"a"}, []string{"a", "ghost"}, []string{"ghost"}},
		{"duplicates collapsed and sorted", []string{"a"}, []string{"z", "ghost", "z"}, []string{"ghost", "z"}},
		{"empty db", []string{"a"}, nil, nil},
		{"empty config means all orphans", nil, []string{"a"}, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, findOrphanAccounts(tt.cfgIDs, tt.dbIDs))
		})
	}
}

func TestRenderStatus(t *testing.T) {
	// 一時 store に行を入れ、ListCalendars の実データで表出力を検証する
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	defer st.Close()

	refA := model.CalendarRef{AccountID: "personal", CalendarID: "primary"}
	refB := model.CalendarRef{AccountID: "work-ms", CalendarID: "primary"}
	require.NoError(t, st.UpsertCalendar(refA))
	require.NoError(t, st.UpsertCalendar(refB))
	require.NoError(t, st.SetCalendarError(refB, "reauth_required"))

	states, err := st.ListCalendars()
	require.NoError(t, err)
	got := renderStatus(states)

	require.Contains(t, got, "ACCOUNT")
	require.Contains(t, got, "CALENDAR")
	require.Contains(t, got, "LAST SYNC")
	require.Contains(t, got, "STATUS")
	// 未同期・エラーなしの行: LAST SYNC は "-"、STATUS は "ok"
	require.Regexp(t, `personal\s+primary\s+-\s+ok`, got)
	// エラーありの行: STATUS にエラー文字列が出る
	require.Regexp(t, `work-ms\s+primary\s+\S+\s+reauth_required`, got)
}

func TestRunDoctor(t *testing.T) {
	dir := t.TempDir()
	tokens := &auth.TokenStore{Dir: dir}
	require.NoError(t, tokens.Save("a", &oauth2.Token{AccessToken: "at", RefreshToken: "rt"}))
	require.NoError(t, tokens.Save("b", &oauth2.Token{AccessToken: "at", RefreshToken: "rt"}))
	// アカウント c はトークン未作成のまま

	// DB に YAML に存在しない孤児アカウント ghost の行を作る
	st, err := store.Open(dir)
	require.NoError(t, err)
	require.NoError(t, st.UpsertCalendar(model.CalendarRef{AccountID: "ghost", CalendarID: "primary"}))
	require.NoError(t, st.Close()) // 書き込み用ハンドルは閉じる(runDoctor は OpenReadOnly で開く)

	cfg := &config.Config{Accounts: []config.Account{
		{ID: "a", Provider: "google", Calendars: []string{"primary"}},
		{ID: "b", Provider: "microsoft", Calendars: []string{"primary"}},
		{ID: "c", Provider: "google", Calendars: []string{"primary"}},
	}}
	// GetCalendarTimezone 疎通の差し替え: a は成功、b は失敗
	probe := func(ctx context.Context, acct config.Account) error {
		if acct.ID == "b" {
			return errors.New("graph unreachable")
		}
		return nil
	}

	var out bytes.Buffer
	err = runDoctor(context.Background(), cfg, dir, probe, &out, "calsync.yaml")
	require.Error(t, err) // b の疎通失敗 + c のトークン欠如 + ghost 孤児

	s := out.String()
	require.Contains(t, s, "account a: token ok")
	require.Contains(t, s, "account a: API check ok")
	require.Contains(t, s, "account b: token ok")
	require.Contains(t, s, "account b: API check FAILED")
	require.Contains(t, s, "graph unreachable")
	require.Contains(t, s, "account c: token MISSING")
	require.Contains(t, s, "calsync auth add c")
	require.Contains(t, s, "WARNING: account ghost")
	require.Contains(t, s, "calsync accounts remove ghost")
}

func TestRunDoctorAllOK(t *testing.T) {
	dir := t.TempDir()
	tokens := &auth.TokenStore{Dir: dir}
	require.NoError(t, tokens.Save("a", &oauth2.Token{AccessToken: "at", RefreshToken: "rt"}))
	cfg := &config.Config{Accounts: []config.Account{
		{ID: "a", Provider: "google", Calendars: []string{"primary"}},
	}}
	probe := func(ctx context.Context, acct config.Account) error { return nil }

	var out bytes.Buffer
	require.NoError(t, runDoctor(context.Background(), cfg, dir, probe, &out, "calsync.yaml"))
	require.Contains(t, out.String(), "all checks passed")
}

// Slack 設定があるのにトークン環境変数が空なら、store を開く前に fail fast する
// (スペック 9 章。status/doctor は環境変数なしで動くため run のみで検証する)。
func TestRunFailsFastWhenSlackTokenMissing(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "calsync.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
notifications:
  slack:
    bot_token_env: CALSYNC_TEST_SLACK_TOKEN
    channel: "C123"
    morning_digest: "07:30"
accounts:
  - id: a
    provider: google
`), 0o600))
	t.Setenv("CALSYNC_TEST_SLACK_TOKEN", "")
	rootCmd.SetArgs([]string{"run", "--config", cfgPath, "--data", t.TempDir()})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	err := rootCmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "CALSYNC_TEST_SLACK_TOKEN")
}

func TestOauthConfigForMicrosoftUsesLocalhostRedirect(t *testing.T) {
	cfg := &config.Config{}
	cfg.Providers.Microsoft.ClientID = "client-id"
	acct := config.Account{ID: "outlook", Provider: "microsoft"}

	oc, err := oauthConfigFor(cfg, acct)
	require.NoError(t, err)
	// MSA(login.live.com)はポートを無視するがパスは照合する(実測 2026-07-03)。
	// アプリ登録 http://localhost に合わせ「localhost・パスなし」の形にする。
	require.Equal(t, "http://localhost", oc.RedirectURL)
}
