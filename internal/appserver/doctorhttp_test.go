package appserver

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/btajp/calsync/internal/config"
)

// TestDoctorLaunchd は launchd モードで token missing シナリオを検証する。
// probe には呼ばれないはずのフェイクを入れて、doctor.Run がトークン欠落時に
// probe を呼ばないこと(既存 doctor.Run の分岐)を確認する。
func TestDoctorLaunchd(t *testing.T) {
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.PlistPath = plist
	s.UID = 501
	s.Runner = &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{
		"launchctl print gui/501/com.btajp.calsync": {out: "state = running\n"},
	}}
	probeCalled := false
	s.Probe = func(ctx context.Context, acct config.Account) error {
		probeCalled = true
		return nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}
	res := get(t, srv, "test-token", "/api/doctor", &got)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got.OK {
		t.Fatalf("ok = true, want false (token missing for configured account)")
	}
	if got.Text == "" {
		t.Fatal("text is empty")
	}
	if probeCalled {
		t.Fatal("probe should not be called when token is missing")
	}
}

// TestDoctorRejectedOutsideLaunchd は launchd 管理外(手動運用)では doctor が
// DB を読まないよう 409 で拒否することを検証する。
func TestDoctorRejectedOutsideLaunchd(t *testing.T) {
	s, _ := testServer(t) // plist なし → manual モード
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res := get(t, srv, "test-token", "/api/doctor", nil)
	if res.StatusCode != 409 {
		t.Fatalf("status = %d", res.StatusCode)
	}
}
