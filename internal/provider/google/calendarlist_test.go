package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListCalendars(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me/calendarList" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if page == 0 {
			page++
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "primary-cal-id", "summary": "Main", "primary": true, "accessRole": "owner"},
				},
				"nextPageToken": "p2",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"id": "team-cal-id@group.calendar.google.com", "summary": "Team", "accessRole": "reader"},
			},
		})
	}))
	defer srv.Close()

	c := New(nil, "personal")
	c.baseURL = srv.URL
	got, err := c.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(got) != 2 || !got[0].Primary || got[1].ID != "team-cal-id@group.calendar.google.com" {
		t.Fatalf("got %+v", got)
	}
}

// TestListCalendarsErrorPrefix は F1: ListCalendars のエラーが同ファイルの他
// メソッド(GetCalendarTimezone 等)と同じ "google[%s]: ..." プレフィックス
// 慣習に従うことを確認する回帰テスト。
func TestListCalendarsErrorPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(nil, "personal")
	c.baseURL = srv.URL
	_, err := c.ListCalendars(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.HasPrefix(err.Error(), "google[personal]: calendar list: ") {
		t.Fatalf("error = %q, want prefix %q", err.Error(), "google[personal]: calendar list: ")
	}
}
