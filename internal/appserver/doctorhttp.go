package appserver

import (
	"bytes"
	"context"
	"net/http"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/clients"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/doctor"
	"github.com/btajp/calsync/internal/model"
)

// handleDoctor は GET /api/doctor を処理する。doctor.Run は SQLite を
// OpenReadOnly で読むため(不変条件: 稼働中の DB へのホスト側アクセスは
// launchd 検出時のみ安全)、launchd 管理外(manual/container/unknown)では
// 409 not_launchd で拒否する。
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if info := s.detectDaemon(r.Context()); info.Mode != "launchd" {
		writeErr(w, http.StatusConflict, "not_launchd",
			"doctor is only available on a launchd-managed setup",
			"launchd 管理外です。./scripts/macos/install-launchd.sh でのセットアップ、または稼働中のデーモンを止めてから CLI の calsync doctor を実行してください")
		return
	}
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "config_read", err.Error(), "")
		return
	}
	tokens := &auth.TokenStore{Dir: s.DataDir}
	probe := s.Probe
	if probe == nil {
		probe = func(ctx context.Context, acct config.Account) error {
			p, err := clients.BuildProvider(cfg, tokens, acct)
			if err != nil {
				return err
			}
			cal := "primary"
			if len(acct.Calendars) > 0 {
				cal = acct.Calendars[0]
			}
			_, err = p.GetCalendarTimezone(ctx, model.CalendarRef{AccountID: acct.ID, CalendarID: cal})
			return err
		}
	}
	var buf bytes.Buffer
	runErr := doctor.Run(r.Context(), cfg, s.DataDir, probe, &buf, s.ConfigPath)
	writeJSON(w, map[string]any{"ok": runErr == nil, "text": buf.String()})
}
