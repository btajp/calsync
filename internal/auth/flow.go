package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// RunLoopbackFlow は認可コード + PKCE + ループバックリダイレクトの OAuth フローを実行する。
// port==0 ならランダムポート(仕様書9.2 の --port はここに固定値を渡す)。
// 認可 URL を標準出力に表示し、ブラウザ起動を試みる(失敗しても URL 表示で続行)。
func RunLoopbackFlow(ctx context.Context, cfg *oauth2.Config, port int) (*oauth2.Token, error) {
	return runLoopbackFlow(ctx, cfg, port, os.Stdout, openBrowser, nil)
}

type flowResult struct {
	tok *oauth2.Token
	err error
}

// runLoopbackFlow は実体。authURLCh が非 nil なら認可 URL を送る(テスト用の観測口)。
func runLoopbackFlow(ctx context.Context, cfg *oauth2.Config, port int, out io.Writer, openURL func(string) error, authURLCh chan<- string) (*oauth2.Token, error) {
	if cfg == nil {
		return nil, errors.New("auth: oauth2 config is nil")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen on loopback port %d: %w", port, err)
	}
	defer ln.Close()
	actualPort := ln.Addr().(*net.TCPAddr).Port

	// リスナーは常に 127.0.0.1。リダイレクト URL のホスト名表記のみ cfg に従う
	// (Microsoft は "http://localhost" — localhost はポート無視でマッチする公式仕様。仕様書9.1)。
	host := "127.0.0.1"
	if cfg.RedirectURL != "" {
		if u, perr := url.Parse(cfg.RedirectURL); perr == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
	}

	conf := *cfg // 呼び出し元の Config は変更しない
	conf.RedirectURL = fmt.Sprintf("http://%s:%d/callback", host, actualPort)

	state, err := randomState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	verifier := oauth2.GenerateVerifier()
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier))

	resultCh := make(chan flowResult, 1)
	var once sync.Once
	deliver := func(res flowResult) { once.Do(func() { resultCh <- res }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// state 不一致(favicon 等のノイズを含む)は 400 で拒否し、フローは継続する
		if q.Get("state") != state {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		if ec := q.Get("error"); ec != "" {
			http.Error(w, "authorization failed: "+ec, http.StatusBadRequest)
			deliver(flowResult{err: fmt.Errorf("authorization failed: %s (%s)", ec, q.Get("error_description"))})
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			return
		}
		tok, xerr := conf.Exchange(ctx, code, oauth2.VerifierOption(verifier))
		if xerr != nil {
			http.Error(w, "token exchange failed", http.StatusInternalServerError)
			deliver(flowResult{err: fmt.Errorf("exchange authorization code: %w", xerr)})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "calsync: authentication complete. You can close this window.")
		deliver(flowResult{tok: tok})
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln) //nolint:errcheck // Close 由来の ErrServerClosed は無視してよい
	defer srv.Close()

	fmt.Fprintf(out, "Open this URL in your browser to authorize calsync:\n\n  %s\n\n", authURL)
	if openURL != nil {
		if berr := openURL(authURL); berr != nil {
			// ブラウザ起動失敗は致命ではない(Docker / SSH 環境)。URL 表示で続行する
			fmt.Fprintf(out, "could not open browser automatically (%v) - open the URL above manually\n", berr)
		}
	}
	if authURLCh != nil {
		authURLCh <- authURL
	}

	select {
	case res := <-resultCh:
		return res.tok, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// openBrowser は OS 既定ブラウザで URL を開く(ベストエフォート)。
func openBrowser(rawURL string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", rawURL).Start()
	case "linux":
		return exec.Command("xdg-open", rawURL).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
}

// RunDeviceFlow は Device Code Flow を実行する(Microsoft のみ。仕様書9.1)。
// 呼び出し元は cfg.Endpoint.DeviceAuthURL を設定しておくこと。
func RunDeviceFlow(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	return runDeviceFlow(ctx, cfg, os.Stdout)
}

func runDeviceFlow(ctx context.Context, cfg *oauth2.Config, out io.Writer) (*oauth2.Token, error) {
	if cfg == nil {
		return nil, errors.New("auth: oauth2 config is nil")
	}
	resp, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("start device authorization: %w", err)
	}
	fmt.Fprintf(out, "To sign in, open %s in a browser and enter the code: %s\n", resp.VerificationURI, resp.UserCode)
	if resp.VerificationURIComplete != "" {
		fmt.Fprintf(out, "Or open this URL directly: %s\n", resp.VerificationURIComplete)
	}
	// DeviceAccessToken はサーバ指定の interval に従い authorization_pending の間
	// ポーリングを続ける。slow_down / expired_token も x/oauth2 側で処理される。
	tok, err := cfg.DeviceAccessToken(ctx, resp)
	if err != nil {
		return nil, fmt.Errorf("wait for device authorization: %w", err)
	}
	return tok, nil
}
