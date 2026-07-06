package google

import (
	"html"
	"regexp"
	"strings"
)

var (
	brTagRe  = regexp.MustCompile(`(?i)<br\s*/?>|</p>`)
	anyTagRe = regexp.MustCompile(`<[^>]*>`)
)

// stripHTML は Google の description(HTML 断片を含みうる)を表示用プレーンテキストへ
// 近似変換する(v2 スペック 3.4)。依存追加なしの簡易変換であり完全な HTML パースは
// しない: (1) <br>/</p> を改行に (2) 残タグを除去 (3) std html で実体参照を復元
// (4) &nbsp; 由来の U+00A0 を通常の半角スペースへ寄せる (5) タグ除去で生じた
// 先頭・末尾の空白/改行を trim する(例: 末尾の </p> が余分な改行を残すため)。
// 表示専用の正規化であり同期ロジック(TimeHash 等)には影響しない。
func stripHTML(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return s
	}
	s = brTagRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, " ", " ")
	return strings.TrimSpace(s)
}
