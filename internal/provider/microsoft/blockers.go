package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/work-a-co/calsync/internal/model"
	"github.com/work-a-co/calsync/internal/provider"
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
	ShowAs                        string            `json:"showAs"`
	IsReminderOn                  bool              `json:"isReminderOn"`
	Sensitivity                   string            `json:"sensitivity"`
	IsAllDay                      bool              `json:"isAllDay"`
	TransactionID                 string            `json:"transactionId,omitempty"`
	Start                         graphTime         `json:"start"`
	End                           graphTime         `json:"end"`
	SingleValueExtendedProperties []singleValueProp `json:"singleValueExtendedProperties"`
}

// blockerBody builds the Graph event payload. Timed blockers are written in
// UTC; all-day blockers use isAllDay=true with midnight bounds in the target
// calendar's timezone (design doc 6.6: UTC midnight would shift the date).
// idemKey=="" omits transactionId (it is create-only).
func blockerBody(b model.Blocker, idemKey string) graphEventBody {
	body := graphEventBody{
		Subject:       b.Title,
		ShowAs:        "busy",
		IsReminderOn:  false,
		Sensitivity:   "private",
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
		// transactionId の再送(クラッシュ後の再実行)。既存ブロッカーをタグで特定する。
		return c.findBlockerByOriginTag(ctx, b.OriginTag)
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
	q := url.Values{}
	q.Set("$filter", fmt.Sprintf(
		"singleValueExtendedProperties/Any(ep: ep/id eq '%s' and ep/value ne null)",
		originPropertyID))
	listURL := c.baseURL + "/me/events?" + encodeQuery(q)

	var records []model.BlockerRecord
	for listURL != "" {
		status, body, err := c.doRead(ctx, http.MethodGet, listURL, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("graph list blockers: status %d: %s", status, body)
		}
		var page struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("graph list blockers: decode: %w", err)
		}
		for _, item := range page.Value {
			rec, err := c.getBlockerRecord(ctx, item.ID)
			if err != nil {
				return nil, err
			}
			records = append(records, rec)
		}
		listURL = page.NextLink
	}
	return records, nil
}

// getBlockerRecord fetches one event with $expand to read the origin tag
// value and computes its TimeHash.
func (c *Client) getBlockerRecord(ctx context.Context, eventID string) (model.BlockerRecord, error) {
	q := url.Values{}
	q.Set("$expand", fmt.Sprintf("singleValueExtendedProperties($filter=id eq '%s')", originPropertyID))
	status, body, err := c.doRead(ctx, http.MethodGet,
		c.baseURL+"/me/events/"+url.PathEscape(eventID)+"?"+encodeQuery(q), nil)
	if err != nil {
		return model.BlockerRecord{}, err
	}
	if status != http.StatusOK {
		return model.BlockerRecord{}, fmt.Errorf("graph get blocker %s: status %d: %s", eventID, status, body)
	}
	var ev struct {
		ID                            string            `json:"id"`
		IsAllDay                      bool              `json:"isAllDay"`
		Start                         graphTime         `json:"start"`
		End                           graphTime         `json:"end"`
		SingleValueExtendedProperties []singleValueProp `json:"singleValueExtendedProperties"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return model.BlockerRecord{}, fmt.Errorf("graph get blocker %s: decode: %w", eventID, err)
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
			return model.BlockerRecord{}, err
		}
		e, err := datePart(ev.End.DateTime)
		if err != nil {
			return model.BlockerRecord{}, err
		}
		nev.AllDayStart, nev.AllDayEnd = s, e
	} else {
		var s, e time.Time
		if s, err = ev.Start.utc(); err != nil {
			return model.BlockerRecord{}, err
		}
		if e, err = ev.End.utc(); err != nil {
			return model.BlockerRecord{}, err
		}
		nev.StartUTC, nev.EndUTC = s, e
	}
	rec.TimeHash = model.TimeHash(nev)
	return rec, nil
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
