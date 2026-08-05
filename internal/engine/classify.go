package engine

import (
	"time"

	"github.com/btajp/calsync/internal/model"
)

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

// EndsBeforeWindow は「ウィンドウ開始より前に終わった予定」(= 過去側に抜けた予定)の
// 判定(2026-08-05 仕様変更: 終わった予定のブロッカーは掃除せず保持する)。
// Window.Contains と同じ近似を使う: 終日は AllDayEnd(排他的終了日)を UTC 日付として
// 比較し、パース不能なら false(= 過去扱いしない → 従来どおり掃除側に倒す)。
func EndsBeforeWindow(w model.Window, ev model.NormalizedEvent) bool {
	if ev.IsAllDay {
		end, err := time.Parse("2006-01-02", ev.AllDayEnd)
		if err != nil {
			return false
		}
		return !end.After(w.Start)
	}
	if ev.EndUTC.IsZero() {
		return false
	}
	return !ev.EndUTC.After(w.Start)
}
