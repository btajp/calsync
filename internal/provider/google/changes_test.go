package google

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	calendar "google.golang.org/api/calendar/v3"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
)

var (
	testRef    = model.CalendarRef{AccountID: "test-account", CalendarID: "primary"}
	testWindow = model.Window{
		Start: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
	}
)

// recorded は httptest ハンドラが受けた 1 リクエストの記録。
type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

// recorder はリクエストを記録する。ハンドラはテスト本体と別ゴルーチンで
// 動くため mutex で保護し、アサーションは必ずテスト本体側(all() の戻り値)で行う。
type recorder struct {
	mu   sync.Mutex
	reqs []recorded
}

func (rec *recorder) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, recorded{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Body:   body,
		})
		rec.mu.Unlock()
		next(w, r)
	}
}

func (rec *recorder) all() []recorded {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]recorded(nil), rec.reqs...)
}

// newTestClient は httptest.Server を立て、baseURL を差し替えた Client を返す。
// TokenSource は nil でよい(oauth2.NewClient(ctx, nil) は素の http.Client を返す)。
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(nil, "test-account")
	c.baseURL = srv.URL
	c.retryBase = time.Millisecond // バックオフをテスト用に短縮
	return c
}

func TestChangesFullSyncPaging(t *testing.T) {
	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			// 1 ページ目: nextSyncToken は返さない(最終ページのみ返る仕様)
			fmt.Fprint(w, `{
				"items": [{"id": "ev-1", "iCalUID": "uid-1@google.com", "status": "confirmed",
					"start": {"dateTime": "2026-07-10T10:00:00+09:00"},
					"end": {"dateTime": "2026-07-10T11:00:00+09:00"}}],
				"nextPageToken": "page-2"
			}`)
			return
		}
		fmt.Fprint(w, `{
			"items": [{"id": "ev-2", "iCalUID": "uid-2@google.com", "status": "confirmed",
				"start": {"dateTime": "2026-07-11T09:00:00Z"},
				"end": {"dateTime": "2026-07-11T10:00:00Z"}}],
			"nextSyncToken": "sync-final"
		}`)
	})
	c := newTestClient(t, handler)

	events, cursor, err := c.Changes(context.Background(), testRef, "", testWindow)
	require.NoError(t, err)
	require.Equal(t, "sync-final", cursor, "最終ページの nextSyncToken だけが newCursor になる")
	require.Len(t, events, 2)
	require.Equal(t, "ev-1", events[0].ID)
	require.Equal(t, time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC), events[0].StartUTC, "+09:00 は UTC に正規化される")
	require.Equal(t, "ev-2", events[1].ID)

	reqs := rec.all()
	require.Len(t, reqs, 2)
	// 1 ページ目: フル同期パラメータ(仕様書 5.1-1)
	require.Equal(t, "/calendars/primary/events", reqs[0].Path)
	q1 := reqs[0].Query
	require.Equal(t, "2026-07-03T00:00:00Z", q1.Get("timeMin"))
	require.Equal(t, "2026-10-03T00:00:00Z", q1.Get("timeMax"))
	require.Equal(t, "true", q1.Get("singleEvents"))
	require.False(t, q1.Has("syncToken"))
	require.False(t, q1.Has("pageToken"))
	// 2 ページ目: pageToken 付きで同一パラメータ
	q2 := reqs[1].Query
	require.Equal(t, "page-2", q2.Get("pageToken"))
	require.Equal(t, "true", q2.Get("singleEvents"))
	require.Equal(t, "2026-07-03T00:00:00Z", q2.Get("timeMin"))
}

func TestChangesIncremental(t *testing.T) {
	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items": [], "nextSyncToken": "sync-2"}`)
	})
	c := newTestClient(t, handler)

	events, cursor, err := c.Changes(context.Background(), testRef, "sync-1", testWindow)
	require.NoError(t, err)
	require.Empty(t, events)
	require.Equal(t, "sync-2", cursor)

	reqs := rec.all()
	require.Len(t, reqs, 1)
	q := reqs[0].Query
	require.Equal(t, "sync-1", q.Get("syncToken"))
	require.Equal(t, "true", q.Get("singleEvents"))
	// syncToken と timeMin/timeMax の併用は 400 になる(仕様書 5.1)ため付けない
	require.False(t, q.Has("timeMin"))
	require.False(t, q.Has("timeMax"))
}

func TestChangesCursorInvalid(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `{"error": {"errors": [{"domain": "global", "reason": "fullSyncRequired",
			"message": "Sync token is no longer valid, a full sync is required."}],
			"code": 410, "message": "Sync token is no longer valid, a full sync is required."}}`)
	})
	c := newTestClient(t, handler)

	_, _, err := c.Changes(context.Background(), testRef, "sync-stale", testWindow)
	require.ErrorIs(t, err, provider.ErrCursorInvalid)
}

func TestChangesAuthExpired(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error": {"errors": [{"domain": "global", "reason": "authError",
			"message": "Invalid Credentials"}], "code": 401, "message": "Invalid Credentials"}}`)
	})
	c := newTestClient(t, handler)

	_, _, err := c.Changes(context.Background(), testRef, "sync-1", testWindow)
	require.ErrorIs(t, err, provider.ErrAuthExpired, "401 はトークン失効として ErrAuthExpired に写像される(仕様書9.3)")
}

func TestChangesRetriesRateLimit(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error": {"errors": [{"domain": "usageLimits", "reason": "rateLimitExceeded",
				"message": "Rate Limit Exceeded"}], "code": 403, "message": "Rate Limit Exceeded"}}`)
			return
		}
		fmt.Fprint(w, `{"items": [{"id": "ev-1", "status": "confirmed",
			"start": {"dateTime": "2026-07-10T10:00:00Z"},
			"end": {"dateTime": "2026-07-10T11:00:00Z"}}],
			"nextSyncToken": "sync-after-retry"}`)
	})
	c := newTestClient(t, handler)

	events, cursor, err := c.Changes(context.Background(), testRef, "sync-1", testWindow)
	require.NoError(t, err, "403 rateLimitExceeded 1 回は再試行で吸収される")
	require.Equal(t, "sync-after-retry", cursor)
	require.Len(t, events, 1)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, calls)
}

func TestChangesNormalization(t *testing.T) {
	cases := []struct {
		name     string
		itemJSON string
		want     model.NormalizedEvent
	}{
		{
			name:     "cancelled は Deleted=true で ID のみ",
			itemJSON: `{"id": "ev-del", "status": "cancelled"}`,
			want:     model.NormalizedEvent{ID: "ev-del", Deleted: true},
		},
		{
			name: "transparency 省略は busy(opaque が既定)",
			itemJSON: `{"id": "ev-busy", "status": "confirmed", "iCalUID": "uid-busy",
				"start": {"dateTime": "2026-07-10T10:00:00Z"},
				"end": {"dateTime": "2026-07-10T11:00:00Z"}}`,
			want: model.NormalizedEvent{
				ID: "ev-busy", ICalUID: "uid-busy", IsBusy: true,
				StartUTC: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "transparency=transparent は IsBusy=false",
			itemJSON: `{"id": "ev-free", "status": "confirmed", "iCalUID": "uid-free",
				"transparency": "transparent",
				"start": {"dateTime": "2026-07-10T10:00:00Z"},
				"end": {"dateTime": "2026-07-10T11:00:00Z"}}`,
			want: model.NormalizedEvent{
				ID: "ev-free", ICalUID: "uid-free", IsBusy: false,
				StartUTC: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "self=true の declined は IsDeclined=true",
			itemJSON: `{"id": "ev-dec", "status": "confirmed", "iCalUID": "uid-dec",
				"attendees": [
					{"email": "other@example.com", "responseStatus": "accepted"},
					{"email": "me@example.com", "self": true, "responseStatus": "declined"}],
				"start": {"dateTime": "2026-07-10T10:00:00Z"},
				"end": {"dateTime": "2026-07-10T11:00:00Z"}}`,
			want: model.NormalizedEvent{
				ID: "ev-dec", ICalUID: "uid-dec", IsBusy: true, IsDeclined: true,
				StartUTC: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "他人の declined は IsDeclined に影響しない",
			itemJSON: `{"id": "ev-oth", "status": "confirmed", "iCalUID": "uid-oth",
				"attendees": [
					{"email": "other@example.com", "responseStatus": "declined"},
					{"email": "me@example.com", "self": true, "responseStatus": "accepted"}],
				"start": {"dateTime": "2026-07-10T10:00:00Z"},
				"end": {"dateTime": "2026-07-10T11:00:00Z"}}`,
			want: model.NormalizedEvent{
				ID: "ev-oth", ICalUID: "uid-oth", IsBusy: true, IsDeclined: false,
				StartUTC: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "extendedProperties.private の calsync-origin を OriginTag へ",
			itemJSON: `{"id": "ev-tag", "status": "confirmed", "iCalUID": "uid-tag",
				"extendedProperties": {"private": {"calsync": "v1", "calsync-origin": "work:ev-9"}},
				"start": {"dateTime": "2026-07-10T10:00:00Z"},
				"end": {"dateTime": "2026-07-10T11:00:00Z"}}`,
			want: model.NormalizedEvent{
				ID: "ev-tag", ICalUID: "uid-tag", IsBusy: true, OriginTag: "work:ev-9",
				StartUTC: time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "dateTime のオフセットは UTC に正規化",
			itemJSON: `{"id": "ev-jst", "status": "confirmed", "iCalUID": "uid-jst",
				"start": {"dateTime": "2026-07-10T10:00:00+09:00", "timeZone": "Asia/Tokyo"},
				"end": {"dateTime": "2026-07-10T11:30:00+09:00", "timeZone": "Asia/Tokyo"}}`,
			want: model.NormalizedEvent{
				ID: "ev-jst", ICalUID: "uid-jst", IsBusy: true,
				StartUTC: time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 2, 30, 0, 0, time.UTC),
			},
		},
		{
			name: "終日は date を現地日付のまま保持(end は排他的)",
			itemJSON: `{"id": "ev-day", "status": "confirmed", "iCalUID": "uid-day",
				"start": {"date": "2026-07-15"},
				"end": {"date": "2026-07-16"}}`,
			want: model.NormalizedEvent{
				ID: "ev-day", ICalUID: "uid-day", IsBusy: true,
				IsAllDay: true, AllDayStart: "2026-07-15", AllDayEnd: "2026-07-16",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"items": [%s], "nextSyncToken": "s"}`, tc.itemJSON)
			})
			c := newTestClient(t, handler)

			events, _, err := c.Changes(context.Background(), testRef, "cursor-x", testWindow)
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, tc.want, events[0])
		})
	}
}

func TestNormalizeEventTitle(t *testing.T) {
	ev := normalizeEvent(&calendar.Event{
		Id:      "ev1",
		Summary: "設計レビュー",
		Start:   &calendar.EventDateTime{DateTime: "2026-07-10T01:00:00Z"},
		End:     &calendar.EventDateTime{DateTime: "2026-07-10T02:00:00Z"},
	})
	require.Equal(t, "設計レビュー", ev.Title)

	// cancelled は ID 以外を保証しない既存契約のまま(Title も空)
	del := normalizeEvent(&calendar.Event{Id: "ev2", Status: "cancelled", Summary: "x"})
	require.True(t, del.Deleted)
	require.Equal(t, "", del.Title)
}

func TestNormalizeEventMeetingFields(t *testing.T) {
	base := func() *calendar.Event {
		return &calendar.Event{
			Id:    "ev1",
			Start: &calendar.EventDateTime{DateTime: "2026-07-10T01:00:00Z"},
			End:   &calendar.EventDateTime{DateTime: "2026-07-10T02:00:00Z"},
		}
	}

	// conferenceData の video エントリポイントが最優先(v2 スペック 3.2)
	ev := base()
	ev.HangoutLink = "https://meet.google.com/fallback"
	ev.ConferenceData = &calendar.ConferenceData{EntryPoints: []*calendar.EntryPoint{
		{EntryPointType: "phone", Uri: "tel:+81-3-0000-0000"},
		{EntryPointType: "video", Uri: "https://example-corp.zoom.us/j/89335149431"},
	}}
	got := normalizeEvent(ev)
	require.Equal(t, "https://example-corp.zoom.us/j/89335149431", got.MeetingURL)

	// conferenceData が無ければ hangoutLink
	ev = base()
	ev.HangoutLink = "https://meet.google.com/abc-defg-hij"
	require.Equal(t, "https://meet.google.com/abc-defg-hij", normalizeEvent(ev).MeetingURL)

	// どちらも無ければ location/description の正規表現フォールバック
	ev = base()
	ev.Location = "https://zoom.us/j/123456789"
	require.Equal(t, "https://zoom.us/j/123456789", normalizeEvent(ev).MeetingURL)

	// htmlLink と description(HTML 除去済み)
	ev = base()
	ev.HtmlLink = "https://www.google.com/calendar/event?eid=xyz"
	ev.Description = "資料<br>リンク: <a href=\"https://example.com\">here</a>&amp;co"
	got = normalizeEvent(ev)
	require.Equal(t, "https://www.google.com/calendar/event?eid=xyz", got.HTMLLink)
	require.Equal(t, "資料\nリンク: here&co", got.Description)
}
