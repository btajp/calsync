package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/notify"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New("xoxb-test", "C123")
	c.BaseURL = srv.URL
	c.TZ = time.UTC
	return c
}

func sampleEntry() engine.DigestEntry {
	return engine.DigestEntry{
		Title:      "e",
		StartUTC:   time.Date(2026, 7, 5, 1, 0, 0, 0, time.UTC),
		EndUTC:     time.Date(2026, 7, 5, 2, 0, 0, 0, time.UTC),
		AccountIDs: []string{"a"},
	}
}

func TestSendReminderPostsMessage(t *testing.T) {
	var got struct {
		Channel     string           `json:"channel"`
		Attachments []map[string]any `json:"attachments"`
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat.postMessage", r.URL.Path)
		require.Equal(t, "Bearer xoxb-test", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Write([]byte(`{"ok":true}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), 10*time.Minute))
	require.Equal(t, "C123", got.Channel)
	// リマインドは blocks を持たないため、通知用テキストは attachment の fallback に入る
	// (トップレベル text には乗せない。v2.1 スペック 6/8 章。二重表示防止)。
	require.Len(t, got.Attachments, 1)
	require.Contains(t, got.Attachments[0]["fallback"], "10分後")
}

// chat.postMessage の実 API 成功応答は "channel" が文字列で返る
// (conversations.open はオブジェクト{"id":...})。共有デコードが
// json.UnmarshalTypeError で失敗し送信成功なのにリトライ扱いになる回帰を防ぐ。
func TestPostMessageChannelAsStringDecodesOK(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat.postMessage", r.URL.Path)
		w.Write([]byte(`{"ok":true,"channel":"C123"}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), time.Minute))
}

// ok:false は未知のエラー文字列も含め既定でリトライ不能(スペック 8 章)。
func TestAPIErrorsAreNonRetryable(t *testing.T) {
	for _, apiErr := range []string{"invalid_auth", "channel_not_found", "some_future_error"} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"ok":false,"error":"` + apiErr + `"}`))
		})
		err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
		require.Error(t, err)
		require.True(t, errors.Is(err, notify.ErrNonRetryable), "error %q should be non-retryable", apiErr)
	}
}

// 200 だが JSON として不正な応答(プロキシ等が返す壊れたボディ)は、再試行しても
// 回復しないためリトライ不能に分類する(スペック 8 章)。
func TestMalformed200BodyIsNonRetryable(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not-json"))
	})
	err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
	require.Error(t, err)
	require.True(t, errors.Is(err, notify.ErrNonRetryable))
}

// 429 / 5xx / ネットワークエラーはリトライ可能(sentinel を含まない)。
func TestTransportErrorsAreRetryable(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
		require.Error(t, err)
		require.False(t, errors.Is(err, notify.ErrNonRetryable), "status %d should be retryable", status)
	}

	c := New("xoxb-test", "C123")
	c.BaseURL = "http://127.0.0.1:1" // 到達不能
	err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
	require.Error(t, err)
	require.False(t, errors.Is(err, notify.ErrNonRetryable))
}

// 素の 4xx(429 を除く)は ok:false と同様にリトライ不能に分類する(スペック 8 章)。
func TestPlainHTTPErrorIsNonRetryable(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
	require.Error(t, err)
	require.True(t, errors.Is(err, notify.ErrNonRetryable))
}

// attachments がペイロードに含まれ、トップレベル text キーは無く、fallback は
// attachment 側に入る(リマインドは単一 attachment・トップレベル blocks は使わない。
// blocks の無いメッセージで text を乗せると Slack が二重描画するため。v2.1 スペック 6/8 章)。
func TestPostMessageIncludesAttachments(t *testing.T) {
	var got map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Write([]byte(`{"ok":true}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), 10*time.Minute))
	_, hasText := got["text"]
	require.False(t, hasText, "reminder payload must not carry top-level text (would duplicate the attachment)")
	_, hasBlocks := got["blocks"]
	require.False(t, hasBlocks)
	attachments, _ := got["attachments"].([]any)
	require.Len(t, attachments, 1)
	att, _ := attachments[0].(map[string]any)
	require.NotEmpty(t, att["color"])
	require.Contains(t, att["fallback"], "10分後") // fallback(v1 テキスト)
	blocks, _ := att["blocks"].([]any)
	require.NotEmpty(t, blocks)
}

// ダイジェストはトップレベル blocks(header)+ 予定ごとの attachments を両方使い、
// トップレベル text も従来どおり(v1 テキスト)が乗る(v2.1 スペック 5/8 章)。
func TestSendDigestIncludesHeaderAndAttachments(t *testing.T) {
	var got struct {
		Text        string           `json:"text"`
		Blocks      []map[string]any `json:"blocks"`
		Attachments []map[string]any `json:"attachments"`
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Write([]byte(`{"ok":true}`))
	})
	entries := []engine.DigestEntry{sampleEntry()}
	require.NoError(t, c.SendDigest(context.Background(), time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), entries, nil))
	require.NotEmpty(t, got.Text) // blocks があるので従来どおりトップレベル text が乗る
	require.Equal(t, "header", got.Blocks[0]["type"])
	require.Len(t, got.Attachments, 1)
}

// unfurl 抑止(v2.1 スペック 5/6 章): htmlLink・本文内 URL のプレビュー展開を防ぐため
// chat.postMessage には常に unfurl_links / unfurl_media を false で付ける。
func TestPostMessageSuppressesUnfurl(t *testing.T) {
	var got map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Write([]byte(`{"ok":true}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), 10*time.Minute))
	require.Equal(t, false, got["unfurl_links"])
	require.Equal(t, false, got["unfurl_media"])
}

// invalid_blocks / invalid_attachments のときだけ fallback text 単体で 1 回縮退再送する
// (v2.1 スペック 8 章)。再送にも unfurl 抑止を付ける。
func TestInvalidBlocksOrAttachmentsFallsBackToTextOnce(t *testing.T) {
	for _, apiErr := range []string{"invalid_blocks", "invalid_attachments"} {
		t.Run(apiErr, func(t *testing.T) {
			var calls []map[string]any
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				calls = append(calls, body)
				if len(calls) == 1 {
					w.Write([]byte(`{"ok":false,"error":"` + apiErr + `"}`))
					return
				}
				w.Write([]byte(`{"ok":true}`))
			})
			require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), time.Minute))
			require.Len(t, calls, 2)
			_, hasBlocks := calls[1]["blocks"]
			_, hasAttachments := calls[1]["attachments"]
			require.False(t, hasBlocks)      // 再送は text のみ
			require.False(t, hasAttachments) // 再送は text のみ
			// リマインドはトップレベル blocks が無いため通常送信では text を乗せないが、
			// 縮退再送は post が受け取った text 引数をそのまま使うため復活する。
			require.Contains(t, calls[1]["text"], "1分後")
			require.Equal(t, false, calls[1]["unfurl_links"])
			require.Equal(t, false, calls[1]["unfurl_media"])
		})
	}
}

// 縮退再送も失敗したら通常分類(ここでは non-retryable)で返る(invalid_blocks/invalid_attachments 共通経路)。
func TestInvalidBlocksFallbackFailurePropagates(t *testing.T) {
	n := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Write([]byte(`{"ok":false,"error":"invalid_blocks"}`))
			return
		}
		w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	})
	err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
	require.Error(t, err)
	require.True(t, errors.Is(err, notify.ErrNonRetryable))
	require.Equal(t, 2, n) // 縮退は 1 回だけ
}

// invalid_blocks 以外のエラーでは縮退しない。
func TestNonBlocksErrorsDoNotFallBack(t *testing.T) {
	n := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	})
	err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
	require.Error(t, err)
	require.Equal(t, 1, n)
}

// colorFor は Accounts の定義順でパレット(8 色)を巡回し、未知は #999999(v2.1 スペック 5 章)。
func TestColorForCyclesPalette(t *testing.T) {
	accounts := make([]string, 9) // パレットは 8 色なので 9 個目で先頭にラップする
	for i := range accounts {
		accounts[i] = fmt.Sprintf("acct-%d", i)
	}
	c := &Client{Accounts: accounts}
	require.Equal(t, c.colorFor(accounts[0]), c.colorFor(accounts[8]))
	require.NotEqual(t, c.colorFor(accounts[0]), c.colorFor(accounts[1]))
	require.Equal(t, "#999999", c.colorFor("nope"))
}

// U… は conversations.open で DM に解決し、プロセス存続中はキャッシュする(スペック 8 章)。
func TestDMResolutionIsCached(t *testing.T) {
	var opens atomic.Int64
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.open":
			opens.Add(1)
			w.Write([]byte(`{"ok":true,"channel":{"id":"D999"}}`))
		case "/chat.postMessage":
			var body struct {
				Channel string `json:"channel"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "D999", body.Channel)
			w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	c.Channel = "U777"
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), time.Minute))
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), time.Minute))
	require.Equal(t, int64(1), opens.Load())
}
