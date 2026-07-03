package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/work-a-co/calsync/internal/model"
	"github.com/work-a-co/calsync/internal/provider"
)

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	return m
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestCreateBlockerTimedBody(t *testing.T) {
	b := model.Blocker{
		Title:     "予定あり",
		StartUTC:  time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:    time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
		OriginTag: model.OriginTagOf("work", "ev-origin-1"),
	}
	idem := model.GoogleBlockerID(b.OriginTag, "test-account")

	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id": %q}`, idem)
	})
	c := newTestClient(t, handler)

	eventID, err := c.CreateBlocker(context.Background(), testRef, b, idem)
	require.NoError(t, err)
	require.Equal(t, idem, eventID)

	reqs := rec.all()
	require.Len(t, reqs, 1)
	require.Equal(t, http.MethodPost, reqs[0].Method)
	require.Equal(t, "/calendars/primary/events", reqs[0].Path)

	m := decodeBody(t, reqs[0].Body)
	require.Equal(t, idem, m["id"], "id はクライアント生成の冪等キー")
	require.Equal(t, "予定あり", m["summary"])
	require.Equal(t, "opaque", m["transparency"])
	require.Equal(t, "private", m["visibility"])

	rem, ok := m["reminders"].(map[string]any)
	require.True(t, ok, "reminders がボディに無い")
	require.Equal(t, false, rem["useDefault"], "useDefault=false を明示送信する(ゼロ値でも省略しない)")

	ext, ok := m["extendedProperties"].(map[string]any)
	require.True(t, ok, "extendedProperties がボディに無い")
	priv, ok := ext["private"].(map[string]any)
	require.True(t, ok, "extendedProperties.private がボディに無い")
	require.Equal(t, "v1", priv["calsync"])
	require.Equal(t, "work:ev-origin-1", priv["calsync-origin"])

	start, ok := m["start"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-07-10T01:00:00Z", start["dateTime"])
	require.Equal(t, "UTC", start["timeZone"])
	end, ok := m["end"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-07-10T02:00:00Z", end["dateTime"])
	require.Equal(t, "UTC", end["timeZone"])
}

func TestCreateBlockerAllDayBody(t *testing.T) {
	b := model.Blocker{
		Title:       "予定あり",
		IsAllDay:    true,
		AllDayStart: "2026-07-15",
		AllDayEnd:   "2026-07-16",
		OriginTag:   model.OriginTagOf("work", "ev-origin-2"),
	}
	idem := model.GoogleBlockerID(b.OriginTag, "test-account")

	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id": %q}`, idem)
	})
	c := newTestClient(t, handler)

	eventID, err := c.CreateBlocker(context.Background(), testRef, b, idem)
	require.NoError(t, err)
	require.Equal(t, idem, eventID)

	m := decodeBody(t, rec.all()[0].Body)
	start, ok := m["start"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-07-15", start["date"], "終日は AllDayStart をそのまま date に")
	require.NotContains(t, start, "dateTime")
	end, ok := m["end"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-07-16", end["date"], "終日 end は排他的日付をそのまま")
	require.NotContains(t, end, "dateTime")
}

func TestCreateBlockerConflictAdoptsExisting(t *testing.T) {
	b := model.Blocker{
		Title:     "予定あり",
		StartUTC:  time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:    time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
		OriginTag: model.OriginTagOf("work", "ev-origin-1"),
	}
	idem := model.GoogleBlockerID(b.OriginTag, "test-account")

	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error": {"errors": [{"domain": "global", "reason": "duplicate",
				"message": "The requested identifier already exists."}],
				"code": 409, "message": "The requested identifier already exists."}}`)
			return
		}
		fmt.Fprintf(w, `{"id": %q, "status": "confirmed"}`, idem)
	})
	c := newTestClient(t, handler)

	eventID, err := c.CreateBlocker(context.Background(), testRef, b, idem)
	require.NoError(t, err, "409 は作成済みとして収容しエラーにしない(仕様書6.4)")
	require.Equal(t, idem, eventID)

	reqs := rec.all()
	require.Len(t, reqs, 2)
	require.Equal(t, http.MethodPost, reqs[0].Method)
	require.Equal(t, http.MethodGet, reqs[1].Method, "409 後は events.get で実在確認する")
	require.Equal(t, "/calendars/primary/events/"+idem, reqs[1].Path)
}

// 409 収容が cancelled(削除済み)イベントの ID を無条件に返すと、busy→free→busy の
// 削除→再作成シナリオでカレンダーに見えないイベントを active mapping に収容してしまい、
// ブロッカーが二度と出現しなくなる(最終ホールブランチレビュー所見1)。
// events.get が status=cancelled を返した場合は events.update で本来の insert ボディ
// (ID を除く)を送って蘇生し、その ID を返さねばならない。
func TestCreateBlockerConflictResurrectsCancelledEvent(t *testing.T) {
	b := model.Blocker{
		Title:     "予定あり",
		StartUTC:  time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:    time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
		OriginTag: model.OriginTagOf("work", "ev-origin-1"),
	}
	idem := model.GoogleBlockerID(b.OriginTag, "test-account")

	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error": {"errors": [{"domain": "global", "reason": "duplicate",
				"message": "The requested identifier already exists."}],
				"code": 409, "message": "The requested identifier already exists."}}`)
		case http.MethodGet:
			fmt.Fprintf(w, `{"id": %q, "status": "cancelled"}`, idem)
		case http.MethodPut:
			fmt.Fprintf(w, `{"id": %q, "status": "confirmed"}`, idem)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})
	c := newTestClient(t, handler)

	eventID, err := c.CreateBlocker(context.Background(), testRef, b, idem)
	require.NoError(t, err, "cancelled イベントの蘇生はエラーにしない")
	require.Equal(t, idem, eventID)

	reqs := rec.all()
	require.Len(t, reqs, 3)
	require.Equal(t, http.MethodPost, reqs[0].Method)
	require.Equal(t, http.MethodGet, reqs[1].Method, "409 後は events.get で状態確認する")
	require.Equal(t, http.MethodPut, reqs[2].Method, "cancelled なら events.update で蘇生する")
	require.Equal(t, "/calendars/primary/events/"+idem, reqs[2].Path)

	m := decodeBody(t, reqs[2].Body)
	require.Equal(t, "confirmed", m["status"], "蘇生ボディは status=confirmed を明示送信する")
	require.Equal(t, "予定あり", m["summary"])
	require.Equal(t, "opaque", m["transparency"])
	require.Equal(t, "private", m["visibility"])

	rem, ok := m["reminders"].(map[string]any)
	require.True(t, ok, "蘇生ボディにも reminders を含める")
	require.Equal(t, false, rem["useDefault"])

	ext, ok := m["extendedProperties"].(map[string]any)
	require.True(t, ok, "蘇生ボディにも extendedProperties を含める")
	priv, ok := ext["private"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "v1", priv["calsync"])
	require.Equal(t, "work:ev-origin-1", priv["calsync-origin"])

	start, ok := m["start"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-07-10T01:00:00Z", start["dateTime"])
	end, ok := m["end"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-07-10T02:00:00Z", end["dateTime"])
}

func TestUpdateBlockerPatchesTimesOnly(t *testing.T) {
	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id": "ev-blk-1"}`)
	})
	c := newTestClient(t, handler)

	b := model.Blocker{
		Title:    "予定あり", // patch には含めない(時刻変更のみ)
		StartUTC: time.Date(2026, 7, 11, 3, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 11, 4, 0, 0, 0, time.UTC),
	}
	err := c.UpdateBlocker(context.Background(), testRef, "ev-blk-1", b)
	require.NoError(t, err)

	reqs := rec.all()
	require.Len(t, reqs, 1)
	require.Equal(t, http.MethodPatch, reqs[0].Method)
	require.Equal(t, "/calendars/primary/events/ev-blk-1", reqs[0].Path)

	m := decodeBody(t, reqs[0].Body)
	require.ElementsMatch(t, []string{"start", "end"}, mapKeys(m), "patch は start/end のみ")
	start, ok := m["start"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-07-11T03:00:00Z", start["dateTime"])
	end, ok := m["end"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-07-11T04:00:00Z", end["dateTime"])
}

// 404(手動削除等でブロッカーが消えている)は provider.ErrNotFound に写像され、
// エンジンの「pending 化して再作成」フォールバックを発動させる(仕様8章4)。
func TestUpdateBlockerNotFoundMapsToErrNotFound(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error": {"code": 404, "message": "Not Found"}}`)
	})
	c := newTestClient(t, handler)

	b := model.Blocker{
		StartUTC: time.Date(2026, 7, 11, 3, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 11, 4, 0, 0, 0, time.UTC),
	}
	err := c.UpdateBlocker(context.Background(), testRef, "ev-gone", b)
	require.ErrorIs(t, err, provider.ErrNotFound)
}

func TestDeleteBlocker(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "204 は成功", status: http.StatusNoContent, body: "", wantErr: false},
		{name: "404 は成功扱い(既に無い)", status: http.StatusNotFound,
			body: `{"error": {"code": 404, "message": "Not Found"}}`, wantErr: false},
		{name: "410 は成功扱い(既に削除済み)", status: http.StatusGone,
			body: `{"error": {"code": 410, "message": "Resource has been deleted"}}`, wantErr: false},
		{name: "500 はエラー", status: http.StatusInternalServerError,
			body: `{"error": {"code": 500, "message": "Backend Error"}}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.body != "" {
					fmt.Fprint(w, tc.body)
				}
			})
			c := newTestClient(t, handler)

			err := c.DeleteBlocker(context.Background(), testRef, "ev-blk-1")
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestListBlockers(t *testing.T) {
	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			fmt.Fprint(w, `{
				"items": [{"id": "blk-1", "status": "confirmed",
					"extendedProperties": {"private": {"calsync": "v1", "calsync-origin": "work:ev-9"}},
					"start": {"dateTime": "2026-07-10T01:00:00Z"},
					"end": {"dateTime": "2026-07-10T02:00:00Z"}}],
				"nextPageToken": "page-2"
			}`)
			return
		}
		fmt.Fprint(w, `{
			"items": [{"id": "blk-2", "status": "confirmed",
				"extendedProperties": {"private": {"calsync": "v1", "calsync-origin": "work:ev-10"}},
				"start": {"date": "2026-07-15"},
				"end": {"date": "2026-07-16"}}]
		}`)
	})
	c := newTestClient(t, handler)

	records, err := c.ListBlockers(context.Background(), testRef, testWindow)
	require.NoError(t, err)

	want := []model.BlockerRecord{
		{
			EventID:   "blk-1",
			OriginTag: "work:ev-9",
			TimeHash: model.TimeHash(model.NormalizedEvent{
				StartUTC: time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
			}),
		},
		{
			EventID:   "blk-2",
			OriginTag: "work:ev-10",
			TimeHash: model.TimeHash(model.NormalizedEvent{
				IsAllDay:    true,
				AllDayStart: "2026-07-15",
				AllDayEnd:   "2026-07-16",
			}),
		},
	}
	require.Equal(t, want, records, "TimeHash は終日/時刻指定に応じて model.TimeHash を適用")

	reqs := rec.all()
	require.Len(t, reqs, 2)
	q := reqs[0].Query
	require.Equal(t, "calsync=v1", q.Get("privateExtendedProperty"))
	require.Equal(t, "2026-07-03T00:00:00Z", q.Get("timeMin"))
	require.Equal(t, "2026-10-03T00:00:00Z", q.Get("timeMax"))
	require.False(t, q.Has("syncToken"), "privateExtendedProperty は syncToken と併用不可(リコンサイル専用)")
	require.Equal(t, "page-2", reqs[1].Query.Get("pageToken"))
}

// GetCalendarTimezone は calendars.get ではなく events.list(maxResults=1)の
// 応答エンベロープの timeZone を使う。calendars.get は calendar.readonly 系
// スコープが必要で、calsync の calendar.events スコープでは実環境 403 になる
// (最終ホールブランチレビュー追補 Issue 1)。
func TestGetCalendarTimezone(t *testing.T) {
	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"timeZone": "Asia/Tokyo", "items": [], "nextSyncToken": "s"}`)
	})
	c := newTestClient(t, handler)

	tz, err := c.GetCalendarTimezone(context.Background(), testRef)
	require.NoError(t, err)
	require.Equal(t, "Asia/Tokyo", tz)

	reqs := rec.all()
	require.Len(t, reqs, 1)
	require.Equal(t, http.MethodGet, reqs[0].Method)
	require.Equal(t, "/calendars/primary/events", reqs[0].Path, "events.list を使う(calendars.get はスコープ不足)")
	require.Equal(t, "1", reqs[0].Query.Get("maxResults"), "maxResults=1 で軽量に取得する")
}

// events.list が timeZone を返さない場合はエラー(空 TZ をキャッシュしない)。
func TestGetCalendarTimezoneMissingIsError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items": []}`)
	})
	c := newTestClient(t, handler)

	_, err := c.GetCalendarTimezone(context.Background(), testRef)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no timeZone")
}
