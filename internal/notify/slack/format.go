package slack

import (
	"fmt"
	"strings"
	"time"

	"github.com/btajp/calsync/internal/engine"
)

// maxDigestEntries は 1 メッセージに載せるエントリ上限(Slack メッセージ長対策。スペック 5 章)。
const maxDigestEntries = 100

var jaWeekdays = [...]string{"日", "月", "火", "水", "木", "金", "土"}

// escapeText は Slack text の必須エスケープ(& < > のみ。スペック 8 章)。
// 件名は外部入力(会議招待)由来で、素通しすると <!channel> 等の特殊メンション
// 構文が発火する(メンションインジェクション)。
func escapeText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// titleOf は表示用件名を返す。空(無題または title 未反映の移行期間)は
// 「(件名なし)」で両ケースを区別しない(スペック 7 章)。
func titleOf(e engine.DigestEntry) string {
	if e.Title == "" {
		return "(件名なし)"
	}
	return escapeText(e.Title)
}

func accountsLabel(ids []string) string {
	return escapeText("[" + strings.Join(ids, ", ") + "]")
}

// timeRange は開始–終了を表示する。当日ウィンドウ外にはみ出す側には日付を付ける
// (例: "7/4 23:00–7/6 01:00"。スペック 7 章)。
func timeRange(e engine.DigestEntry, day time.Time, loc *time.Location) string {
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)
	s, en := e.StartUTC.In(loc), e.EndUTC.In(loc)
	start := s.Format("15:04")
	if s.Before(dayStart) {
		start = fmt.Sprintf("%d/%d %s", int(s.Month()), s.Day(), start)
	}
	end := en.Format("15:04")
	if en.After(dayEnd) {
		end = fmt.Sprintf("%d/%d %s", int(en.Month()), en.Day(), end)
	}
	return start + "–" + end
}

func formatDigest(day time.Time, entries []engine.DigestEntry, failedAccounts []string, loc *time.Location) string {
	var b strings.Builder
	d := day.In(loc)
	fmt.Fprintf(&b, "📅 %d/%d(%s) の予定\n", int(d.Month()), d.Day(), jaWeekdays[d.Weekday()])
	if len(entries) == 0 {
		// 0 件の日も送る(デーモンの生存確認を兼ねる。スペック 5 章)
		b.WriteString("今日の予定はありません\n")
	}
	shown := entries
	if len(shown) > maxDigestEntries {
		shown = shown[:maxDigestEntries]
	}
	for _, e := range shown {
		if e.IsAllDay {
			fmt.Fprintf(&b, "・(終日) %s %s\n", titleOf(e), accountsLabel(e.AccountIDs))
			continue
		}
		fmt.Fprintf(&b, "・%s %s %s\n", timeRange(e, d, loc), titleOf(e), accountsLabel(e.AccountIDs))
	}
	if n := len(entries) - len(shown); n > 0 {
		fmt.Fprintf(&b, "…他 %d 件\n", n)
	}
	for _, id := range failedAccounts {
		// 取得に失敗したアカウントは黙って欠落させない(スペック 5 章)
		fmt.Fprintf(&b, "⚠ %s: 取得失敗\n", escapeText(id))
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatReminder の「N 分後」は送信時点の実残り時間(分丸め)。1 分未満は「まもなく」
// (遅延通知・再起動復帰時に設定値と実態がずれるため設定値は表示しない。スペック 7 章)。
func formatReminder(e engine.DigestEntry, lead time.Duration, loc *time.Location) string {
	mins := int(lead.Round(time.Minute) / time.Minute)
	prefix := "まもなく"
	if mins >= 1 {
		prefix = fmt.Sprintf("%d分後", mins)
	}
	day := e.StartUTC.In(loc)
	return fmt.Sprintf("⏰ %s: %s %s %s", prefix, timeRange(e, day, loc), titleOf(e), accountsLabel(e.AccountIDs))
}
