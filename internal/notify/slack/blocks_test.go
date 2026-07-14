package slack

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/engine"
)

// testColorFor は指定した Accounts 定義順で Client.colorFor を組み立てる(テスト用)。
func testColorFor(accounts ...string) func(string) string {
	return (&Client{Accounts: accounts}).colorFor
}

func TestDigestMessageLayout(t *testing.T) {
	entries := []engine.DigestEntry{
		{Title: "終日イベント", IsAllDay: true, AllDayStart: "2026-07-05", AccountIDs: []string{"a"},
			HTMLLink: "https://cal/allday"},
		entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "b", "c"),
	}
	entries[1].HTMLLink = "https://outlook.live.com/calendar/item/x"
	entries[1].MeetingURL = "https://zoom.us/j/123"

	blocks, attachments := digestMessage(digestDay, entries, []string{"acct-x"}, jst, testColorFor("a", "b", "c"))

	// トップレベル blocks は header + context 系のみ。
	require.Equal(t, "header", blocks[0].Type)
	require.Equal(t, "7/5(日) の予定", blocks[0].Text.Text)

	require.Len(t, attachments, 2)

	// 終日: リンクラベル・(終日) プレフィクス・ボタンなし。色はアカウント a(index 0)。
	first := attachments[0].Blocks[0]
	require.Equal(t, "section", first.Type)
	require.Contains(t, first.Text.Text, "*(終日)*")
	require.Contains(t, first.Text.Text, "<https://cal/allday|終日イベント>")
	require.Nil(t, first.Accessory)
	require.Equal(t, "#4285F4", attachments[0].Color)

	// 時刻指定: 太字レンジ・リンク・複数アカウント併記・参加ボタン。色は先頭アカウント b(index 1)。
	second := attachments[1].Blocks[0]
	require.Contains(t, second.Text.Text, "*10:00–11:00*")
	require.Contains(t, second.Text.Text, "<https://outlook.live.com/calendar/item/x|設計レビュー>")
	require.Contains(t, second.Text.Text, "[b, c]")
	require.NotNil(t, second.Accessory)
	require.Equal(t, "https://zoom.us/j/123", second.Accessory.URL)
	require.Equal(t, "参加", second.Accessory.Text.Text)
	require.Equal(t, "#0F9D58", attachments[1].Color)

	// 取得失敗 context はトップレベル blocks の末尾。
	last := blocks[len(blocks)-1]
	require.Equal(t, "context", last.Type)
	require.Contains(t, last.Elements[0].Text, "⚠ acct-x: 取得失敗")
}

// 21 件・同一アカウント → 連続同色グルーピングにより attachment は 1 個に束ねられ、
// sections は上限 20 個で打ち切り(v2.2 スペック 5 章)。
func TestDigestMessageCapsAt20(t *testing.T) {
	var entries []engine.DigestEntry
	for i := 0; i < 21; i++ {
		entries = append(entries, entry("e", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a"))
	}
	blocks, attachments := digestMessage(digestDay, entries, nil, jst, testColorFor("a"))
	require.Len(t, attachments, 1)
	require.Len(t, attachments[0].Blocks, 20)
	last := blocks[len(blocks)-1]
	require.Equal(t, "context", last.Type)
	require.Contains(t, last.Elements[0].Text, "…他 1 件")
}

// 連続する同一アカウントの予定は 1 attachment に束ねられ、section は予定ごとに維持される
// (accessory=参加ボタンも個別に維持。v2.2 スペック 5 章)。
func TestDigestMessageGroupsConsecutiveSameAccount(t *testing.T) {
	entries := []engine.DigestEntry{
		entry("朝会", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 9, 30, 0, 0, jst), "work-a"),
		entry("Daily", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 30, 0, 0, jst), "work-a"),
		entry("Weekly", time.Date(2026, 7, 5, 11, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 30, 0, 0, jst), "work-a"),
	}
	for i := range entries {
		entries[i].MeetingURL = "https://zoom.us/j/123"
	}
	blocks, attachments := digestMessage(digestDay, entries, nil, jst, testColorFor("work-a"))
	require.Len(t, blocks, 1) // header のみ(超過・失敗なし)
	require.Len(t, attachments, 1)
	require.Len(t, attachments[0].Blocks, 3)
	require.Equal(t, "#4285F4", attachments[0].Color)
	for _, sec := range attachments[0].Blocks {
		require.Equal(t, "section", sec.Type)
		require.NotNil(t, sec.Accessory)
	}
}

// アカウントが交互(a, b, a)に変わる場合は並べ替えず、都度新しい attachment を開始する
// (時系列維持の検証。v2.2 スペック 5 章)。
func TestDigestMessageAlternatingAccountsDoNotMerge(t *testing.T) {
	entries := []engine.DigestEntry{
		entry("A1", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 9, 30, 0, 0, jst), "a"),
		entry("B1", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 30, 0, 0, jst), "b"),
		entry("A2", time.Date(2026, 7, 5, 11, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 30, 0, 0, jst), "a"),
	}
	_, attachments := digestMessage(digestDay, entries, nil, jst, testColorFor("a", "b"))
	require.Len(t, attachments, 3)
	require.Len(t, attachments[0].Blocks, 1)
	require.Contains(t, attachments[0].Blocks[0].Text.Text, "A1")
	require.Equal(t, "#4285F4", attachments[0].Color)
	require.Len(t, attachments[1].Blocks, 1)
	require.Contains(t, attachments[1].Blocks[0].Text.Text, "B1")
	require.Equal(t, "#0F9D58", attachments[1].Color)
	require.Len(t, attachments[2].Blocks, 1)
	require.Contains(t, attachments[2].Blocks[0].Text.Text, "A2")
	require.Equal(t, "#4285F4", attachments[2].Color)
}

// 実運用の典型形: 終日(personal)1 件 → 時刻付き(work-a)が連続 → attachment 2 個。
func TestDigestMessageRealisticAllDayThenConsecutiveTimed(t *testing.T) {
	entries := []engine.DigestEntry{
		{Title: "ゴミの日", IsAllDay: true, AllDayStart: "2026-07-05", AccountIDs: []string{"personal"}},
		entry("朝会", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 9, 30, 0, 0, jst), "work-a"),
		entry("Daily", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 30, 0, 0, jst), "work-a"),
	}
	_, attachments := digestMessage(digestDay, entries, nil, jst, testColorFor("personal", "work-a"))
	require.Len(t, attachments, 2)
	require.Len(t, attachments[0].Blocks, 1)
	require.Contains(t, attachments[0].Blocks[0].Text.Text, "*(終日)*")
	require.Len(t, attachments[1].Blocks, 2)
}

// 20 件目がグループ途中でも件数優先で打ち切ること(グループは途中で切れてよい。v2.2 スペック 5 章)。
func TestDigestMessageTruncatesMidGroupAt20(t *testing.T) {
	var entries []engine.DigestEntry
	// 19 件は account a、続く 3 件は account b(20 件目で打ち切られ b は 1 件のみ表示)。
	for i := 0; i < 19; i++ {
		entries = append(entries, entry("a-event", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 9, 30, 0, 0, jst), "a"))
	}
	for i := 0; i < 3; i++ {
		entries = append(entries, entry("b-event", time.Date(2026, 7, 5, 11, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 30, 0, 0, jst), "b"))
	}
	blocks, attachments := digestMessage(digestDay, entries, nil, jst, testColorFor("a", "b"))
	require.Len(t, attachments, 2)
	require.Len(t, attachments[0].Blocks, 19)
	require.Len(t, attachments[1].Blocks, 1) // b は 20 件目の 1 件だけ表示され打ち切り
	last := blocks[len(blocks)-1]
	require.Equal(t, "context", last.Type)
	require.Contains(t, last.Elements[0].Text, "…他 2 件")
}

func TestDigestMessageZeroEvents(t *testing.T) {
	blocks, attachments := digestMessage(digestDay, nil, nil, jst, testColorFor())
	require.Len(t, blocks, 2)
	require.Contains(t, blocks[1].Text.Text, "今日の予定はありません")
	require.Empty(t, attachments)
}

// 色の割当: 未知アカウント(Accounts に含まれない)は #999999(v2.1 スペック 5 章)。
func TestDigestMessageUnknownAccountColor(t *testing.T) {
	e := entry("件名", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "unknown")
	_, attachments := digestMessage(digestDay, []engine.DigestEntry{e}, nil, jst, testColorFor("a", "b"))
	require.Len(t, attachments, 1)
	require.Equal(t, "#999999", attachments[0].Color)
}

// URL 検証: https 以外・禁止文字入り・超長はリンク化/ボタン化しない(v2 スペック 7 章)。
func TestDigestMessageRejectsInvalidURLs(t *testing.T) {
	e := entry("件名", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a")
	e.HTMLLink = "http://insecure.example.com"
	e.MeetingURL = "https://zoom.us/j/1 23" // 空白入り
	_, attachments := digestMessage(digestDay, []engine.DigestEntry{e}, nil, jst, testColorFor("a"))
	sectionText := attachments[0].Blocks[0].Text.Text
	require.NotContains(t, sectionText, "<http")
	require.Contains(t, sectionText, "件名") // プレーン表示
	require.Nil(t, attachments[0].Blocks[0].Accessory)

	e.MeetingURL = "https://zoom.us/j/" + strings.Repeat("9", 2100) // 2,000 rune 超
	_, attachments = digestMessage(digestDay, []engine.DigestEntry{e}, nil, jst, testColorFor("a"))
	require.Nil(t, attachments[0].Blocks[0].Accessory)
}

// 超長件名はエスケープ後 200 rune で切り詰め(1 予定で section 3,000 字を超えない)。
func TestDigestMessageTruncatesLongTitle(t *testing.T) {
	e := entry(strings.Repeat("あ", 500), time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a")
	e.HTMLLink = "https://cal/x"
	_, attachments := digestMessage(digestDay, []engine.DigestEntry{e}, nil, jst, testColorFor("a"))
	text := attachments[0].Blocks[0].Text.Text
	require.Less(t, utf8.RuneCountInString(text), 300)
	require.Contains(t, text, "…")
}

func TestReminderMessage(t *testing.T) {
	e := entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "b")
	e.MeetingURL = "https://zoom.us/j/123"
	e.HTMLLink = "https://cal/x"
	e.Description = "アジェンダ\n1. 進捗"

	blocks, attachments := reminderMessage(e, 8*time.Minute, jst, testColorFor("a", "b"))
	require.Empty(t, blocks) // トップレベル blocks は使わない(v2.1 スペック 6 章)
	require.Len(t, attachments, 1)
	require.Equal(t, "#0F9D58", attachments[0].Color) // b は index 1

	sec := attachments[0].Blocks
	require.Len(t, sec, 2)
	require.Contains(t, sec[0].Text.Text, "⏰ *8分後*")
	require.Contains(t, sec[0].Text.Text, "<https://cal/x|設計レビュー>")
	require.NotNil(t, sec[0].Accessory)
	require.Equal(t, "アジェンダ\n1. 進捗", sec[1].Text.Text)

	// 空白のみ description は section を出さない(v2 スペック 6 章)
	e.Description = " \r\n\t"
	_, attachments = reminderMessage(e, 8*time.Minute, jst, testColorFor("a", "b"))
	require.Len(t, attachments[0].Blocks, 1)

	// 1 分未満は「まもなく」
	_, attachments = reminderMessage(e, 20*time.Second, jst, testColorFor("a", "b"))
	require.Contains(t, attachments[0].Blocks[0].Text.Text, "*まもなく*")
}

// description のエスケープとエスケープ後切り詰め(v2 スペック 7 章)。
func TestReminderDescriptionEscapeAndTruncate(t *testing.T) {
	e := entry("x", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "a")

	// <!channel> インジェクション
	e.Description = "<!channel> 全員集合 & <b>太字</b>"
	_, attachments := reminderMessage(e, time.Minute, jst, testColorFor("a"))
	desc := attachments[0].Blocks[1].Text.Text
	require.NotContains(t, desc, "<!channel>")
	require.Contains(t, desc, "&lt;!channel&gt;")

	// & 連続本文: エスケープ後(1 文字 → &amp; の 5 文字)に切り詰めるため 3,000 未満に収まる
	e.Description = strings.Repeat("&", 2000)
	_, attachments = reminderMessage(e, time.Minute, jst, testColorFor("a"))
	got := attachments[0].Blocks[1].Text.Text
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
