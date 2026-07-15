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

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
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
	mux.HandleFunc("/me/events/existing-ev-9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"existing-ev-9"}`)
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

	require.Len(t, reqs, 3)
	require.Equal(t, http.MethodGet, reqs[1].Method)
	u, err := url.Parse(reqs[1].URL)
	require.NoError(t, err)
	require.Equal(t,
		"singleValueExtendedProperties/Any(ep: ep/id eq '"+wantOriginPropID+"' and ep/value eq 'personal:ev1')",
		u.Query().Get("$filter"))
	// クラッシュ再実行の収容では既存ブロッカーの内容が古い可能性があるため、
	// 作成しようとしていた内容で PATCH してから ID を返す(スペック 2026-07-15 §5)
	require.Equal(t, http.MethodPatch, reqs[2].Method)
	var patched map[string]any
	require.NoError(t, json.Unmarshal(reqs[2].Body, &patched))
	require.Equal(t, "予定あり", patched["subject"], "PATCH ボディに転記内容(subject)が入る")
	body, ok := patched["body"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "", body["content"], "description も揃える(この Blocker は説明なし)")
	requireCommonHeaders(t, reqs)
}

// TestFindBlockerByOriginTagEncodesSpacesAsPercent20 は 409 収容時の
// findBlockerByOriginTag が発行する $filter クエリで、空白が "+" ではなく
// "%20" でエンコードされていることを確認する。net/url は "+" と "%20" を
// symmetric に相互変換するため r.URL.Query() 経由の検査では検出できず、
// 実際にワイヤへ送られるバイト列である r.URL.RawQuery を直接見る必要がある
// (Microsoft Graph の OData パーサは $filter/$expand 中の空白代替 "+" を
// 受理しない既知挙動があるため)。
func TestFindBlockerByOriginTagEncodesSpacesAsPercent20(t *testing.T) {
	var rawQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/me/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":{"code":"ErrorDuplicateTransactionId","message":"duplicate transactionId"}}`)
			return
		}
		rawQuery = r.URL.RawQuery
		fmt.Fprint(w, `{"value":[{"id":"existing-ev-9"}]}`)
	})
	mux.HandleFunc("/me/events/existing-ev-9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"existing-ev-9"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	b := model.Blocker{
		Title:     "予定あり",
		StartUTC:  time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:    time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
		OriginTag: "personal:ev1",
	}
	_, err := c.CreateBlocker(context.Background(), testCal, b, "calsync-abc123")
	require.NoError(t, err)

	require.NotEmpty(t, rawQuery)
	require.Contains(t, rawQuery, "%20", "raw query must percent-encode spaces as %%20: %s", rawQuery)
	require.NotContains(t, rawQuery, "+", "raw query must not contain a literal '+' (Graph rejects it as a space substitute in $filter/$expand): %s", rawQuery)
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

// 404(手動削除等でブロッカーが消えている)は provider.ErrNotFound に写像され、
// エンジンの「pending 化して再作成」フォールバックを発動させる(仕様8章4)。
func TestUpdateBlockerNotFoundMapsToErrNotFound(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"ErrorItemNotFound","message":"The specified object was not found in the store."}}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	b := model.Blocker{
		StartUTC: time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
	}
	err := c.UpdateBlocker(context.Background(), testCal, "ev-gone", b)
	require.ErrorIs(t, err, provider.ErrNotFound)
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

// TestListBlockersEncodesSpacesAsPercent20 は ListBlockers の一覧 $filter と、
// 各件取得(getBlockerRecord)の $expand の両方で、空白が "+" ではなく "%20"
// でエンコードされていることを確認する。デコード後の r.URL.Query() では
// net/url が "+"/"%20" を対称変換してしまい検出できないため、実際に送信
// されたバイト列である r.URL.RawQuery を直接検査する。
func TestListBlockersEncodesSpacesAsPercent20(t *testing.T) {
	var listRawQuery, itemRawQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/me/events", func(w http.ResponseWriter, r *http.Request) {
		listRawQuery = r.URL.RawQuery
		fmt.Fprint(w, `{"value":[{"id":"blk-1"}]}`)
	})
	mux.HandleFunc("/me/events/blk-1", func(w http.ResponseWriter, r *http.Request) {
		itemRawQuery = r.URL.RawQuery
		fmt.Fprint(w, `{"id":"blk-1","isAllDay":false,
			"start":{"dateTime":"2026-07-10T01:00:00.0000000","timeZone":"UTC"},
			"end":{"dateTime":"2026-07-10T02:00:00.0000000","timeZone":"UTC"},
			"singleValueExtendedProperties":[{"id":"String {b7dbd76c-3a35-4b41-9d80-6a3f31f2a6b9} Name calsyncOrigin","value":"personal:ev1"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL, []string{"busy"})
	_, err := c.ListBlockers(context.Background(), testCal, testWindow)
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"$filter (list)", listRawQuery},
		{"$expand (item)", itemRawQuery},
	} {
		require.NotEmpty(t, tc.raw, tc.name)
		require.Contains(t, tc.raw, "%20", "%s: raw query must percent-encode spaces as %%20: %s", tc.name, tc.raw)
		require.NotContains(t, tc.raw, "+", "%s: raw query must not contain a literal '+' (Graph rejects it as a space substitute in $filter/$expand): %s", tc.name, tc.raw)
	}
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

func TestGraphBlockerDescriptionSentAndCleared(t *testing.T) {
	var created, patched map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/me/events", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&created)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"blk1"}`)
	})
	mux.HandleFunc("/me/events/blk1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&patched)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"blk1"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv.URL, []string{"busy"})

	b := model.Blocker{
		Title:       "予定あり",
		StartUTC:    time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:      time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
		OriginTag:   "a:ev1",
		Description: "calsync: ミラー元アカウント = a",
	}
	_, err := c.CreateBlocker(context.Background(), testCal, b, "txn1")
	require.NoError(t, err)
	body, ok := created["body"].(map[string]any)
	require.True(t, ok, "作成ボディに body(説明)が入る")
	require.Equal(t, "text", body["contentType"])
	require.Equal(t, "calsync: ミラー元アカウント = a", body["content"])

	b.Description = ""
	require.NoError(t, c.UpdateBlocker(context.Background(), testCal, "blk1", b))
	pb, ok := patched["body"].(map[string]any)
	require.True(t, ok, "patch は body キーを常に含む(空でクリア)")
	require.Equal(t, "", pb["content"])
}
