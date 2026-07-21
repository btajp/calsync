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
				MeetingURL: "https://zoom.us/j/1",
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
