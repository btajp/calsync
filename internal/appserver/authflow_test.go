package appserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
