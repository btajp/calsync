package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
)

// recordedRequest は httptest ハンドラが受けたリクエストの記録。
// require はテスト goroutine 以外で FailNow できないため、
// ハンドラ内では記録のみ行い、アサーションはテスト側で行う。
type recordedRequest struct {
	Method string
	URL    string // r.URL.String()(パス+クエリ)
	Header http.Header
	Body   []byte
}

func record(reqs *[]recordedRequest, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, recordedRequest{
			Method: r.Method,
			URL:    r.URL.String(),
			Header: r.Header.Clone(),
			Body:   body,
		})
		next(w, r)
	}
}

// requireCommonHeaders は「全リクエストに Prefer: IdType="ImmutableId" が付き、
// odata.maxpagesize が一切付かない」ことの共通アサート。
func requireCommonHeaders(t *testing.T, reqs []recordedRequest) {
	t.Helper()
	require.NotEmpty(t, reqs)
	for i, rr := range reqs {
		require.Equal(t, `IdType="ImmutableId", outlook.body-content-type="text"`, rr.Header.Get("Prefer"), "request %d", i)
		for _, v := range rr.Header.Values("Prefer") {
			require.NotContains(t, v, "odata.maxpagesize", "request %d", i)
		}
	}
}

func newTestClient(t *testing.T, srvURL string, busyShowAs []string) *Client {
	t.Helper()
	c := New(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}), "ms-acct", busyShowAs)
	c.baseURL = srvURL
	c.sleep = func(time.Duration) {} // テストでは待機しない(必要なテストは個別に差し替えて記録する)
	return c
}

var testWindow = model.Window{
	Start: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
	End:   time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
}

var testCal = model.CalendarRef{AccountID: "ms-acct", CalendarID: "primary"}

func TestChangesInitialSyncPaging(t *testing.T) {
	var reqs []recordedRequest
	page := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		page++
		switch page {
		case 1:
			fmt.Fprintf(w, `{
				"value": [{
					"id": "ev-1",
					"iCalUId": "uid-1",
					"showAs": "busy",
					"isAllDay": false,
					"start": {"dateTime": "2026-07-10T09:00:00.0000000", "timeZone": "UTC"},
					"end": {"dateTime": "2026-07-10T10:00:00.0000000", "timeZone": "UTC"},
					"responseStatus": {"response": "organizer"}
				}],
				"@odata.nextLink": "http://%s/me/calendarView/delta?$skiptoken=p2"
			}`, r.Host)
		case 2:
			fmt.Fprintf(w, `{
				"value": [{
					"id": "ev-2",
					"iCalUId": "uid-2",
					"showAs": "free",
					"isAllDay": false,
					"start": {"dateTime": "2026-07-11T09:00:00.0000000", "timeZone": "UTC"},
					"end": {"dateTime": "2026-07-11T10:00:00.0000000", "timeZone": "UTC"},
					"responseStatus": {"response": "accepted"}
				}],
				"@odata.deltaLink": "http://%s/me/calendarView/delta?$deltatoken=final"
			}`, r.Host)
		}
	}
	srv := httptest.NewServer(record(&reqs, handler))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy", "oof", "tentative"})
	events, newCursor, err := c.Changes(context.Background(), testCal, "", testWindow)
	require.NoError(t, err)

	require.Len(t, reqs, 2)
	u0, err := url.Parse(reqs[0].URL)
	require.NoError(t, err)
	require.Equal(t, "/me/calendarView/delta", u0.Path)
	require.Equal(t, "2026-07-03T00:00:00Z", u0.Query().Get("startDateTime"))
	require.Equal(t, "2026-10-03T00:00:00Z", u0.Query().Get("endDateTime"))
	u1, err := url.Parse(reqs[1].URL)
	require.NoError(t, err)
	require.Equal(t, "p2", u1.Query().Get("$skiptoken"))

	require.Len(t, events, 2)
	require.Equal(t, "ev-1", events[0].ID)
	require.Equal(t, "uid-1", events[0].ICalUID)
	require.True(t, events[0].IsBusy)
	require.Equal(t, time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC), events[0].StartUTC)
	require.Equal(t, time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), events[0].EndUTC)
	require.Equal(t, "ev-2", events[1].ID)
	require.False(t, events[1].IsBusy) // showAs=free は busyShowAs リストに含まれない

	require.Contains(t, newCursor, "$deltatoken=final") // deltaLink がそのまま newCursor
	requireCommonHeaders(t, reqs)
}

func TestChangesIncrementalUsesCursorAsIs(t *testing.T) {
	var reqs []recordedRequest
	handler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"value":[],"@odata.deltaLink":"http://%s/me/calendarView/delta?$deltatoken=next"}`, r.Host)
	}
	srv := httptest.NewServer(record(&reqs, handler))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	cursor := srv.URL + "/me/calendarView/delta?$deltatoken=prev"
	events, newCursor, err := c.Changes(context.Background(), testCal, cursor, testWindow)
	require.NoError(t, err)
	require.Empty(t, events)
	require.Contains(t, newCursor, "$deltatoken=next")

	require.Len(t, reqs, 1)
	u, err := url.Parse(reqs[0].URL)
	require.NoError(t, err)
	require.Equal(t, "prev", u.Query().Get("$deltatoken")) // cursor をそのまま GET
	require.Empty(t, u.Query().Get("startDateTime"))       // 増分にウィンドウは付けない
	requireCommonHeaders(t, reqs)
}

func TestNormalizeDeltaEvent(t *testing.T) {
	busy := map[string]bool{"busy": true, "oof": true, "tentative": true}
	tests := []struct {
		name string
		json string
		want model.NormalizedEvent
	}{
		{
			name: "busy timed event in JST normalized to UTC",
			json: `{"id":"ev1","iCalUId":"uid1","showAs":"busy","isAllDay":false,
				"start":{"dateTime":"2026-07-10T09:00:00.0000000","timeZone":"Asia/Tokyo"},
				"end":{"dateTime":"2026-07-10T10:00:00.0000000","timeZone":"Asia/Tokyo"},
				"responseStatus":{"response":"accepted"}}`,
			want: model.NormalizedEvent{
				ID: "ev1", ICalUID: "uid1", IsBusy: true,
				StartUTC: time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "showAs=free is not busy",
			json: `{"id":"ev2","iCalUId":"uid2","showAs":"free","isAllDay":false,
				"start":{"dateTime":"2026-07-10T09:00:00.0000000","timeZone":"UTC"},
				"end":{"dateTime":"2026-07-10T10:00:00.0000000","timeZone":"UTC"}}`,
			want: model.NormalizedEvent{
				ID: "ev2", ICalUID: "uid2", IsBusy: false,
				StartUTC: time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "declined sets IsDeclined",
			json: `{"id":"ev3","iCalUId":"uid3","showAs":"busy","isAllDay":false,
				"start":{"dateTime":"2026-07-10T09:00:00.0000000","timeZone":"UTC"},
				"end":{"dateTime":"2026-07-10T10:00:00.0000000","timeZone":"UTC"},
				"responseStatus":{"response":"declined"}}`,
			want: model.NormalizedEvent{
				ID: "ev3", ICalUID: "uid3", IsBusy: true, IsDeclined: true,
				StartUTC: time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "all-day keeps local dates",
			json: `{"id":"ev4","iCalUId":"uid4","showAs":"busy","isAllDay":true,
				"start":{"dateTime":"2026-07-15T00:00:00.0000000","timeZone":"UTC"},
				"end":{"dateTime":"2026-07-16T00:00:00.0000000","timeZone":"UTC"}}`,
			want: model.NormalizedEvent{
				ID: "ev4", ICalUID: "uid4", IsBusy: true, IsAllDay: true,
				AllDayStart: "2026-07-15", AllDayEnd: "2026-07-16",
			},
		},
		{
			name: "@removed is Deleted with ID only",
			json: `{"id":"gone-1","@removed":{"reason":"deleted"}}`,
			want: model.NormalizedEvent{ID: "gone-1", Deleted: true},
		},
		{
			name: "isCancelled is Deleted",
			json: `{"id":"cancel-1","iCalUId":"uid-c","isCancelled":true,"showAs":"busy"}`,
			want: model.NormalizedEvent{ID: "cancel-1", ICalUID: "uid-c", Deleted: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var de deltaEvent
			require.NoError(t, json.Unmarshal([]byte(tc.json), &de))
			got, err := normalizeDeltaEvent(de, busy)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestNormalizeDeltaEventTitle(t *testing.T) {
	busy := map[string]bool{"busy": true}
	de := deltaEvent{
		ID:      "ev1",
		Subject: "設計レビュー",
		ShowAs:  "busy",
		Start:   &graphTime{DateTime: "2026-07-10T01:00:00.0000000", TimeZone: "UTC"},
		End:     &graphTime{DateTime: "2026-07-10T02:00:00.0000000", TimeZone: "UTC"},
	}
	got, err := normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "設計レビュー", got.Title)
}

func TestNormalizeDeltaEventMeetingFields(t *testing.T) {
	busy := map[string]bool{"busy": true}
	base := func() deltaEvent {
		return deltaEvent{
			ID: "ev1", ShowAs: "busy",
			Start: &graphTime{DateTime: "2026-07-10T01:00:00.0000000", TimeZone: "UTC"},
			End:   &graphTime{DateTime: "2026-07-10T02:00:00.0000000", TimeZone: "UTC"},
		}
	}

	// onlineMeeting.joinUrl が最優先(v2 スペック 3.2)
	de := base()
	de.OnlineMeetingURL = "https://legacy.example.com"
	de.OnlineMeeting = &graphOnlineMeeting{JoinURL: "https://teams.microsoft.com/l/meetup-join/19%3ax"}
	got, err := normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "https://teams.microsoft.com/l/meetup-join/19%3ax", got.MeetingURL)

	// joinUrl が無ければ onlineMeetingUrl
	de = base()
	de.OnlineMeetingURL = "https://legacy.example.com"
	got, err = normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "https://legacy.example.com", got.MeetingURL)

	// どちらも無ければ location/body の正規表現フォールバック
	de = base()
	de.Location = &graphLocation{DisplayName: "https://work-a.zoom.us/j/86032012178"}
	got, err = normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "https://work-a.zoom.us/j/86032012178", got.MeetingURL)

	// body(Prefer で text 化済み)と webLink の素通し
	de = base()
	de.Body = &graphBody{ContentType: "text", Content: "アジェンダ\n1. 進捗"}
	de.WebLink = "https://outlook.live.com/calendar/item/xyz"
	got, err = normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "アジェンダ\n1. 進捗", got.Description)
	require.Equal(t, "https://outlook.live.com/calendar/item/xyz", got.HTMLLink)
}

func TestChangesCursorInvalid(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantInvalid bool
	}{
		{
			name:        "HTTP 410 Gone",
			status:      http.StatusGone,
			body:        `{"error":{"code":"fullSyncRequired","message":"full sync required"}}`,
			wantInvalid: true,
		},
		{
			name:        "404 with syncStateNotFound",
			status:      http.StatusNotFound,
			body:        `{"error":{"code":"syncStateNotFound","message":"sync state not found"}}`,
			wantInvalid: true,
		},
		{
			name:        "400 with syncStateNotFound",
			status:      http.StatusBadRequest,
			body:        `{"error":{"code":"syncStateNotFound","message":"sync state not found"}}`,
			wantInvalid: true,
		},
		{
			name:        "403 other error is not ErrCursorInvalid",
			status:      http.StatusForbidden,
			body:        `{"error":{"code":"ErrorAccessDenied","message":"denied"}}`,
			wantInvalid: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reqs []recordedRequest
			handler := func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}
			srv := httptest.NewServer(record(&reqs, handler))
			defer srv.Close()

			c := newTestClient(t, srv.URL, []string{"busy"})
			_, newCursor, err := c.Changes(context.Background(), testCal,
				srv.URL+"/me/calendarView/delta?$deltatoken=stale", testWindow)
			require.Error(t, err)
			require.Empty(t, newCursor)
			if tc.wantInvalid {
				require.ErrorIs(t, err, provider.ErrCursorInvalid)
			} else {
				require.NotErrorIs(t, err, provider.ErrCursorInvalid)
			}
			requireCommonHeaders(t, reqs)
		})
	}
}

func TestChangesCircuitBreaker(t *testing.T) {
	t.Run("identical page repeated", func(t *testing.T) {
		var reqs []recordedRequest
		handler := func(w http.ResponseWriter, r *http.Request) {
			// 常に同じイベントID列 + nextLink を返す(無限ページネーション状態)
			fmt.Fprintf(w, `{
				"value": [{"id": "loop-ev", "iCalUId": "uid-loop", "showAs": "busy", "isAllDay": false,
					"start": {"dateTime": "2026-07-10T09:00:00.0000000", "timeZone": "UTC"},
					"end": {"dateTime": "2026-07-10T10:00:00.0000000", "timeZone": "UTC"}}],
				"@odata.nextLink": "http://%s/me/calendarView/delta?$skiptoken=again"
			}`, r.Host)
		}
		srv := httptest.NewServer(record(&reqs, handler))
		defer srv.Close()

		c := newTestClient(t, srv.URL, []string{"busy"})
		_, _, err := c.Changes(context.Background(), testCal, "", testWindow)
		require.ErrorIs(t, err, provider.ErrCursorInvalid)
		require.Len(t, reqs, 2) // 2ページ目で「直前ページとID列同一」を検知して中断
	})

	t.Run("page limit 50", func(t *testing.T) {
		var reqs []recordedRequest
		n := 0
		handler := func(w http.ResponseWriter, r *http.Request) {
			n++
			// 毎ページ異なるIDを返す(フィンガープリント一致では止まらない)
			fmt.Fprintf(w, `{
				"value": [{"id": "ev-%d", "iCalUId": "uid-%d", "showAs": "busy", "isAllDay": false,
					"start": {"dateTime": "2026-07-10T09:00:00.0000000", "timeZone": "UTC"},
					"end": {"dateTime": "2026-07-10T10:00:00.0000000", "timeZone": "UTC"}}],
				"@odata.nextLink": "http://%s/me/calendarView/delta?$skiptoken=p%d"
			}`, n, n, r.Host, n+1)
		}
		srv := httptest.NewServer(record(&reqs, handler))
		defer srv.Close()

		c := newTestClient(t, srv.URL, []string{"busy"})
		_, _, err := c.Changes(context.Background(), testCal, "", testWindow)
		require.ErrorIs(t, err, provider.ErrCursorInvalid)
		require.Len(t, reqs, 50) // 1ラウンド50ページで打ち切り
	})
}

func TestChanges429RetryAfter(t *testing.T) {
	var reqs []recordedRequest
	calls := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprintf(w, `{"value":[],"@odata.deltaLink":"http://%s/me/calendarView/delta?$deltatoken=d1"}`, r.Host)
	}
	srv := httptest.NewServer(record(&reqs, handler))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	var slept []time.Duration
	c.sleep = func(d time.Duration) { slept = append(slept, d) } // 実待機せず記録のみ

	events, newCursor, err := c.Changes(context.Background(), testCal, "", testWindow)
	require.NoError(t, err)
	require.Empty(t, events)
	require.Contains(t, newCursor, "$deltatoken=d1")
	require.Len(t, reqs, 2)
	require.Equal(t, []time.Duration{1 * time.Second}, slept) // Retry-After: 1 に従った
	requireCommonHeaders(t, reqs)
}

func TestChanges5xxBackoff(t *testing.T) {
	var reqs []recordedRequest
	calls := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"value":[],"@odata.deltaLink":"http://%s/me/calendarView/delta?$deltatoken=d2"}`, r.Host)
	}
	srv := httptest.NewServer(record(&reqs, handler))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	var slept []time.Duration
	c.sleep = func(d time.Duration) { slept = append(slept, d) }

	_, newCursor, err := c.Changes(context.Background(), testCal, "", testWindow)
	require.NoError(t, err)
	require.Contains(t, newCursor, "$deltatoken=d2")
	require.Len(t, reqs, 3)
	require.Len(t, slept, 2)
	require.Greater(t, slept[1], slept[0]) // 指数バックオフで待機が伸びる
	requireCommonHeaders(t, reqs)
}

// TestChanges401AuthExpired はコーディネーター追加要件(Task 14 レビュー由来):
// Graph が HTTP 401 を返した場合、provider.ErrAuthExpired に正規化されることを
// 確認する(common error path = Client.doRead 経由、autherr.go の
// NormalizeAuthErr と揃える)。
func TestChanges401AuthExpired(t *testing.T) {
	var reqs []recordedRequest
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"InvalidAuthenticationToken","message":"access token is expired"}}`)
	}
	srv := httptest.NewServer(record(&reqs, handler))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	_, newCursor, err := c.Changes(context.Background(), testCal, "", testWindow)
	require.Error(t, err)
	require.Empty(t, newCursor)
	require.ErrorIs(t, err, provider.ErrAuthExpired)
	requireCommonHeaders(t, reqs)
}
