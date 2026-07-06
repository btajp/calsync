package slack

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/btajp/calsync/internal/engine"
)

// Block Kit の最小型(chat.postMessage の blocks/attachments ペイロード用。v2/v2.1 スペック 8 章)。
type block struct {
	Type      string         `json:"type"`
	Text      *textObject    `json:"text,omitempty"`
	Elements  []textObject   `json:"elements,omitempty"` // context 用
	Accessory *buttonElement `json:"accessory,omitempty"`
}

type textObject struct {
	Type string `json:"type"` // "mrkdwn" | "plain_text"
	Text string `json:"text"`
}

type buttonElement struct {
	Type string     `json:"type"` // 常に "button"
	Text textObject `json:"text"` // plain_text
	URL  string     `json:"url"`
}

// attachment は予定 1 件分の色付きラッパー(左バーの色 = 由来アカウント色。v2.1 スペック 5 章)。
type attachment struct {
	Color  string  `json:"color"`
	Blocks []block `json:"blocks"`
}

const (
	// maxDigestAttachments: 予定ごとの attachment はSlackの実用上限に合わせ最大 20 件。
	// 超過分はトップレベル blocks の context「…他 N 件」に集約する
	// (46 件キャップは v2.1 で 20 に変更。v2.1 スペック 5 章)
	maxDigestAttachments = 20
	maxLabelRunes        = 200  // リンクラベル(件名)の上限
	maxDescRunes         = 2900 // description section の上限(text 3,000 の安全マージン)
	maxURLRunes          = 2000 // リンク・ボタン URL の上限
)

// validRenderURL はレンダリング直前の URL 検証(v2 スペック 7 章)。
// 構造化フィールド由来の URL は https 保証がないため、出所を問わず適用する。
func validRenderURL(u string) bool {
	if !strings.HasPrefix(u, "https://") || utf8.RuneCountInString(u) > maxURLRunes {
		return false
	}
	return !strings.ContainsAny(u, " |<>\t\r\n")
}

// truncateEscaped は escapeText 適用済みの文字列を rune 単位で limit に切り詰める。
// 切断位置が実体参照(&…;)の途中なら参照ごと落とす(v2 スペック 7 章)。
func truncateEscaped(s string, limit int, ellipsis string) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	cut := string(runes[:limit])
	if i := strings.LastIndex(cut, "&"); i >= 0 && !strings.Contains(cut[i:], ";") {
		cut = cut[:i]
	}
	return cut + ellipsis
}

// linkLabel はリンクラベル用の件名を組み立てる(escapeText + `|` 置換 + 200 rune 切り詰め)。
func linkLabel(title string) string {
	if title == "" {
		return "(件名なし)"
	}
	s := strings.ReplaceAll(escapeText(title), "|", "/")
	return truncateEscaped(s, maxLabelRunes, "…")
}

// linkedTitle は件名を HTMLLink へのリンクにする。URL 検証不合格ならプレーン表示。
func linkedTitle(e engine.DigestEntry) string {
	label := linkLabel(e.Title)
	if validRenderURL(e.HTMLLink) {
		return "<" + e.HTMLLink + "|" + label + ">"
	}
	return label
}

// joinButton は会議 URL の「参加」ボタンを返す。URL 検証不合格なら nil(v2 スペック 5 章)。
func joinButton(meetingURL string) *buttonElement {
	if !validRenderURL(meetingURL) {
		return nil
	}
	return &buttonElement{Type: "button", Text: textObject{Type: "plain_text", Text: "参加"}, URL: meetingURL}
}

// colorAccountOf はエントリの色決定に使うアカウントを返す(dedupe 統合の先頭 = AccountIDs[0]。
// v2.1 スペック 5 章)。AccountIDs が空(想定外)なら空文字を返し、colorFor は未知色にフォールバックする。
func colorAccountOf(e engine.DigestEntry) string {
	if len(e.AccountIDs) == 0 {
		return ""
	}
	return e.AccountIDs[0]
}

// eventSection は 1 予定分の section block(会議 URL があれば参加ボタンを accessory に)を組み立てる。
func eventSection(e engine.DigestEntry, day time.Time, loc *time.Location) block {
	prefix := "*(終日)*"
	if !e.IsAllDay {
		prefix = "*" + timeRange(e, day, loc) + "*"
	}
	return block{
		Type:      "section",
		Text:      &textObject{Type: "mrkdwn", Text: prefix + "  " + linkedTitle(e) + " " + accountsLabel(e.AccountIDs)},
		Accessory: joinButton(e.MeetingURL),
	}
}

// digestMessage はダイジェストの (blocks, attachments) を組み立てる(v2.1 スペック 5 章)。
// トップレベル blocks は header + 必要時の context(他 N 件・取得失敗)のみ。
// 予定ごとに 1 attachment(色付き左バー)にする。colorFor はアカウント ID → 16 進色。
func digestMessage(day time.Time, entries []engine.DigestEntry, failedAccounts []string, loc *time.Location, colorFor func(string) string) ([]block, []attachment) {
	d := day.In(loc)
	blocks := []block{{
		Type: "header",
		Text: &textObject{Type: "plain_text", Text: fmt.Sprintf("%d/%d(%s) の予定", int(d.Month()), d.Day(), jaWeekdays[d.Weekday()])},
	}}
	if len(entries) == 0 {
		blocks = append(blocks, block{Type: "section", Text: &textObject{Type: "mrkdwn", Text: "今日の予定はありません"}})
	}
	shown := entries
	if len(shown) > maxDigestAttachments {
		shown = shown[:maxDigestAttachments]
	}
	attachments := make([]attachment, 0, len(shown))
	for _, e := range shown {
		attachments = append(attachments, attachment{
			Color:  colorFor(colorAccountOf(e)),
			Blocks: []block{eventSection(e, d, loc)},
		})
	}
	if n := len(entries) - len(shown); n > 0 {
		blocks = append(blocks, block{Type: "context", Elements: []textObject{{Type: "mrkdwn", Text: fmt.Sprintf("…他 %d 件", n)}}})
	}
	if len(failedAccounts) > 0 {
		parts := make([]string, 0, len(failedAccounts))
		for _, id := range failedAccounts {
			parts = append(parts, "⚠ "+escapeText(id)+": 取得失敗")
		}
		blocks = append(blocks, block{Type: "context", Elements: []textObject{{Type: "mrkdwn", Text: strings.Join(parts, " / ")}}})
	}
	return blocks, attachments
}

// reminderMessage はリマインドの (blocks, attachments) を組み立てる(v2.1 スペック 6 章)。
// トップレベル blocks は使わず、単一 attachment(色は由来アカウント色)に
// line section(+参加ボタン)と description section(非空時のみ)を入れる。
func reminderMessage(e engine.DigestEntry, lead time.Duration, loc *time.Location, colorFor func(string) string) ([]block, []attachment) {
	mins := int(lead.Round(time.Minute) / time.Minute)
	prefix := "まもなく"
	if mins >= 1 {
		prefix = fmt.Sprintf("%d分後", mins)
	}
	day := e.StartUTC.In(loc)
	line := fmt.Sprintf("⏰ *%s* %s %s %s", prefix, timeRange(e, day, loc), linkedTitle(e), accountsLabel(e.AccountIDs))
	sections := []block{{
		Type:      "section",
		Text:      &textObject{Type: "mrkdwn", Text: line},
		Accessory: joinButton(e.MeetingURL),
	}}
	// 非空 = TrimSpace 後に長さ > 0。表示にも trim 後を使う(v2 スペック 6 章)
	if desc := strings.TrimSpace(e.Description); desc != "" {
		sections = append(sections, block{
			Type: "section",
			Text: &textObject{Type: "mrkdwn", Text: truncateEscaped(escapeText(desc), maxDescRunes, "…(省略)")},
		})
	}
	return nil, []attachment{{Color: colorFor(colorAccountOf(e)), Blocks: sections}}
}
