package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestAuthFlowLifecycle(t *testing.T) {
	s, dir := testServer(t)
	release := make(chan struct{})
	s.RunFlow = func(ctx context.Context, ocfg *oauth2.Config, port int) (*oauth2.Token, error) {
		select {
		case <-release:
			return &oauth2.Token{AccessToken: "at", RefreshToken: "rt"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func(path, body string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := post("/api/auth/start", `{"account_id":"new-acct","provider":"microsoft"}`); res.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d", res.StatusCode)
	}
	// 進行中の二重開始は 409
	if res := post("/api/auth/start", `{"account_id":"other","provider":"microsoft"}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("second start = %d", res.StatusCode)
	}
	close(release)
	// done になるまでポーリング
	deadline := time.Now().Add(2 * time.Second)
	for {
		var st struct {
			Phase string `json:"phase"`
		}
		get(t, srv, "test-token", "/api/auth/state", &st)
		if st.Phase == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %s", st.Phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// トークンが保存されている
	if _, err := os.Stat(filepath.Join(dir, "tokens", "new-acct.json")); err != nil {
		t.Fatalf("token not saved: %v", err)
	}
}

// TestAuthStartRejectsInvalidAccountID は F4 の回帰テスト。account_id が
// auth.ValidateAccountID の検証に落ちる(パストラバーサル等)場合、ブラウザ
// 往復を伴う OAuth フロー(RunFlow)を一切起動せず 400 invalid_account_id を
// 即座に返すこと。
func TestAuthStartRejectsInvalidAccountID(t *testing.T) {
	s, _ := testServer(t)
	var flowCalled int32
	s.RunFlow = func(ctx context.Context, ocfg *oauth2.Config, port int) (*oauth2.Token, error) {
		atomic.AddInt32(&flowCalled, 1)
		return &oauth2.Token{AccessToken: "at"}, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, id := range []string{"", "..", "../escape", "a/b"} {
		req, _ := http.NewRequest("POST", srv.URL+"/api/auth/start",
			strings.NewReader(`{"account_id":`+`"`+id+`"`+`,"provider":"microsoft"}`))
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("account_id=%q: status = %d, want 400", id, res.StatusCode)
		}
		var body apiError
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if body.Code != "invalid_account_id" {
			t.Fatalf("account_id=%q: code = %q, want invalid_account_id", id, body.Code)
		}
	}
	// フローは一度も起動されていない(ブラウザ往復前に弾かれている)
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt32(&flowCalled) != 0 {
		t.Fatalf("RunFlow was called %d times, want 0", flowCalled)
	}
}

func TestAuthFlowCancel(t *testing.T) {
	s, _ := testServer(t)
	s.RunFlow = func(ctx context.Context, ocfg *oauth2.Config, port int) (*oauth2.Token, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/auth/start", strings.NewReader(`{"account_id":"x","provider":"microsoft"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(req)
	creq, _ := http.NewRequest("POST", srv.URL+"/api/auth/cancel", nil)
	creq.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(creq)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var st struct {
			Phase string `json:"phase"`
		}
		get(t, srv, "test-token", "/api/auth/state", &st)
		if st.Phase == "error" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %s", st.Phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAuthFlowRestartAfterError は、失敗 → 再試行成功のとき (a) error 後の
// 再 start が受理されること (b) done 到達時に古い error/hint が残っていない
// ことを検証する(レビュー指摘: start のリセット代入に hint が含まれていなかった)。
func TestAuthFlowRestartAfterError(t *testing.T) {
	s, _ := testServer(t)
	var calls int32
	s.RunFlow = func(ctx context.Context, ocfg *oauth2.Config, port int) (*oauth2.Token, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, errors.New("boom")
		}
		return &oauth2.Token{AccessToken: "at2", RefreshToken: "rt2"}, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func(path, body string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := post("/api/auth/start", `{"account_id":"retry-acct","provider":"microsoft"}`); res.StatusCode != http.StatusAccepted {
		t.Fatalf("first start = %d", res.StatusCode)
	}
	// 1 回目の失敗で error になるまでポーリング
	deadline := time.Now().Add(2 * time.Second)
	for {
		var st struct {
			Phase string `json:"phase"`
		}
		get(t, srv, "test-token", "/api/auth/state", &st)
		if st.Phase == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %s (want error)", st.Phase)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// (a) error 後の再 start は 202 で受理される
	if res := post("/api/auth/start", `{"account_id":"retry-acct","provider":"microsoft"}`); res.StatusCode != http.StatusAccepted {
		t.Fatalf("restart = %d", res.StatusCode)
	}

	// (b) done 到達時、error と hint がともに空(古い失敗の残留がない)
	deadline = time.Now().Add(2 * time.Second)
	for {
		var st struct {
			Phase string `json:"phase"`
			Error string `json:"error"`
			Hint  string `json:"hint"`
		}
		get(t, srv, "test-token", "/api/auth/state", &st)
		if st.Phase == "done" {
			if st.Error != "" || st.Hint != "" {
				t.Fatalf("stale error/hint after restart: error=%q hint=%q", st.Error, st.Hint)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %s (want done)", st.Phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
