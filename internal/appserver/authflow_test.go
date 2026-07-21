package appserver

import (
	"context"
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
