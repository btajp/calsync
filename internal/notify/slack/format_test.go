package slack

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/engine"
)

var jst = time.FixedZone("JST", 9*3600)

// 2026-07-05 は日曜(JST)。
var digestDay = time.Date(2026, 7, 5, 0, 0, 0, 0, jst)

func entry(title string, startJST, endJST time.Time, accts ...string) engine.DigestEntry {
	return engine.DigestEntry{Title: title, StartUTC: startJST.UTC(), EndUTC: endJST.UTC(), AccountIDs: accts}
}

func TestFormatDigest(t *testing.T) {
	entries := []engine.DigestEntry{
		{Title: "社内イベント", IsAllDay: true, AllDayStart: "2026-07-05", AccountIDs: []string{"work-google"}},
		entry("朝会", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 9, 30, 0, 0, jst), "work-google"),
		entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "work-ms", "personal"),
	}
	got := formatDigest(digestDay, entries, nil, jst)
	want := "📅 7/5(日) の予定\n" +
		"・(終日) 社内イベント [work-google]\n" +
		"・09:00–09:30 朝会 [work-google]\n" +
		"・10:00–11:00 設計レビュー [work-ms, personal]"
	require.Equal(t, want, got)
}

func TestFormatDigestEmptyAndFailed(t *testing.T) {
	got := formatDigest(digestDay, nil, []string{"acct-x"}, jst)
	require.Contains(t, got, "今日の予定はありません")
	require.Contains(t, got, "⚠ acct-x: 取得失敗")
}

// 件名は外部入力(会議招待)由来。<!channel> をエスケープしないと全員メンションが
// 発火する(メンションインジェクション。スペック 8 章)。
func TestFormatEscapesMentionInjection(t *testing.T) {
	e := entry("<!channel> attack & <evil>", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a")
	got := formatDigest(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.NotContains(t, got, "<!channel>")
	require.Contains(t, got, "&lt;!channel&gt; attack &amp; &lt;evil&gt;")
}

// 当日ウィンドウ外にはみ出す側に日付を付ける(スペック 7 章)。
func TestFormatCrossMidnight(t *testing.T) {
	e := entry("夜勤", time.Date(2026, 7, 4, 23, 0, 0, 0, jst), time.Date(2026, 7, 6, 1, 0, 0, 0, jst), "a")
	got := formatDigest(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.Contains(t, got, "7/4 23:00–7/6 01:00")
}

func TestFormatDigestCapsAt100(t *testing.T) {
	var entries []engine.DigestEntry
	for i := 0; i < 105; i++ {
		entries = append(entries, entry("e", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a"))
	}
	got := formatDigest(digestDay, entries, nil, jst)
	require.Contains(t, got, "…他 5 件")
}

func TestFormatReminder(t *testing.T) {
	e := entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "work-ms")
	got := formatReminder(e, 8*time.Minute, jst)
	require.Equal(t, "⏰ 8分後: 10:00–11:00 設計レビュー [work-ms]", got)

	// 1 分未満は「まもなく」
	got = formatReminder(e, 20*time.Second, jst)
	require.Contains(t, got, "まもなく")

	// 無題は「(件名なし)」(スペック 7 章)
	e.Title = ""
	got = formatReminder(e, 8*time.Minute, jst)
	require.Contains(t, got, "(件名なし)")
}
