// Package microsoft implements the calsync Provider for Microsoft Graph
// (v1.0, delegated permissions, primary calendar only in v1).
// Graph is called directly over net/http; no SDK or MSAL is used.
package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/oauth2"

	"github.com/btajp/calsync/internal/provider"
)

const defaultBaseURL = "https://graph.microsoft.com/v1.0"

// originPropertyID is the fixed single-value extended property id that
// carries the calsync origin tag. The GUID is a source-embedded constant
// (design doc 6.3): making it configurable would orphan old tags.
const originPropertyID = "String {b7dbd76c-3a35-4b41-9d80-6a3f31f2a6b9} Name calsyncOrigin"

// Client is the Microsoft Graph provider implementation.
type Client struct {
	hc         *http.Client
	accountID  string
	busyShowAs map[string]bool // showAs values treated as busy (config busy_show_as)
	baseURL    string          // replaced with an httptest URL in tests
	sleep      func(time.Duration)
}

// New builds a Graph client. busyShowAs is the list of showAs values that
// count as busy (default: busy, oof, tentative).
func New(ts oauth2.TokenSource, accountID string, busyShowAs []string) *Client {
	m := make(map[string]bool, len(busyShowAs))
	for _, s := range busyShowAs {
		m[s] = true
	}
	return &Client{
		hc:         oauth2.NewClient(context.Background(), ts),
		accountID:  accountID,
		busyShowAs: m,
		baseURL:    defaultBaseURL,
		sleep:      time.Sleep,
	}
}

// do sends one Graph request with the mandatory Prefer: IdType="ImmutableId",
// outlook.body-content-type="text" header (never odata.maxpagesize) and the
// retry policy:
//   - 429: wait Retry-After seconds, retry (max 3 retries)
//   - 5xx: exponential backoff, retry (max 3 retries)
//
// Any other status is returned to the caller as-is.
func (c *Client) do(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	const maxRetries = 3
	backoff := 500 * time.Millisecond
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return nil, err
		}
		// 全 Graph リクエスト共通: ImmutableId(v1 スペック)+ body の text 化(v2 スペック 3.4)。
		// delta 以外のエンドポイントは応答から id しか読まないため body-content-type は無害
		req.Header.Set("Prefer", `IdType="ImmutableId", outlook.body-content-type="text"`)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries:
			wait := backoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, perr := strconv.Atoi(ra); perr == nil && secs >= 0 {
					wait = time.Duration(secs) * time.Second
				}
			}
			resp.Body.Close()
			c.sleep(wait)
		case resp.StatusCode >= 500 && attempt < maxRetries:
			resp.Body.Close()
			c.sleep(backoff)
			backoff *= 2
		default:
			return resp, nil
		}
	}
}

// doRead runs do and drains/closes the body.
//
// It is also the common error path for authentication-expiry normalization
// (coordinator addendum, Task 14 review): a TokenSource refresh failure
// (invalid_grant / interaction_required) surfaces as an error from
// c.hc.Do inside do and is passed through provider.NormalizeAuthErr; an
// HTTP 401 response from Graph itself is wrapped as provider.ErrAuthExpired
// explicitly, since Graph does not reliably return a machine-readable
// error.code for token expiry. Centralizing this here means every caller
// built on doRead (delta.go today, blocker endpoints later) gets the same
// normalization without repeating the check.
func (c *Client) doRead(ctx context.Context, method, url string, body []byte) (int, []byte, error) {
	resp, err := c.do(ctx, method, url, body)
	if err != nil {
		return 0, nil, provider.NormalizeAuthErr(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, data, fmt.Errorf("graph: status 401: %w", provider.ErrAuthExpired)
	}
	return resp.StatusCode, data, nil
}

// graphErrorCode extracts error.code from a Graph error body ("" if absent).
func graphErrorCode(body []byte) string {
	var ge struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &ge); err != nil {
		return ""
	}
	return ge.Error.Code
}

// graphTime is Graph's dateTimeTimeZone resource.
type graphTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// utc parses a Graph dateTime (optionally with fractional seconds) in its
// declared timeZone and converts it to UTC. calsync never sends
// Prefer: outlook.timezone, so Graph returns UTC (design doc 6.6); IANA
// names are handled as a fallback via time.LoadLocation.
func (t graphTime) utc() (time.Time, error) {
	const layout = "2006-01-02T15:04:05.9999999"
	loc := time.UTC
	if t.TimeZone != "" && t.TimeZone != "UTC" {
		l, err := time.LoadLocation(t.TimeZone)
		if err != nil {
			return time.Time{}, fmt.Errorf("graph time: unknown timeZone %q: %w", t.TimeZone, err)
		}
		loc = l
	}
	parsed, err := time.ParseInLocation(layout, t.DateTime, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("graph time: parse %q: %w", t.DateTime, err)
	}
	return parsed.UTC(), nil
}

// datePart returns the YYYY-MM-DD prefix of a Graph dateTime string.
func datePart(dt string) (string, error) {
	if len(dt) < 10 {
		return "", fmt.Errorf("graph time: malformed dateTime %q", dt)
	}
	return dt[:10], nil
}
