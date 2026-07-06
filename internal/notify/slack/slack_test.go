package slack

import (
	"context"
	"encoding/json"
	"errors"
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
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat.postMessage", r.URL.Path)
		require.Equal(t, "Bearer xoxb-test", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Write([]byte(`{"ok":true}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), 10*time.Minute))
	require.Equal(t, "C123", got.Channel)
	require.Contains(t, got.Text, "10分後")
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

// blocks がペイロードに含まれ、text は fallback として残る。
func TestPostMessageIncludesBlocks(t *testing.T) {
	var got struct {
		Text   string           `json:"text"`
		Blocks []map[string]any `json:"blocks"`
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Write([]byte(`{"ok":true}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), 10*time.Minute))
	require.NotEmpty(t, got.Text) // fallback(v1 テキスト)
	require.NotEmpty(t, got.Blocks)
	require.Equal(t, "section", got.Blocks[0]["type"])
}

// invalid_blocks のときだけ fallback text 単体で 1 回縮退再送する(v2 スペック 8 章)。
func TestInvalidBlocksFallsBackToTextOnce(t *testing.T) {
	var calls []map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		calls = append(calls, body)
		if _, hasBlocks := body["blocks"]; hasBlocks {
			w.Write([]byte(`{"ok":false,"error":"invalid_blocks"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), time.Minute))
	require.Len(t, calls, 2)
	_, hasBlocks := calls[1]["blocks"]
	require.False(t, hasBlocks) // 再送は text のみ
}

// 縮退再送も失敗したら通常分類(ここでは non-retryable)で返る。
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
