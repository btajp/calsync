package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/work-a-co/calsync/internal/store"
)

// renderStatus は ListCalendars の結果を表形式文字列にする(テスト対象の純粋整形)。
func renderStatus(states []store.CalendarState) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ACCOUNT\tCALENDAR\tLAST SYNC\tSTATUS")
	for _, cs := range states {
		last := "-"
		if !cs.LastSyncedAt.IsZero() {
			last = cs.LastSyncedAt.UTC().Format(time.RFC3339)
		}
		status := "ok"
		if cs.LastError != "" {
			status = cs.LastError
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cs.Ref.AccountID, cs.Ref.CalendarID, last, status)
	}
	_ = w.Flush()
	return b.String()
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show last sync time and error state for each calendar",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// 読み取り専用: デーモン稼働中(flock 保持中)でも実行できる
		st, err := store.OpenReadOnly(flagData)
		if err != nil {
			return err
		}
		defer st.Close()
		states, err := st.ListCalendars()
		if err != nil {
			return err
		}
		if len(states) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no calendars in the local state yet (run `calsync sync --once` first)")
			return nil
		}
		fmt.Fprint(cmd.OutOrStdout(), renderStatus(states))
		return nil
	},
}

func init() { rootCmd.AddCommand(statusCmd) }
