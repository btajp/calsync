// Package appserver はデスクトップアプリ向けの localhost 限定 HTTP API を提供する。
// 不変条件: SQLite は OpenReadOnly のみ・launchd 検出時のみ。書き込みはファイル
// (YAML・トークン)に限る。認証は起動ごとのワンタイム Bearer トークン。
package appserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/doctor"
	"github.com/btajp/calsync/internal/provider/google"
)

// Server は appserver の状態と依存を保持する。
type Server struct {
	ConfigPath string
	DataDir    string
	Token      string
	Runner     CmdRunner
	UID        int
	PlistPath  string
	// LookPath は "docker" 等の存在確認に使う(既定 exec.LookPath)。CI 等
	// docker が PATH に無い環境でも detectDaemon を決定的にするためテストで
	// 差し替え可能にしてある。
	LookPath func(string) (string, error)
	// RunFlow は OAuth 認可フロー本体(既定 auth.RunLoopbackFlow)。テストは
	// フェイクを注入してブラウザ操作なしに検証する。
	RunFlow func(ctx context.Context, ocfg *oauth2.Config, port int) (*oauth2.Token, error)
	// ListCals は GET /api/accounts/{id}/calendars の実体(既定
	// defaultListCals = clients.BuildGoogleClient → ListCalendars)。テストは
	// フェイクを注入して実 API 呼び出しなしに検証する。
	ListCals func(ctx context.Context, cfg *config.Config, acct config.Account, dataDir string) ([]google.CalendarListEntry, error)
	// Probe は GET /api/doctor の API 疎通確認(既定は nil。handleDoctor が
	// cmd_doctor.go と同じ実装を都度組み立てる)。テストはフェイクを注入する。
	Probe  doctor.Probe
	authSt authState
}

// New は既定の依存(実 exec ベースの Runner・os.Getuid・既定 plist パス)で
// Server を組み立てる。
func New(configPath, dataDir, token string) *Server {
	home, _ := os.UserHomeDir()
	return &Server{
		ConfigPath: configPath,
		DataDir:    dataDir,
		Token:      token,
		Runner:     execRunner{},
		UID:        os.Getuid(),
		PlistPath:  filepath.Join(home, "Library", "LaunchAgents", "com.btajp.calsync.plist"),
		LookPath:   exec.LookPath,
		RunFlow:    auth.RunLoopbackFlow,
		ListCals:   defaultListCals,
		authSt:     authState{phase: "idle"},
	}
}

// Handler は全エンドポイントを requireToken でラップして返す。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/daemon/{action}", s.handleDaemonAction)
	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("PUT /api/config", s.handleConfigPut)
	mux.HandleFunc("POST /api/auth/start", s.handleAuthStart)
	mux.HandleFunc("GET /api/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /api/auth/cancel", s.handleAuthCancel)
	mux.HandleFunc("GET /api/accounts/{id}/calendars", s.handleCalendars)
	mux.HandleFunc("GET /api/doctor", s.handleDoctor)
	return s.requireToken(mux)
}

// requireToken は Bearer トークン一致以外を一律 401 にする。
// 比較は subtle.ConstantTimeCompare(タイミング攻撃対策)。
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or missing token", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func writeErr(w http.ResponseWriter, status int, code, message, hint string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Code: code, Message: message, Hint: hint})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Serve は ln で HTTP を提供し、開始直後に {"port":N,"token":"..."} を out に
// 1 行 JSON で書く(親の殻がこれを読んでハンドシェイクする)。ctx キャンセルで
// graceful shutdown する。
func (s *Server) Serve(ctx context.Context, ln net.Listener, out io.Writer) error {
	hs, _ := json.Marshal(map[string]any{"port": ln.Addr().(*net.TCPAddr).Port, "token": s.Token})
	fmt.Fprintln(out, string(hs))
	srv := &http.Server{Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// WatchStdinEOF は親(Tauri 殻)の死亡を stdin の EOF で検知して cancel を呼ぶ。
// サイドカーの孤児化防止の最後の砦(仕様 §5)。
func WatchStdinEOF(r io.Reader, cancel context.CancelFunc) {
	go func() {
		_, _ = io.Copy(io.Discard, r)
		cancel()
	}()
}

// GenerateToken は起動ごとのワンタイムトークンを作る(32 バイト乱数の hex)。
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
