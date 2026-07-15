package config

import (
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

// DetailSyncPair は detail_sync の 1 エントリ(スペック 2026-07-15 §2)。
// origin(From)→ target(To)アカウントの一方通行のペアで、指定したペアに限り
// ブロッカーのタイトル/説明を元イベントから転記する(既定は完全匿名のまま)。
// fields は検証時に bool へ正規化する(正規順 title → description はハッシュ側で担保)。
type DetailSyncPair struct {
	From, To           string
	Title, Description bool
}

// rawConfig は YAML の生の形。KnownFields(true) の照合対象になるため、
// 受理するキーはここに列挙されたものが全て。
type rawConfig struct {
	PollInterval      string           `yaml:"poll_interval"`
	SyncWindow        string           `yaml:"sync_window"`
	BlockerTitle      string           `yaml:"blocker_title"`
	ReconcileAt       string           `yaml:"reconcile_at"`
	DedupeSameMeeting *bool            `yaml:"dedupe_same_meeting"` // 未指定(nil)と false を区別する
	BusyShowAs        []string         `yaml:"busy_show_as"`
	Notifications     rawNotifications `yaml:"notifications"`
	Providers         rawProviders     `yaml:"providers"`
	Accounts          []rawAccount     `yaml:"accounts"`
	DetailSync        []rawDetailSync  `yaml:"detail_sync"`
}

type rawNotifications struct {
	Slack *rawSlack `yaml:"slack"`
}

type rawSlack struct {
	BotTokenEnv   string `yaml:"bot_token_env"`
	Channel       string `yaml:"channel"`
	MorningDigest string `yaml:"morning_digest"`
	RemindBefore  string `yaml:"remind_before"`
}

type rawProviders struct {
	Google    rawGoogleProvider    `yaml:"google"`
	Microsoft rawMicrosoftProvider `yaml:"microsoft"`
}

type rawGoogleProvider struct {
	CredentialsFile string `yaml:"credentials_file"`
}

type rawMicrosoftProvider struct {
	ClientID string `yaml:"client_id"`
}

type rawAccount struct {
	ID                      string   `yaml:"id"`
	Provider                string   `yaml:"provider"`
	Email                   string   `yaml:"email"`
	Calendars               []string `yaml:"calendars"`
	DigestCalendars         []string `yaml:"digest_calendars"`
	BlockerCalendar         string   `yaml:"blocker_calendar"`
	ShowOriginInDescription bool     `yaml:"show_origin_in_description"`
}

type rawDetailSync struct {
	From   string   `yaml:"from"`
	To     string   `yaml:"to"`
	Fields []string `yaml:"fields"`
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
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer f.Close()

	var raw rawConfig
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // 未知キーはエラー(タイポの黙殺を防ぐ)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
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
