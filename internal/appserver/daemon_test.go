package appserver

import (
	"encoding/json"
	"errors"
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

func TestDaemonStart(t *testing.T) {
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	os.WriteFile(plist, []byte("<plist/>"), 0o600)
	s.PlistPath = plist
	s.UID = 501
	fr := &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{
		"launchctl print gui/501/com.btajp.calsync": {out: "state = running\n"},
		"launchctl bootstrap gui/501 " + plist:      {out: ""},
	}}
	s.Runner = fr
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/daemon/start", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestDaemonStop(t *testing.T) {
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	os.WriteFile(plist, []byte("<plist/>"), 0o600)
	s.PlistPath = plist
	s.UID = 501
	fr := &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{
		"launchctl print gui/501/com.btajp.calsync":   {out: "state = running\n"},
		"launchctl bootout gui/501/com.btajp.calsync": {out: ""},
	}}
	s.Runner = fr
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/daemon/stop", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

// TestDaemonActionFallsBackToErrErrorWhenStderrEmpty は F3 の回帰テスト。
// launchctl が失敗したのに stderr へ何も書かない(fakeRunner が返す stderr は
// 常に空文字列。実運用でも実行ファイル不在等で起こりうる)場合、応答メッセージが
// 空文字列にならず err.Error() にフォールバックすること。
func TestDaemonActionFallsBackToErrErrorWhenStderrEmpty(t *testing.T) {
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	os.WriteFile(plist, []byte("<plist/>"), 0o600)
	s.PlistPath = plist
	s.UID = 501
	wantErr := errors.New("exec: \"launchctl\": executable file not found in $PATH")
	fr := &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{
		"launchctl print gui/501/com.btajp.calsync":        {out: "state = running\n"},
		"launchctl kickstart -k gui/501/com.btajp.calsync": {err: wantErr},
	}}
	s.Runner = fr
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/daemon/restart", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}
	var body apiError
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Message != wantErr.Error() {
		t.Fatalf("message = %q, want %q", body.Message, wantErr.Error())
	}
}

// TestDaemonDetectionStalePlistPrefersContainer は最終ホールレビュー Fix 2 の
// 回帰テスト。plist は存在するが launchctl print が失敗する(installed but not
// loaded = 未ロード)状態で、かつ docker で calsync コンテナが稼働中の場合は
// container を優先して返すこと(ホストが誤って DB 読み取りに到達するのを防ぐ)。
func TestDaemonDetectionStalePlistPrefersContainer(t *testing.T) {
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	os.WriteFile(plist, []byte("<plist/>"), 0o600)
	s.PlistPath = plist
	s.UID = 501
	// "launchctl print ..." は台本に無いので fakeRunner がエラーを返す
	// (= plist はあるが未ロード)。
	s.Runner = &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{
		"docker ps --format {{.Names}}": {out: "calsync\nother\n"},
	}}
	s.LookPath = func(string) (string, error) { return "/usr/bin/docker", nil }
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got StatusResponse
	get(t, srv, "test-token", "/api/status", &got)
	if got.Daemon.Mode != "container" {
		t.Fatalf("mode = %q, want container (stale plist + running container must prefer container)", got.Daemon.Mode)
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
