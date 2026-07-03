package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/work-a-co/calsync/internal/config"
	"github.com/work-a-co/calsync/internal/model"
)

var syncOnce bool

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Run one sync cycle and exit (requires --once)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !syncOnce {
			return errors.New("continuous sync is `calsync run`; pass --once to sync a single cycle")
		}
		cfg, err := config.Load(flagConfig)
		if err != nil {
			return err
		}
		eng, err := buildEngine(cfg, flagData)
		if err != nil {
			return err
		}
		defer eng.Store.Close()
		var firstErr error
		for _, acct := range cfg.Accounts {
			for _, calID := range acct.Calendars {
				ref := model.CalendarRef{AccountID: acct.ID, CalendarID: calID}
				if err := eng.SyncCalendar(cmd.Context(), ref); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "sync %s: %v\n", ref, err)
					if firstErr == nil {
						firstErr = err
					}
				}
			}
		}
		return firstErr
	},
}

func init() {
	syncCmd.Flags().BoolVar(&syncOnce, "once", false, "sync one cycle and exit")
	rootCmd.AddCommand(syncCmd)
}
