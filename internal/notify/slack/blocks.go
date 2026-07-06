package slack

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/btajp/calsync/internal/engine"
)

// Block Kit の最小型(chat.postMessage の blocks ペイロード用。v2 スペック 8 章)。
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

const (
	// maxDigestBlockEvents: Slack の 50 ブロック上限に対し
	// header 1 + sections 46 + context 他N件 1 + context 取得失敗 1 = 49。
	// 50 ちょうどを避ける意図的な 1 ブロックの安全マージン(v2 スペック 5 章)
	maxDigestBlockEvents = 46
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

func digestBlocks(day time.Time, entries []engine.DigestEntry, failedAccounts []string, loc *time.Location) []block {
	d := day.In(loc)
	blocks := []block{{
		Type: "header",
		Text: &textObject{Type: "plain_text", Text: fmt.Sprintf("%d/%d(%s) の予定", int(d.Month()), d.Day(), jaWeekdays[d.Weekday()])},
	}}
	if len(entries) == 0 {
		blocks = append(blocks, block{Type: "section", Text: &textObject{Type: "mrkdwn", Text: "今日の予定はありません"}})
	}
	shown := entries
	if len(shown) > maxDigestBlockEvents {
		shown = shown[:maxDigestBlockEvents]
	}
	for _, e := range shown {
		prefix := "*(終日)*"
		if !e.IsAllDay {
			prefix = "*" + timeRange(e, d, loc) + "*"
		}
		blocks = append(blocks, block{
			Type:      "section",
			Text:      &textObject{Type: "mrkdwn", Text: prefix + "  " + linkedTitle(e) + " " + accountsLabel(e.AccountIDs)},
			Accessory: joinButton(e.MeetingURL),
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
	return blocks
}

func reminderBlocks(e engine.DigestEntry, lead time.Duration, loc *time.Location) []block {
	mins := int(lead.Round(time.Minute) / time.Minute)
	prefix := "まもなく"
	if mins >= 1 {
		prefix = fmt.Sprintf("%d分後", mins)
	}
	day := e.StartUTC.In(loc)
	line := fmt.Sprintf("⏰ *%s* %s %s %s", prefix, timeRange(e, day, loc), linkedTitle(e), accountsLabel(e.AccountIDs))
	blocks := []block{{
		Type:      "section",
		Text:      &textObject{Type: "mrkdwn", Text: line},
		Accessory: joinButton(e.MeetingURL),
	}}
	// 非空 = TrimSpace 後に長さ > 0。表示にも trim 後を使う(v2 スペック 6 章)
	if desc := strings.TrimSpace(e.Description); desc != "" {
		blocks = append(blocks, block{
			Type: "section",
			Text: &textObject{Type: "mrkdwn", Text: truncateEscaped(escapeText(desc), maxDescRunes, "…(省略)")},
		})
	}
	return blocks
}
