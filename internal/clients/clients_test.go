package clients

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/btajp/calsync/internal/auth"
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

// TestBuildReadOnlyProviderMissingToken はトークン未保存アカウントに対して
// エラーを返すこと(BuildProvider と同じトークン欠落時の契約)を検証する。
func TestBuildReadOnlyProviderMissingToken(t *testing.T) {
	cfg := &config.Config{}
	tokens := &auth.TokenStore{Dir: t.TempDir()}
	_, err := BuildReadOnlyProvider(cfg, tokens, config.Account{ID: "personal", Provider: "google"})
	if err == nil {
		t.Fatal("expected error for missing token, got nil")
	}
}

// TestBuildReadOnlyProviderUsesStaticTokenSource は、保存済みトークンから
// リフレッシュ・永続化を一切しない静的トークンソースでプロバイダを構築できる
// ことを検証する(デスクトップカレンダービュー設計 2026-07-21 §3: appserver は
// 稼働中デーモンとのトークンリフレッシュ競合を避けるためこの経路を使う)。
// google/microsoft いずれも構築できること、未知プロバイダはエラーになることを見る。
func TestBuildReadOnlyProviderUsesStaticTokenSource(t *testing.T) {
	dir := t.TempDir()
	tokens := &auth.TokenStore{Dir: dir}
	if err := tokens.Save("personal", &oauth2.Token{AccessToken: "at-personal"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if err := tokens.Save("work-ms", &oauth2.Token{AccessToken: "at-work-ms"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	cfg := &config.Config{}

	if _, err := BuildReadOnlyProvider(cfg, tokens, config.Account{ID: "personal", Provider: "google"}); err != nil {
		t.Fatalf("google: %v", err)
	}
	if _, err := BuildReadOnlyProvider(cfg, tokens, config.Account{ID: "work-ms", Provider: "microsoft"}); err != nil {
		t.Fatalf("microsoft: %v", err)
	}
	if _, err := BuildReadOnlyProvider(cfg, tokens, config.Account{ID: "personal", Provider: "unknown"}); err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
}
