package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
