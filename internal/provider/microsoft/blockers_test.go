package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/work-a-co/calsync/internal/model"
)

const wantOriginPropID = "String {b7dbd76c-3a35-4b41-9d80-6a3f31f2a6b9} Name calsyncOrigin"

func TestCreateBlocker(t *testing.T) {
	tests := []struct {
		name       string
		blocker    model.Blocker
		idemKey    string
		wantAllDay bool
		wantStart  map[string]string
		wantEnd    map[string]string
	}{
		{
			name: "timed blocker uses UTC dateTime",
			blocker: model.Blocker{
				Title:     "予定あり",
				StartUTC:  time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
				EndUTC:    time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
				OriginTag: "personal:ev1",
			},
			idemKey:    "calsync-abc123",
			wantAllDay: false,
			wantStart:  map[string]string{"dateTime": "2026-07-10T01:00:00", "timeZone": "UTC"},
			wantEnd:    map[string]string{"dateTime": "2026-07-10T02:00:00", "timeZone": "UTC"},
		},
		{
			name: "all-day blocker uses target timezone midnight bounds",
			blocker: model.Blocker{
				Title:          "予定あり",
				IsAllDay:       true,
				AllDayStart:    "2026-07-15",
				AllDayEnd:      "2026-07-16",
				TargetTimezone: "Tokyo Standard Time",
				OriginTag:      "personal:ev2",
			},
			idemKey:    "calsync-def456",
			wantAllDay: true,
			wantStart:  map[string]string{"dateTime": "2026-07-15T00:00:00", "timeZone": "Tokyo Standard Time"},
			wantEnd:    map[string]string{"dateTime": "2026-07-16T00:00:00", "timeZone": "Tokyo Standard Time"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reqs []recordedRequest
			handler := func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"id":"created-ev-1"}`)
			}
			srv := httptest.NewServer(record(&reqs, handler))
			defer srv.Close()

			c := newTestClient(t, srv.URL, []string{"busy"})
			id, err := c.CreateBlocker(context.Background(), testCal, tc.blocker, tc.idemKey)
			require.NoError(t, err)
			require.Equal(t, "created-ev-1", id)

			require.Len(t, reqs, 1)
			require.Equal(t, http.MethodPost, reqs[0].Method)
			require.Equal(t, "/me/events", reqs[0].URL)

			var body map[string]any
			require.NoError(t, json.Unmarshal(reqs[0].Body, &body))
			require.Equal(t, "予定あり", body["subject"])
			require.Equal(t, "busy", body["showAs"])
			require.Equal(t, false, body["isReminderOn"])
			require.Equal(t, "private", body["sensitivity"])
			require.Equal(t, tc.idemKey, body["transactionId"])
			require.Equal(t, tc.wantAllDay, body["isAllDay"])

			start, ok := body["start"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, tc.wantStart["dateTime"], start["dateTime"])
			require.Equal(t, tc.wantStart["timeZone"], start["timeZone"])
			end, ok := body["end"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, tc.wantEnd["dateTime"], end["dateTime"])
			require.Equal(t, tc.wantEnd["timeZone"], end["timeZone"])

			props, ok := body["singleValueExtendedProperties"].([]any)
			require.True(t, ok)
			require.Len(t, props, 1)
			prop, ok := props[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, wantOriginPropID, prop["id"])
			require.Equal(t, tc.blocker.OriginTag, prop["value"])

			requireCommonHeaders(t, reqs)
		})
	}
}

func TestCreateBlockerConflictReturnsExistingID(t *testing.T) {
	var reqs []recordedRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/me/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":{"code":"ErrorDuplicateTransactionId","message":"duplicate transactionId"}}`)
			return
		}
		fmt.Fprint(w, `{"value":[{"id":"existing-ev-9"}]}`)
	})
	srv := httptest.NewServer(record(&reqs, mux.ServeHTTP))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	b := model.Blocker{
		Title:     "予定あり",
		StartUTC:  time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:    time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
		OriginTag: "personal:ev1",
	}
	id, err := c.CreateBlocker(context.Background(), testCal, b, "calsync-abc123")
	require.NoError(t, err)
	require.Equal(t, "existing-ev-9", id)

	require.Len(t, reqs, 2)
	require.Equal(t, http.MethodGet, reqs[1].Method)
	u, err := url.Parse(reqs[1].URL)
	require.NoError(t, err)
	require.Equal(t,
		"singleValueExtendedProperties/Any(ep: ep/id eq '"+wantOriginPropID+"' and ep/value eq 'personal:ev1')",
		u.Query().Get("$filter"))
	requireCommonHeaders(t, reqs)
}

func TestUpdateBlocker(t *testing.T) {
	var reqs []recordedRequest
	handler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"ev-1"}`)
	}
	srv := httptest.NewServer(record(&reqs, handler))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	b := model.Blocker{
		Title:     "予定あり",
		StartUTC:  time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC),
		EndUTC:    time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
		OriginTag: "personal:ev1",
	}
	err := c.UpdateBlocker(context.Background(), testCal, "ev-1", b)
	require.NoError(t, err)

	require.Len(t, reqs, 1)
	require.Equal(t, http.MethodPatch, reqs[0].Method)
	require.Equal(t, "/me/events/ev-1", reqs[0].URL)

	var body map[string]any
	require.NoError(t, json.Unmarshal(reqs[0].Body, &body))
	require.NotContains(t, body, "transactionId") // transactionId は作成専用
	start, ok := body["start"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-07-10T03:00:00", start["dateTime"])
	require.Equal(t, "UTC", start["timeZone"])
	requireCommonHeaders(t, reqs)
}

func TestDeleteBlocker(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204 No Content", http.StatusNoContent, false},
		{"404 Not Found is success (idempotent)", http.StatusNotFound, false},
		{"403 Forbidden is an error", http.StatusForbidden, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reqs []recordedRequest
			handler := func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}
			srv := httptest.NewServer(record(&reqs, handler))
			defer srv.Close()

			c := newTestClient(t, srv.URL, []string{"busy"})
			err := c.DeleteBlocker(context.Background(), testCal, "ev-1")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, reqs, 1)
			require.Equal(t, http.MethodDelete, reqs[0].Method)
			require.Equal(t, "/me/events/ev-1", reqs[0].URL)
			requireCommonHeaders(t, reqs)
		})
	}
}

func TestListBlockers(t *testing.T) {
	var reqs []recordedRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/me/events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"value":[{"id":"blk-1"},{"id":"blk-2"}]}`)
	})
	mux.HandleFunc("/me/events/blk-1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"blk-1","isAllDay":false,
			"start":{"dateTime":"2026-07-10T01:00:00.0000000","timeZone":"UTC"},
			"end":{"dateTime":"2026-07-10T02:00:00.0000000","timeZone":"UTC"},
			"singleValueExtendedProperties":[{"id":"String {b7dbd76c-3a35-4b41-9d80-6a3f31f2a6b9} Name calsyncOrigin","value":"personal:ev1"}]}`)
	})
	mux.HandleFunc("/me/events/blk-2", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"blk-2","isAllDay":true,
			"start":{"dateTime":"2026-07-15T00:00:00.0000000","timeZone":"Tokyo Standard Time"},
			"end":{"dateTime":"2026-07-16T00:00:00.0000000","timeZone":"Tokyo Standard Time"},
			"singleValueExtendedProperties":[{"id":"String {b7dbd76c-3a35-4b41-9d80-6a3f31f2a6b9} Name calsyncOrigin","value":"work:ev2"}]}`)
	})
	srv := httptest.NewServer(record(&reqs, mux.ServeHTTP))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	recs, err := c.ListBlockers(context.Background(), testCal, testWindow)
	require.NoError(t, err)

	wantTimedHash := model.TimeHash(model.NormalizedEvent{
		StartUTC: time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
	})
	wantAllDayHash := model.TimeHash(model.NormalizedEvent{
		IsAllDay: true, AllDayStart: "2026-07-15", AllDayEnd: "2026-07-16",
	})
	require.Equal(t, []model.BlockerRecord{
		{EventID: "blk-1", OriginTag: "personal:ev1", TimeHash: wantTimedHash},
		{EventID: "blk-2", OriginTag: "work:ev2", TimeHash: wantAllDayHash},
	}, recs)

	// 列挙: $filter(value ne null)。GUID の { } が URL エンコードされていること
	require.Len(t, reqs, 3)
	u0, err := url.Parse(reqs[0].URL)
	require.NoError(t, err)
	require.Equal(t, "/me/events", u0.Path)
	require.Equal(t,
		"singleValueExtendedProperties/Any(ep: ep/id eq '"+wantOriginPropID+"' and ep/value ne null)",
		u0.Query().Get("$filter"))
	require.Contains(t, reqs[0].URL, "%7Bb7dbd76c-3a35-4b41-9d80-6a3f31f2a6b9%7D")

	// 各件 GET + $expand で value を取得
	u1, err := url.Parse(reqs[1].URL)
	require.NoError(t, err)
	require.Equal(t, "/me/events/blk-1", u1.Path)
	require.Equal(t,
		"singleValueExtendedProperties($filter=id eq '"+wantOriginPropID+"')",
		u1.Query().Get("$expand"))

	requireCommonHeaders(t, reqs)
}

func TestGetCalendarTimezone(t *testing.T) {
	var reqs []recordedRequest
	handler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"@odata.context":"https://graph.microsoft.com/v1.0/$metadata#users('me')/mailboxSettings/timeZone","value":"Tokyo Standard Time"}`)
	}
	srv := httptest.NewServer(record(&reqs, handler))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	tz, err := c.GetCalendarTimezone(context.Background(), testCal)
	require.NoError(t, err)
	require.Equal(t, "Tokyo Standard Time", tz) // mailboxSettings の value をそのまま返す

	require.Len(t, reqs, 1)
	require.Equal(t, "/me/mailboxSettings/timeZone", reqs[0].URL)
	requireCommonHeaders(t, reqs)
}
