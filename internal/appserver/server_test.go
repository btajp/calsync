package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer は Serve のバックグラウンドゴルーチンからの書き込みとテスト本体からの
// ポーリング読み取りを安全に行う io.Writer。strings.Builder は並行アクセスに
// 対応しないため、素朴なポーリングは go test -race が data race として検出する。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// fakeRunner は launchctl / docker 呼び出しを台本で返す。
type fakeRunner struct {
	outputs map[string]struct {
		out string
		err error
	}
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	key := name + " " + strings.Join(args, " ")
	if r, ok := f.outputs[key]; ok {
		return r.out, "", r.err
	}
	return "", "", fmt.Errorf("unexpected command: %s", key)
}

func testServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "calsync.yaml")
	os.WriteFile(cfgPath, []byte(`
providers:
  google: {credentials_file: /tmp/creds.json}
  microsoft: {client_id: test-client-id}
accounts:
  - {id: personal, provider: google}
`), 0o600)
	s := New(cfgPath, dir, "test-token")
	s.PlistPath = filepath.Join(dir, "no-such.plist") // 既定: launchd 未検出
	s.Runner = &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{}}
	// docker が PATH に無い CI 環境でもテストが決定的になるように注入する。
	s.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	return s, dir
}

func TestAuthRequired(t *testing.T) {
	s, _ := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/status")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func get(t *testing.T, srv *httptest.Server, token, path string, into any) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if into != nil {
		defer res.Body.Close()
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return res
}

func TestStatusManualMode(t *testing.T) {
	s, _ := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got StatusResponse
	res := get(t, srv, "test-token", "/api/status", &got)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got.Daemon.Mode != "manual" {
		t.Fatalf("mode = %q", got.Daemon.Mode)
	}
	// launchd 未検出時は DB に触れない(不変条件)
	if got.DBNote == "" || len(got.Calendars) != 0 {
		t.Fatalf("expected db skip note, got %+v", got)
	}
	// トークン状態はファイルベースなのでどのモードでも返る
	if len(got.Tokens) != 1 || got.Tokens[0].State != "missing" {
		t.Fatalf("tokens = %+v", got.Tokens)
	}
}

func TestStatusContainerGuard(t *testing.T) {
	s, _ := testServer(t)
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
		t.Fatalf("mode = %q", got.Daemon.Mode)
	}
}

func TestStatusLaunchdRunning(t *testing.T) {
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	os.WriteFile(plist, []byte("<plist/>"), 0o600)
	s.PlistPath = plist
	s.UID = 501
	s.Runner = &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{
		"launchctl print gui/501/com.btajp.calsync": {out: "state = running\n"},
	}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got StatusResponse
	get(t, srv, "test-token", "/api/status", &got)
	if got.Daemon.Mode != "launchd" || !got.Daemon.Running {
		t.Fatalf("daemon = %+v", got.Daemon)
	}
	// DB 未作成は正常系(db_note で伝える)
	if got.DBNote == "" {
		t.Fatalf("want db_note for missing db, got %+v", got)
	}
}

func TestServeHandshake(t *testing.T) {
	s, _ := testServer(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, &out) }()
	// ハンドシェイク行が出るまで少し待つ
	deadline := time.Now().Add(2 * time.Second)
	for out.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var hs struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &hs); err != nil {
		t.Fatalf("handshake %q: %v", out.String(), err)
	}
	if hs.Token != "test-token" || hs.Port == 0 {
		t.Fatalf("handshake = %+v", hs)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}
