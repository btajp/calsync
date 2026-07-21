package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/clients"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/doctor"
	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/provider"
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

func oauthConfigFor(cfg *config.Config, acct config.Account) (*oauth2.Config, error) {
	return clients.OAuthConfigFor(cfg, acct)
}

func buildProvider(cfg *config.Config, tokens *auth.TokenStore, acct config.Account) (provider.Provider, error) {
	return clients.BuildProvider(cfg, tokens, acct)
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

// findOrphanAccounts は doctor.FindOrphanAccounts への委譲ラッパー。
func findOrphanAccounts(cfgIDs, dbIDs []string) []string {
	return doctor.FindOrphanAccounts(cfgIDs, dbIDs)
}
