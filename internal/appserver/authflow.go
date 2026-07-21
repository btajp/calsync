package appserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/clients"
	"github.com/btajp/calsync/internal/config"
)

// authState は進行中の OAuth 認可フロー1本分の状態。goroutine(handleAuthStart
// が起動する認可フロー本体)とリクエストハンドラ(state/cancel)の両方から
// 触られるため、フィールドは必ず mu で保護すること。
type authState struct {
	mu        sync.Mutex
	phase     string // "idle" | "running" | "done" | "error"
	accountID string
	errMsg    string
	hint      string
	cancel    context.CancelFunc
}

// handleAuthStart は POST /api/auth/start。ループバック OAuth フローを
// バックグラウンドで開始し、直ちに 202 を返す。進行中に再度呼ぶと 409。
func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID string `json:"account_id"`
		Provider  string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), "")
		return
	}
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "config_read", err.Error(), "")
		return
	}
	acct := config.Account{ID: body.AccountID, Provider: body.Provider}
	ocfg, err := clients.OAuthConfigFor(cfg, acct)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "oauth_config", err.Error(),
			"providers 設定(credentials_file / client_id)を確認してください")
		return
	}

	s.authSt.mu.Lock()
	if s.authSt.phase == "running" {
		s.authSt.mu.Unlock()
		writeErr(w, http.StatusConflict, "auth_in_progress", "another authorization is running", "")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	s.authSt.phase, s.authSt.accountID, s.authSt.errMsg, s.authSt.hint, s.authSt.cancel = "running", body.AccountID, "", "", cancel
	s.authSt.mu.Unlock()

	go func() {
		defer cancel()
		tok, err := s.RunFlow(ctx, ocfg, 0)
		s.authSt.mu.Lock()
		defer s.authSt.mu.Unlock()
		if err != nil {
			s.authSt.phase = "error"
			s.authSt.errMsg = err.Error()
			s.authSt.hint = "ブラウザでの認可が完了しませんでした。再試行してください"
			return
		}
		tokens := &auth.TokenStore{Dir: s.DataDir}
		if err := tokens.Save(body.AccountID, tok); err != nil {
			s.authSt.phase = "error"
			s.authSt.errMsg = err.Error()
			return
		}
		s.authSt.phase = "done"
	}()
	// Content-Type は WriteHeader の前に設定する: writeJSON 内の Set はヘッダ送出後
	// のため無効になり、応答が text/plain になってしまう(レビュー指摘)。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"ok": true})
}

// handleAuthState は GET /api/auth/state。トークン値・認可 URL は含めず、
// phase/account_id/error/hint のみをスナップショットで返す。
func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	s.authSt.mu.Lock()
	resp := map[string]string{
		"phase":      s.authSt.phase,
		"account_id": s.authSt.accountID,
		"error":      s.authSt.errMsg,
		"hint":       s.authSt.hint,
	}
	s.authSt.mu.Unlock()
	writeJSON(w, resp)
}

// handleAuthCancel は POST /api/auth/cancel。進行中のフローがあれば
// context をキャンセルする。フロー側の goroutine が ctx.Err() を受けて
// phase を "error" に遷移させる。
func (s *Server) handleAuthCancel(w http.ResponseWriter, r *http.Request) {
	s.authSt.mu.Lock()
	cancel := s.authSt.cancel
	s.authSt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	writeJSON(w, map[string]bool{"ok": true})
}
