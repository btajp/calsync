package doctor

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btajp/calsync/internal/config"
)

func TestRunReportsMissingToken(t *testing.T) {
	dir := t.TempDir() // tokens/ が無い = 全アカウント token MISSING
	cfg := &config.Config{Accounts: []config.Account{{ID: "personal", Provider: "google"}}}
	var out bytes.Buffer
	probe := func(ctx context.Context, acct config.Account) error { return errors.New("should not be called") }
	err := Run(context.Background(), cfg, dir, probe, &out, "calsync.yaml")
	if err == nil {
		t.Fatal("want problem error")
	}
	if !bytes.Contains(out.Bytes(), []byte("token MISSING")) {
		t.Fatalf("out = %s", out.String())
	}
}

func TestFindOrphanAccounts(t *testing.T) {
	got := FindOrphanAccounts([]string{"personal"}, []string{"personal", "old-acct", "old-acct"})
	if len(got) != 1 || got[0] != "old-acct" {
		t.Fatalf("got %v", got)
	}
}
