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
				require.Empty(t, c.Accounts[0].DigestCalendars)
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
			name: "digest_calendars are accepted for google accounts",
			yaml: `
accounts:
  - id: personal
    provider: google
    email: user@gmail.com
    calendars: [primary]
    digest_calendars: [gomi@group.calendar.google.com]
`,
			check: func(t *testing.T, c *Config) {
				require.Equal(t, []string{"gomi@group.calendar.google.com"}, c.Accounts[0].DigestCalendars)
			},
		},
		{
			name: "digest_calendars are rejected for microsoft accounts (v1 constraint)",
			yaml: `
accounts:
  - id: work-ms
    provider: microsoft
    email: a@example.com
    digest_calendars: [second]
`,
			wantErr: "microsoft supports only the primary calendar",
		},
		{
			name: "digest_calendars duplicating calendars is rejected",
			yaml: `
accounts:
  - id: personal
    provider: google
    email: user@gmail.com
    calendars: [primary, team@group.calendar.google.com]
    digest_calendars: [team@group.calendar.google.com]
`,
			wantErr: "duplicates calendars",
		},
		{
			name: "duplicate entries within digest_calendars are rejected",
			yaml: `
accounts:
  - id: personal
    provider: google
    email: user@gmail.com
    digest_calendars: [gomi@x, gomi@x]
`,
			wantErr: "duplicate digest_calendars entry",
		},
		{
			name: "empty string in digest_calendars is rejected",
			yaml: `
accounts:
  - id: personal
    provider: google
    email: user@gmail.com
    digest_calendars: [""]
`,
			wantErr: "digest_calendars entries must not be empty",
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
		{
			name: "slack notifications parsed with defaults",
			yaml: minimalYAML + `
notifications:
  slack:
    channel: "C0123"
    morning_digest: "07:30"
    remind_before: 10m
`,
			check: func(t *testing.T, c *Config) {
				sc := c.Notifications.Slack
				require.NotNil(t, sc)
				require.Equal(t, "SLACK_BOT_TOKEN", sc.BotTokenEnv)
				require.Equal(t, "C0123", sc.Channel)
				require.Equal(t, "07:30", sc.MorningDigest)
				require.Equal(t, 10*time.Minute, sc.RemindBefore)
			},
		},
		{
			name: "slack custom bot_token_env and digest-only",
			yaml: minimalYAML + `
notifications:
  slack:
    bot_token_env: MY_SLACK_TOKEN
    channel: "U0456"
    morning_digest: "06:00"
`,
			check: func(t *testing.T, c *Config) {
				sc := c.Notifications.Slack
				require.NotNil(t, sc)
				require.Equal(t, "MY_SLACK_TOKEN", sc.BotTokenEnv)
				require.Equal(t, time.Duration(0), sc.RemindBefore)
			},
		},
		{
			name: "no notifications section means disabled",
			yaml: minimalYAML,
			check: func(t *testing.T, c *Config) {
				require.Nil(t, c.Notifications.Slack)
			},
		},
		{
			name:    "slack requires channel",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    morning_digest: \"07:30\"\n",
			wantErr: "notifications.slack.channel is required",
		},
		{
			name:    "slack rejects invalid morning_digest",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n    morning_digest: \"7時半\"\n",
			wantErr: "invalid notifications.slack.morning_digest",
		},
		{
			name:    "slack rejects non-positive remind_before",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n    remind_before: -5m\n",
			wantErr: "invalid notifications.slack.remind_before",
		},
		{
			name:    "slack rejects remind_before shorter than poll_interval",
			yaml:    "poll_interval: 5m\n" + minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n    remind_before: 1m\n",
			wantErr: "must be >= poll_interval",
		},
		{
			name:    "slack requires at least one of digest or remind",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n",
			wantErr: "set at least one of morning_digest or remind_before",
		},
		{
			name:    "unknown notification keys are rejected",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n    morning_digest: \"07:30\"\n    webhook_url: \"https://x\"\n",
			wantErr: "webhook_url",
		},
		{
			name: "detail_sync pairs are parsed and normalized",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title, description]
  - from: b
    to: a
    fields: [title]
`,
			check: func(t *testing.T, c *Config) {
				require.Equal(t, []DetailSyncPair{
					{From: "a", To: "b", Title: true, Description: true, Visibility: "private"},
					{From: "b", To: "a", Title: true, Visibility: "private"},
				}, c.DetailSync)
			},
		},
		{
			name: "detail_sync unknown account is rejected",
			yaml: minimalYAML + `
detail_sync:
  - from: typo
    to: personal
    fields: [title]
`,
			wantErr: `detail_sync[0]: unknown account "typo"`,
		},
		{
			name: "detail_sync from==to is rejected",
			yaml: minimalYAML + `
detail_sync:
  - from: personal
    to: personal
    fields: [title]
`,
			wantErr: "from and to must differ",
		},
		{
			name: "detail_sync duplicate pair is rejected",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title]
  - from: a
    to: b
    fields: [description]
`,
			wantErr: `duplicate pair "a" => "b"`,
		},
		{
			name: "detail_sync invalid field is rejected",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [location]
`,
			wantErr: `invalid field "location" (want title or description)`,
		},
		{
			name: "detail_sync empty fields is rejected",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: []
`,
			wantErr: "fields must not be empty",
		},
		{
			name: "detail_sync duplicate field is rejected",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title, title]
`,
			wantErr: `duplicate field "title"`,
		},
		{
			name: "detail_sync unknown key is rejected by KnownFields",
			yaml: minimalYAML + `
detail_sync:
  - form: personal
    to: personal
    fields: [title]
`,
			wantErr: "field form not found",
		},
		{
			name: "detail_sync visibility parsed and normalized",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title]
    visibility: default
  - from: b
    to: a
    fields: [title]
`,
			check: func(t *testing.T, c *Config) {
				require.Equal(t, "default", c.DetailSync[0].Visibility)
				require.Equal(t, "private", c.DetailSync[1].Visibility, "未指定は private に正規化")
			},
		},
		{
			name: "detail_sync invalid visibility is rejected",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title]
    visibility: secret
`,
			wantErr: `invalid visibility "secret" (want private, default, or public)`,
		},
		{
			name: "detail_sync unknown visibility-like key is rejected by KnownFields",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title]
    visibilty: default
`,
			wantErr: "field visibilty not found",
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

func TestLoadShowOriginInDescription(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "calsync.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
providers:
  google:
    credentials_file: /data/g.json
accounts:
  - id: a
    provider: google
    email: a@example.com
    show_origin_in_description: true
  - id: b
    provider: google
    email: b@example.com
`), 0o600))
	cfg, err := Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Accounts[0].ShowOriginInDescription)
	require.False(t, cfg.Accounts[1].ShowOriginInDescription, "既定は false")
}

func TestDetailSyncFor(t *testing.T) {
	src := `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title]
`
	cfg, err := Load(writeConfig(t, src))
	require.NoError(t, err)

	p := cfg.DetailSyncFor("a", "b")
	require.NotNil(t, p)
	require.True(t, p.Title)
	require.False(t, p.Description)

	require.Nil(t, cfg.DetailSyncFor("b", "a"), "方向は一方通行(逆方向は別エントリ)")
	require.Nil(t, cfg.DetailSyncFor("a", "missing"))
}
