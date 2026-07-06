package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/notify/slack"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the sync daemon (polling loop + daily reconcile)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(flagConfig)
		if err != nil {
			return err
		}
		// Slack 通知が設定されていればトークンを先に検証する(store を開く前に
		// fail fast。デーモン専用機能のため run でのみ検証する。スペック 9 章)
		var notifier engine.Notifier
		if sc := cfg.Notifications.Slack; sc != nil {
			token := os.Getenv(sc.BotTokenEnv)
			if token == "" {
				return fmt.Errorf("notifications.slack: environment variable %s is not set (export the bot token or remove the notifications section)", sc.BotTokenEnv)
			}
			sl := slack.New(token, sc.Channel)
			ids := make([]string, len(cfg.Accounts))
			for i, a := range cfg.Accounts {
				ids[i] = a.ID
			}
			sl.Accounts = ids
			notifier = sl
		}
		eng, err := buildEngine(cfg, flagData)
		if err != nil {
			return err
		}
		defer eng.Store.Close()
		eng.Notifier = notifier
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return eng.Run(ctx)
	},
}

func init() { rootCmd.AddCommand(runCmd) }
