package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/provider"
	"github.com/btajp/calsync/internal/store"
)

var accountsForce bool

var accountsCmd = &cobra.Command{
	Use:   "accounts",
	Short: "Manage synced accounts",
}

var accountsRemoveCmd = &cobra.Command{
	Use:   "remove <account-id>",
	Short: "Delete distributed blockers, received blockers and local state for an account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		accountID := args[0]
		cfg, err := config.Load(flagConfig)
		if err != nil {
			return err
		}
		st, err := store.Open(flagData)
		if err != nil {
			return err
		}
		defer st.Close()

		// remove は「認証が切れた/YAML から消したアカウントを片付ける」用途があるため、
		// buildEngine(全アカウント必須)ではなく、構築できないプロバイダは警告して
		// スキップする。欠けた provider は RemoveAccount が force に応じて
		// エラー/スキップ処理する。
		tokens := &auth.TokenStore{Dir: flagData}
		providers := make(map[string]provider.Provider, len(cfg.Accounts))
		for _, acct := range cfg.Accounts {
			p, perr := buildProvider(cfg, tokens, acct)
			if perr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: provider for account %s unavailable: %v\n", acct.ID, perr)
				continue
			}
			providers[acct.ID] = p
		}
		eng := &engine.Engine{Store: st, Providers: providers, Cfg: cfg, Now: time.Now}

		if err := engine.RemoveAccount(cmd.Context(), eng, tokens, accountID, accountsForce); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "account %s removed\n", accountID)
		if accountsForce {
			fmt.Fprintln(cmd.OutOrStdout(), "note: --force was used; blockers may remain on calendars whose provider was unavailable")
		}
		if cfg.AccountByID(accountID) != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "note: %s is still defined in %s -- remove it from the config, or it will sync again\n", accountID, flagConfig)
		}
		return nil
	},
}

func init() {
	accountsRemoveCmd.Flags().BoolVar(&accountsForce, "force", false, "skip remote blocker deletion when a provider is unavailable (blockers may remain)")
	accountsCmd.AddCommand(accountsRemoveCmd)
	rootCmd.AddCommand(accountsCmd)
}
