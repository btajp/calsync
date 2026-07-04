package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/store"
)

// probeFunc は 1 アカウントの API 疎通確認。実体は GetCalendarTimezone 呼び出しで、
// テストではフェイクに差し替える。
type probeFunc func(ctx context.Context, acct config.Account) error

// runDoctor は doctor の本体。probe と出力先を注入可能にしてテストする。
// 診断項目: (1) 設定ロード済みの確認表示 (2) 各アカウントのトークンファイル有無
// (3) API 疎通(GetCalendarTimezone) (4) YAML に無い DB アカウントの孤児警告。
// 問題が 1 つでもあれば error を返す(exit code 非 0)。
func runDoctor(ctx context.Context, cfg *config.Config, dataDir string, probe probeFunc, out io.Writer, configPath string) error {
	fmt.Fprintf(out, "config: ok (%d accounts)\n", len(cfg.Accounts))
	tokens := &auth.TokenStore{Dir: dataDir}
	problems := 0

	for _, acct := range cfg.Accounts {
		if _, err := tokens.Load(acct.ID); err != nil {
			fmt.Fprintf(out, "account %s: token MISSING -- run: calsync auth add %s\n", acct.ID, acct.ID)
			problems++
			continue
		}
		fmt.Fprintf(out, "account %s: token ok\n", acct.ID)
		if err := probe(ctx, acct); err != nil {
			fmt.Fprintf(out, "account %s: API check FAILED: %v\n", acct.ID, err)
			problems++
		} else {
			fmt.Fprintf(out, "account %s: API check ok\n", acct.ID)
		}
	}

	// 孤児検出(YAML から消しただけで accounts remove していないアカウント)。
	// 読み取り専用オープン: デーモン稼働中でも診断できる。
	// DB ファイルが未作成(初回同期前の正常な状態)の場合は問題としてカウントしない。
	// それ以外の open 失敗(破損・権限等)のみ問題として扱う。
	if _, statErr := os.Stat(filepath.Join(dataDir, "calsync.db")); os.IsNotExist(statErr) {
		fmt.Fprintln(out, "store: no local DB yet (run `calsync sync --once` first)")
	} else {
		st, err := store.OpenReadOnly(dataDir)
		if err != nil {
			fmt.Fprintf(out, "store: cannot open: %v\n", err)
			problems++
		} else {
			defer st.Close()
			states, lerr := st.ListCalendars()
			if lerr != nil {
				return lerr
			}
			cfgIDs := make([]string, 0, len(cfg.Accounts))
			for _, a := range cfg.Accounts {
				cfgIDs = append(cfgIDs, a.ID)
			}
			dbIDs := make([]string, 0, len(states))
			for _, cs := range states {
				dbIDs = append(dbIDs, cs.Ref.AccountID)
			}
			for _, id := range findOrphanAccounts(cfgIDs, dbIDs) {
				fmt.Fprintf(out, "WARNING: account %s exists in the local DB but not in %s -- run: calsync accounts remove %s\n", id, configPath, id)
				problems++
			}
		}
	}

	if problems > 0 {
		return fmt.Errorf("doctor found %d problem(s)", problems)
	}
	fmt.Fprintln(out, "all checks passed")
	return nil
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
