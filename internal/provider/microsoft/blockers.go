package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
)

// Client satisfies provider.Provider once this file's methods exist.
var _ provider.Provider = (*Client)(nil)

type singleValueProp struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// graphEventBody is the POST/PATCH payload for blocker events.
type graphEventBody struct {
	Subject                       string            `json:"subject"`
	Body                          *graphItemBody    `json:"body,omitempty"`
	ShowAs                        string            `json:"showAs"`
	IsReminderOn                  bool              `json:"isReminderOn"`
	Sensitivity                   string            `json:"sensitivity"`
	IsAllDay                      bool              `json:"isAllDay"`
	TransactionID                 string            `json:"transactionId,omitempty"`
	Start                         graphTime         `json:"start"`
	End                           graphTime         `json:"end"`
	SingleValueExtendedProperties []singleValueProp `json:"singleValueExtendedProperties"`
}

// graphItemBody はイベント本文(説明欄)。空文字の content 送信でクリアできる。
type graphItemBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// graphSensitivity は Blocker.Visibility を Graph の sensitivity へ写像する
// (allowlist — スペック 2026-07-15 §12.2)。
func graphSensitivity(b model.Blocker) string {
	switch b.Visibility {
	case "default", "public":
		// Graph に「公開」の段階は無いため両方 normal = 普通の予定(スペック 2026-07-15 §12.2)
		return "normal"
	default:
		// 空文字(ペアなし・既定)と検証外の値は private(安全側)
		return "private"
	}
}

// blockerBody builds the Graph event payload. Timed blockers are written in
// UTC; all-day blockers use isAllDay=true with midnight bounds in the target
// calendar's timezone (design doc 6.6: UTC midnight would shift the date).
// idemKey=="" omits transactionId (it is create-only).
func blockerBody(b model.Blocker, idemKey string) graphEventBody {
	body := graphEventBody{
		Body:          &graphItemBody{ContentType: "text", Content: b.Description},
		Subject:       b.Title,
		ShowAs:        "busy",
		IsReminderOn:  false,
		Sensitivity:   graphSensitivity(b),
		TransactionID: idemKey,
		SingleValueExtendedProperties: []singleValueProp{
			{ID: originPropertyID, Value: b.OriginTag},
		},
	}
	if b.IsAllDay {
		body.IsAllDay = true
		body.Start = graphTime{DateTime: b.AllDayStart + "T00:00:00", TimeZone: b.TargetTimezone}
		body.End = graphTime{DateTime: b.AllDayEnd + "T00:00:00", TimeZone: b.TargetTimezone}
	} else {
		const layout = "2006-01-02T15:04:05"
		body.Start = graphTime{DateTime: b.StartUTC.UTC().Format(layout), TimeZone: "UTC"}
		body.End = graphTime{DateTime: b.EndUTC.UTC().Format(layout), TimeZone: "UTC"}
	}
	return body
}

// odataQuote escapes single quotes for OData string literals.
func odataQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }

// encodeQuery encodes q and rewrites the space encoding from "+" to "%20":
// Microsoft Graph's OData parser rejects "+" as a space substitute inside
// $filter/$expand (known Graph behavior), even though it is otherwise a
// valid application/x-www-form-urlencoded convention that Go's url.Values.Encode
// produces. This is safe for literal "+" bytes in values too, since
// url.Values.Encode already percent-escapes them as "%2B" before this
// function ever sees the string, so the blanket "+"->"%20" replace only
// ever touches encoded spaces.
func encodeQuery(q url.Values) string { return strings.ReplaceAll(q.Encode(), "+", "%20") }

// CreateBlocker implements provider.Provider. idemKey becomes the Graph
// transactionId; a 409 (duplicate transactionId) is resolved by looking up
// the existing event via its origin tag and returning its ID.
func (c *Client) CreateBlocker(ctx context.Context, cal model.CalendarRef, b model.Blocker, idemKey string) (string, error) {
	payload, err := json.Marshal(blockerBody(b, idemKey))
	if err != nil {
		return "", err
	}
	status, body, err := c.doRead(ctx, http.MethodPost, c.baseURL+"/me/events", payload)
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusCreated || status == http.StatusOK:
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &created); err != nil {
			return "", fmt.Errorf("graph create blocker: decode: %w", err)
		}
		if created.ID == "" {
			return "", fmt.Errorf("graph create blocker: response has no id")
		}
		return created.ID, nil
	case status == http.StatusConflict:
		// transactionId の再送(クラッシュ後の再実行)。既存ブロッカーをタグで特定し、
		// 停止中に origin の内容が変わっている可能性に備えて、作成しようとしていた
		// 内容で PATCH してから返す(スペック 2026-07-15 §5。Google の 409 収容と対称)
		id, err := c.findBlockerByOriginTag(ctx, b.OriginTag)
		if err != nil {
			return "", err
		}
		if err := c.UpdateBlocker(ctx, cal, id, b); err != nil {
			return "", fmt.Errorf("graph create blocker: align existing %s: %w", id, err)
		}
		return id, nil
	default:
		return "", fmt.Errorf("graph create blocker: status %d: %s", status, body)
	}
}

// findBlockerByOriginTag locates an existing blocker whose calsyncOrigin
// extended property equals originTag.
func (c *Client) findBlockerByOriginTag(ctx context.Context, originTag string) (string, error) {
	q := url.Values{}
	q.Set("$filter", fmt.Sprintf(
		"singleValueExtendedProperties/Any(ep: ep/id eq '%s' and ep/value eq '%s')",
		originPropertyID, odataQuote(originTag)))
	q.Set("$select", "id")
	status, body, err := c.doRead(ctx, http.MethodGet, c.baseURL+"/me/events?"+encodeQuery(q), nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("graph find blocker by tag: status %d: %s", status, body)
	}
	var page struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", fmt.Errorf("graph find blocker by tag: decode: %w", err)
	}
	if len(page.Value) == 0 {
		return "", fmt.Errorf("graph create blocker: 409 conflict but no event with origin tag %q", originTag)
	}
	return page.Value[0].ID, nil
}

// UpdateBlocker implements provider.Provider (PATCH; no transactionId).
// A 404 (blocker deleted by hand etc.) maps to provider.ErrNotFound so the
// engine can fall back to re-creating it (design doc 8.4).
func (c *Client) UpdateBlocker(ctx context.Context, cal model.CalendarRef, eventID string, b model.Blocker) error {
	payload, err := json.Marshal(blockerBody(b, ""))
	if err != nil {
		return err
	}
	status, body, err := c.doRead(ctx, http.MethodPatch, c.baseURL+"/me/events/"+url.PathEscape(eventID), payload)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("graph update blocker %s: status 404: %w", eventID, provider.ErrNotFound)
	}
	if status != http.StatusOK {
		return fmt.Errorf("graph update blocker %s: status %d: %s", eventID, status, body)
	}
	return nil
}

// DeleteBlocker implements provider.Provider. 404 is treated as success
// (the blocker is already gone — deletion is idempotent).
func (c *Client) DeleteBlocker(ctx context.Context, cal model.CalendarRef, eventID string) error {
	status, body, err := c.doRead(ctx, http.MethodDelete, c.baseURL+"/me/events/"+url.PathEscape(eventID), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("graph delete blocker %s: status %d: %s", eventID, status, body)
}

// ListBlockers implements provider.Provider. Graph cannot return extended
// property values in a filtered listing (official limitation), so it lists
// matching event IDs first, then fetches each event with $expand to read the
// origin tag and times for the BlockerRecord.
func (c *Client) ListBlockers(ctx context.Context, cal model.CalendarRef, window model.Window) ([]model.BlockerRecord, error) {
	// 過去ブロッカー保持(2026-08-05 仕様変更)に伴い、Google の TimeMin と同じ
	// 「終了がウィンドウ開始以降」の時刻下限を $filter に付ける。これが無いと、
	// 掃除されず蓄積し続ける過去ブロッカーを毎リコンサイルで全列挙+1 件ずつ GET する
	// ことになり、API コストが際限なく線形成長する(レビュー指摘)。
	// 拡張プロパティとの組合せ $filter は実 API 未実測(仕様書 15 章スパイク)のため、
	// 400 で拒否された場合は時刻条件なしで 1 回だけ再試行して従来挙動に戻す。
	base := fmt.Sprintf(
		"singleValueExtendedProperties/Any(ep: ep/id eq '%s' and ep/value ne null)",
		originPropertyID)
	// gt(排他)は Google の TimeMin(「終了の排他的下限」)・engine.EndsBeforeWindow・
	// fake と境界を揃えるため
	bounded := base + fmt.Sprintf(" and end/dateTime gt '%s'",
		window.Start.UTC().Format("2006-01-02T15:04:05"))
	records, status, err := c.listBlockersFiltered(ctx, bounded, window.Start)
	if err != nil && status == http.StatusBadRequest {
		// フォールバック時もクライアント側の時刻下限(listBlockersFiltered の minEnd)は
		// 効いたままなので、「過去ブロッカーを列挙しない」契約は保たれる(検証指摘:
		// これが無いと DB 全損時にフェーズ0が過去分を収容 → 汚染掃除が全削除する
		// 元欠陥がフォールバック経路で再発する)。増えるのは列挙 API コストのみ
		log.Printf("graph list blockers: time-bounded filter rejected, falling back to unbounded listing: %v", err)
		records, _, err = c.listBlockersFiltered(ctx, base, window.Start)
	}
	return records, err
}

// listBlockersFiltered は指定 $filter でブロッカーを列挙する。minEndExclusive は
// クライアント側の時刻下限(終了がこれ以前のブロッカーを除外。サーバー側 $filter の
// 有無に関わらず常に適用する二重防御)。エラー時は呼び出し元がフォールバック判断に
// 使えるよう HTTP ステータスも返す(HTTP 層以外のエラーは status 0)。
func (c *Client) listBlockersFiltered(ctx context.Context, filter string, minEndExclusive time.Time) ([]model.BlockerRecord, int, error) {
	q := url.Values{}
	q.Set("$filter", filter)
	listURL := c.baseURL + "/me/events?" + encodeQuery(q)

	var records []model.BlockerRecord
	for listURL != "" {
		status, body, err := c.doRead(ctx, http.MethodGet, listURL, nil)
		if err != nil {
			return nil, 0, err
		}
		if status != http.StatusOK {
			return nil, status, fmt.Errorf("graph list blockers: status %d: %s", status, body)
		}
		var page struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, 0, fmt.Errorf("graph list blockers: decode: %w", err)
		}
		for _, item := range page.Value {
			rec, nev, err := c.getBlockerRecord(ctx, item.ID)
			if err != nil {
				return nil, 0, err
			}
			if blockerEndsBefore(nev, minEndExclusive) {
				continue
			}
			records = append(records, rec)
		}
		listURL = page.NextLink
	}
	return records, http.StatusOK, nil
}

// blockerEndsBefore は engine.EndsBeforeWindow と同じ近似(終日は AllDayEnd の
// UTC 日付・パース不能や時刻ゼロは false = 除外しない)での過去判定。
func blockerEndsBefore(nev model.NormalizedEvent, minEndExclusive time.Time) bool {
	if nev.IsAllDay {
		end, err := time.Parse("2006-01-02", nev.AllDayEnd)
		if err != nil {
			return false
		}
		return !end.After(minEndExclusive)
	}
	if nev.EndUTC.IsZero() {
		return false
	}
	return !nev.EndUTC.After(minEndExclusive)
}

// getBlockerRecord fetches one event with $expand to read the origin tag
// value and computes its TimeHash. 時刻情報(nev)も返し、呼び出し元の
// クライアント側時刻下限フィルタに使う。
func (c *Client) getBlockerRecord(ctx context.Context, eventID string) (model.BlockerRecord, model.NormalizedEvent, error) {
	q := url.Values{}
	q.Set("$expand", fmt.Sprintf("singleValueExtendedProperties($filter=id eq '%s')", originPropertyID))
	status, body, err := c.doRead(ctx, http.MethodGet,
		c.baseURL+"/me/events/"+url.PathEscape(eventID)+"?"+encodeQuery(q), nil)
	if err != nil {
		return model.BlockerRecord{}, model.NormalizedEvent{}, err
	}
	if status != http.StatusOK {
		return model.BlockerRecord{}, model.NormalizedEvent{}, fmt.Errorf("graph get blocker %s: status %d: %s", eventID, status, body)
	}
	var ev struct {
		ID                            string            `json:"id"`
		IsAllDay                      bool              `json:"isAllDay"`
		Start                         graphTime         `json:"start"`
		End                           graphTime         `json:"end"`
		SingleValueExtendedProperties []singleValueProp `json:"singleValueExtendedProperties"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return model.BlockerRecord{}, model.NormalizedEvent{}, fmt.Errorf("graph get blocker %s: decode: %w", eventID, err)
	}
	rec := model.BlockerRecord{EventID: ev.ID}
	for _, p := range ev.SingleValueExtendedProperties {
		if p.ID == originPropertyID {
			rec.OriginTag = p.Value
		}
	}
	nev := model.NormalizedEvent{IsAllDay: ev.IsAllDay}
	if ev.IsAllDay {
		s, err := datePart(ev.Start.DateTime)
		if err != nil {
			return model.BlockerRecord{}, model.NormalizedEvent{}, err
		}
		e, err := datePart(ev.End.DateTime)
		if err != nil {
			return model.BlockerRecord{}, model.NormalizedEvent{}, err
		}
		nev.AllDayStart, nev.AllDayEnd = s, e
	} else {
		var s, e time.Time
		if s, err = ev.Start.utc(); err != nil {
			return model.BlockerRecord{}, model.NormalizedEvent{}, err
		}
		if e, err = ev.End.utc(); err != nil {
			return model.BlockerRecord{}, model.NormalizedEvent{}, err
		}
		nev.StartUTC, nev.EndUTC = s, e
	}
	rec.TimeHash = model.TimeHash(nev)
	return rec, nev, nil
}

// GetCalendarTimezone implements provider.Provider. It returns the
// mailboxSettings timeZone value verbatim (typically a Windows timezone
// name); it is passed back to Graph as-is when creating all-day blockers.
func (c *Client) GetCalendarTimezone(ctx context.Context, cal model.CalendarRef) (string, error) {
	status, body, err := c.doRead(ctx, http.MethodGet, c.baseURL+"/me/mailboxSettings/timeZone", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("graph get mailbox timezone: status %d: %s", status, body)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("graph get mailbox timezone: decode: %w", err)
	}
	if out.Value == "" {
		return "", fmt.Errorf("graph get mailbox timezone: empty value")
	}
	return out.Value, nil
}
