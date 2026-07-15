package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractMeetingURL(t *testing.T) {
	tests := []struct {
		name, location, description, want string
	}{
		{"zoom with subdomain in location", "https://example-corp.zoom.us/j/89335149431?pwd=abc", "", "https://example-corp.zoom.us/j/89335149431?pwd=abc"},
		{"zoom without subdomain", "", "join: https://zoom.us/j/123456789", "https://zoom.us/j/123456789"},
		{"zoom my-path", "", "https://zoom.us/my/example", "https://zoom.us/my/example"},
		{"meet", "", "https://meet.google.com/abc-defg-hij", "https://meet.google.com/abc-defg-hij"},
		{"teams", "", "https://teams.microsoft.com/l/meetup-join/19%3ameeting_x", "https://teams.microsoft.com/l/meetup-join/19%3ameeting_x"},
		{"location wins over description", "https://meet.google.com/loc-loc-loc", "https://zoom.us/j/999", "https://meet.google.com/loc-loc-loc"},
		{"leftmost match within a field (meet before zoom)", "", "先: https://meet.google.com/aaa-bbbb-ccc 後: https://zoom.us/j/1", "https://meet.google.com/aaa-bbbb-ccc"},
		{"parenthesized url drops trailing paren", "", "(https://meet.google.com/abc-defg-hij)", "https://meet.google.com/abc-defg-hij"},
		{"trailing period dropped", "", "https://zoom.us/j/123456789.", "https://zoom.us/j/123456789"},
		{"pipe terminates url", "", "https://meet.google.com/abc|x", "https://meet.google.com/abc"},
		{"http is ignored", "", "http://zoom.us/j/123", ""},
		{"no match", "会議室A", "資料を読んでおく", ""},
		{"full-width space terminates url", "", "参加URL: https://meet.google.com/abc-defg-hij　会議室B", "https://meet.google.com/abc-defg-hij"},
		{"japanese period terminates url", "", "こちら:https://zoom.us/j/123456789。よろしく", "https://zoom.us/j/123456789"},
		{"japanese comma terminates url", "", "https://zoom.us/j/123、資料は後送", "https://zoom.us/j/123"},
		{"schemeless meet in location", "meet.google.com/abc-defg-hij", "", "https://meet.google.com/abc-defg-hij"},
		{"schemeless zoom with japanese period", "", "参加: zoom.us/j/123456789。", "https://zoom.us/j/123456789"},
		{"schemeless zoom with subdomain", "example-corp.zoom.us/j/555", "", "https://example-corp.zoom.us/j/555"},
		{"schemeless teams", "", "teams.microsoft.com/l/meetup-join/19%3ax", "https://teams.microsoft.com/l/meetup-join/19%3ax"},
		{"http is still ignored (no partial upgrade)", "", "http://zoom.us/j/123", ""},
		{"https wins over earlier schemeless in same field", "", "zoom.us/j/1 と https://meet.google.com/abc", "https://meet.google.com/abc"},
		{"schemeless in location wins over https in description", "meet.google.com/loc-loc-loc", "https://zoom.us/j/999", "https://meet.google.com/loc-loc-loc"},
		{"hostname suffix is not a boundary", "", "notzoom.us/j/123", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ExtractMeetingURL(tt.location, tt.description))
		})
	}
}
