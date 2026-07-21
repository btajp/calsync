package appserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonRestart(t *testing.T) {
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	os.WriteFile(plist, []byte("<plist/>"), 0o600)
	s.PlistPath = plist
	s.UID = 501
	fr := &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{
		"launchctl print gui/501/com.btajp.calsync":        {out: "state = running\n"},
		"launchctl kickstart -k gui/501/com.btajp.calsync": {out: ""},
	}}
	s.Runner = fr
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/daemon/restart", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestDaemonRejectedOutsideLaunchd(t *testing.T) {
	s, _ := testServer(t) // plist なし → manual モード
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/daemon/stop", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d", res.StatusCode)
	}
}
