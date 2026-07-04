package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/provider"
	googleprov "github.com/btajp/calsync/internal/provider/google"
	msprov "github.com/btajp/calsync/internal/provider/microsoft"
	"github.com/btajp/calsync/internal/store"
)

var (
	flagConfig string
	flagData   string
)

var rootCmd = &cobra.Command{
	Use:           "calsync",
	Short:         "Mirror busy slots across Google and Microsoft calendars as blocker events",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagConfig, "config", "calsync.yaml", "path to the calsync YAML config")
	rootCmd.PersistentFlags().StringVar(&flagData, "data", "./data", "data directory (SQLite state and OAuth tokens)")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// oauthConfigFor は各プロバイダの oauth2.Config を組み立てる。
// auth add(認可フロー)とトークンリフレッシュ(TokenSource)の両方で使う。
func oauthConfigFor(cfg *config.Config, acct config.Account) (*oauth2.Config, error) {
	switch acct.Provider {
	case "google":
		if cfg.Providers.Google.CredentialsFile == "" {
			return nil, errors.New("providers.google.credentials_file is not set in the config")
		}
		b, err := os.ReadFile(cfg.Providers.Google.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("read google credentials: %w", err)
		}
		return googleoauth.ConfigFromJSON(b, "https://www.googleapis.com/auth/calendar.events")
	case "microsoft":
		if cfg.Providers.Microsoft.ClientID == "" {
			return nil, errors.New("providers.microsoft.client_id is not set in the config")
		}
		return &oauth2.Config{
			ClientID: cfg.Providers.Microsoft.ClientID,
			// アプリ登録(http://localhost)と同じ「localhost・パスなし」の形にする。
			// MSA(login.live.com)はポートを無視するがパスは照合するため、
			// /callback 付きだと invalid_request になる(実測 2026-07-03。MSAL と同じ形)。
			// 実ポートは RunLoopbackFlow がホスト名・パスを保持したまま差し込む。
			RedirectURL: "http://localhost",
			Endpoint: oauth2.Endpoint{
				AuthURL:       "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
				TokenURL:      "https://login.microsoftonline.com/common/oauth2/v2.0/token",
				DeviceAuthURL: "https://login.microsoftonline.com/common/oauth2/v2.0/devicecode",
			},
			// MailboxSettings.Read は GetCalendarTimezone(/me/mailboxSettings/timeZone)
			// に必要(Calendars.ReadWrite だけでは 403。最終ホールブランチレビュー追補 Issue 1)
			Scopes: []string{
				"offline_access",
				"https://graph.microsoft.com/Calendars.ReadWrite",
				"https://graph.microsoft.com/MailboxSettings.Read",
			},
		}, nil
	default:
		return nil, fmt.Errorf("account %s: unknown provider %q", acct.ID, acct.Provider)
	}
}

// buildProvider は 1 アカウント分の Provider を構築する。
// トークンは PersistingTokenSource で包み、リフレッシュ(MS のローテーション含む)
// のたびにディスクへ書き戻す(仕様 9.3)。
func buildProvider(cfg *config.Config, tokens *auth.TokenStore, acct config.Account) (provider.Provider, error) {
	tok, err := tokens.Load(acct.ID)
	if err != nil {
		return nil, fmt.Errorf("account %s: no token (run: calsync auth add %s): %w", acct.ID, acct.ID, err)
	}
	ocfg, err := oauthConfigFor(cfg, acct)
	if err != nil {
		return nil, err
	}
	ts := auth.PersistingTokenSource(acct.ID, tokens, ocfg.TokenSource(context.Background(), tok))
	switch acct.Provider {
	case "google":
		return googleprov.New(ts, acct.ID), nil
	case "microsoft":
		return msprov.New(ts, acct.ID, cfg.BusyShowAs), nil
	default:
		return nil, fmt.Errorf("account %s: unknown provider %q", acct.ID, acct.Provider)
	}
}

// buildEngine は store・トークン・全アカウントの Provider を組み立てる。
// 1 つでも構築できないアカウントがあればエラー(run/sync/reconcile 用の厳格版。
// doctor と accounts remove はアカウント単位で degrade するため使わない)。
// 呼び出し側は使用後に eng.Store.Close() すること。
func buildEngine(cfg *config.Config, dataDir string) (*engine.Engine, error) {
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	tokens := &auth.TokenStore{Dir: dataDir}
	providers := make(map[string]provider.Provider, len(cfg.Accounts))
	for _, acct := range cfg.Accounts {
		p, err := buildProvider(cfg, tokens, acct)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		providers[acct.ID] = p
	}
	return &engine.Engine{Store: st, Providers: providers, Cfg: cfg, Now: time.Now}, nil
}

// findOrphanAccounts は DB に行があるのに設定に存在しないアカウント ID を
// 重複除去・昇順ソートして返す純関数(doctor の孤児警告用。仕様 11 章)。
func findOrphanAccounts(cfgIDs, dbIDs []string) []string {
	known := make(map[string]bool, len(cfgIDs))
	for _, id := range cfgIDs {
		known[id] = true
	}
	seen := make(map[string]bool, len(dbIDs))
	var orphans []string
	for _, id := range dbIDs {
		if !known[id] && !seen[id] {
			seen[id] = true
			orphans = append(orphans, id)
		}
	}
	sort.Strings(orphans)
	return orphans
}
