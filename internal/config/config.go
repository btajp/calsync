package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/work-a-co/calsync/internal/model"
)

type Config struct {
	PollInterval      time.Duration // 既定 1m
	SyncWindowMonths  int           // "3mo" → 3。SyncWindowDays と排他
	SyncWindowDays    int           // "90d" → 90
	BlockerTitle      string        // 既定 "予定あり"
	ReconcileAt       string        // 既定 "04:00"(コンテナのローカルTZで解釈)
	DedupeSameMeeting bool          // 既定 true
	BusyShowAs        []string      // 既定 [busy, oof, tentative]
	Providers         ProvidersConfig
	Accounts          []Account
}

type ProvidersConfig struct {
	Google    struct{ CredentialsFile string } // GCP クライアントJSON のパス
	Microsoft struct{ ClientID string }        // Entra アプリの client_id
}

type Account struct {
	ID, Provider, Email string   // Provider は "google" | "microsoft"
	Calendars           []string // 既定 ["primary"]。microsoft は ["primary"] 以外エラー(v1制約)
	BlockerCalendar     string   // 既定 "primary"
}

// rawConfig は YAML の生の形。KnownFields(true) の照合対象になるため、
// 受理するキーはここに列挙されたものが全て。
type rawConfig struct {
	PollInterval      string       `yaml:"poll_interval"`
	SyncWindow        string       `yaml:"sync_window"`
	BlockerTitle      string       `yaml:"blocker_title"`
	ReconcileAt       string       `yaml:"reconcile_at"`
	DedupeSameMeeting *bool        `yaml:"dedupe_same_meeting"` // 未指定(nil)と false を区別する
	BusyShowAs        []string     `yaml:"busy_show_as"`
	Providers         rawProviders `yaml:"providers"`
	Accounts          []rawAccount `yaml:"accounts"`
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
	ID              string   `yaml:"id"`
	Provider        string   `yaml:"provider"`
	Email           string   `yaml:"email"`
	Calendars       []string `yaml:"calendars"`
	BlockerCalendar string   `yaml:"blocker_calendar"`
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

	seen := make(map[string]bool, len(raw.Accounts))
	for i, ra := range raw.Accounts {
		a := Account{
			ID:              ra.ID,
			Provider:        ra.Provider,
			Email:           ra.Email,
			Calendars:       ra.Calendars,
			BlockerCalendar: ra.BlockerCalendar,
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
		}
		cfg.Accounts = append(cfg.Accounts, a)
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
