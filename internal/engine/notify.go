package engine

import (
	"context"
	"time"
)

// DigestEntry は通知 1 件分の構造化データ。文言の組み立て(フォーマット・
// エスケープ)は Notifier 実装(internal/notify/slack)の責務で、エンジンは
// データだけを渡す(スペック 7 章)。
type DigestEntry struct {
	Title       string
	StartUTC    time.Time
	EndUTC      time.Time
	IsAllDay    bool
	AllDayStart string
	AccountIDs  []string // dedupe 統合後の由来アカウント(YAML の id)
}

// Notifier は通知送信のインターフェース。Engine.Notifier が nil なら通知機能は
// 完全に無効(スペック 9 章)。エラーは notify.ErrNonRetryable への包み込みで
// リトライ可否を表現すること。
type Notifier interface {
	SendDigest(ctx context.Context, day time.Time, entries []DigestEntry, failedAccounts []string) error
	// lead は送信時点の実残り時間(スペック 7 章)
	SendReminder(ctx context.Context, e DigestEntry, lead time.Duration) error
}
