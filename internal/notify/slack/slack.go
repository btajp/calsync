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
	"io"
	"log"
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
	Token    string
	Channel  string         // C…/G…: そのまま / U…: conversations.open で DM に解決
	BaseURL  string         // テスト用。空なら https://slack.com/api
	TZ       *time.Location // 表示 TZ。nil なら time.Local(コンテナ TZ)
	Accounts []string       // YAML 定義順のアカウント ID 列(色割当用。v2.1 スペック 5 章)

	httpc *http.Client

	mu       sync.Mutex
	resolved string // U… を解決した DM チャンネル ID のキャッシュ(プロセス存続中)
}

func New(token, channel string) *Client {
	// 1 リクエスト 10 秒タイムアウト: Run ループ内の同期呼び出しのため上限を保証する(スペック 8 章)
	return &Client{Token: token, Channel: channel, httpc: &http.Client{Timeout: 10 * time.Second}}
}

// colorPalette はアカウント色の固定パレット(v2.1 スペック 5 章)。
var colorPalette = [...]string{
	"#4285F4", "#0F9D58", "#F4B400", "#DB4437", "#7B1FA2", "#00ACC1", "#FF7043", "#5C6BC0",
}

// unknownAccountColor は Accounts に含まれないアカウント ID の既定色。
const unknownAccountColor = "#999999"

// colorFor はアカウント ID から表示色を決める。Accounts の定義順でパレットを巡回し、
// 未知のアカウントは unknownAccountColor にする(v2.1 スペック 5 章)。
func (c *Client) colorFor(accountID string) string {
	for i, id := range c.Accounts {
		if id == accountID {
			return colorPalette[i%len(colorPalette)]
		}
	}
	return unknownAccountColor
}

func (c *Client) SendDigest(ctx context.Context, day time.Time, entries []engine.DigestEntry, failedAccounts []string) error {
	blocks, attachments := digestMessage(day, entries, failedAccounts, c.loc(), c.colorFor)
	return c.post(ctx, formatDigest(day, entries, failedAccounts, c.loc()), blocks, attachments)
}

func (c *Client) SendReminder(ctx context.Context, e engine.DigestEntry, lead time.Duration) error {
	blocks, attachments := reminderMessage(e, lead, c.loc(), c.colorFor)
	return c.post(ctx, formatReminder(e, lead, c.loc()), blocks, attachments)
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

// Channel は json.RawMessage で受ける: chat.postMessage の成功応答は
// "channel" が文字列("C123…")、conversations.open の成功応答はオブジェクト
// ({"id":"D…"})で返る。共有の apiResponse で型を固定すると片方が
// json.UnmarshalTypeError になるため、必要な側(channelID)だけで解釈する。
type apiResponse struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error"`
	Channel json.RawMessage `json:"channel"`
}

// call は Slack Web API を 1 回呼ぶ。分類規則(スペック 8 章):
//   - ネットワークエラー・5xx・429 → リトライ可能(sentinel を含まない)
//   - それ以外の非 2xx・ok:false(未知のエラー文字列含む)・200 だが JSON デコード失敗
//     → notify.ErrNonRetryable
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
	defer func() {
		// keep-alive で接続を再利用できるよう、Close 前にボディを読み切る。
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("slack %s: status %d", method, resp.StatusCode) // リトライ可能
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack %s: status %d: %w", method, resp.StatusCode, notify.ErrNonRetryable)
	}
	var ar apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		// 200 なのに JSON が壊れている経路(プロキシ等)は毎 tick 再試行しても
		// 回復しないためリトライ不能に分類する(スペック 8 章)。
		return nil, fmt.Errorf("slack %s: decode: %w: %w", method, err, notify.ErrNonRetryable)
	}
	if !ar.OK {
		errMsg := ar.Error
		if errMsg == "" {
			errMsg = "unknown_error"
		}
		return nil, fmt.Errorf("slack %s: %s: %w", method, errMsg, notify.ErrNonRetryable)
	}
	return &ar, nil
}

// channelID は投稿先チャンネル ID を返す。U… は初回のみ conversations.open で
// DM チャンネルに解決し、プロセス存続中はキャッシュする(スペック 8 章)。
//
// このクライアントの送信頻度(digest/reminder のみ)では、解決からキャッシュ
// 書き込みまで mutex を握りっぱなしにしても実害がない。sync.Once は失敗時に
// 再試行できず失敗を永続キャッシュしてしまうため使わず、mutex で
// resolve-or-fetch 全体を保護して TOCTOU(並行初回送信での二重 open)を防ぐ。
func (c *Client) channelID(ctx context.Context) (string, error) {
	if !strings.HasPrefix(c.Channel, "U") {
		return c.Channel, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved != "" {
		return c.resolved, nil
	}
	ar, err := c.call(ctx, "conversations.open", map[string]string{"users": c.Channel})
	if err != nil {
		return "", err
	}
	var ch struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(ar.Channel, &ch); err != nil {
		return "", fmt.Errorf("slack conversations.open: decode channel: %w: %w", err, notify.ErrNonRetryable)
	}
	if ch.ID == "" {
		return "", fmt.Errorf("slack conversations.open: empty channel id: %w", notify.ErrNonRetryable)
	}
	c.resolved = ch.ID
	return ch.ID, nil
}

// post は blocks/attachments 付きで投稿する(text は通知プレビュー・非対応面用の fallback)。
// unfurl_links / unfurl_media は常に false(htmlLink・本文内 URL のプレビュー展開が
// 1 予定ごとに巨大カードとして展開される実害があるため。v2.1 スペック 5/6 章)。
// blocks/attachments 起因のエラー(invalid_blocks / invalid_attachments)はイベントデータ
// (件名・本文)由来でありうるため、fallback text 単体で 1 回だけ縮退再送する(v2.1 スペック 8 章)。
func (c *Client) post(ctx context.Context, text string, blocks []block, attachments []attachment) error {
	ch, err := c.channelID(ctx)
	if err != nil {
		return err
	}
	payload := textOnlyPayload(ch, text)
	if len(blocks) > 0 {
		payload["blocks"] = blocks
	}
	if len(attachments) > 0 {
		payload["attachments"] = attachments
	}
	_, err = c.call(ctx, "chat.postMessage", payload)
	if err != nil && (len(blocks) > 0 || len(attachments) > 0) &&
		(strings.Contains(err.Error(), "invalid_blocks") || strings.Contains(err.Error(), "invalid_attachments")) {
		log.Printf("slack: invalid blocks/attachments; resending as plain text: %v", err)
		_, err = c.call(ctx, "chat.postMessage", textOnlyPayload(ch, text))
	}
	return err
}

// textOnlyPayload は unfurl 抑止込みの text 単体ペイロード(縮退再送にも使う)。
func textOnlyPayload(channel, text string) map[string]any {
	return map[string]any{
		"channel":      channel,
		"text":         text,
		"unfurl_links": false,
		"unfurl_media": false,
	}
}
