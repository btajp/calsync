package slack

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/engine"
)

func TestDigestBlocksLayout(t *testing.T) {
	entries := []engine.DigestEntry{
		{Title: "終日イベント", IsAllDay: true, AllDayStart: "2026-07-05", AccountIDs: []string{"a"},
			HTMLLink: "https://cal/allday"},
		entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "b", "c"),
	}
	entries[1].HTMLLink = "https://outlook.live.com/calendar/item/x"
	entries[1].MeetingURL = "https://zoom.us/j/123"

	blocks := digestBlocks(digestDay, entries, []string{"acct-x"}, jst)
	require.Equal(t, "header", blocks[0].Type)
	require.Equal(t, "7/5(日) の予定", blocks[0].Text.Text)

	// 終日: リンクラベル・(終日) プレフィクス・ボタンなし
	require.Equal(t, "section", blocks[1].Type)
	require.Contains(t, blocks[1].Text.Text, "*(終日)*")
	require.Contains(t, blocks[1].Text.Text, "<https://cal/allday|終日イベント>")
	require.Nil(t, blocks[1].Accessory)

	// 時刻指定: 太字レンジ・リンク・複数アカウント併記・参加ボタン
	require.Contains(t, blocks[2].Text.Text, "*10:00–11:00*")
	require.Contains(t, blocks[2].Text.Text, "<https://outlook.live.com/calendar/item/x|設計レビュー>")
	require.Contains(t, blocks[2].Text.Text, "[b, c]")
	require.NotNil(t, blocks[2].Accessory)
	require.Equal(t, "https://zoom.us/j/123", blocks[2].Accessory.URL)
	require.Equal(t, "参加", blocks[2].Accessory.Text.Text)

	// 取得失敗 context
	last := blocks[len(blocks)-1]
	require.Equal(t, "context", last.Type)
	require.Contains(t, last.Elements[0].Text, "⚠ acct-x: 取得失敗")
}

func TestDigestBlocksCapsAt46(t *testing.T) {
	var entries []engine.DigestEntry
	for i := 0; i < 50; i++ {
		entries = append(entries, entry("e", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a"))
	}
	blocks := digestBlocks(digestDay, entries, nil, jst)
	require.LessOrEqual(t, len(blocks), 49) // header 1 + sections 46 + context 他N件 1(+失敗 context なし)
	last := blocks[len(blocks)-1]
	require.Equal(t, "context", last.Type)
	require.Contains(t, last.Elements[0].Text, "…他 4 件")
}

func TestDigestBlocksZeroEvents(t *testing.T) {
	blocks := digestBlocks(digestDay, nil, nil, jst)
	require.Len(t, blocks, 2)
	require.Contains(t, blocks[1].Text.Text, "今日の予定はありません")
}

// URL 検証: https 以外・禁止文字入り・超長はリンク化/ボタン化しない(v2 スペック 7 章)。
func TestBlocksRejectInvalidURLs(t *testing.T) {
	e := entry("件名", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a")
	e.HTMLLink = "http://insecure.example.com"
	e.MeetingURL = "https://zoom.us/j/1 23" // 空白入り
	blocks := digestBlocks(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.NotContains(t, blocks[1].Text.Text, "<http")
	require.Contains(t, blocks[1].Text.Text, "件名") // プレーン表示
	require.Nil(t, blocks[1].Accessory)

	e.MeetingURL = "https://zoom.us/j/" + strings.Repeat("9", 2100) // 2,000 rune 超
	blocks = digestBlocks(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.Nil(t, blocks[1].Accessory)
}

// 超長件名はエスケープ後 200 rune で切り詰め(1 予定で section 3,000 字を超えない)。
func TestBlocksTruncateLongTitle(t *testing.T) {
	e := entry(strings.Repeat("あ", 500), time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a")
	e.HTMLLink = "https://cal/x"
	blocks := digestBlocks(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.Less(t, utf8.RuneCountInString(blocks[1].Text.Text), 300)
	require.Contains(t, blocks[1].Text.Text, "…")
}

func TestReminderBlocks(t *testing.T) {
	e := entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "b")
	e.MeetingURL = "https://zoom.us/j/123"
	e.HTMLLink = "https://cal/x"
	e.Description = "アジェンダ\n1. 進捗"

	blocks := reminderBlocks(e, 8*time.Minute, jst)
	require.Len(t, blocks, 2)
	require.Contains(t, blocks[0].Text.Text, "⏰ *8分後*")
	require.Contains(t, blocks[0].Text.Text, "<https://cal/x|設計レビュー>")
	require.NotNil(t, blocks[0].Accessory)
	require.Equal(t, "アジェンダ\n1. 進捗", blocks[1].Text.Text)

	// 空白のみ description は section を出さない(v2 スペック 6 章)
	e.Description = " \r\n\t"
	blocks = reminderBlocks(e, 8*time.Minute, jst)
	require.Len(t, blocks, 1)

	// 1 分未満は「まもなく」
	blocks = reminderBlocks(e, 20*time.Second, jst)
	require.Contains(t, blocks[0].Text.Text, "*まもなく*")
}

// description のエスケープとエスケープ後切り詰め(v2 スペック 7 章)。
func TestReminderDescriptionEscapeAndTruncate(t *testing.T) {
	e := entry("x", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "a")

	// <!channel> インジェクション
	e.Description = "<!channel> 全員集合 & <b>太字</b>"
	blocks := reminderBlocks(e, time.Minute, jst)
	require.NotContains(t, blocks[1].Text.Text, "<!channel>")
	require.Contains(t, blocks[1].Text.Text, "&lt;!channel&gt;")

	// & 連続本文: エスケープ後(1 文字 → &amp; の 5 文字)に切り詰めるため 3,000 未満に収まる
	e.Description = strings.Repeat("&", 2000)
	blocks = reminderBlocks(e, time.Minute, jst)
	got := blocks[1].Text.Text
	require.LessOrEqual(t, utf8.RuneCountInString(got), 2905+len([]rune("…(省略)")))
	// 実体参照の途中で切れていない(末尾は …(省略) の直前が完全な &amp;)
	require.NotRegexp(t, `&a?m?p?$`, strings.TrimSuffix(got, "…(省略)"))
}

func TestTruncateEscaped(t *testing.T) {
	// 実体参照の途中に切断位置が当たる場合は参照ごと落とす
	s := strings.Repeat("あ", 8) + "&amp;" // 13 rune
	require.Equal(t, strings.Repeat("あ", 8)+"…", truncateEscaped(s, 10, "…"))
	// 上限以内はそのまま
	require.Equal(t, s, truncateEscaped(s, 13, "…"))
}
