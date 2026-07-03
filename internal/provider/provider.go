package provider

import (
	"context"
	"errors"

	"github.com/work-a-co/calsync/internal/model"
)

var (
	ErrCursorInvalid = errors.New("sync cursor invalidated") // Google 410 / Graph 410・syncStateNotFound系
	ErrAuthExpired   = errors.New("authentication expired")  // invalid_grant / interaction_required 系
	ErrNotFound      = errors.New("event not found")
)

type Provider interface {
	// cursor=="" ならウィンドウ付きフル同期。newCursor は完走時のみ非空。
	// ウィンドウ外イベントも返りうる(エンジン側でフィルタ)。
	Changes(ctx context.Context, cal model.CalendarRef, cursor string, window model.Window) (events []model.NormalizedEvent, newCursor string, err error)

	// idemKey: Google はクライアント生成イベントID / Graph は transactionId。
	// 既に同キーで作成済みの場合もエラーにせず既存IDを返すこと。
	CreateBlocker(ctx context.Context, cal model.CalendarRef, b model.Blocker, idemKey string) (eventID string, err error)
	UpdateBlocker(ctx context.Context, cal model.CalendarRef, eventID string, b model.Blocker) error
	DeleteBlocker(ctx context.Context, cal model.CalendarRef, eventID string) error // 404 は成功扱い

	// calsync タグ付きイベントの列挙(リコンサイル・再構築用)
	ListBlockers(ctx context.Context, cal model.CalendarRef, window model.Window) ([]model.BlockerRecord, error)

	// 終日ブロッカー用のタイムゾーン取得(Google: IANA / Graph: mailboxSettings の値をそのまま)
	GetCalendarTimezone(ctx context.Context, cal model.CalendarRef) (string, error)
}
