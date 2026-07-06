package google

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripHTML(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain text passes through", "普通のテキスト", "普通のテキスト"},
		{"br to newline", "1行目<br>2行目<br/>3行目", "1行目\n2行目\n3行目"},
		{"closing p to newline", "<p>段落1</p><p>段落2</p>", "段落1\n段落2"},
		{"anchor tag removed keeping text", `<a href="https://zoom.us/j/1">参加</a>`, "参加"},
		{"entities unescaped", "A &amp; B &lt;C&gt;", "A & B <C>"},
		{"mixed", `会議<br><a href="https://x">リンク</a>&nbsp;end`, "会議\nリンク end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, stripHTML(tt.in))
		})
	}
}
