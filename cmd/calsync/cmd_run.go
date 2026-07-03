package main

import (
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/work-a-co/calsync/internal/config"
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
		eng, err := buildEngine(cfg, flagData)
		if err != nil {
			return err
		}
		defer eng.Store.Close()
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return eng.Run(ctx)
	},
}

func init() { rootCmd.AddCommand(runCmd) }
