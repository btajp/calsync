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

// Provider の全メソッドが揃う本ファイルで、コンパイル時に実装を保証する。
var _ provider.Provider = (*Client)(nil)

// blockerStart / blockerEnd: 時刻指定は dateTime(UTC 固定)、
// 終日は date に現地日付をそのまま入れる(end は排他的。仕様書6.6)。
func blockerStart(b model.Blocker) *calendar.EventDateTime {
	if b.IsAllDay {
		return &calendar.EventDateTime{Date: b.AllDayStart}
	}
	return &calendar.EventDateTime{
		DateTime: b.StartUTC.UTC().Format(time.RFC3339),
		TimeZone: "UTC",
	}
}

func blockerEnd(b model.Blocker) *calendar.EventDateTime {
	if b.IsAllDay {
		return &calendar.EventDateTime{Date: b.AllDayEnd}
	}
	return &calendar.EventDateTime{
		DateTime: b.EndUTC.UTC().Format(time.RFC3339),
		TimeZone: "UTC",
	}
}

// blockerEventBody はブロッカーの本体(summary / transparency / visibility /
// reminders / extendedProperties / start / end)を組み立てる。events.insert
// (呼び出し元が Id を追加設定する)と、409 収容が cancelled イベントを蘇生させる
// events.update の両方で共用する(仕様書6.4。最終ホールブランチレビュー修正1)。
func blockerEventBody(b model.Blocker) *calendar.Event {
	return &calendar.Event{
		Summary:      b.Title,
		Transparency: "opaque",
		Visibility:   "private",
		Start:        blockerStart(b),
		End:          blockerEnd(b),
		Reminders: &calendar.EventReminders{
			UseDefault: false,
			// ゼロ値の false は omitempty で消えるため明示送信する
			ForceSendFields: []string{"UseDefault"},
		},
		ExtendedProperties: &calendar.EventExtendedProperties{
			Private: map[string]string{
				"calsync":        "v1",
				"calsync-origin": b.OriginTag,
			},
		},
	}
}

// CreateBlocker は idemKey をクライアント生成イベント ID として events.insert する
// (冪等作成。仕様書6.4)。409(ID 衝突)は「同一冪等キーで作成済み」を意味するため、
// events.get で実在確認する。
//
// ただし Google は削除済みイベントを cancelled 状態のまま保持し、同一 ID の
// 再 insert も 409 を返す。cancelled の ID をそのまま返すと、そのブロッカーは
// カレンダー上に見えないにもかかわらず active mapping に収容されてしまい、
// busy→free→busy のような削除→再作成シナリオでブロッカーが二度と出現しなくなる
// (最終ホールブランチレビュー所見1)。そのため cancelled の場合は events.update で
// 本来 insert するはずだったボディを送って蘇生し、その ID を返す。
func (c *Client) CreateBlocker(ctx context.Context, cal model.CalendarRef, b model.Blocker, idemKey string) (string, error) {
	svc, err := c.service(ctx)
	if err != nil {
		return "", err
	}
	body := blockerEventBody(b)
	body.Id = idemKey
	insert := svc.Events.Insert(cal.CalendarID, body).Context(ctx)
	var created *calendar.Event
	err = c.doWithRetry(ctx, func() error {
		var e error
		created, e = insert.Do()
		return e
	})
	if err == nil {
		return created.Id, nil
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusConflict {
		get := svc.Events.Get(cal.CalendarID, idemKey).Context(ctx)
		var existing *calendar.Event
		if err := c.doWithRetry(ctx, func() error {
			var e error
			existing, e = get.Do()
			return e
		}); err != nil {
			return "", fmt.Errorf("google[%s]: confirm existing blocker %s: %w", c.accountID, idemKey, normalizeAuthErr(err))
		}
		if existing.Status != "cancelled" {
			return existing.Id, nil
		}
		update := svc.Events.Update(cal.CalendarID, idemKey, blockerEventBody(b)).Context(ctx)
		var revived *calendar.Event
		if err := c.doWithRetry(ctx, func() error {
			var e error
			revived, e = update.Do()
			return e
		}); err != nil {
			return "", fmt.Errorf("google[%s]: resurrect cancelled blocker %s: %w", c.accountID, idemKey, normalizeAuthErr(err))
		}
		return revived.Id, nil
	}
	return "", fmt.Errorf("google[%s]: events.insert %s: %w", c.accountID, cal, normalizeAuthErr(err))
}

// UpdateBlocker は events.patch で start/end のみ更新する(タイトル等は送らない)。
// 404(ブロッカーが手動削除等で消えている)は provider.ErrNotFound に写像し、
// エンジン側が「pending 化して再作成」にフォールバックできるようにする(仕様8章4)。
func (c *Client) UpdateBlocker(ctx context.Context, cal model.CalendarRef, eventID string, b model.Blocker) error {
	svc, err := c.service(ctx)
	if err != nil {
		return err
	}
	patch := &calendar.Event{
		Start: blockerStart(b),
		End:   blockerEnd(b),
	}
	call := svc.Events.Patch(cal.CalendarID, eventID, patch).Context(ctx)
	err = c.doWithRetry(ctx, func() error {
		_, e := call.Do()
		return e
	})
	if err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
			return fmt.Errorf("google[%s]: events.patch %s/%s: %w", c.accountID, cal, eventID, provider.ErrNotFound)
		}
		return fmt.Errorf("google[%s]: events.patch %s/%s: %w", c.accountID, cal, eventID, normalizeAuthErr(err))
	}
	return nil
}

// DeleteBlocker は events.delete を実行する。404(存在しない)・410(削除済み)は
// 成功扱いで nil を返す(冪等削除。コントラクト: 404 は成功扱い)。
func (c *Client) DeleteBlocker(ctx context.Context, cal model.CalendarRef, eventID string) error {
	svc, err := c.service(ctx)
	if err != nil {
		return err
	}
	call := svc.Events.Delete(cal.CalendarID, eventID).Context(ctx)
	err = c.doWithRetry(ctx, func() error { return call.Do() })
	if err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && (gerr.Code == http.StatusNotFound || gerr.Code == http.StatusGone) {
			return nil
		}
		return fmt.Errorf("google[%s]: events.delete %s/%s: %w", c.accountID, cal, eventID, normalizeAuthErr(err))
	}
	return nil
}

// ListBlockers は calsync タグ付きイベントを privateExtendedProperty で列挙する。
// privateExtendedProperty は syncToken と併用できないためリコンサイル専用(仕様書4章)。
// TimeHash は normalizeEvent の結果(終日/時刻指定を吸収済み)に model.TimeHash を適用する。
func (c *Client) ListBlockers(ctx context.Context, cal model.CalendarRef, window model.Window) ([]model.BlockerRecord, error) {
	svc, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	var (
		records   []model.BlockerRecord
		pageToken string
	)
	for {
		call := svc.Events.List(cal.CalendarID).Context(ctx).
			PrivateExtendedProperty("calsync=v1").
			TimeMin(window.Start.UTC().Format(time.RFC3339)).
			TimeMax(window.End.UTC().Format(time.RFC3339))
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
			return nil, fmt.Errorf("google[%s]: list blockers %s: %w", c.accountID, cal, normalizeAuthErr(err))
		}
		for _, item := range resp.Items {
			if item.Status == "cancelled" {
				continue
			}
			nev := normalizeEvent(item)
			records = append(records, model.BlockerRecord{
				EventID:   nev.ID,
				OriginTag: nev.OriginTag,
				TimeHash:  model.TimeHash(nev),
			})
		}
		if resp.NextPageToken == "" {
			return records, nil
		}
		pageToken = resp.NextPageToken
	}
}
