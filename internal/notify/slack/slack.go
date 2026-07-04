// Package slack は Slack Web API(chat.postMessage / conversations.open)への
// 通知送信を実装する。依存追加なし(net/http 直叩き。スペック 8 章)。
// Slack の方言(ok:false + error 文字列)はこのパッケージから漏らさない:
// リトライ可否は notify.ErrNonRetryable への包み込みで表現する。
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/notify"
)

// コンパイル時に engine.Notifier を満たすことを保証する。
var _ engine.Notifier = (*Client)(nil)

// Client は engine.Notifier の Slack 実装。
type Client struct {
	Token   string
	Channel string         // C…/G…: そのまま / U…: conversations.open で DM に解決
	BaseURL string         // テスト用。空なら https://slack.com/api
	TZ      *time.Location // 表示 TZ。nil なら time.Local(コンテナ TZ)

	httpc *http.Client

	mu       sync.Mutex
	resolved string // U… を解決した DM チャンネル ID のキャッシュ(プロセス存続中)
}

func New(token, channel string) *Client {
	// 1 リクエスト 10 秒タイムアウト: Run ループ内の同期呼び出しのため上限を保証する(スペック 8 章)
	return &Client{Token: token, Channel: channel, httpc: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) SendDigest(ctx context.Context, day time.Time, entries []engine.DigestEntry, failedAccounts []string) error {
	return c.post(ctx, formatDigest(day, entries, failedAccounts, c.loc()))
}

func (c *Client) SendReminder(ctx context.Context, e engine.DigestEntry, lead time.Duration) error {
	return c.post(ctx, formatReminder(e, lead, c.loc()))
}

func (c *Client) loc() *time.Location {
	if c.TZ != nil {
		return c.TZ
	}
	return time.Local
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://slack.com/api"
}

type apiResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
}

// call は Slack Web API を 1 回呼ぶ。分類規則(スペック 8 章):
//   - ネットワークエラー・5xx・429 → リトライ可能(sentinel を含まない)
//   - それ以外の非 2xx・ok:false(未知のエラー文字列含む)→ notify.ErrNonRetryable
func (c *Client) call(ctx context.Context, method string, payload any) (*apiResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("slack %s: encode: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("slack %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack %s: %w", method, err) // ネットワーク系 → リトライ可能
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("slack %s: status %d", method, resp.StatusCode) // リトライ可能
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack %s: status %d: %w", method, resp.StatusCode, notify.ErrNonRetryable)
	}
	var ar apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, fmt.Errorf("slack %s: decode: %w", method, err)
	}
	if !ar.OK {
		return nil, fmt.Errorf("slack %s: %s: %w", method, ar.Error, notify.ErrNonRetryable)
	}
	return &ar, nil
}

// channelID は投稿先チャンネル ID を返す。U… は初回のみ conversations.open で
// DM チャンネルに解決し、プロセス存続中はキャッシュする(スペック 8 章)。
func (c *Client) channelID(ctx context.Context) (string, error) {
	if !strings.HasPrefix(c.Channel, "U") {
		return c.Channel, nil
	}
	c.mu.Lock()
	cached := c.resolved
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	ar, err := c.call(ctx, "conversations.open", map[string]string{"users": c.Channel})
	if err != nil {
		return "", err
	}
	if ar.Channel.ID == "" {
		return "", fmt.Errorf("slack conversations.open: empty channel id: %w", notify.ErrNonRetryable)
	}
	c.mu.Lock()
	c.resolved = ar.Channel.ID
	c.mu.Unlock()
	return ar.Channel.ID, nil
}

func (c *Client) post(ctx context.Context, text string) error {
	ch, err := c.channelID(ctx)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, "chat.postMessage", map[string]string{"channel": ch, "text": text})
	return err
}
