package appserver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/btajp/calsync/internal/config"
)

const seedYAML = `# top comment
poll_interval: 1m
providers:
  google:
    credentials_file: /tmp/creds.json # keep me
accounts:
  # personal account comment
  - id: personal
    provider: google
  - id: work-ms
    provider: microsoft
`

func writeSeed(t *testing.T) (string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "calsync.yaml")
	if err := os.WriteFile(p, []byte(seedYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	return p, fi.ModTime()
}

func loadRaw(t *testing.T, p string) *config.Raw {
	t.Helper()
	b, _ := os.ReadFile(p)
	var raw config.Raw
	if err := yaml.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	return &raw
}

func TestSaveConfigPreservesComments(t *testing.T) {
	p, mtime := writeSeed(t)
	raw := loadRaw(t, p)
	raw.PollInterval = "2m" // 値を 1 つ変更
	if err := SaveConfig(p, raw, mtime); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, _ := os.ReadFile(p)
	s := string(out)
	for _, want := range []string{"# top comment", "# keep me", "# personal account comment", "2m"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	// .bak に旧内容が残る
	bak, err := os.ReadFile(p + ".bak")
	if err != nil || !strings.Contains(string(bak), "poll_interval: 1m") {
		t.Fatalf("bak: %v %s", err, bak)
	}
}

func TestSaveConfigRejectsInvalid(t *testing.T) {
	p, mtime := writeSeed(t)
	raw := loadRaw(t, p)
	raw.PollInterval = "banana"
	if err := SaveConfig(p, raw, mtime); err == nil {
		t.Fatal("want validation error")
	}
	out, _ := os.ReadFile(p)
	if !strings.Contains(string(out), "poll_interval: 1m") {
		t.Fatal("file must be unchanged on validation failure")
	}
}

func TestSaveConfigConflict(t *testing.T) {
	p, mtime := writeSeed(t)
	raw := loadRaw(t, p)
	if err := SaveConfig(p, raw, mtime.Add(-time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestSaveConfigAddAccountKeepsOtherComments(t *testing.T) {
	p, mtime := writeSeed(t)
	raw := loadRaw(t, p)
	raw.Accounts = append(raw.Accounts, config.RawAccount{ID: "work-a", Provider: "google"})
	if err := SaveConfig(p, raw, mtime); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, _ := os.ReadFile(p)
	s := string(out)
	if !strings.Contains(s, "# personal account comment") || !strings.Contains(s, "work-a") {
		t.Fatalf("out:\n%s", s)
	}
}
