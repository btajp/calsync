// Package doctor は calsync のアカウント/トークン/DB 状態を診断する doctor
// コマンド(CLI・appserver 双方から利用)の本体を提供する。
package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/store"
)

// Probe は 1 アカウントの API 疎通確認。実体は GetCalendarTimezone 呼び出しで、
// テストではフェイクに差し替える。
type Probe func(ctx context.Context, acct config.Account) error

// Run は doctor の本体。probe と出力先を注入可能にしてテストする。
// 診断項目: (1) 設定ロード済みの確認表示 (2) 各アカウントのトークンファイル有無
// (3) API 疎通(GetCalendarTimezone) (4) YAML に無い DB アカウントの孤児警告。
// 問題が 1 つでもあれば error を返す(exit code 非 0)。
func Run(ctx context.Context, cfg *config.Config, dataDir string, probe Probe, out io.Writer, configPath string) error {
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
			for _, id := range FindOrphanAccounts(cfgIDs, dbIDs) {
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

// FindOrphanAccounts は DB に行があるのに設定に存在しないアカウント ID を
// 重複除去・昇順ソートして返す純関数(doctor の孤児警告用。仕様 11 章)。
func FindOrphanAccounts(cfgIDs, dbIDs []string) []string {
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
