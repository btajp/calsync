package appserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/provider/google"
)

func TestCalendarsEndpoint(t *testing.T) {
	s, dir := testServer(t)
	// new-acct はまだ config には無いが(追加ウィザードの想定どおり)、直前の
	// OAuth 認可フローでトークンだけは保存済み、という状態を再現する。
	tokens := &auth.TokenStore{Dir: dir}
	if err := tokens.Save("new-acct", &oauth2.Token{AccessToken: "at", RefreshToken: "rt"}); err != nil {
		t.Fatalf("save token: %v", err)
	}
	s.ListCals = func(ctx context.Context, cfg *config.Config, acct config.Account, dataDir string) ([]google.CalendarListEntry, error) {
		if acct.ID != "new-acct" {
			t.Errorf("acct = %+v", acct)
		}
		return []google.CalendarListEntry{{ID: "primary", Summary: "Main", Primary: true, AccessRole: "owner"}}, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got struct {
		Calendars []google.CalendarListEntry `json:"calendars"`
	}
	res := get(t, srv, "test-token", "/api/accounts/new-acct/calendars?provider=google", &got)
	if res.StatusCode != 200 || len(got.Calendars) != 1 {
		t.Fatalf("res=%d got=%+v", res.StatusCode, got)
	}
	// microsoft は 400
	res2 := get(t, srv, "test-token", "/api/accounts/work-ms/calendars?provider=microsoft", nil)
	if res2.StatusCode != http.StatusBadRequest {
		t.Fatalf("ms status = %d", res2.StatusCode)
	}
}

// TestCalendarsEndpointEmptyReturnsEmptyArray は最終ホールレビュー Fix 1 の回帰
// テスト。ListCals が 0 件(nil スライス)を返しても、レスポンス JSON の
// calendars は "[]"(null ではない)になること。フロントの res.calendars 走査は
// null だとクラッシュする。
func TestCalendarsEndpointEmptyReturnsEmptyArray(t *testing.T) {
	s, dir := testServer(t)
	tokens := &auth.TokenStore{Dir: dir}
	if err := tokens.Save("new-acct", &oauth2.Token{AccessToken: "at", RefreshToken: "rt"}); err != nil {
		t.Fatalf("save token: %v", err)
	}
	s.ListCals = func(ctx context.Context, cfg *config.Config, acct config.Account, dataDir string) ([]google.CalendarListEntry, error) {
		return nil, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/accounts/new-acct/calendars?provider=google", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `"calendars":[]`) {
		t.Fatalf(`expected literal "calendars":[] in response body, got %s`, body)
	}
}

func TestCalendarsTokenMissing(t *testing.T) {
	s, _ := testServer(t)
	s.ListCals = func(ctx context.Context, cfg *config.Config, acct config.Account, dataDir string) ([]google.CalendarListEntry, error) {
		t.Fatal("ListCals should not be called when token is missing")
		return nil, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res := get(t, srv, "test-token", "/api/accounts/no-token/calendars?provider=google", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d", res.StatusCode)
	}
}
