package appserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// containerModeServer は testServer をベースに、TestStatusContainerGuard と
// 同じ台本(docker ps に calsync コンテナが写る)でコンテナモードを固定する。
func containerModeServer(t *testing.T) *Server {
	t.Helper()
	s, _ := testServer(t)
	s.Runner = &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{
		"docker ps --format {{.Names}}": {out: "calsync\nother\n"},
	}}
	s.LookPath = func(string) (string, error) { return "/usr/bin/docker", nil }
	return s
}

// TestContainerGuardBlocksWriteEndpoints は仕様§9(コンテナ運用のホストからは
// DB 読み取りを含む全機能を停止して案内表示モードに落とす)に対する回帰テスト。
// PUT /api/config・POST /api/auth/start・GET /api/accounts/{id}/calendars は
// コンテナモード検出時に 409 container_mode を返すこと。
func TestContainerGuardBlocksWriteEndpoints(t *testing.T) {
	s := containerModeServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	do := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"config put", "PUT", "/api/config", `{"raw":{},"base_mtime":"1"}`},
		{"auth start", "POST", "/api/auth/start", `{"account_id":"x","provider":"google"}`},
		{"calendars", "GET", "/api/accounts/x/calendars?provider=google", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := do(tc.method, tc.path, tc.body)
			if res.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409", res.StatusCode)
			}
			var got apiError
			defer res.Body.Close()
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Code != "container_mode" {
				t.Fatalf("code = %q, want container_mode", got.Code)
			}
		})
	}
}

// TestContainerGuardAllowsReadEndpoints は GET /api/config・GET /api/status が
// コンテナモードでもガードされない(プレーンファイル読み取りのみで安全、かつ
// ダッシュボードの案内表示に必要)ことを確認する。
func TestContainerGuardAllowsReadEndpoints(t *testing.T) {
	s := containerModeServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if res := get(t, srv, "test-token", "/api/config", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/config status = %d", res.StatusCode)
	}
	var got StatusResponse
	if res := get(t, srv, "test-token", "/api/status", &got); res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/status status = %d", res.StatusCode)
	}
	if got.Daemon.Mode != "container" {
		t.Fatalf("mode = %q, want container", got.Daemon.Mode)
	}
}
