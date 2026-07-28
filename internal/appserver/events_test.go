package appserver

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/model"
)

// launchdServer は testServer をベースに launchd 稼働中モードへ固定する
// (GET /api/events は doctor と同じく launchd 管理下限定。TestDoctorLaunchd と同じ台本)。
func launchdServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.PlistPath = plist
	s.UID = 501
	s.Runner = &fakeRunner{outputs: map[string]struct {
		out string
		err error
	}{
		"launchctl print gui/501/com.btajp.calsync": {out: "state = running\n"},
	}}
	return s, dir
}

// TestEventsRejectedOutsideLaunchd は launchd 管理外(手動運用)では events が
// 409 で拒否されることを検証する(mappings 読み取りに DB アクセスが要るため。
// container モードも detectDaemon が "container" を返し同じ分岐で 409 になる)。
func TestEventsRejectedOutsideLaunchd(t *testing.T) {
	s, _ := testServer(t) // plist なし → manual モード
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res := get(t, srv, "test-token", "/api/events?from=2026-07-01T00:00:00Z&to=2026-07-02T00:00:00Z", nil)
	if res.StatusCode != 409 {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

// TestEventsInvalidWindow はパース失敗・from>=to・幅 62 日超がいずれも 400 に
// なることを検証する(スペック §4)。
func TestEventsInvalidWindow(t *testing.T) {
	s, _ := launchdServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	cases := []struct {
		name string
		qs   string
	}{
		{"bad from", "from=not-a-time&to=2026-07-02T00:00:00Z"},
		{"bad to", "from=2026-07-01T00:00:00Z&to=not-a-time"},
		{"from == to", "from=2026-07-01T00:00:00Z&to=2026-07-01T00:00:00Z"},
		{"from > to", "from=2026-07-02T00:00:00Z&to=2026-07-01T00:00:00Z"},
		{"width 90 days > 62", "from=2026-01-01T00:00:00Z&to=2026-04-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := get(t, srv, "test-token", "/api/events?"+tc.qs, nil)
			if res.StatusCode != 400 {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
		})
	}
}

// TestEventsWindowBoundaryAllowed はちょうど 62 日幅は拒否されないことを検証する
// (逸脱 = 62 日超のみが 400。境界は許可)。
func TestEventsWindowBoundaryAllowed(t *testing.T) {
	s, _ := launchdServer(t)
	s.CollectEvents = func(ctx context.Context, w model.Window) ([]engine.DigestEntry, []string, error) {
		return nil, nil, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res := get(t, srv, "test-token", "/api/events?from=2026-01-01T00:00:00Z&to=2026-03-04T00:00:00Z", nil) // 62日
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}

// TestEventsMapsFakeCollectEvents はフェイク CollectEvents の DigestEntry が
// スペック §4 の JSON 形へ正しく写像されることを検証する
// (account_id=代表=AccountIDs[0]・account_ids・failed 伝搬)。
func TestEventsMapsFakeCollectEvents(t *testing.T) {
	s, _ := launchdServer(t)
	start := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	s.CollectEvents = func(ctx context.Context, w model.Window) ([]engine.DigestEntry, []string, error) {
		return []engine.DigestEntry{
			{
				Title: "設計レビュー", StartUTC: start, EndUTC: start.Add(time.Hour),
				AccountIDs: []string{"personal", "work-ms"}, HTMLLink: "https://cal/x",
				MeetingURL: "https://zoom.us/j/1", Description: "議題:\n- A\n- B",
			},
			{
				Title: "終日イベント", IsAllDay: true, AllDayStart: "2026-07-05",
				AccountIDs: []string{"personal"},
			},
		}, []string{"work-ms"}, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got EventsResponse
	res := get(t, srv, "test-token", "/api/events?from=2026-07-05T00:00:00Z&to=2026-07-06T00:00:00Z", &got)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if len(got.Events) != 2 {
		t.Fatalf("events = %+v", got.Events)
	}
	ev := got.Events[0]
	if ev.AccountID != "personal" || len(ev.AccountIDs) != 2 || ev.AccountIDs[1] != "work-ms" {
		t.Fatalf("event[0] account fields = %+v", ev)
	}
	if ev.Title != "設計レビュー" || ev.HTMLLink != "https://cal/x" || ev.MeetingURL != "https://zoom.us/j/1" {
		t.Fatalf("event[0] display fields = %+v", ev)
	}
	if ev.Description != "議題:\n- A\n- B" {
		t.Fatalf("event[0] description = %+v", ev)
	}
	if ev.Start != "2026-07-05T10:00:00Z" || ev.End != "2026-07-05T11:00:00Z" || ev.AllDay {
		t.Fatalf("event[0] time fields = %+v", ev)
	}
	if !got.Events[1].AllDay || got.Events[1].AllDayStart != "2026-07-05" {
		t.Fatalf("event[1] all-day fields = %+v", got.Events[1])
	}
	if len(got.Failed) != 1 || got.Failed[0] != "work-ms" {
		t.Fatalf("failed = %+v", got.Failed)
	}
}

// TestEventsMapsAllDayEnd は DigestEntry.AllDayEnd(複数日終日イベントの排他的
// 終了日)が EventOut.AllDayEnd(json: all_day_end)へ欠落なく写像されることを
// 検証する(レビュー Important 1: NormalizedEvent → DigestEntry → EventOut の
// 3 層すべてで運ばれて初めてフロントが複数日終日イベントを正しく描画できる)。
func TestEventsMapsAllDayEnd(t *testing.T) {
	s, _ := launchdServer(t)
	s.CollectEvents = func(ctx context.Context, w model.Window) ([]engine.DigestEntry, []string, error) {
		return []engine.DigestEntry{
			{
				Title: "3日間の休暇", IsAllDay: true, AllDayStart: "2026-07-05", AllDayEnd: "2026-07-08",
				AccountIDs: []string{"personal"},
			},
		}, nil, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got EventsResponse
	res := get(t, srv, "test-token", "/api/events?from=2026-07-06T00:00:00Z&to=2026-07-07T00:00:00Z", &got)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if len(got.Events) != 1 {
		t.Fatalf("events = %+v", got.Events)
	}
	if got.Events[0].AllDayStart != "2026-07-05" || got.Events[0].AllDayEnd != "2026-07-08" {
		t.Fatalf("all-day fields = %+v", got.Events[0])
	}
}

// TestEventsCacheSkipsSecondCallAndRefreshBypasses は 60 秒メモリキャッシュが
// 同一 (from,to) の 2 回目呼び出しでフェイクを再実行しないこと、refresh=1 が
// キャッシュをバイパスして再実行することを検証する(スペック §4)。
// TestEventsCacheSetSweepsExpiredEntries は F6 の回帰テスト。set のたびに
// 期限切れエントリを掃除すること(ビュー切替の連打で from/to の組が際限なく
// 積み上がるのを防ぐ)。TTL 切れ直後のエントリは stale-while-revalidate に使う
// ため掃除の基準は「stale 猶予(eventsCacheStaleMax)も過ぎたもの」であり、
// 猶予超過キーは消え、猶予内のキーは残ることを確認する。now は eventsCacheSet の
// 引数として直接注入できるため、専用の clock フィールドは追加せず、この注入経路を
// そのままテストで使う。
func TestEventsCacheSetSweepsExpiredEntries(t *testing.T) {
	s := &Server{}
	t0 := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)

	stale := eventsCacheKey{from: "stale-from", to: "stale-to"}
	fresh := eventsCacheKey{from: "fresh-from", to: "fresh-to"}
	newKey := eventsCacheKey{from: "new-from", to: "new-to"}

	s.eventsCacheSet(stale, EventsResponse{Failed: []string{}}, t0)
	// fresh は stale より後に set し、掃除の時点でもまだ stale 猶予内に収まるようにする
	tMid := t0.Add(20 * time.Minute)
	s.eventsCacheSet(fresh, EventsResponse{Failed: []string{}}, tMid)

	if len(s.eventsCache) != 2 {
		t.Fatalf("want 2 entries before sweep, got %d", len(s.eventsCache))
	}

	// stale だけが猶予超過になる時刻で新規 set する
	tSweep := t0.Add(eventsCacheTTL + eventsCacheStaleMax + time.Second)
	s.eventsCacheSet(newKey, EventsResponse{Failed: []string{}}, tSweep)

	if _, ok := s.eventsCache[stale]; ok {
		t.Fatalf("stale (beyond grace) key must be swept on set, cache = %+v", s.eventsCache)
	}
	if _, ok := s.eventsCache[fresh]; !ok {
		t.Fatalf("fresh (still within grace at sweep time) key must survive, cache = %+v", s.eventsCache)
	}
	if _, ok := s.eventsCache[newKey]; !ok {
		t.Fatalf("newly set key must be present, cache = %+v", s.eventsCache)
	}
	if len(s.eventsCache) != 2 {
		t.Fatalf("want 2 entries after sweep (fresh + new), got %d: %+v", len(s.eventsCache), s.eventsCache)
	}
}

func TestEventsCacheSkipsSecondCallAndRefreshBypasses(t *testing.T) {
	s, _ := launchdServer(t)
	calls := 0
	s.CollectEvents = func(ctx context.Context, w model.Window) ([]engine.DigestEntry, []string, error) {
		calls++
		return nil, nil, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	qs := "from=2026-07-05T00:00:00Z&to=2026-07-06T00:00:00Z"
	res1 := get(t, srv, "test-token", "/api/events?"+qs, nil)
	if res1.StatusCode != 200 {
		t.Fatalf("status = %d", res1.StatusCode)
	}
	res2 := get(t, srv, "test-token", "/api/events?"+qs, nil)
	if res2.StatusCode != 200 {
		t.Fatalf("status = %d", res2.StatusCode)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (second call should hit cache)", calls)
	}

	res3 := get(t, srv, "test-token", "/api/events?"+qs+"&refresh=1", nil)
	if res3.StatusCode != 200 {
		t.Fatalf("status = %d", res3.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (refresh=1 must bypass cache)", calls)
	}

	// キャッシュは refresh=1 の結果でも更新される: 直後の非 refresh 呼び出しは
	// 再度キャッシュを使い、フェイクは呼ばれない。
	res4 := get(t, srv, "test-token", "/api/events?"+qs, nil)
	if res4.StatusCode != 200 {
		t.Fatalf("status = %d", res4.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (cache repopulated by refresh call)", calls)
	}
}

// TestEventsStaleWhileRevalidate は TTL 切れ・猶予内のキャッシュが stale: true で
// 即返され、バックグラウンド更新が同一キーのキャッシュを最新化して次回は最新内容が
// fresh(stale なし)で返ることを検証する(2026-07-28 体感速度対策)。
func TestEventsStaleWhileRevalidate(t *testing.T) {
	s, _ := launchdServer(t)
	collected := make(chan struct{}, 1)
	s.CollectEvents = func(ctx context.Context, w model.Window) ([]engine.DigestEntry, []string, error) {
		select {
		case collected <- struct{}{}:
		default:
		}
		return []engine.DigestEntry{{Title: "最新の予定", AccountIDs: []string{"personal"}}}, nil, nil
	}
	// TTL 切れ・猶予内の stale エントリを直接 seed(eventsCacheSet の now 注入を利用)
	const path = "/api/events?from=2026-07-05T00:00:00Z&to=2026-07-06T00:00:00Z"
	key := eventsCacheKey{from: "2026-07-05T00:00:00Z", to: "2026-07-06T00:00:00Z"}
	staleResp := EventsResponse{
		Events: []EventOut{{Title: "古い予定", AccountID: "personal", AccountIDs: []string{"personal"}}},
		Failed: []string{},
	}
	s.eventsCacheSet(key, staleResp, time.Now().Add(-(eventsCacheTTL + time.Minute)))

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	var got EventsResponse
	res := get(t, srv, "test-token", path, &got)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !got.Stale || len(got.Events) != 1 || got.Events[0].Title != "古い予定" {
		t.Fatalf("want stale cached response, got %+v", got)
	}

	select {
	case <-collected:
	case <-time.After(5 * time.Second):
		t.Fatal("background refresh did not run")
	}
	// collect 完了から eventsCacheSet までは goroutine のスケジュール次第なので
	// 反映をポーリングで待つ(反映後は最新内容が fresh で返り stale が消える)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got2 EventsResponse
		res2 := get(t, srv, "test-token", path, &got2)
		if res2.StatusCode != 200 {
			t.Fatalf("status = %d", res2.StatusCode)
		}
		if !got2.Stale && len(got2.Events) == 1 && got2.Events[0].Title == "最新の予定" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache was not refreshed in time, last = %+v", got2)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
