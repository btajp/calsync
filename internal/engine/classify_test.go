package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/work-a-co/calsync/internal/model"
)

// ShouldBlock の真理値表: IsBusy / IsDeclined / Deleted の全 8 組合せ。
// true になるのは「busy かつ未辞退かつ削除通知でない」の 1 組合せだけ(仕様6.2)。
func TestShouldBlock_TruthTable(t *testing.T) {
	cases := []struct {
		busy     bool
		declined bool
		deleted  bool
		want     bool
	}{
		{false, false, false, false},
		{false, false, true, false},
		{false, true, false, false},
		{false, true, true, false},
		{true, false, false, true},
		{true, false, true, false},
		{true, true, false, false},
		{true, true, true, false},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("busy=%v_declined=%v_deleted=%v", tc.busy, tc.declined, tc.deleted)
		t.Run(name, func(t *testing.T) {
			ev := model.NormalizedEvent{
				ID:         "e1",
				IsBusy:     tc.busy,
				IsDeclined: tc.declined,
				Deleted:    tc.deleted,
			}
			require.Equal(t, tc.want, ShouldBlock(ev))
		})
	}
}

// InWindow / Window.Contains の境界: end > w.Start && start < w.End
// (Google timeMin/timeMax と同じ境界セマンティクス。仕様5.3)。
func TestInWindow_Boundaries(t *testing.T) {
	w := model.Window{
		Start: time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC),
	}
	timed := func(start, end time.Time) model.NormalizedEvent {
		return model.NormalizedEvent{ID: "e1", StartUTC: start, EndUTC: end}
	}
	allDay := func(startDate, endDate string) model.NormalizedEvent {
		return model.NormalizedEvent{ID: "e1", IsAllDay: true, AllDayStart: startDate, AllDayEnd: endDate}
	}

	cases := []struct {
		name string
		ev   model.NormalizedEvent
		want bool
	}{
		{"完全に内側", timed(
			time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
			time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC)), true},
		{"終了がウィンドウ開始と一致(end > Start は排他)", timed(
			w.Start.Add(-time.Hour), w.Start), false},
		{"終了がウィンドウ開始の1秒後(境界を跨ぐ重なり)", timed(
			w.Start.Add(-time.Hour), w.Start.Add(time.Second)), true},
		{"開始がウィンドウ終了と一致(start < End は排他)", timed(
			w.End, w.End.Add(time.Hour)), false},
		{"開始がウィンドウ終了の1秒前(境界を跨ぐ重なり)", timed(
			w.End.Add(-time.Second), w.End.Add(time.Hour)), true},
		{"ウィンドウ全体を包含する長期イベント", timed(
			w.Start.Add(-24*time.Hour), w.End.Add(24*time.Hour)), true},
		{"完全に過去", timed(
			w.Start.Add(-2*time.Hour), w.Start.Add(-time.Hour)), false},
		{"完全に未来", timed(
			w.End.Add(time.Hour), w.End.Add(2*time.Hour)), false},
		{"終日: ウィンドウ内", allDay("2026-07-10", "2026-07-11"), true},
		{"終日: 排他的終了日がウィンドウ開始日(00:00 < 09:00 で対象外)", allDay("2026-07-02", "2026-07-03"), false},
		{"終日: ウィンドウ終了日に開始(00:00 < 09:00 で重なりあり)", allDay("2026-10-03", "2026-10-04"), true},
		{"終日: 開始日が不正な文字列", allDay("not-a-date", "2026-07-11"), false},
		{"終日: 終了日が不正な文字列", allDay("2026-07-10", ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, InWindow(w, tc.ev))
			// InWindow は Window.Contains への委譲であることも固定する
			require.Equal(t, tc.want, w.Contains(tc.ev))
		})
	}
}
