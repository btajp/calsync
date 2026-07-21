package clients

import (
	"os"
	"strings"
	"testing"

	"github.com/btajp/calsync/internal/config"
)

func TestOAuthConfigForMicrosoft(t *testing.T) {
	cfg := &config.Config{}
	cfg.Providers.Microsoft.ClientID = "client-id-value"
	oc, err := OAuthConfigFor(cfg, config.Account{ID: "work-ms", Provider: "microsoft"})
	if err != nil {
		t.Fatalf("OAuthConfigFor: %v", err)
	}
	// 「localhost・パスなし」のリダイレクト形式(不変条件)
	if oc.RedirectURL != "http://localhost" {
		t.Fatalf("redirect = %q", oc.RedirectURL)
	}
}

func TestOAuthConfigForGoogleScopes(t *testing.T) {
	// credentials ファイルを一時生成
	dir := t.TempDir()
	credPath := dir + "/creds.json"
	credJSON := `{"installed":{"client_id":"x","client_secret":"y","redirect_uris":["http://localhost"],"auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`
	if err := os.WriteFile(credPath, []byte(credJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Providers.Google.CredentialsFile = credPath
	oc, err := OAuthConfigFor(cfg, config.Account{ID: "personal", Provider: "google"})
	if err != nil {
		t.Fatalf("OAuthConfigFor: %v", err)
	}
	joined := strings.Join(oc.Scopes, " ")
	if !strings.Contains(joined, "calendar.events") || !strings.Contains(joined, "calendar.calendarlist.readonly") {
		t.Fatalf("scopes = %v", oc.Scopes)
	}
}
