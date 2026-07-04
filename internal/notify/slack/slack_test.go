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
