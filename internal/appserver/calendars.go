package appserver

import (
	"context"
	"net/http"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/clients"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/provider/google"
)

// defaultListCals は Server.ListCals の既定実装。google アカウントの
// CalendarList を取得する(アカウント追加ウィザードでのカレンダー選択用)。
func defaultListCals(ctx context.Context, cfg *config.Config, acct config.Account, dataDir string) ([]google.CalendarListEntry, error) {
	tokens := &auth.TokenStore{Dir: dataDir}
	c, err := clients.BuildGoogleClient(cfg, tokens, acct)
	if err != nil {
		return nil, err
	}
	return c.ListCalendars(ctx)
}

// handleCalendars は GET /api/accounts/{id}/calendars?provider=google を
// 処理する。id は設定に未登録でも良い(追加ウィザードではまだ config に
// アカウントが無い状態でトークンだけ保存済み、というケースがある)。
func (s *Server) handleCalendars(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("provider") != "google" {
		writeErr(w, http.StatusBadRequest, "unsupported_provider",
			"calendar listing is only available for google accounts",
			"microsoft は v1 では primary 固定です")
		return
	}
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "config_read", err.Error(), "")
		return
	}
	acct := config.Account{ID: r.PathValue("id"), Provider: "google"}
	tokens := &auth.TokenStore{Dir: s.DataDir}
	if _, err := tokens.Load(acct.ID); err != nil {
		writeErr(w, http.StatusConflict, "token_missing", err.Error(), "先に認可を実行してください")
		return
	}
	cals, err := s.ListCals(r.Context(), cfg, acct, s.DataDir)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error(),
			"スコープ不足の場合は再認可すると取得できます。カレンダー ID の手入力でも設定できます")
		return
	}
	writeJSON(w, map[string]any{"calendars": cals})
}
