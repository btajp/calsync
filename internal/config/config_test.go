package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calsync.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

const minimalYAML = `
accounts:
  - id: personal
    provider: google
    email: user@gmail.com
`

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string                        // 空なら成功を期待。非空なら err.Error() に含まれる文字列
		check   func(t *testing.T, c *Config) // 成功時の追加検証
	}{
		{
			name: "defaults are applied for a minimal config",
			yaml: minimalYAML,
			check: func(t *testing.T, c *Config) {
				require.Equal(t, time.Minute, c.PollInterval)
				require.Equal(t, 3, c.SyncWindowMonths)
				require.Equal(t, 0, c.SyncWindowDays)
				require.Equal(t, "予定あり", c.BlockerTitle)
				require.Equal(t, "04:00", c.ReconcileAt)
				require.True(t, c.DedupeSameMeeting)
				require.Equal(t, []string{"busy", "oof", "tentative"}, c.BusyShowAs)
				require.Len(t, c.Accounts, 1)
				require.Equal(t, "personal", c.Accounts[0].ID)
				require.Equal(t, "google", c.Accounts[0].Provider)
				require.Equal(t, "user@gmail.com", c.Accounts[0].Email)
				require.Equal(t, []string{"primary"}, c.Accounts[0].Calendars)
				require.Equal(t, "primary", c.Accounts[0].BlockerCalendar)
			},
		},
		{
			name: "explicit values override defaults",
			yaml: `
poll_interval: 5m
sync_window: 90d
blocker_title: Busy
reconcile_at: "03:30"
dedupe_same_meeting: false
busy_show_as: [busy, oof]
providers:
  google:
    credentials_file: /data/google-client.json
  microsoft:
    client_id: 00000000-1111-2222-3333-444444444444
accounts:
  - id: personal
    provider: google
    email: user@gmail.com
    calendars: [primary, team@group.calendar.google.com]
    blocker_calendar: blockers@group.calendar.google.com
  - id: work-ms
    provider: microsoft
    email: user@example365.co.jp
`,
			check: func(t *testing.T, c *Config) {
				require.Equal(t, 5*time.Minute, c.PollInterval)
				require.Equal(t, 0, c.SyncWindowMonths)
				require.Equal(t, 90, c.SyncWindowDays)
				require.Equal(t, "Busy", c.BlockerTitle)
				require.Equal(t, "03:30", c.ReconcileAt)
				require.False(t, c.DedupeSameMeeting)
				require.Equal(t, []string{"busy", "oof"}, c.BusyShowAs)
				require.Equal(t, "/data/google-client.json", c.Providers.Google.CredentialsFile)
				require.Equal(t, "00000000-1111-2222-3333-444444444444", c.Providers.Microsoft.ClientID)
				require.Len(t, c.Accounts, 2)
				require.Equal(t, []string{"primary", "team@group.calendar.google.com"}, c.Accounts[0].Calendars)
				require.Equal(t, "blockers@group.calendar.google.com", c.Accounts[0].BlockerCalendar)
				require.Equal(t, []string{"primary"}, c.Accounts[1].Calendars)
				require.Equal(t, "primary", c.Accounts[1].BlockerCalendar)
			},
		},
		{
			name: "sync_window in months",
			yaml: "sync_window: 6mo\n" + minimalYAML,
			check: func(t *testing.T, c *Config) {
				require.Equal(t, 6, c.SyncWindowMonths)
				require.Equal(t, 0, c.SyncWindowDays)
			},
		},
		{
			name:    "invalid sync_window unit is rejected",
			yaml:    "sync_window: 3w\n" + minimalYAML,
			wantErr: "invalid sync_window",
		},
		{
			name:    "invalid poll_interval is rejected",
			yaml:    "poll_interval: fast\n" + minimalYAML,
			wantErr: "invalid poll_interval",
		},
		{
			name:    "invalid reconcile_at is rejected",
			yaml:    "reconcile_at: \"25:99\"\n" + minimalYAML,
			wantErr: "invalid reconcile_at",
		},
		{
			name: "duplicate account id is rejected",
			yaml: `
accounts:
  - id: personal
    provider: google
    email: a@gmail.com
  - id: personal
    provider: microsoft
    email: b@example.com
`,
			wantErr: `duplicate account id "personal"`,
		},
		{
			name: "missing account id is rejected",
			yaml: `
accounts:
  - provider: google
    email: a@gmail.com
`,
			wantErr: "id is required",
		},
		{
			// OriginTag は "<account_id>:<event_id>" 形式で、parseOriginTag は最初の
			// ":" で切る(engine/reconcile.go)。account id に ":" を許すと adoption が
			// タグを誤パースし、正規ブロッカーを孤児と誤認して削除しうる
			// (最終ホールブランチレビュー所見3)。
			name: "account id containing a colon is rejected",
			yaml: `
accounts:
  - id: "work:primary"
    provider: google
    email: a@gmail.com
`,
			wantErr: `id must not contain ":"`,
		},
		{
			// busy_show_as は Graph の freeBusyStatus 列挙値のみ許容する
			// (タイポを黙って「busy 扱いされない値」として吸収しない)
			name:    "invalid busy_show_as value is rejected",
			yaml:    "busy_show_as: [busy, ooof]\n" + minimalYAML,
			wantErr: "invalid busy_show_as",
		},
		{
			// 大文字小文字は Graph の camelCase 表記に厳密一致(WorkingElsewhere は不可)
			name:    "busy_show_as is case-sensitive",
			yaml:    "busy_show_as: [Busy]\n" + minimalYAML,
			wantErr: "invalid busy_show_as",
		},
		{
			name: "all valid busy_show_as values are accepted",
			yaml: "busy_show_as: [free, tentative, busy, oof, workingElsewhere, unknown]\n" + minimalYAML,
			check: func(t *testing.T, c *Config) {
				require.Equal(t, []string{"free", "tentative", "busy", "oof", "workingElsewhere", "unknown"}, c.BusyShowAs)
			},
		},
		{
			name: "unsupported provider is rejected",
			yaml: `
accounts:
  - id: icloud
    provider: apple
    email: a@icloud.com
`,
			wantErr: `unsupported provider "apple"`,
		},
		{
			name: "microsoft non-primary calendars are rejected (v1 constraint)",
			yaml: `
accounts:
  - id: work-ms
    provider: microsoft
    email: a@example.com
    calendars: [primary, second]
`,
			wantErr: "microsoft supports only the primary calendar",
		},
		{
			name: "microsoft non-primary blocker_calendar is rejected (v1 constraint)",
			yaml: `
accounts:
  - id: work-ms
    provider: microsoft
    email: a@example.com
    blocker_calendar: second
`,
			wantErr: "microsoft supports only the primary calendar",
		},
		{
			name:    "unknown top-level key is rejected by KnownFields",
			yaml:    "pol_interval: 1m\n" + minimalYAML,
			wantErr: "field pol_interval not found",
		},
		{
			name: "unknown account key is rejected by KnownFields",
			yaml: `
accounts:
  - id: personal
    provider: google
    email: a@gmail.com
    callendars: [primary]
`,
			wantErr: "field callendars not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			tt.check(t, cfg)
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err)
}

func TestWindowFrom(t *testing.T) {
	now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		cfg     Config
		wantEnd time.Time
	}{
		{
			name:    "months uses AddDate(0, mo, 0)",
			cfg:     Config{SyncWindowMonths: 3},
			wantEnd: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC),
		},
		{
			name:    "days uses AddDate(0, 0, d)",
			cfg:     Config{SyncWindowDays: 90},
			wantEnd: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.cfg.WindowFrom(now)
			require.Equal(t, now, w.Start)
			require.Equal(t, tt.wantEnd, w.End)
		})
	}
}

func TestTargetsOfAndAccountByID(t *testing.T) {
	src := `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: microsoft
    email: b@example.com
  - id: c
    provider: google
    email: c@gmail.com
`
	cfg, err := Load(writeConfig(t, src))
	require.NoError(t, err)

	targets := cfg.TargetsOf("b")
	ids := make([]string, 0, len(targets))
	for _, a := range targets {
		ids = append(ids, a.ID)
	}
	require.Equal(t, []string{"a", "c"}, ids, "TargetsOf must return all accounts except the origin")

	acct := cfg.AccountByID("b")
	require.NotNil(t, acct)
	require.Equal(t, "microsoft", acct.Provider)
	require.Nil(t, cfg.AccountByID("missing"))
}
