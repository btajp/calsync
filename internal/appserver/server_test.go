package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// TestRequireTokenRejectsNonLocalHost は最終ホールレビュー Fix 3(DNS rebinding
// 対策)の回帰テスト。トークンが正しくても Host が 127.0.0.1/localhost 以外なら
// 403 で拒否すること。
func TestRequireTokenRejectsNonLocalHost(t *testing.T) {
	s, _ := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Host = "evil.example"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
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

// TestStatusTokensEmptyArrayNotNull は最終ホールレビュー Fix 1 の回帰テスト。
// アカウント 0 件の有効な config では tokens が JSON で "[]"(null ではない)に
// なること。フロントの status.tokens.map(...) は null だとクラッシュする。
func TestStatusTokensEmptyArrayNotNull(t *testing.T) {
	s, dir := testServer(t)
	cfgPath := filepath.Join(dir, "calsync.yaml")
	os.WriteFile(cfgPath, []byte(`
providers:
  google: {credentials_file: /tmp/creds.json}
`), 0o600)
	s.ConfigPath = cfgPath
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `"tokens":[]`) {
		t.Fatalf(`expected literal "tokens":[] in response body, got %s`, body)
	}
}

// TestStatusTokensEmptyArrayOnConfigLoadFailure は同じく Fix 1 の回帰テスト。
// config.Load 自体が失敗する(壊れた YAML)場合でも tokens ループが回らないだけで
// tokens は空配列のままであること(null にならないこと)を確認する。
func TestStatusTokensEmptyArrayOnConfigLoadFailure(t *testing.T) {
	s, dir := testServer(t)
	cfgPath := filepath.Join(dir, "calsync.yaml")
	os.WriteFile(cfgPath, []byte("not: [valid: yaml"), 0o600)
	s.ConfigPath = cfgPath
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `"tokens":[]`) {
		t.Fatalf(`expected literal "tokens":[] in response body, got %s`, body)
	}
}

// TestServeRejectsEmptyToken は F2 の回帰テスト。requireToken は
// subtle.ConstantTimeCompare で比較するため、Token が空だと Authorization
// ヘッダの無いリクエストも一致してしまい認証が素通しになる。Serve はこれを
// 未然に防ぐため Token 空なら起動せず即座にエラーを返すこと。
func TestServeRejectsEmptyToken(t *testing.T) {
	s, _ := testServer(t)
	s.Token = ""
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	var out syncBuffer
	if err := s.Serve(context.Background(), ln, &out); !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("Serve error = %v, want ErrEmptyToken", err)
	}
	if out.Len() != 0 {
		t.Fatalf("handshake must not be written when refusing to start, got %q", out.String())
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

// TestServeBootstrapsWhenMaintenanceRunning は最終ホールレビュー Fix 1 の回帰テスト。
// メンテナンス実行中(maintSt.phase=="running")に Serve の ctx がキャンセルされる
// (Cmd+Q・stdin EOF 等、フロントの確認ダイアログを経由しない終了経路)場合、
// ensureBootstrapOnShutdown が fresh ctx で launchctl bootstrap を best-effort 実行し、
// デーモンが bootout されたまま残らないことを確認する。
func TestServeBootstrapsWhenMaintenanceRunning(t *testing.T) {
	s, _ := launchdServer(t)
	rec := &callRecorder{}
	fr := &fakeRunner{outputs: maintenanceScript(s.UID, s.PlistPath)}
	s.Runner = &orderRunner{fakeRunner: fr, rec: rec}
	// 実運用では runMaintenanceWindow が設定する状態を直接注入する(この
	// テストの関心は Serve 側の最終防衛だけであり、メンテナンス窓そのものの
	// 呼び出し順序は maintenance_test.go の別テストで検証済み)。
	s.maintSt.mu.Lock()
	s.maintSt.phase = "running"
	s.maintSt.mu.Unlock()

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, &out) }()
	deadline := time.Now().Add(2 * time.Second)
	for out.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}

	if calls := rec.snapshot(); len(calls) != 1 || calls[0] != "bootstrap" {
		t.Fatalf("launchctl calls = %v, want [bootstrap]", calls)
	}
}

// TestServeSkipsBootstrapWhenMaintenanceIdle は上のテストの対照。メンテナンスが
// 実行中でなければ Serve の shutdown 経路は launchctl を一切呼ばないこと
// (idle/manual/container 運用で余計な bootstrap 副作用を起こさないため)。
func TestServeSkipsBootstrapWhenMaintenanceIdle(t *testing.T) {
	s, _ := launchdServer(t)
	rec := &callRecorder{}
	fr := &fakeRunner{outputs: maintenanceScript(s.UID, s.PlistPath)}
	s.Runner = &orderRunner{fakeRunner: fr, rec: rec}
	// s.maintSt.phase は既定の "idle" のまま。

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, &out) }()
	deadline := time.Now().Add(2 * time.Second)
	for out.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}

	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("launchctl calls = %v, want none", calls)
	}
}

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	s, _ := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// プリフライトにはトークンが載らない(ブラウザ仕様)。認証なしで 204 が返ること
	req, _ := http.NewRequest("OPTIONS", srv.URL+"/api/status", nil)
	req.Header.Set("Origin", "tauri://localhost")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "tauri://localhost" {
		t.Fatalf("allow-origin = %q", got)
	}
	if got := res.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Fatalf("allow-headers = %q", got)
	}
}

func TestCORSActualRequestEchoesOrigin(t *testing.T) {
	s, _ := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", "tauri://localhost")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "tauri://localhost" {
		t.Fatalf("allow-origin = %q", got)
	}
}

func TestCORSDisallowedOrigin(t *testing.T) {
	s, _ := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// 未許可オリジンのプリフライトは 403・CORS ヘッダなし
	pre, _ := http.NewRequest("OPTIONS", srv.URL+"/api/status", nil)
	pre.Header.Set("Origin", "https://evil.example")
	pre.Header.Set("Access-Control-Request-Method", "GET")
	preRes, err := http.DefaultClient.Do(pre)
	if err != nil {
		t.Fatal(err)
	}
	if preRes.StatusCode != http.StatusForbidden || preRes.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight status=%d acao=%q", preRes.StatusCode, preRes.Header.Get("Access-Control-Allow-Origin"))
	}
	// 未許可オリジン付きの実リクエストにも CORS ヘッダを付けない(トークンが正しくても)
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("acao must be empty for disallowed origin")
	}
}
