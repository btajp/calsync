package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/store"
)

// TestCollectWindowAllDayRangeIntersection は複数日窓での終日イベントの現地日付
// 範囲交差判定を検証する(デスクトップカレンダービュー設計 2026-07-21 §2)。
// 窓は JST の 7/5 00:00 〜 7/8 00:00(半開区間。含む日は 7/5・7/6・7/7 の 3 日、
// 7/8 は含まない)。境界を跨ぐ終日イベント(開始側・終了側)が交差判定で
// 正しく含まれ、窓の完全外側(前日・翌日)は除外されることを確認する。
func TestCollectWindowAllDayRangeIntersection(t *testing.T) {
	e, f := newTestEngine(t)
	winStart := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)
	winEnd := time.Date(2026, 7, 8, 0, 0, 0, 0, jstLoc)

	f.SetFullState(refA, []model.NormalizedEvent{
		{ID: "before", ICalUID: "before@x", Title: "窓の前", IsAllDay: true, AllDayStart: "2026-07-03", AllDayEnd: "2026-07-04", IsBusy: true},
		{ID: "span-start", ICalUID: "span-start@x", Title: "開始側で跨ぐ", IsAllDay: true, AllDayStart: "2026-07-04", AllDayEnd: "2026-07-06", IsBusy: true},
		{ID: "within", ICalUID: "within@x", Title: "窓の内側", IsAllDay: true, AllDayStart: "2026-07-06", AllDayEnd: "2026-07-07", IsBusy: true},
		{ID: "span-end", ICalUID: "span-end@x", Title: "終了側で跨ぐ", IsAllDay: true, AllDayStart: "2026-07-07", AllDayEnd: "2026-07-10", IsBusy: true},
		{ID: "after", ICalUID: "after@x", Title: "窓の後", IsAllDay: true, AllDayStart: "2026-07-08", AllDayEnd: "2026-07-09", IsBusy: true},
	})
	f.SetFullState(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, nil)
	f.SetFullState(model.CalendarRef{AccountID: "c", CalendarID: "primary"}, nil)

	entries, failed := e.CollectWindow(context.Background(), model.Window{Start: winStart, End: winEnd})
	require.Empty(t, failed)

	var titles []string
	for _, en := range entries {
		titles = append(titles, en.Title)
	}
	require.ElementsMatch(t, []string{"開始側で跨ぐ", "窓の内側", "終了側で跨ぐ"}, titles)
}

// TestCollectWindowExcludesBlockersAcrossMultiDay は複数日窓でもブロッカー除外
// (mappings 一次判定)が効くことを検証する(スペック §2 の 3 層除外のうち
// ブロッカー層。デスクトップカレンダービュー用途では受領ブロッカーの表示混入は
// 「予定あり」の二重表示になるため必ず除外されなければならない)。
func TestCollectWindowExcludesBlockersAcrossMultiDay(t *testing.T) {
	e, f := newTestEngine(t)
	winStart := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)
	winEnd := time.Date(2026, 7, 8, 0, 0, 0, 0, jstLoc)

	f.SetFullState(refA, []model.NormalizedEvent{
		timedEvent("real", "real@x", "本物の予定", time.Date(2026, 7, 6, 10, 0, 0, 0, jstLoc), true),
		timedEvent("blocker", "blocker@x", "受領ブロッカー", time.Date(2026, 7, 6, 14, 0, 0, 0, jstLoc), true),
	})
	// mappings 一次判定: アカウント a 上の "blocker" は受領ブロッカー
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "b", OriginCalendar: "primary", OriginEventID: "src1",
		TargetAccount: "a", TargetCalendar: "primary", BlockerEventID: "blocker",
		IdempotencyKey: "k1", TimeHash: "h1", Status: store.StatusActive,
	}))
	f.SetFullState(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, nil)
	f.SetFullState(model.CalendarRef{AccountID: "c", CalendarID: "primary"}, nil)

	entries, failed := e.CollectWindow(context.Background(), model.Window{Start: winStart, End: winEnd})
	require.Empty(t, failed)
	require.Len(t, entries, 1)
	require.Equal(t, "本物の予定", entries[0].Title)
}

// TestCollectWindowAllDayBoundaryDependsOnWindowOffset は、終日イベントの
// 日付境界判定が w.Start/w.End が保持するオフセット(Location)に依存すること、
// つまり appserver の /api/events が「閲覧者のローカルオフセット付き RFC3339」を
// 要求する契約の根拠を検証する(events.go の handleEvents doc コメント、
// デスクトップカレンダービュー設計 2026-07-21 §4)。
//
// jstWindow と utcWindow は絶対時刻としては全く同一の瞬間区間(jstWindow.UTC()
// を取っただけ)だが、winStartDate/winEndDateInclusive は各時刻が保持する
// Location でフォーマットされるため文字列が変わる: JST の "2026-07-05" 00:00
// は UTC 表記だと前日 "2026-07-04" 15:00 になり、CollectWindow が算出する
// 現地日付ラベルが 1 日ずれる。これにより、JST の暦日 7/5 の終日イベントは
// jstWindow(+09:00 のまま送った場合)では含まれるが、utcWindow(UTC に変換して
// 送った場合)では消える — フロントが UTC を送ってはいけない理由そのもの。
func TestCollectWindowAllDayBoundaryDependsOnWindowOffset(t *testing.T) {
	e, f := newTestEngine(t)
	f.SetFullState(refA, []model.NormalizedEvent{
		{ID: "jst-day", ICalUID: "jst-day@x", Title: "JST 7/5 の終日", IsAllDay: true, AllDayStart: "2026-07-05", AllDayEnd: "2026-07-06", IsBusy: true},
	})
	f.SetFullState(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, nil)
	f.SetFullState(model.CalendarRef{AccountID: "c", CalendarID: "primary"}, nil)

	// jstWindow: JST の暦日 7/5 をそのまま +09:00 で表した窓(正しい契約)。
	jstStart := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)
	jstEnd := time.Date(2026, 7, 6, 0, 0, 0, 0, jstLoc)
	jstWindow := model.Window{Start: jstStart, End: jstEnd}

	// utcWindow: jstWindow と絶対時刻は完全に同一(.UTC() は瞬間を変えない)だが、
	// Location が UTC になるため Format("2006-01-02") の結果が変わる
	// (フロントが誤って "Z" 付きに変換して送った場合の再現)。
	utcWindow := model.Window{Start: jstStart.UTC(), End: jstEnd.UTC()}
	require.True(t, jstWindow.Start.Equal(utcWindow.Start), "same instant, different Location")
	require.True(t, jstWindow.End.Equal(utcWindow.End), "same instant, different Location")

	jstEntries, failed := e.CollectWindow(context.Background(), jstWindow)
	require.Empty(t, failed)
	require.Len(t, jstEntries, 1, "JST offset window must include the JST calendar-day all-day event")

	utcEntries, failed := e.CollectWindow(context.Background(), utcWindow)
	require.Empty(t, failed)
	require.Empty(t, utcEntries, "UTC-converted window must NOT include it (date label shifts a day earlier)")
}
