package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/doctor"
	"github.com/btajp/calsync/internal/model"
)

// probeFunc は doctor.Probe への型エイリアス。
type probeFunc = doctor.Probe

// runDoctor は doctor.Run への委譲ラッパー。
func runDoctor(ctx context.Context, cfg *config.Config, dataDir string, probe probeFunc, out io.Writer, configPath string) error {
	return doctor.Run(ctx, cfg, dataDir, probe, out, configPath)
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose config, tokens, API connectivity and DB consistency",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(flagConfig)
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		tokens := &auth.TokenStore{Dir: flagData}
		probe := func(ctx context.Context, acct config.Account) error {
			p, err := buildProvider(cfg, tokens, acct)
			if err != nil {
				return err
			}
			cal := "primary"
			if len(acct.Calendars) > 0 {
				cal = acct.Calendars[0]
			}
			_, err = p.GetCalendarTimezone(ctx, model.CalendarRef{AccountID: acct.ID, CalendarID: cal})
			return err
		}
		return runDoctor(cmd.Context(), cfg, flagData, probe, cmd.OutOrStdout(), flagConfig)
	},
}

func init() { rootCmd.AddCommand(doctorCmd) }
