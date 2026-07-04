package main

import (
	"github.com/spf13/cobra"

	"github.com/btajp/calsync/internal/config"
)

var reconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Run a full reconcile (window slide, orphan adoption, self-heal)",
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
		return eng.Reconcile(cmd.Context())
	},
}

func init() { rootCmd.AddCommand(reconcileCmd) }
