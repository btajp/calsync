package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
)

// maxDeltaPages is the per-round page cap of the pagination circuit breaker
// (design doc 5.2: known Graph infinite-pagination issue with recurring events).
const maxDeltaPages = 50

type deltaRemoved struct {
	Reason string `json:"reason"`
}

type deltaResponseStatus struct {
	Response string `json:"response"`
}

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphLocation struct {
	DisplayName string `json:"displayName"`
}

type graphOnlineMeeting struct {
	JoinURL string `json:"joinUrl"`
}

type deltaEvent struct {
	ID               string               `json:"id"`
	Removed          *deltaRemoved        `json:"@removed"`
	ICalUID          string               `json:"iCalUId"`
	Subject          string               `json:"subject"`
	IsCancelled      bool                 `json:"isCancelled"`
	IsAllDay         bool                 `json:"isAllDay"`
	ShowAs           string               `json:"showAs"`
	Start            *graphTime           `json:"start"`
	End              *graphTime           `json:"end"`
	ResponseStatus   *deltaResponseStatus `json:"responseStatus"`
	Body             *graphBody           `json:"body"`
	Location         *graphLocation       `json:"location"`
	OnlineMeeting    *graphOnlineMeeting  `json:"onlineMeeting"`
	OnlineMeetingURL string               `json:"onlineMeetingUrl"`
	WebLink          string               `json:"webLink"`
}

type deltaPage struct {
	Value     []deltaEvent `json:"value"`
	NextLink  string       `json:"@odata.nextLink"`
	DeltaLink string       `json:"@odata.deltaLink"`
}

// Changes implements provider.Provider. An empty cursor starts a windowed
// full sync via /me/calendarView/delta; a non-empty cursor (the previous
// deltaLink URL) is fetched as-is. newCursor is only non-empty when the
// round completed (deltaLink reached).
func (c *Client) Changes(ctx context.Context, cal model.CalendarRef, cursor string, window model.Window) ([]model.NormalizedEvent, string, error) {
	reqURL := cursor
	if reqURL == "" {
		q := url.Values{}
		q.Set("startDateTime", window.Start.UTC().Format(time.RFC3339))
		q.Set("endDateTime", window.End.UTC().Format(time.RFC3339))
		reqURL = c.baseURL + "/me/calendarView/delta?" + q.Encode()
	}

	var events []model.NormalizedEvent
	prevFingerprint := ""
	for page := 0; ; page++ {
		if page >= maxDeltaPages {
			return nil, "", fmt.Errorf("graph delta: pagination exceeded %d pages: %w", maxDeltaPages, provider.ErrCursorInvalid)
		}
		status, body, err := c.doRead(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, "", err
		}
		if status == http.StatusGone {
			return nil, "", provider.ErrCursorInvalid
		}
		if status != http.StatusOK {
			if graphErrorCode(body) == "syncStateNotFound" {
				return nil, "", provider.ErrCursorInvalid
			}
			return nil, "", fmt.Errorf("graph delta: status %d: %s", status, body)
		}
		var pg deltaPage
		if err := json.Unmarshal(body, &pg); err != nil {
			return nil, "", fmt.Errorf("graph delta: decode page: %w", err)
		}
		fp := pageFingerprint(pg.Value)
		if fp != "" && fp == prevFingerprint {
			return nil, "", fmt.Errorf("graph delta: identical page repeated (pagination loop): %w", provider.ErrCursorInvalid)
		}
		prevFingerprint = fp
		for _, de := range pg.Value {
			ev, err := normalizeDeltaEvent(de, c.busyShowAs)
			if err != nil {
				return nil, "", err
			}
			events = append(events, ev)
		}
		if pg.DeltaLink != "" {
			return events, pg.DeltaLink, nil
		}
		if pg.NextLink == "" {
			return nil, "", fmt.Errorf("graph delta: page has neither nextLink nor deltaLink")
		}
		reqURL = pg.NextLink
	}
}

// pageFingerprint joins the event IDs of one page for loop detection.
func pageFingerprint(evs []deltaEvent) string {
	ids := make([]string, len(evs))
	for i, e := range evs {
		ids[i] = e.ID
	}
	return strings.Join(ids, "\x00")
}

// normalizeDeltaEvent converts a Graph delta event into the provider-neutral
// form. @removed elements and isCancelled=true both map to Deleted=true
// (only the ID is guaranteed). OriginTag stays "" — Graph delta cannot
// return extended properties (design doc 6.3).
func normalizeDeltaEvent(de deltaEvent, busyShowAs map[string]bool) (model.NormalizedEvent, error) {
	ev := model.NormalizedEvent{ID: de.ID, ICalUID: de.ICalUID}
	if de.Removed != nil || de.IsCancelled {
		ev.Deleted = true
		return ev, nil
	}
	// NOTE: delta 応答に subject が含まれることはユニット(フィクスチャ)で検証済み。
	// 実 API の応答は初回稼働時に要実測(スペック 13 章スパイク 1)
	ev.Title = de.Subject
	ev.HTMLLink = de.WebLink
	if de.Body != nil {
		// Prefer: outlook.body-content-type="text" によりプレーンテキスト(実測 2026-07-06)
		ev.Description = de.Body.Content
	}
	loc := ""
	if de.Location != nil {
		loc = de.Location.DisplayName
	}
	switch {
	case de.OnlineMeeting != nil && de.OnlineMeeting.JoinURL != "":
		ev.MeetingURL = de.OnlineMeeting.JoinURL
	case de.OnlineMeetingURL != "":
		ev.MeetingURL = de.OnlineMeetingURL
	default:
		ev.MeetingURL = model.ExtractMeetingURL(loc, ev.Description)
	}
	ev.IsBusy = busyShowAs[de.ShowAs]
	if de.ResponseStatus != nil && de.ResponseStatus.Response == "declined" {
		ev.IsDeclined = true
	}
	if de.Start == nil || de.End == nil {
		return model.NormalizedEvent{}, fmt.Errorf("graph delta: event %q has no start/end", de.ID)
	}
	if de.IsAllDay {
		ev.IsAllDay = true
		// NOTE: 終日イベントの start/end が実 API で UTC 変換され日付がズレないこと要実測(spec 15章)
		s, err := datePart(de.Start.DateTime)
		if err != nil {
			return model.NormalizedEvent{}, err
		}
		e, err := datePart(de.End.DateTime)
		if err != nil {
			return model.NormalizedEvent{}, err
		}
		ev.AllDayStart, ev.AllDayEnd = s, e
		return ev, nil
	}
	start, err := de.Start.utc()
	if err != nil {
		return model.NormalizedEvent{}, err
	}
	end, err := de.End.utc()
	if err != nil {
		return model.NormalizedEvent{}, err
	}
	ev.StartUTC, ev.EndUTC = start, end
	return ev, nil
}
