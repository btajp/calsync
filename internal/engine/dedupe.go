package engine

import (
	"context"

	"github.com/work-a-co/calsync/internal/config"
	"github.com/work-a-co/calsync/internal/model"
)

// isDuplicateOnTarget は「ターゲットに同一 iCalUID・同一開始時刻の busy 実予定が
// あるか」を判定する(仕様6.5)。Task 9 で実装する。それまでは常に false
// (= 重複抑止しない)を返すスタブ。
func (e *Engine) isDuplicateOnTarget(target config.Account, ev model.NormalizedEvent) (bool, error) {
	return false, nil
}

// promoteSuppressed はターゲットの実予定が消えたとき、suppressed な mapping を
// ブロッカー作成に昇格する(仕様6.5)。Task 9 で実装する。それまでは no-op スタブ。
func (e *Engine) promoteSuppressed(ctx context.Context, targetAccountID, icalUID string) error {
	return nil
}
