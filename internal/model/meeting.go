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

// schemelessMeetingURLRe はスキームなしの手貼り URL 用(v2.1 追補)。URL 本体の許可
// 列挙は meetingURLRe と同一。境界は「文字列先頭、または直前の 1 文字が
// [A-Za-z0-9/.-] 以外」を要求する非キャプチャ先頭グループで表現し(Go の RE2 には
// lookbehind が無いため)、本体をキャプチャグループ 1 に入れて FindStringSubmatch で
// 取り出す。この境界規則により http://zoom.us/j/1 の途中一致(直前が `/`)は昇格せず、
// notzoom.us/j/123 のような「zoom.us がホスト名の末尾に含まれるだけ」の文字列も
// (直前が英字のため)マッチしない
var schemelessMeetingURLRe = regexp.MustCompile(
	`(?:^|[^A-Za-z0-9/.-])((?:[a-z0-9-]+\.)?zoom\.us/(?:j|my)/[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]+` +
		`|meet\.google\.com/[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]+` +
		`|teams\.microsoft\.com/l/meetup-join/[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]+)`)

// ExtractMeetingURL は location → description の順に会議 URL を探す。各フィールド
// 内ではまず https:// 付きパターンを探し(出現位置が最も先頭の一致。パターン種別間に
// 優先順位は付けない)、無ければスキームなしパターン(v2.1 追補)を探して https:// を
// 補完する。フィールド優先(location → description)はこの 2 パスより上位 — つまり
// location のスキームなし一致が description の https 付き一致より優先される。
// 切り出し後、末尾の ) ] . , ; を除去する(括弧囲い・文末句読点の巻き込み防止。
// v2 スペック 3.2)。
func ExtractMeetingURL(location, description string) string {
	for _, s := range []string{location, description} {
		if m := meetingURLRe.FindString(s); m != "" {
			return strings.TrimRight(m, ")].,;")
		}
		if sm := schemelessMeetingURLRe.FindStringSubmatch(s); sm != nil {
			return strings.TrimRight("https://"+sm[1], ")].,;")
		}
	}
	return ""
}
