package engine

import "github.com/btajp/calsync/internal/model"

// ShouldBlock は「busy かつ未辞退かつ削除通知でない」イベントだけを
// ブロッカー配布の対象とする(仕様6.2。未返信・仮承諾は IsBusy=true として
// 正規化されるためブロック対象になる)。
func ShouldBlock(ev model.NormalizedEvent) bool {
	return ev.IsBusy && !ev.IsDeclined && !ev.Deleted
}

// InWindow は同期ウィンドウ判定。クライアント側フィルタが正であり(仕様5.3)、
// 判定本体は model.Window.Contains(end > Start && start < End)に委譲する。
func InWindow(w model.Window, ev model.NormalizedEvent) bool {
	return w.Contains(ev)
}
