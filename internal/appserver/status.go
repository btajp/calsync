package appserver

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/store"
)

// TokenStatus はアカウント 1 件分のトークン保存状態。
type TokenStatus struct {
	AccountID string `json:"account_id"`
	State     string `json:"state"` // "ok" | "missing" | "no_refresh_token"
}

// CalendarStatus はカレンダー 1 件分の同期状態(store.CalendarState から変換)。
type CalendarStatus struct {
	AccountID  string `json:"account_id"`
	CalendarID string `json:"calendar_id"`
	LastSync   string `json:"last_sync"` // RFC3339 or ""
	Status     string `json:"status"`    // "ok" or エラー文字列
}

// StatusResponse は GET /api/status のレスポンス。
type StatusResponse struct {
	Daemon    DaemonInfo       `json:"daemon"`
	Tokens    []TokenStatus    `json:"tokens"`
	Calendars []CalendarStatus `json:"calendars,omitempty"`
	DBNote    string           `json:"db_note,omitempty"` // DB を読まなかった/読めなかった理由
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	daemon := s.detectDaemon(r.Context())
	resp := StatusResponse{Daemon: daemon}

	// トークン状態はファイルベース(DB 非依存)なので常に返す
	if cfg, err := config.Load(s.ConfigPath); err == nil {
		tokens := &auth.TokenStore{Dir: s.DataDir}
		for _, acct := range cfg.Accounts {
			st := "ok"
			tok, terr := tokens.Load(acct.ID)
			switch {
			case terr != nil:
				st = "missing"
			case tok.RefreshToken == "":
				st = "no_refresh_token"
			}
			resp.Tokens = append(resp.Tokens, TokenStatus{AccountID: acct.ID, State: st})
		}
	}

	// DB は launchd 検出時のみ・読み取り専用で開く(仕様 §4/§9 の不変条件)
	switch {
	case daemon.Mode != "launchd":
		resp.DBNote = "db skipped: not a launchd-managed setup"
	default:
		if _, err := os.Stat(filepath.Join(s.DataDir, "calsync.db")); os.IsNotExist(err) {
			resp.DBNote = "no local DB yet"
			break
		}
		st, err := store.OpenReadOnly(s.DataDir)
		if err != nil {
			resp.DBNote = "db open failed: " + err.Error()
			break
		}
		defer st.Close()
		states, err := st.ListCalendars()
		if err != nil {
			resp.DBNote = "db read failed: " + err.Error()
			break
		}
		for _, cs := range states {
			last := ""
			if !cs.LastSyncedAt.IsZero() {
				last = cs.LastSyncedAt.UTC().Format(time.RFC3339)
			}
			status := "ok"
			if cs.LastError != "" {
				status = cs.LastError
			}
			resp.Calendars = append(resp.Calendars, CalendarStatus{
				AccountID: cs.Ref.AccountID, CalendarID: cs.Ref.CalendarID,
				LastSync: last, Status: status,
			})
		}
	}
	writeJSON(w, resp)
}
