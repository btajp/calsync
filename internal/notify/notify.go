// Package notify は通知送信のエラー分類(sentinel)を提供する。
// 送信実装の方言(Slack API の ok:false + error 文字列等)は各サブパッケージに
// 閉じ込め、エンジンは errors.Is(err, ErrNonRetryable) だけで再試行可否を判断する
// (provider の autherr と同じ「方言を漏らさない」方針。スペック 8 章)。
package notify

import "errors"

// ErrNonRetryable は再試行しても回復しない送信エラー(設定起因等)。
// これにマッチしないエラーはリトライ可能(ネットワーク・5xx・429)として扱う。
var ErrNonRetryable = errors.New("non-retryable notification error")
