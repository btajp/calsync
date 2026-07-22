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
    show_origin_in_description: false # keep anonymous per policy
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

// TestSaveConfigPreservesExplicitFalseBool は show_origin_in_description のような
// *bool フィールドについて、明示的な false(既定値と同じ)がキーごと消えたり、
// そのキーに付いた行コメントが失われたりしないことを確認する回帰テスト。
// RawAccount.ShowOriginInDescription が plain bool のままだと、omitempty により
// 「明示 false」と「未指定」が区別できず、書き戻し時にキーとコメントが消えていた。
func TestSaveConfigPreservesExplicitFalseBool(t *testing.T) {
	p, mtime := writeSeed(t)
	raw := loadRaw(t, p)
	raw.PollInterval = "2m" // show_origin_in_description には触れず、他の値だけ変更する
	if err := SaveConfig(p, raw, mtime); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, _ := os.ReadFile(p)
	s := string(out)
	for _, want := range []string{"show_origin_in_description: false", "# keep anonymous per policy"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

// seedYAMLFlowDigest はデジェスト専用カレンダーをフロー記法(`[a]`)で書き、
// その行末に行コメントを付けたアカウントを1件だけ持つ最小構成。blocker_calendar
// は既定値("primary")に任せるため書かない(実機で観測した構成を再現する:
// digest_calendars の次に来るキーが直接 show_origin_in_description になる)。
const seedYAMLFlowDigest = `accounts:
  - id: personal
    provider: google
    digest_calendars: [cal-a@example.com] # trash day (notify only)
`

// TestSaveConfigDigestCalendarsCommentStaysOnDigestCalendars は F7 の再現テスト。
//
// 実機観測: digest_calendars(フロー記法)の行末コメントが付いたアカウントに
// フォーム経由で新規キー show_origin_in_description を追加して保存すると、
// コメントが show_origin_in_description の行へ移動してしまっていた。
//
// 原因: フロー記法の行末コメントは yaml.v3 のデコード時、コレクション値
// ノード(sequence)自身の LineComment として保持される。SaveConfig は常に
// yaml.Marshal(raw) 経由でブロック形式(`- a` を改行で並べる形)の新ツリーを
// 作るため、旧ツリーのその LineComment をそのままコレクション値ノードへ
// コピーすると、エンコーダがブロック形式のコレクション値には LineComment の
// 描画位置を持てず、直後の兄弟キー(この場合 show_origin_in_description)の
// 行末へ取り違えて出力してしまっていた。
func TestSaveConfigDigestCalendarsCommentStaysOnDigestCalendars(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "calsync.yaml")
	if err := os.WriteFile(p, []byte(seedYAMLFlowDigest), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	mtime := fi.ModTime()

	raw := loadRaw(t, p)
	// フォームで「元アカウントIDを表示」トグルを ON にした想定(= 新規キー追加)
	on := true
	raw.Accounts[0].ShowOriginInDescription = &on

	if err := SaveConfig(p, raw, mtime); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, _ := os.ReadFile(p)
	s := string(out)

	if strings.Contains(s, "show_origin_in_description: true # trash day") {
		t.Fatalf("comment drifted onto show_origin_in_description line:\n%s", s)
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "digest_calendars") && !strings.Contains(line, "# trash day (notify only)") {
			t.Fatalf("digest_calendars line lost its comment:\n%s", s)
		}
	}
	if !strings.Contains(s, "# trash day (notify only)") {
		t.Fatalf("comment missing entirely:\n%s", s)
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
