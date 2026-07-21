package clients

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/provider"
	"github.com/btajp/calsync/internal/provider/google"
	msprov "github.com/btajp/calsync/internal/provider/microsoft"
)

// OAuthConfigFor は各プロバイダの oauth2.Config を組み立てる。
// auth add(認可フロー)とトークンリフレッシュ(TokenSource)の両方で使う。
func OAuthConfigFor(cfg *config.Config, acct config.Account) (*oauth2.Config, error) {
	switch acct.Provider {
	case "google":
		if cfg.Providers.Google.CredentialsFile == "" {
			return nil, errors.New("providers.google.credentials_file is not set in the config")
		}
		b, err := os.ReadFile(cfg.Providers.Google.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("read google credentials: %w", err)
		}
		// calendarlist.readonly はデスクトップアプリのカレンダー選択 UI 用。refresh には影響しない(既存トークンは再認可まで list 不可)。
		return googleoauth.ConfigFromJSON(b, "https://www.googleapis.com/auth/calendar.events", "https://www.googleapis.com/auth/calendar.calendarlist.readonly")
	case "microsoft":
		if cfg.Providers.Microsoft.ClientID == "" {
			return nil, errors.New("providers.microsoft.client_id is not set in the config")
		}
		return &oauth2.Config{
			ClientID: cfg.Providers.Microsoft.ClientID,
			// アプリ登録(http://localhost)と同じ「localhost・パスなし」の形にする。
			// MSA(login.live.com)はポートを無視するがパスは照合するため、
			// /callback 付きだと invalid_request になる(実測 2026-07-03。MSAL と同じ形)。
			// 実ポートは RunLoopbackFlow がホスト名・パスを保持したまま差し込む。
			RedirectURL: "http://localhost",
			Endpoint: oauth2.Endpoint{
				AuthURL:       "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
				TokenURL:      "https://login.microsoftonline.com/common/oauth2/v2.0/token",
				DeviceAuthURL: "https://login.microsoftonline.com/common/oauth2/v2.0/devicecode",
			},
			// MailboxSettings.Read は GetCalendarTimezone(/me/mailboxSettings/timeZone)
			// に必要(Calendars.ReadWrite だけでは 403。最終ホールブランチレビュー追補 Issue 1)
			Scopes: []string{
				"offline_access",
				"https://graph.microsoft.com/Calendars.ReadWrite",
				"https://graph.microsoft.com/MailboxSettings.Read",
			},
		}, nil
	default:
		return nil, fmt.Errorf("account %s: unknown provider %q", acct.ID, acct.Provider)
	}
}

// BuildProvider は 1 アカウント分の Provider を構築する。
// トークンは PersistingTokenSource で包み、リフレッシュ(MS のローテーション含む)
// のたびにディスクへ書き戻す(仕様 9.3)。
func BuildProvider(cfg *config.Config, tokens *auth.TokenStore, acct config.Account) (provider.Provider, error) {
	tok, err := tokens.Load(acct.ID)
	if err != nil {
		return nil, fmt.Errorf("account %s: no token (run: calsync auth add %s): %w", acct.ID, acct.ID, err)
	}
	ocfg, err := OAuthConfigFor(cfg, acct)
	if err != nil {
		return nil, err
	}
	ts := auth.PersistingTokenSource(acct.ID, tokens, ocfg.TokenSource(context.Background(), tok))
	switch acct.Provider {
	case "google":
		return google.New(ts, acct.ID), nil
	case "microsoft":
		return msprov.New(ts, acct.ID, cfg.BusyShowAs), nil
	default:
		return nil, fmt.Errorf("account %s: unknown provider %q", acct.ID, acct.Provider)
	}
}

// BuildReadOnlyProvider は 1 アカウント分の読み取り専用 Provider を構築する。
// トークンファイルをロードして oauth2.StaticTokenSource に包むだけで、
// リフレッシュも TokenStore への書き戻しも一切行わない(BuildProvider の
// PersistingTokenSource とは異なる)。
//
// 用途: appserver(デスクトップアプリの GET /api/events)が稼働中デーモンと
// 同じトークンファイルを読む際、両者が同時にリフレッシュを試みると Microsoft の
// リフレッシュトークンローテーションでどちらかが失効側を掴む競合が起こりうる
// (デスクトップカレンダービュー設計 2026-07-21 §3)。静的トークンソースなら
// 構造的にこの競合を避けられる。デーモンは毎分の同期でトークンを更新・永続化
// しているためディスク上の access token は通常有効だが、期限切れ(エッジ)の
// 場合はそのまま使われて API 側で 401(provider.ErrAuthExpired 相当)になる —
// 呼び出し側がそのアカウントを failed 扱いにすること。
func BuildReadOnlyProvider(cfg *config.Config, tokens *auth.TokenStore, acct config.Account) (provider.Provider, error) {
	tok, err := tokens.Load(acct.ID)
	if err != nil {
		return nil, fmt.Errorf("account %s: no token (run: calsync auth add %s): %w", acct.ID, acct.ID, err)
	}
	ts := oauth2.StaticTokenSource(tok)
	switch acct.Provider {
	case "google":
		return google.New(ts, acct.ID), nil
	case "microsoft":
		return msprov.New(ts, acct.ID, cfg.BusyShowAs), nil
	default:
		return nil, fmt.Errorf("account %s: unknown provider %q", acct.ID, acct.Provider)
	}
}

// BuildGoogleClient は google 用の具象クライアントを返す(CalendarList 用)。
// provider.Provider インターフェースには CalendarList が無いため具象型を返す。
func BuildGoogleClient(cfg *config.Config, tokens *auth.TokenStore, acct config.Account) (*google.Client, error) {
	if acct.Provider != "google" {
		return nil, fmt.Errorf("account %s: provider %q does not support calendar listing", acct.ID, acct.Provider)
	}
	tok, err := tokens.Load(acct.ID)
	if err != nil {
		return nil, fmt.Errorf("account %s: no token: %w", acct.ID, err)
	}
	ocfg, err := OAuthConfigFor(cfg, acct)
	if err != nil {
		return nil, err
	}
	ts := auth.PersistingTokenSource(acct.ID, tokens, ocfg.TokenSource(context.Background(), tok))
	return google.New(ts, acct.ID), nil
}
