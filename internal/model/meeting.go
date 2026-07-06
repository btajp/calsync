package model

import (
	"regexp"
	"strings"
)

// meetingURLRe は location / description から会議 URL を拾うフォールバック正規表現
// (v2 スペック 3.2)。https:// 固定(スキーム注入を構造的に排除)。zoom はサブドメイン
// 省略可。URL 本体は RFC 3986 の ASCII 文字集合のみ許可する(許可列挙)。全角スペース・
// 「。」「、」等の非 ASCII はすべて終端になる(日本語本文への URL 手貼りが常態のため、
// 除外列挙(\s ベース)では ASCII 空白しか終端にならず壊れた URL を抽出してしまう)
var meetingURLRe = regexp.MustCompile(
	`https://(?:[a-z0-9-]+\.)?zoom\.us/(?:j|my)/[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]+` +
		`|https://meet\.google\.com/[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]+` +
		`|https://teams\.microsoft\.com/l/meetup-join/[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]+`)

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
