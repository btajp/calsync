package appserver

import (
	"context"
	"fmt"
	"log"
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

// eventsCacheStaleMax は TTL 切れエントリを stale-while-revalidate に流用できる
// 追加猶予(2026-07-28 体感速度対策)。TTL 切れでも猶予内なら古い内容を即座に返し、
// バックグラウンドで取り直す(レスポンスに stale: true を付け、フロントが少し後に
// 再取得して最新化する)。猶予も過ぎたエントリは通常のミスとして同期取得する。
const eventsCacheStaleMax = 30 * time.Minute

// eventsRefreshTimeout はバックグラウンド更新 1 回の上限。リクエストの ctx とは
// 独立(stale 応答を返した時点でリクエストは完了しているため)。
const eventsRefreshTimeout = 60 * time.Second

// EventOut は GET /api/events の 1 件(engine.DigestEntry の JSON 写像。スペック §4)。
type EventOut struct {
	AccountID   string   `json:"account_id"`  // 代表アカウント = AccountIDs[0]
	AccountIDs  []string `json:"account_ids"` // dedupe 統合後の全由来アカウント
	Title       string   `json:"title"`
	Start       string   `json:"start"` // RFC3339
	End         string   `json:"end"`   // RFC3339
	AllDay      bool     `json:"all_day"`
	AllDayStart string   `json:"all_day_start"` // YYYY-MM-DD(AllDay時のみ)
	AllDayEnd   string   `json:"all_day_end"`   // 排他的終了日・YYYY-MM-DD(AllDay時、複数日イベントのみ非空)
	MeetingURL  string   `json:"meeting_url"`
	HTMLLink    string   `json:"html_link"`
	Description string   `json:"description,omitempty"` // プレーンテキスト(Graph は Prefer text / Google は HTML 除去済み。デスクトップ予定詳細設計 2026-07-24 §2)
}

// EventsResponse は GET /api/events のレスポンス全体。
type EventsResponse struct {
	Events []EventOut `json:"events"`
	Failed []string   `json:"failed"`
	// Stale は TTL 切れキャッシュを stale-while-revalidate で返したとき true。
	// バックグラウンドで最新化が進行中なので、フロントは少し後に再取得するとよい。
	Stale bool `json:"stale,omitempty"`
}

type eventsCacheKey struct {
	from string
	to   string
}

type eventsCacheEntry struct {
	resp    EventsResponse
	expires time.Time
	// gen はエントリ書き込みごとに増える世代番号。バックグラウンド更新(SWR)が
	// 開始時点の世代を控え、完了時に世代が変わっていたら書き込みを捨てるための
	// もの(refresh=1 の同期取得が先に新しい結果を書いた場合に、古いスナップ
	// ショットで上書きする lost-update を防ぐ。レビュー指摘)。
	gen uint64
}

// handleEvents は GET /api/events?from=<RFC3339>&to=<RFC3339>&refresh=<0|1> を
// 処理する(スペック §4)。ブロッカー除外の一次判定に mappings(SQLite・
// OpenReadOnly)が必要なため、doctor と同じく launchd 管理外は 409 で拒否する
// (container はここで Mode が "container" になり同じく 409 not_launchd になる)。
//
// タイムゾーン契約: from/to は閲覧者のローカルオフセット付き RFC3339 で送ること
// (例 "2026-07-21T00:00:00+09:00")。終日イベントの日付境界(model.Window ベース
// の CollectWindow の終日交差判定)は from/to が保持するオフセットの現地日付で
// 解釈される。UTC(例 "...Z")を送ると、UTC の日付境界と閲覧者の実際のカレンダー
// 日付境界がずれる TZ(JST 等)では終日イベントの表示日が 1 日ずれる。
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
		resp, fresh, stale := s.eventsCacheGet(key, now)
		if fresh {
			writeJSON(w, resp)
			return
		}
		if stale {
			// stale-while-revalidate: 古い内容を即返し、裏で取り直す(2026-07-28
			// 体感速度対策。プロバイダのライブ取得は数秒かかるため、まず前回の
			// 結果で描画してもらう)。
			resp.Stale = true
			s.refreshEventsAsync(key, from, to)
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

// eventsCacheGet はキャッシュを引く。fresh は TTL 内、stale は TTL 切れだが
// eventsCacheStaleMax の猶予内(stale-while-revalidate に使える)。両方 false なら
// エントリ無し(またはstale猶予も超過)で、呼び出し側が同期取得する。
func (s *Server) eventsCacheGet(key eventsCacheKey, now time.Time) (resp EventsResponse, fresh bool, stale bool) {
	s.eventsCacheMu.Lock()
	defer s.eventsCacheMu.Unlock()
	e, ok := s.eventsCache[key]
	if !ok || now.After(e.expires.Add(eventsCacheStaleMax)) {
		return EventsResponse{}, false, false
	}
	if now.After(e.expires) {
		return e.resp, false, true
	}
	return e.resp, true, false
}

func (s *Server) eventsCacheSet(key eventsCacheKey, resp EventsResponse, now time.Time) {
	s.eventsCacheMu.Lock()
	defer s.eventsCacheMu.Unlock()
	s.eventsCacheSetLocked(key, resp, now)
}

// eventsCacheSetLocked は eventsCacheSet の本体(eventsCacheMu 保持前提)。
func (s *Server) eventsCacheSetLocked(key eventsCacheKey, resp EventsResponse, now time.Time) {
	if s.eventsCache == nil {
		s.eventsCache = make(map[eventsCacheKey]eventsCacheEntry)
	}
	// 窓を変えながらのビュー切替を繰り返すと eventsCache のキー(from/to の
	// 組)が際限なく増える(全キーが期限切れでも参照されない限りマップに
	// 残り続ける)。書き込みのたびに期限切れエントリを掃除して、無制限に
	// メモリを積み上げないようにする。TTL 切れ直後は stale-while-revalidate に
	// 使うため、掃除の基準は stale 猶予(eventsCacheStaleMax)も過ぎたもの。
	for k, e := range s.eventsCache {
		if now.After(e.expires.Add(eventsCacheStaleMax)) {
			delete(s.eventsCache, k)
		}
	}
	s.eventsCacheGen++
	s.eventsCache[key] = eventsCacheEntry{resp: resp, expires: now.Add(eventsCacheTTL), gen: s.eventsCacheGen}
}

// eventsCacheSetIfGen は key の世代が expect のままのときだけ書き込む
// (compare-and-set。チェックと書き込みを同一ロック内で行う)。他の書き込み
// (refresh=1 の同期取得等)が先行していた場合は false を返して何もしない。
func (s *Server) eventsCacheSetIfGen(key eventsCacheKey, resp EventsResponse, now time.Time, expect uint64) bool {
	s.eventsCacheMu.Lock()
	defer s.eventsCacheMu.Unlock()
	if s.eventsCache[key].gen != expect {
		return false
	}
	s.eventsCacheSetLocked(key, resp, now)
	return true
}

// refreshEventsAsync は stale 応答を返した窓をバックグラウンドで取り直し、成功時に
// キャッシュを最新化する。同一キーの更新は single-flight(進行中なら何もしない)。
// リクエストの ctx から独立した専用 ctx を使う(呼び出し元のリクエストは stale 応答で
// 既に完了しているため。maintenance と同じ理由で goroutine は panic を回収する)。
func (s *Server) refreshEventsAsync(key eventsCacheKey, from, to time.Time) {
	s.eventsCacheMu.Lock()
	if s.eventsRefreshing == nil {
		s.eventsRefreshing = make(map[eventsCacheKey]bool)
	}
	if s.eventsRefreshing[key] {
		s.eventsCacheMu.Unlock()
		return
	}
	s.eventsRefreshing[key] = true
	// 開始時点の世代を控える。完了時に世代が進んでいたら(refresh=1 の同期取得等が
	// 先に書いた)、こちらの古いスナップショットでの上書きを捨てる(レビュー指摘の
	// lost-update 防止)。
	startGen := s.eventsCache[key].gen
	s.eventsCacheMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("events refresh panic: %v", r)
			}
			s.eventsCacheMu.Lock()
			delete(s.eventsRefreshing, key)
			s.eventsCacheMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), eventsRefreshTimeout)
		defer cancel()
		collect := s.CollectEvents
		if collect == nil {
			collect = s.defaultCollectEvents
		}
		entries, failed, err := collect(ctx, model.Window{Start: from, End: to})
		if err != nil {
			log.Printf("events refresh %s..%s: %v", key.from, key.to, err)
			return
		}
		// 全滅ガード: 1 件も取れず失敗アカウントがある(典型はネットワーク断)結果で
		// 既存の正常な stale エントリを上書きすると、直前まで表示できていた予定一覧が
		// 無警告で「予定なし」に差し替わる(PanelApp は failed を表示しない)。この
		// 場合は既存エントリを温存し、次の同期取得に任せる(レビュー指摘)。
		if len(entries) == 0 && len(failed) > 0 {
			log.Printf("events refresh %s..%s: all failed (%v), keeping previous cache entry", key.from, key.to, failed)
			return
		}
		resp := EventsResponse{Events: toEventOut(entries), Failed: failed}
		if resp.Failed == nil {
			resp.Failed = []string{}
		}
		if !s.eventsCacheSetIfGen(key, resp, time.Now(), startGen) {
			log.Printf("events refresh %s..%s: discarded (newer result was cached meanwhile)", key.from, key.to)
		}
	}()
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
			AllDayEnd:   en.AllDayEnd,
			MeetingURL:  en.MeetingURL,
			HTMLLink:    en.HTMLLink,
			Description: en.Description,
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
