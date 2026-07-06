package model

import (
	"regexp"
	"strings"
)

// meetingURLRe は location / description から会議 URL を拾うフォールバック正規表現
// (v2 スペック 3.2)。https:// 固定(スキーム注入を構造的に排除)。zoom はサブドメイン
// 省略可。終端は空白・" < > | まで(mrkdwn 構文文字を URL に混入させない)。
var meetingURLRe = regexp.MustCompile(
	`https://(?:[a-z0-9-]+\.)?zoom\.us/(?:j|my)/[^\s"<>|]+` +
		`|https://meet\.google\.com/[^\s"<>|]+` +
		`|https://teams\.microsoft\.com/l/meetup-join/[^\s"<>|]+`)

// ExtractMeetingURL は location → description の順に会議 URL を探す。
// 各フィールド内では出現位置が最も先頭の一致を採用(leftmost match。パターン種別間に
// 優先順位は付けない)。切り出し後、末尾の ) ] . , ; を除去する(括弧囲い・文末句読点の
// 巻き込み防止。v2 スペック 3.2)。
func ExtractMeetingURL(location, description string) string {
	for _, s := range []string{location, description} {
		if m := meetingURLRe.FindString(s); m != "" {
			return strings.TrimRight(m, ")].,;")
		}
	}
	return ""
}
