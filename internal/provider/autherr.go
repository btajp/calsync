package provider

import (
	"errors"
	"fmt"

	"golang.org/x/oauth2"
)

// NormalizeAuthErr は OAuth2 のトークン更新失敗のうち、リフレッシュトークンの
// 失効・再同意要求を示すもの(invalid_grant / interaction_required)を
// ErrAuthExpired にラップして返す(仕様書9.3)。該当しない場合は err をそのまま返す。
//
// TokenSource の失敗は http.Client(oauth2.Transport 経由)によって *url.Error に
// 包まれて届くため、型スイッチではなく errors.As でエラーチェーンを掘り下げて
// *oauth2.RetrieveError を探す。google/microsoft 双方のプロバイダから共用する。
func NormalizeAuthErr(err error) error {
	var rerr *oauth2.RetrieveError
	if errors.As(err, &rerr) {
		switch rerr.ErrorCode {
		case "invalid_grant", "interaction_required":
			return fmt.Errorf("%w: %w", ErrAuthExpired, err)
		}
	}
	return err
}
