package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTimeHash(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	timed := func(startHour, endHour int) NormalizedEvent {
		return NormalizedEvent{
			StartUTC: time.Date(2026, 7, 10, startHour, 0, 0, 0, time.UTC),
			EndUTC:   time.Date(2026, 7, 10, endHour, 0, 0, 0, time.UTC),
		}
	}
	allDay := func(start, end string) NormalizedEvent {
		return NormalizedEvent{IsAllDay: true, AllDayStart: start, AllDayEnd: end}
	}

	tests := []struct {
		name      string
		a, b      NormalizedEvent
		wantEqual bool
	}{
		{
			name:      "same timed input is stable",
			a:         timed(1, 2),
			b:         timed(1, 2),
			wantEqual: true,
		},
		{
			name: "non-UTC location is normalized before hashing",
			a:    timed(1, 2),
			b: NormalizedEvent{
				StartUTC: time.Date(2026, 7, 10, 10, 0, 0, 0, jst), // == 2026-07-10T01:00Z
				EndUTC:   time.Date(2026, 7, 10, 11, 0, 0, 0, jst), // == 2026-07-10T02:00Z
			},
			wantEqual: true,
		},
		{
			name:      "different end time differs",
			a:         timed(1, 2),
			b:         timed(1, 3),
			wantEqual: false,
		},
		{
			name:      "same all-day input is stable",
			a:         allDay("2026-07-10", "2026-07-11"),
			b:         allDay("2026-07-10", "2026-07-11"),
			wantEqual: true,
		},
		{
			name: "all-day differs from timed event covering the same instant range",
			a:    allDay("2026-07-10", "2026-07-11"),
			b: NormalizedEvent{
				StartUTC: time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
				EndUTC:   time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
			},
			wantEqual: false,
		},
		{
			name:      "different all-day dates differ",
			a:         allDay("2026-07-10", "2026-07-11"),
			b:         allDay("2026-07-10", "2026-07-12"),
			wantEqual: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ha, hb := TimeHash(tt.a), TimeHash(tt.b)
			require.Regexp(t, `^[0-9a-f]{16}$`, ha, "TimeHash must be 16 hex chars")
			require.Regexp(t, `^[0-9a-f]{16}$`, hb, "TimeHash must be 16 hex chars")
			if tt.wantEqual {
				require.Equal(t, ha, hb)
			} else {
				require.NotEqual(t, ha, hb)
			}
		})
	}
}

func TestIdempotencyKeys(t *testing.T) {
	tests := []struct {
		name          string
		originTag     string
		targetAccount string
	}{
		{name: "basic", originTag: "acct-a:ev123", targetAccount: "acct-b"},
		{name: "different target yields different keys", originTag: "acct-a:ev123", targetAccount: "acct-c"},
		{name: "different origin event yields different keys", originTag: "acct-a:ev999", targetAccount: "acct-b"},
	}
	seenGoogle := map[string]bool{}
	seenMS := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := GoogleBlockerID(tt.originTag, tt.targetAccount)
			// 接頭辞 cs + sha256 先頭20バイトの base32(5bit×32文字)。Google の
			// クライアント生成 ID 許容文字 a-v0-9(base32hex 小文字)のみで構成される。
			require.Regexp(t, `^cs[a-v0-9]{32}$`, g)
			require.Equal(t, g, GoogleBlockerID(tt.originTag, tt.targetAccount), "must be deterministic")
			require.False(t, seenGoogle[g], "GoogleBlockerID must be unique per (originTag, targetAccount)")
			seenGoogle[g] = true

			m := MSTransactionID(tt.originTag, tt.targetAccount)
			// calsync- + sha256 先頭16バイトの hex(32桁)。
			require.Regexp(t, `^calsync-[0-9a-f]{32}$`, m)
			require.Equal(t, m, MSTransactionID(tt.originTag, tt.targetAccount), "must be deterministic")
			require.False(t, seenMS[m], "MSTransactionID must be unique per (originTag, targetAccount)")
			seenMS[m] = true
		})
	}
}

func TestOriginTagOf(t *testing.T) {
	require.Equal(t, "acct-a:ev123", OriginTagOf("acct-a", "ev123"))
}

func TestCalendarRefString(t *testing.T) {
	require.Equal(t, "acct-a/primary", CalendarRef{AccountID: "acct-a", CalendarID: "primary"}.String())
}

// Title は通知表示専用であり、TimeHash(ブロッカー変更検出)に影響してはならない
// (スペック 4.1。件名変更でブロッカー更新が走ると Graph/Google への無駄な PATCH が出る)。
func TestTimeHashIgnoresTitle(t *testing.T) {
	base := NormalizedEvent{
		StartUTC: time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
	}
	titled := base
	titled.Title = "設計レビュー"
	require.Equal(t, TimeHash(base), TimeHash(titled))

	allday := NormalizedEvent{IsAllDay: true, AllDayStart: "2026-07-10", AllDayEnd: "2026-07-11"}
	alldayTitled := allday
	alldayTitled.Title = "終日イベント"
	require.Equal(t, TimeHash(allday), TimeHash(alldayTitled))
}

func TestWindowContains(t *testing.T) {
	w := Window{
		Start: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	d := func(month time.Month, day, hour int) time.Time {
		return time.Date(2026, month, day, hour, 0, 0, 0, time.UTC)
	}
	timed := func(start, end time.Time) NormalizedEvent {
		return NormalizedEvent{StartUTC: start, EndUTC: end}
	}
	allDay := func(start, end string) NormalizedEvent {
		return NormalizedEvent{IsAllDay: true, AllDayStart: start, AllDayEnd: end}
	}

	tests := []struct {
		name string
		ev   NormalizedEvent
		want bool
	}{
		{"fully inside is included", timed(d(7, 10, 10), d(7, 10, 11)), true},
		{"end exactly at window start is excluded", timed(d(6, 30, 23), d(7, 1, 0)), false},
		{"start exactly at window end is excluded", timed(d(10, 1, 0), d(10, 1, 1)), false},
		{"partial overlap across window start is included", timed(d(6, 30, 23), d(7, 1, 1)), true},
		{"partial overlap across window end is included", timed(d(9, 30, 23), d(10, 1, 1)), true},
		{"event spanning the entire window is included", timed(d(6, 1, 0), d(11, 1, 0)), true},
		{"entirely before window is excluded", timed(d(6, 1, 0), d(6, 2, 0)), false},
		{"entirely after window is excluded", timed(d(10, 2, 0), d(10, 3, 0)), false},
		{"all-day inside window is included", allDay("2026-07-10", "2026-07-11"), true},
		{"all-day ending on window start date is excluded", allDay("2026-06-30", "2026-07-01"), false},
		{"all-day overlapping window start is included", allDay("2026-06-30", "2026-07-02"), true},
		{"all-day starting at window end is excluded", allDay("2026-10-01", "2026-10-02"), false},
		{"all-day with unparsable dates is excluded", NormalizedEvent{IsAllDay: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, w.Contains(tt.ev))
		})
	}
}
