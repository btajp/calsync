package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestRunLoopbackFlow(t *testing.T) {
	// httptest 側 token エンドポイント。受信内容を記録して後で検証する
	type tokenRequest struct {
		grantType   string
		code        string
		verifier    string
		redirectURI string
	}
	var (
		mu        sync.Mutex
		tokenReqs []tokenRequest
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		tokenReqs = append(tokenReqs, tokenRequest{
			grantType:   r.Form.Get("grant_type"),
			code:        r.Form.Get("code"),
			verifier:    r.Form.Get("code_verifier"),
			redirectURI: r.Form.Get("redirect_uri"),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"atoken","token_type":"Bearer","refresh_token":"rtoken","expires_in":3600}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// oauth2.Config の Endpoint を httptest サーバに向ける
	cfg := &oauth2.Config{
		ClientID: "test-client",
		Endpoint: oauth2.Endpoint{
			AuthURL:   srv.URL + "/authorize",
			TokenURL:  srv.URL + "/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		Scopes: []string{"https://www.googleapis.com/auth/calendar.events"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type flowRes struct {
		tok *oauth2.Token
		err error
	}
	authURLCh := make(chan string, 1)
	resCh := make(chan flowRes, 1)
	browserErr := errors.New("no browser in test")
	var out bytes.Buffer
	go func() {
		tok, err := runLoopbackFlow(ctx, cfg, 0, &out,
			func(string) error { return browserErr }, // ブラウザ起動は常に失敗させる
			authURLCh)
		resCh <- flowRes{tok: tok, err: err}
	}()

	// 表示された認可 URL をチャネルから取得
	var authURL string
	select {
	case authURL = <-authURLCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for authorization URL")
	}

	u, err := url.Parse(authURL)
	require.NoError(t, err)
	q := u.Query()
	state := q.Get("state")
	challenge := q.Get("code_challenge")
	redirect := q.Get("redirect_uri")
	require.NotEmpty(t, state)
	require.NotEmpty(t, challenge)
	require.NotEmpty(t, redirect)
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	require.Equal(t, "test-client", q.Get("client_id"))

	ru, err := url.Parse(redirect)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", ru.Hostname())
	require.NotEmpty(t, ru.Port())
	require.NotEqual(t, "0", ru.Port()) // port==0 指定でも実ポートが割り当てられている

	// 不正コールバックはすべて 400 で拒否され、フローは完了しない
	badCallbacks := []struct {
		name  string
		query string
	}{
		{name: "wrong state", query: "?state=WRONG&code=evil"},
		{name: "missing state", query: "?code=evil"},
		{name: "missing code", query: "?state=" + url.QueryEscape(state)},
	}
	for _, tc := range badCallbacks {
		resp, gerr := http.Get(redirect + tc.query)
		require.NoError(t, gerr, tc.name)
		require.NoError(t, resp.Body.Close(), tc.name)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, tc.name)
	}
	select {
	case r := <-resCh:
		t.Fatalf("flow completed after invalid callback: tok=%v err=%v", r.tok, r.err)
	case <-time.After(200 * time.Millisecond):
		// 完了していない = 期待どおり
	}
	mu.Lock()
	require.Empty(t, tokenReqs, "invalid callback must not reach token exchange")
	mu.Unlock()

	// 正しい state + code のコールバックで完了する
	resp, err := http.Get(redirect + "?state=" + url.QueryEscape(state) + "&code=good-code")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var res flowRes
	select {
	case res = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for flow result")
	}
	require.NoError(t, res.err)
	require.Equal(t, "atoken", res.tok.AccessToken)
	require.Equal(t, "rtoken", res.tok.RefreshToken)

	// PKCE: token エンドポイントへ code_verifier が送られ、
	// S256(code_verifier) が認可 URL の code_challenge と一致する
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, tokenReqs, 1)
	tr := tokenReqs[0]
	require.Equal(t, "authorization_code", tr.grantType)
	require.Equal(t, "good-code", tr.code)
	require.Equal(t, redirect, tr.redirectURI)
	require.NotEmpty(t, tr.verifier)
	sum := sha256.Sum256([]byte(tr.verifier))
	require.Equal(t, challenge, base64.RawURLEncoding.EncodeToString(sum[:]))

	// 認可 URL は out(標準出力相当)に表示され、ブラウザ起動失敗でも続行している
	require.Contains(t, out.String(), authURL)
	require.Contains(t, out.String(), browserErr.Error())
}

func TestRunLoopbackFlowContextCancel(t *testing.T) {
	cfg := &oauth2.Config{
		ClientID: "test-client",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "http://127.0.0.1:1/authorize", // 実際には接続しない
			TokenURL: "http://127.0.0.1:1/token",
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	authURLCh := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, err := runLoopbackFlow(ctx, cfg, 0, io.Discard, nil, authURLCh)
		done <- err
	}()

	select {
	case <-authURLCh: // フロー起動(リスナー確立)を確認してからキャンセル
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for authorization URL")
	}
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("flow did not stop on context cancel")
	}
}

func TestRunDeviceFlow(t *testing.T) {
	var (
		mu          sync.Mutex
		tokenCalls  int
		grantTypes  []string
		deviceCodes []string
		clientIDs   []string
	)
	mux := http.NewServeMux()
	// device_authorization エンドポイント: user_code / verification_uri / interval を返す
	mux.HandleFunc("/devicecode", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		clientIDs = append(clientIDs, r.Form.Get("client_id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://microsoft.com/devicelogin","expires_in":900,"interval":1}`)
	})
	// token エンドポイント: 1回目 authorization_pending → 2回目成功
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		tokenCalls++
		n := tokenCalls
		grantTypes = append(grantTypes, r.Form.Get("grant_type"))
		deviceCodes = append(deviceCodes, r.Form.Get("device_code"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// RFC 8628: まだユーザーが認可していない
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"authorization_pending"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"dev-atoken","token_type":"Bearer","refresh_token":"dev-rtoken","expires_in":3600}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &oauth2.Config{
		ClientID: "test-client",
		Endpoint: oauth2.Endpoint{
			AuthURL:       srv.URL + "/authorize",
			TokenURL:      srv.URL + "/token",
			DeviceAuthURL: srv.URL + "/devicecode",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
		Scopes: []string{"offline_access", "https://graph.microsoft.com/Calendars.ReadWrite"},
	}

	// interval=1 秒 × 2 回ポーリングのため余裕を持たせる
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var out bytes.Buffer
	tok, err := runDeviceFlow(ctx, cfg, &out)
	require.NoError(t, err)
	require.Equal(t, "dev-atoken", tok.AccessToken)
	require.Equal(t, "dev-rtoken", tok.RefreshToken)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"test-client"}, clientIDs)
	require.Equal(t, 2, tokenCalls, "authorization_pending must trigger exactly one retry")
	for i, gt := range grantTypes {
		require.Equal(t, "urn:ietf:params:oauth:grant-type:device_code", gt, "call %d", i+1)
		require.Equal(t, "dc-1", deviceCodes[i], "call %d", i+1)
	}

	// user_code と verification_uri が表示される(仕様書9.2: コードと URL の表示で完結)
	require.Contains(t, out.String(), "ABCD-1234")
	require.Contains(t, out.String(), "https://microsoft.com/devicelogin")
}
