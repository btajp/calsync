package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/btajp/calsync/internal/model"
)

type Config struct {
	PollInterval      time.Duration // 既定 1m
	SyncWindowMonths  int           // "3mo" → 3。SyncWindowDays と排他
	SyncWindowDays    int           // "90d" → 90
	BlockerTitle      string        // 既定 "予定あり"
	ReconcileAt       string        // 既定 "04:00"(コンテナのローカルTZで解釈)
	DedupeSameMeeting bool          // 既定 true
	BusyShowAs        []string      // 既定 [busy, oof, tentative]
	Notifications     NotificationsConfig
	Providers         ProvidersConfig
	Accounts          []Account
	DetailSync        []DetailSyncPair
}

type ProvidersConfig struct {
	Google    struct{ CredentialsFile string } // GCP クライアントJSON のパス
	Microsoft struct{ ClientID string }        // Entra アプリの client_id
}

// NotificationsConfig は通知設定。Slack が nil なら通知機能は完全に無効
// (Engine.Notifier が注入されない。スペック 3 章)。
type NotificationsConfig struct {
	Slack *SlackConfig
}

// SlackConfig は Slack Bot 通知の設定(スペック 3 章)。
type SlackConfig struct {
	BotTokenEnv   string        // トークンを読む環境変数名。既定 "SLACK_BOT_TOKEN"
	Channel       string        // C…/G…: チャンネル / U…: DM(conversations.open で解決)
	MorningDigest string        // "HH:MM"(コンテナ TZ)。空ならダイジェスト無効
	RemindBefore  time.Duration // 0 ならリマインド無効
}

type Account struct {
	ID, Provider, Email string   // Provider は "google" | "microsoft"
	Calendars           []string // 既定 ["primary"]。microsoft は ["primary"] 以外エラー(v1制約)
	// DigestCalendars: ダイジェストのライブ取得にだけ参加するカレンダー ID。
	// 監視対象(Calendars)には含まれないため、tick の同期・カーソル・events キャッシュ・
	// 日次リコンサイル・ブロッカー配布の対象に構造的にならない。google のみ許容(v1制約)。
	DigestCalendars []string
	BlockerCalendar string // 既定 "primary"
	// ShowOriginInDescription: true のとき、このアカウントのカレンダーに作成される
	// ブロッカーの説明欄に元アカウントの ID(YAML の id。メールアドレスは含めない)を
	// 記載する。既定 false(完全匿名)。トグル変更は次回リコンサイルで既存分にも遡及する。
	ShowOriginInDescription bool `yaml:"show_origin_in_description"`
}

// DetailSyncPair は detail_sync の 1 エントリ(スペック 2026-07-15 §2, §12)。
// origin(From)→ target(To)アカウントの一方通行のペアで、指定したペアに限り
// ブロッカーのタイトル/説明を元イベントから転記する(既定は完全匿名のまま)。
// fields は検証時に bool へ正規化する(正規順 title → description はハッシュ側で担保)。
// Visibility は検証時に正規化済み("private" | "default" | "public"。未指定は "private")。
type DetailSyncPair struct {
	From, To           string
	Title, Description bool
	Visibility         string
}

// Raw は YAML の生の形。KnownFields(true) の照合対象になるため、
// 受理するキーはここに列挙されたものが全て。
type Raw struct {
	PollInterval      string           `yaml:"poll_interval,omitempty" json:"poll_interval,omitempty"`
	SyncWindow        string           `yaml:"sync_window,omitempty" json:"sync_window,omitempty"`
	BlockerTitle      string           `yaml:"blocker_title,omitempty" json:"blocker_title,omitempty"`
	ReconcileAt       string           `yaml:"reconcile_at,omitempty" json:"reconcile_at,omitempty"`
	DedupeSameMeeting *bool            `yaml:"dedupe_same_meeting,omitempty" json:"dedupe_same_meeting,omitempty"` // 未指定(nil)と false を区別する
	BusyShowAs        []string         `yaml:"busy_show_as,omitempty" json:"busy_show_as,omitempty"`
	Notifications     RawNotifications `yaml:"notifications,omitempty" json:"notifications,omitempty"`
	Providers         RawProviders     `yaml:"providers,omitempty" json:"providers,omitempty"`
	Accounts          []RawAccount     `yaml:"accounts,omitempty" json:"accounts,omitempty"`
	DetailSync        []RawDetailSync  `yaml:"detail_sync,omitempty" json:"detail_sync,omitempty"`
}

type RawNotifications struct {
	Slack *RawSlack `yaml:"slack,omitempty" json:"slack,omitempty"`
}

type RawSlack struct {
	BotTokenEnv   string `yaml:"bot_token_env,omitempty" json:"bot_token_env,omitempty"`
	Channel       string `yaml:"channel,omitempty" json:"channel,omitempty"`
	MorningDigest string `yaml:"morning_digest,omitempty" json:"morning_digest,omitempty"`
	RemindBefore  string `yaml:"remind_before,omitempty" json:"remind_before,omitempty"`
}

type RawProviders struct {
	Google    RawGoogleProvider    `yaml:"google,omitempty" json:"google,omitempty"`
	Microsoft RawMicrosoftProvider `yaml:"microsoft,omitempty" json:"microsoft,omitempty"`
}

type RawGoogleProvider struct {
	CredentialsFile string `yaml:"credentials_file,omitempty" json:"credentials_file,omitempty"`
}

type RawMicrosoftProvider struct {
	ClientID string `yaml:"client_id,omitempty" json:"client_id,omitempty"`
}

type RawAccount struct {
	ID                      string   `yaml:"id,omitempty" json:"id,omitempty"`
	Provider                string   `yaml:"provider,omitempty" json:"provider,omitempty"`
	Email                   string   `yaml:"email,omitempty" json:"email,omitempty"`
	Calendars               []string `yaml:"calendars,omitempty" json:"calendars,omitempty"`
	DigestCalendars         []string `yaml:"digest_calendars,omitempty" json:"digest_calendars,omitempty"`
	BlockerCalendar         string   `yaml:"blocker_calendar,omitempty" json:"blocker_calendar,omitempty"`
	ShowOriginInDescription bool     `yaml:"show_origin_in_description,omitempty" json:"show_origin_in_description,omitempty"`
}

type RawDetailSync struct {
	From       string   `yaml:"from,omitempty" json:"from,omitempty"`
	To         string   `yaml:"to,omitempty" json:"to,omitempty"`
	Fields     []string `yaml:"fields,omitempty" json:"fields,omitempty"`
	Visibility string   `yaml:"visibility,omitempty" json:"visibility,omitempty"`
}

var syncWindowRe = regexp.MustCompile(`^([0-9]+)(mo|d)$`)

// validShowAs は Graph の freeBusyStatus が取りうる値(busy_show_as の許容値)。
// 大文字小文字は Graph の応答表記(camelCase)に合わせて厳密比較する。
var validShowAs = map[string]bool{
	"free":             true,
	"tentative":        true,
	"busy":             true,
	"oof":              true,
	"workingElsewhere": true,
	"unknown":          true,
}

// Load は YAML 設定を読み込み、検証とデフォルト補完を行う。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(b, path)
}

// Parse は YAML バイト列を検証・デフォルト補完して Config にする。
// source はエラーメッセージに使う表示名(ファイルパス等)。
func Parse(data []byte, source string) (*Config, error) {
	var raw Raw
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // 未知キーはエラー(タイポの黙殺を防ぐ)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", source, err)
	}

	cfg := &Config{
		PollInterval:      time.Minute,
		SyncWindowMonths:  3,
		BlockerTitle:      "予定あり",
		ReconcileAt:       "04:00",
		DedupeSameMeeting: true,
		BusyShowAs:        []string{"busy", "oof", "tentative"},
	}
	cfg.Providers.Google.CredentialsFile = raw.Providers.Google.CredentialsFile
	cfg.Providers.Microsoft.ClientID = raw.Providers.Microsoft.ClientID

	if raw.PollInterval != "" {
		d, err := time.ParseDuration(raw.PollInterval)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("config: invalid poll_interval %q (want a positive Go duration such as \"1m\")", raw.PollInterval)
		}
		cfg.PollInterval = d
	}

	if raw.SyncWindow != "" {
		m := syncWindowRe.FindStringSubmatch(raw.SyncWindow)
		if m == nil {
			return nil, fmt.Errorf("config: invalid sync_window %q (want \"<n>mo\" or \"<n>d\", e.g. \"3mo\", \"90d\")", raw.SyncWindow)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("config: invalid sync_window %q (value must be a positive integer)", raw.SyncWindow)
		}
		if m[2] == "mo" {
			cfg.SyncWindowMonths, cfg.SyncWindowDays = n, 0
		} else {
			cfg.SyncWindowMonths, cfg.SyncWindowDays = 0, n
		}
	}

	if raw.BlockerTitle != "" {
		cfg.BlockerTitle = raw.BlockerTitle
	}
	if raw.ReconcileAt != "" {
		cfg.ReconcileAt = raw.ReconcileAt
	}
	if _, err := time.Parse("15:04", cfg.ReconcileAt); err != nil {
		return nil, fmt.Errorf("config: invalid reconcile_at %q (want \"HH:MM\", e.g. \"04:00\")", cfg.ReconcileAt)
	}
	if raw.DedupeSameMeeting != nil {
		cfg.DedupeSameMeeting = *raw.DedupeSameMeeting
	}
	if len(raw.BusyShowAs) > 0 {
		for _, v := range raw.BusyShowAs {
			if !validShowAs[v] {
				return nil, fmt.Errorf("config: invalid busy_show_as value %q (want one of free, tentative, busy, oof, workingElsewhere, unknown)", v)
			}
		}
		cfg.BusyShowAs = raw.BusyShowAs
	}

	if rs := raw.Notifications.Slack; rs != nil {
		sc := &SlackConfig{BotTokenEnv: rs.BotTokenEnv, Channel: rs.Channel, MorningDigest: rs.MorningDigest}
		if sc.BotTokenEnv == "" {
			sc.BotTokenEnv = "SLACK_BOT_TOKEN"
		}
		if sc.Channel == "" {
			return nil, fmt.Errorf("config: notifications.slack.channel is required")
		}
		if sc.MorningDigest != "" {
			if _, err := time.Parse("15:04", sc.MorningDigest); err != nil {
				return nil, fmt.Errorf("config: invalid notifications.slack.morning_digest %q (want \"HH:MM\", e.g. \"07:30\")", sc.MorningDigest)
			}
		}
		if rs.RemindBefore != "" {
			d, err := time.ParseDuration(rs.RemindBefore)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("config: invalid notifications.slack.remind_before %q (want a positive Go duration such as \"10m\")", rs.RemindBefore)
			}
			// リマインド判定は tick(poll_interval)毎のため、これを下回るとウィンドウが
			// tick 間隔より狭くなり発火保証がなくなる(スペック 3 章)
			if d < cfg.PollInterval {
				return nil, fmt.Errorf("config: notifications.slack.remind_before %q must be >= poll_interval %q (reminders are checked once per poll tick)", rs.RemindBefore, cfg.PollInterval)
			}
			sc.RemindBefore = d
		}
		if sc.MorningDigest == "" && sc.RemindBefore == 0 {
			return nil, fmt.Errorf("config: notifications.slack: set at least one of morning_digest or remind_before")
		}
		cfg.Notifications.Slack = sc
	}

	seen := make(map[string]bool, len(raw.Accounts))
	for i, ra := range raw.Accounts {
		a := Account{
			ID:                      ra.ID,
			Provider:                ra.Provider,
			Email:                   ra.Email,
			Calendars:               ra.Calendars,
			DigestCalendars:         ra.DigestCalendars,
			BlockerCalendar:         ra.BlockerCalendar,
			ShowOriginInDescription: ra.ShowOriginInDescription,
		}
		if a.ID == "" {
			return nil, fmt.Errorf("config: accounts[%d]: id is required", i)
		}
		if strings.Contains(a.ID, ":") {
			// OriginTag は "<account_id>:<event_id>" 形式で、parseOriginTag は最初の
			// ":" で切る(engine/reconcile.go)。account id に ":" を許すと adoption が
			// タグを誤パースし、正規ブロッカーを孤児と誤認して削除しうる。
			return nil, fmt.Errorf("config: account %q: id must not contain %q (reserved as the origin tag separator)", a.ID, ":")
		}
		if seen[a.ID] {
			return nil, fmt.Errorf("config: duplicate account id %q", a.ID)
		}
		seen[a.ID] = true
		if a.Provider != "google" && a.Provider != "microsoft" {
			return nil, fmt.Errorf("config: account %q: unsupported provider %q (want \"google\" or \"microsoft\")", a.ID, a.Provider)
		}
		if len(a.Calendars) == 0 {
			a.Calendars = []string{"primary"}
		}
		if a.BlockerCalendar == "" {
			a.BlockerCalendar = "primary"
		}
		if a.Provider == "microsoft" {
			for _, cal := range a.Calendars {
				if cal != "primary" {
					return nil, fmt.Errorf("config: account %q: microsoft supports only the primary calendar in v1 (got calendar %q)", a.ID, cal)
				}
			}
			if a.BlockerCalendar != "primary" {
				return nil, fmt.Errorf("config: account %q: microsoft supports only the primary calendar in v1 (got blocker_calendar %q)", a.ID, a.BlockerCalendar)
			}
			if len(a.DigestCalendars) > 0 {
				// v1 の Graph 実装は /me/calendarView 固定でプライマリ以外を取得できないため、
				// 既存の「microsoft は primary のみ」制約と同型でエラーにする(スペック 3 章)。
				return nil, fmt.Errorf("config: account %q: microsoft supports only the primary calendar in v1 (got digest_calendars %q)", a.ID, a.DigestCalendars)
			}
		}
		// digest_calendars 内の重複・空文字列、および calendars との重複を検証する
		// (二重取得・二重表示の防止。スペック 3 章)。microsoft は上で既に弾かれているため
		// ここに到達するのは google のみ。
		calSet := make(map[string]bool, len(a.Calendars))
		for _, cal := range a.Calendars {
			calSet[cal] = true
		}
		seenDigest := make(map[string]bool, len(a.DigestCalendars))
		for _, cal := range a.DigestCalendars {
			if cal == "" {
				return nil, fmt.Errorf("config: account %q: digest_calendars entries must not be empty", a.ID)
			}
			if calSet[cal] {
				return nil, fmt.Errorf("config: account %q: digest_calendars %q duplicates calendars", a.ID, cal)
			}
			if cal == a.BlockerCalendar {
				// blocker_calendar は受領ブロッカーの置き場。重複を許すと「通知専用
				// カレンダーにブロッカーは存在しない」前提(アンインストール手順等)が崩れる
				return nil, fmt.Errorf("config: account %q: digest_calendars %q duplicates blocker_calendar", a.ID, cal)
			}
			if seenDigest[cal] {
				return nil, fmt.Errorf("config: account %q: duplicate digest_calendars entry %q", a.ID, cal)
			}
			seenDigest[cal] = true
		}
		cfg.Accounts = append(cfg.Accounts, a)
	}

	// detail_sync の検証(スペック 2026-07-15 §2)。アカウント id の実在チェックが
	// 必要なため、アカウントを全件読み終えた後の後検証パスで行う。
	seenPair := make(map[string]bool, len(raw.DetailSync))
	for i, rd := range raw.DetailSync {
		for _, id := range []string{rd.From, rd.To} {
			if !seen[id] {
				return nil, fmt.Errorf("config: detail_sync[%d]: unknown account %q", i, id)
			}
		}
		if rd.From == rd.To {
			return nil, fmt.Errorf("config: detail_sync[%d]: from and to must differ (got %q)", i, rd.From)
		}
		// アカウント id は ":" を含まない(上で検証済み)ため区切りに使える
		key := rd.From + ":" + rd.To
		if seenPair[key] {
			return nil, fmt.Errorf("config: detail_sync[%d]: duplicate pair %q => %q", i, rd.From, rd.To)
		}
		seenPair[key] = true
		if len(rd.Fields) == 0 {
			return nil, fmt.Errorf("config: detail_sync[%d]: fields must not be empty", i)
		}
		p := DetailSyncPair{From: rd.From, To: rd.To}
		for _, fld := range rd.Fields {
			switch fld {
			case "title":
				if p.Title {
					return nil, fmt.Errorf("config: detail_sync[%d]: duplicate field %q", i, fld)
				}
				p.Title = true
			case "description":
				if p.Description {
					return nil, fmt.Errorf("config: detail_sync[%d]: duplicate field %q", i, fld)
				}
				p.Description = true
			default:
				return nil, fmt.Errorf("config: detail_sync[%d]: invalid field %q (want title or description)", i, fld)
			}
		}
		switch rd.Visibility {
		case "":
			p.Visibility = "private" // 未指定 = 従来どおり非公開(スペック §12.1)
		case "private", "default", "public":
			p.Visibility = rd.Visibility
		default:
			return nil, fmt.Errorf("config: detail_sync[%d]: invalid visibility %q (want private, default, or public)", i, rd.Visibility)
		}
		cfg.DetailSync = append(cfg.DetailSync, p)
	}

	return cfg, nil
}

// WindowFrom は now を起点とする同期ウィンドウを返す。
// months は AddDate(0, mo, 0)、days は AddDate(0, 0, d) で終端を計算する。
func (c *Config) WindowFrom(now time.Time) model.Window {
	end := now.AddDate(0, c.SyncWindowMonths, 0)
	if c.SyncWindowDays > 0 {
		end = now.AddDate(0, 0, c.SyncWindowDays)
	}
	return model.Window{Start: now, End: end}
}

// TargetsOf は origin 以外の全アカウント(= ブロッカー配布先)を返す。
func (c *Config) TargetsOf(originAccountID string) []Account {
	var out []Account
	for _, a := range c.Accounts {
		if a.ID != originAccountID {
			out = append(out, a)
		}
	}
	return out
}

// AccountByID は該当アカウントへのポインタを返す。無ければ nil。
func (c *Config) AccountByID(id string) *Account {
	for i := range c.Accounts {
		if c.Accounts[i].ID == id {
			return &c.Accounts[i]
		}
	}
	return nil
}

// DetailSyncFor は (origin, target) ペアの detail_sync エントリを返す。無ければ nil。
// 方向は一方通行(from => to)で、逆方向は別エントリ。
func (c *Config) DetailSyncFor(originID, targetID string) *DetailSyncPair {
	for i := range c.DetailSync {
		if c.DetailSync[i].From == originID && c.DetailSync[i].To == targetID {
			return &c.DetailSync[i]
		}
	}
	return nil
}
