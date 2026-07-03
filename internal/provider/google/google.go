// Package google は Google Calendar API(calendar/v3)向けの
// provider.Provider 実装を提供する。
package google

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/work-a-co/calsync/internal/provider"
)

// maxRetries は 403/429(usageLimits 系)に対する再試行回数の上限(仕様書10章)。
const maxRetries = 5

// Client は Google Calendar 用のプロバイダ実装。
type Client struct {
	ts        oauth2.TokenSource
	accountID string

	// baseURL はテストで httptest.Server.URL に差し替える。空なら本番エンドポイント。
	baseURL string
	// retryBase はバックオフの基準待ち時間。テストでは短縮する。
	retryBase time.Duration

	mu  sync.Mutex
	svc *calendar.Service
}

// New は Client を作る。calendar.Service は初回利用時に遅延構築する
// (構築前にテストが baseURL を差し替えられるようにするため)。
func New(ts oauth2.TokenSource, accountID string) *Client {
	return &Client{ts: ts, accountID: accountID, retryBase: 500 * time.Millisecond}
}

// service は calendar.Service を遅延構築して返す(並行呼び出し安全)。
// option.WithHTTPClient を渡すため、ライブラリ側のデフォルト認証解決は行われない
// (テストでは ts=nil でも動く)。
func (c *Client) service(ctx context.Context) (*calendar.Service, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.svc != nil {
		return c.svc, nil
	}
	opts := []option.ClientOption{option.WithHTTPClient(oauth2.NewClient(ctx, c.ts))}
	if c.baseURL != "" {
		opts = append(opts, option.WithEndpoint(c.baseURL))
	}
	svc, err := calendar.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("google[%s]: build calendar service: %w", c.accountID, err)
	}
	c.svc = svc
	return svc, nil
}

// doWithRetry は fn を実行し、403/429 の usageLimits 系エラーの場合のみ
// 指数バックオフ+ジッターで最大 maxRetries 回まで再試行する(仕様書10章)。
// それ以外のエラー(410 等)は即座に返す。
func (c *Client) doWithRetry(ctx context.Context, fn func() error) error {
	base := c.retryBase
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	var err error
	for attempt := 0; ; attempt++ {
		err = fn()
		if err == nil || !isRateLimited(err) || attempt >= maxRetries {
			return err
		}
		delay := base<<attempt + rand.N(base) // 指数バックオフ + ジッター
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// normalizeAuthErr は Google 由来の認証失効エラーを provider.ErrAuthExpired に
// 正規化する(仕様書9.3)。判定対象:
//   - googleapi.Error で Code==401(Google 固有の判定なのでここで行う)
//   - oauth2.RetrieveError で invalid_grant / interaction_required
//     (TokenSource の失敗は http.Client 経由で *url.Error に包まれて届く。
//     この判定自体は provider.NormalizeAuthErr に委譲し、autherr.go に
//     googleapi 依存を持ち込まないようにする)
//
// 該当しない場合は err をそのまま返す。Changes 等、doWithRetry の呼び出し元が
// 共通で使う。
func normalizeAuthErr(err error) error {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusUnauthorized {
		return fmt.Errorf("%w: %w", provider.ErrAuthExpired, err)
	}
	return provider.NormalizeAuthErr(err)
}

// isRateLimited は Google のクォータ系エラー(再試行対象)かを判定する。
func isRateLimited(err error) bool {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return false
	}
	if gerr.Code == http.StatusTooManyRequests {
		return true
	}
	if gerr.Code != http.StatusForbidden {
		return false
	}
	for _, item := range gerr.Errors {
		switch item.Reason {
		case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded", "dailyLimitExceeded":
			return true
		}
	}
	return false
}
