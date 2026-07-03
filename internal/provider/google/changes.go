package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"

	"github.com/work-a-co/calsync/internal/model"
	"github.com/work-a-co/calsync/internal/provider"
)

// Changes は events.list による差分取得を実装する(仕様書5.1)。
//   - cursor=="" のフル同期: timeMin/timeMax/singleEvents=true
//   - 増分: syncToken + singleEvents=true のみ(timeMin 等との併用は 400)
//   - nextSyncToken は最終ページにのみ返るため、全ページ完走時だけ newCursor を返す。
//     途中で失敗した場合は newCursor="" のままエラーを返す(呼び出し側は旧カーソルで再実行)
//   - 410 GONE は provider.ErrCursorInvalid に写像する
//   - showDeleted は指定しない(増分での明示指定は 400。仕様書5.1-4)
func (c *Client) Changes(ctx context.Context, cal model.CalendarRef, cursor string, window model.Window) ([]model.NormalizedEvent, string, error) {
	svc, err := c.service(ctx)
	if err != nil {
		return nil, "", err
	}
	var (
		events    []model.NormalizedEvent
		pageToken string
	)
	for {
		call := svc.Events.List(cal.CalendarID).Context(ctx).SingleEvents(true)
		if cursor != "" {
			call.SyncToken(cursor)
		} else {
			call.TimeMin(window.Start.UTC().Format(time.RFC3339))
			call.TimeMax(window.End.UTC().Format(time.RFC3339))
		}
		if pageToken != "" {
			call.PageToken(pageToken)
		}
		var resp *calendar.Events
		err := c.doWithRetry(ctx, func() error {
			var e error
			resp, e = call.Do()
			return e
		})
		if err != nil {
			var gerr *googleapi.Error
			if errors.As(err, &gerr) && gerr.Code == http.StatusGone {
				return nil, "", provider.ErrCursorInvalid
			}
			return nil, "", fmt.Errorf("google[%s]: events.list %s: %w", c.accountID, cal, err)
		}
		for _, item := range resp.Items {
			events = append(events, normalizeEvent(item))
		}
		if resp.NextPageToken == "" {
			return events, resp.NextSyncToken, nil
		}
		pageToken = resp.NextPageToken
	}
}

// normalizeEvent は Google のイベントを NormalizedEvent に正規化する。
//   - status=cancelled: 削除通知。id 以外は保証されないため ID のみ設定(仕様書6.1)
//   - transparency: 省略時 opaque = busy(仕様書6.2)
//   - 辞退: attendees の self==true エントリの responseStatus==declined(仕様書6.2)
//   - タグ: extendedProperties.private の calsync-origin(仕様書6.3)
//   - 終日: date を現地日付のまま保持(end は排他的)。時刻指定は UTC に正規化(仕様書6.6)
func normalizeEvent(item *calendar.Event) model.NormalizedEvent {
	ev := model.NormalizedEvent{ID: item.Id}
	if item.Status == "cancelled" {
		ev.Deleted = true
		return ev
	}
	ev.ICalUID = item.ICalUID
	ev.IsBusy = item.Transparency != "transparent"
	for _, a := range item.Attendees {
		if a != nil && a.Self && a.ResponseStatus == "declined" {
			ev.IsDeclined = true
		}
	}
	if item.ExtendedProperties != nil && item.ExtendedProperties.Private != nil {
		ev.OriginTag = item.ExtendedProperties.Private["calsync-origin"]
	}
	if item.Start != nil && item.Start.Date != "" {
		ev.IsAllDay = true
		ev.AllDayStart = item.Start.Date
		if item.End != nil {
			ev.AllDayEnd = item.End.Date
		}
		return ev
	}
	if item.Start != nil && item.Start.DateTime != "" {
		if t, err := time.Parse(time.RFC3339, item.Start.DateTime); err == nil {
			ev.StartUTC = t.UTC()
		}
	}
	if item.End != nil && item.End.DateTime != "" {
		if t, err := time.Parse(time.RFC3339, item.End.DateTime); err == nil {
			ev.EndUTC = t.UTC()
		}
	}
	return ev
}
