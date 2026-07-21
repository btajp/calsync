package appserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/clients"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
	"github.com/btajp/calsync/internal/store"
)

// maxEventsWindow は GET /api/events が受け付ける窓の最大幅(月ビュー+前後余白を
// 包含する 62 日。デスクトップカレンダービュー設計 2026-07-21 §4)。逸脱は 400。
const maxEventsWindow = 62 * 24 * time.Hour

// eventsCacheTTL は同一窓の連続取得を抑える appserver 内メモリキャッシュの TTL
// (ビュー切替の連打対策。手動更新は refresh=1 でバイパスする。スペック §4)。
const eventsCacheTTL = 60 * time.Second

// EventOut は GET /api/events の 1 件(engine.DigestEntry の JSON 写像。スペック §4)。
type EventOut struct {
	AccountID   string   `json:"account_id"`  // 代表アカウント = AccountIDs[0]
	AccountIDs  []string `json:"account_ids"` // dedupe 統合後の全由来アカウント
	Title       string   `json:"title"`
	Start       string   `json:"start"` // RFC3339
	End         string   `json:"end"`   // RFC3339
	AllDay      bool     `json:"all_day"`
	AllDayStart string   `json:"all_day_start"` // YYYY-MM-DD(AllDay時のみ)
	MeetingURL  string   `json:"meeting_url"`
	HTMLLink    string   `json:"html_link"`
}

// EventsResponse は GET /api/events のレスポンス全体。
type EventsResponse struct {
	Events []EventOut `json:"events"`
	Failed []string   `json:"failed"`
}

type eventsCacheKey struct {
	from string
	to   string
}

type eventsCacheEntry struct {
	resp    EventsResponse
	expires time.Time
}

// handleEvents は GET /api/events?from=<RFC3339>&to=<RFC3339>&refresh=<0|1> を
// 処理する(スペック §4)。ブロッカー除外の一次判定に mappings(SQLite・
// OpenReadOnly)が必要なため、doctor と同じく launchd 管理外は 409 で拒否する
// (container はここで Mode が "container" になり同じく 409 not_launchd になる)。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if info := s.detectDaemon(r.Context()); info.Mode != "launchd" {
		writeErr(w, http.StatusConflict, "not_launchd",
			"events is only available on a launchd-managed setup",
			"launchd 管理外です。./scripts/macos/install-launchd.sh でのセットアップ、または稼働中のデーモンを止めてから CLI を使ってください")
		return
	}

	q := r.URL.Query()
	from, err := time.Parse(time.RFC3339, q.Get("from"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_window", "from must be a valid RFC3339 timestamp", "")
		return
	}
	to, err := time.Parse(time.RFC3339, q.Get("to"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_window", "to must be a valid RFC3339 timestamp", "")
		return
	}
	if !from.Before(to) {
		writeErr(w, http.StatusBadRequest, "invalid_window", "from must be before to", "")
		return
	}
	if to.Sub(from) > maxEventsWindow {
		writeErr(w, http.StatusBadRequest, "invalid_window", "window must not exceed 62 days", "")
		return
	}

	refresh := q.Get("refresh") == "1"
	key := eventsCacheKey{from: from.Format(time.RFC3339), to: to.Format(time.RFC3339)}
	now := time.Now()
	if !refresh {
		if resp, ok := s.eventsCacheGet(key, now); ok {
			writeJSON(w, resp)
			return
		}
	}

	collect := s.CollectEvents
	if collect == nil {
		collect = s.defaultCollectEvents
	}
	entries, failed, err := collect(r.Context(), model.Window{Start: from, End: to})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "collect_failed", err.Error(), "")
		return
	}
	resp := EventsResponse{Events: toEventOut(entries), Failed: failed}
	if resp.Failed == nil {
		resp.Failed = []string{}
	}
	s.eventsCacheSet(key, resp, now)
	writeJSON(w, resp)
}

func (s *Server) eventsCacheGet(key eventsCacheKey, now time.Time) (EventsResponse, bool) {
	s.eventsCacheMu.Lock()
	defer s.eventsCacheMu.Unlock()
	e, ok := s.eventsCache[key]
	if !ok || now.After(e.expires) {
		return EventsResponse{}, false
	}
	return e.resp, true
}

func (s *Server) eventsCacheSet(key eventsCacheKey, resp EventsResponse, now time.Time) {
	s.eventsCacheMu.Lock()
	defer s.eventsCacheMu.Unlock()
	if s.eventsCache == nil {
		s.eventsCache = make(map[eventsCacheKey]eventsCacheEntry)
	}
	s.eventsCache[key] = eventsCacheEntry{resp: resp, expires: now.Add(eventsCacheTTL)}
}

// toEventOut は engine.DigestEntry を API レスポンスの形へ写像する(スペック §4)。
func toEventOut(entries []engine.DigestEntry) []EventOut {
	out := make([]EventOut, 0, len(entries))
	for _, en := range entries {
		accountID := ""
		if len(en.AccountIDs) > 0 {
			accountID = en.AccountIDs[0]
		}
		out = append(out, EventOut{
			AccountID:   accountID,
			AccountIDs:  en.AccountIDs,
			Title:       en.Title,
			Start:       en.StartUTC.UTC().Format(time.RFC3339),
			End:         en.EndUTC.UTC().Format(time.RFC3339),
			AllDay:      en.IsAllDay,
			AllDayStart: en.AllDayStart,
			MeetingURL:  en.MeetingURL,
			HTMLLink:    en.HTMLLink,
		})
	}
	return out
}

// defaultCollectEvents は Server.CollectEvents の既定実装(スペック §2/§3/§4)。
// config.Load → store.OpenReadOnly(読み取り専用) → 読み取り専用 Engine を
// 組み立てて CollectWindow に委譲する。トークン欠落等でプロバイダを構築できない
// アカウントは providers マップに登録しないだけでよい — Engine.CollectWindow は
// providerFor が見つからないアカウントを自動的に failed へ足す(内部で共有する
// ダイジェスト収集ロジックと同じ経路)。
func (s *Server) defaultCollectEvents(ctx context.Context, w model.Window) ([]engine.DigestEntry, []string, error) {
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("config load: %w", err)
	}
	st, err := store.OpenReadOnly(s.DataDir)
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	tokens := &auth.TokenStore{Dir: s.DataDir}
	providers := make(map[string]provider.Provider, len(cfg.Accounts))
	for _, acct := range cfg.Accounts {
		p, err := clients.BuildReadOnlyProvider(cfg, tokens, acct)
		if err != nil {
			continue // トークン欠落等 → 登録しない。CollectWindow が failed に足す
		}
		providers[acct.ID] = p
	}
	eng := &engine.Engine{Store: st, Providers: providers, Cfg: cfg, Now: time.Now}
	entries, failed := eng.CollectWindow(ctx, w)
	return entries, failed, nil
}
