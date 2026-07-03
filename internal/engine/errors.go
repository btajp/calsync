package engine

import (
	"errors"
	"fmt"

	"github.com/work-a-co/calsync/internal/provider"
)

// TargetAuthError は「ターゲットアカウントへのブロッカー書き込みが認証失効で
// 失敗した」ことを表す型付きエラー(仕様9.3)。origin アカウントの同期中に
// ターゲット側の provider.ErrAuthExpired をそのまま返すと、scheduler が
// errors.Is(err, ErrAuthExpired) で origin に誤帰属して origin の同期まで
// 止めてしまうため、失効したアカウント ID を保持してラップする。
type TargetAuthError struct {
	AccountID string // 失効したターゲットアカウントの ID
	Err       error  // 元の provider.ErrAuthExpired チェーン
}

func (e *TargetAuthError) Error() string {
	return fmt.Sprintf("target account %s: authentication expired: %v", e.AccountID, e.Err)
}

func (e *TargetAuthError) Unwrap() error { return e.Err }

// wrapTargetAuth は err が provider.ErrAuthExpired を含む場合に TargetAuthError
// へ包む(既に TargetAuthError ならそのまま)。それ以外の err は素通しする。
func wrapTargetAuth(targetAccountID string, err error) error {
	if err == nil {
		return nil
	}
	var tae *TargetAuthError
	if errors.As(err, &tae) {
		return err // 二重ラップしない(createFromMapping 経由等)
	}
	if errors.Is(err, provider.ErrAuthExpired) {
		return &TargetAuthError{AccountID: targetAccountID, Err: err}
	}
	return err
}

// collectTargetAuthErrors は err のエラーツリー(errors.Join / fmt.Errorf の
// %w 連鎖)を走査し、含まれる全 *TargetAuthError を返す。TargetAuthError の
// 配下(ラップされた ErrAuthExpired)へは降りない。
func collectTargetAuthErrors(err error) []*TargetAuthError {
	var out []*TargetAuthError
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if tae, ok := e.(*TargetAuthError); ok {
			out = append(out, tae)
			return
		}
		switch x := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range x.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(x.Unwrap())
		}
	}
	walk(err)
	return out
}

// originAuthExpired は err のエラーツリーに「TargetAuthError の配下ではない」
// provider.ErrAuthExpired が含まれるか(= origin アカウント自身の認証失効か)を
// 返す。TargetAuthError のサブツリーには降りないため、ターゲット失効由来の
// ErrAuthExpired は origin に誤帰属しない(仕様9.3)。
func originAuthExpired(err error) bool {
	var walk func(error) bool
	walk = func(e error) bool {
		if e == nil {
			return false
		}
		if _, ok := e.(*TargetAuthError); ok {
			return false // ターゲット失効の配下は origin の失効ではない
		}
		if e == provider.ErrAuthExpired {
			return true
		}
		switch x := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range x.Unwrap() {
				if walk(c) {
					return true
				}
			}
		case interface{ Unwrap() error }:
			return walk(x.Unwrap())
		}
		return false
	}
	return walk(err)
}

// onlyTargetAuthErrors は err が TargetAuthError のみで構成されるか
// (= origin 自身の同期は成功しており、ターゲットの失効だけが原因か)を返す。
// err==nil は false(「エラーがあり、かつ全部ターゲット失効」の判定専用)。
func onlyTargetAuthErrors(err error) bool {
	if err == nil {
		return false
	}
	var all func(error) bool
	all = func(e error) bool {
		if e == nil {
			return false
		}
		if _, ok := e.(*TargetAuthError); ok {
			return true
		}
		switch x := e.(type) {
		case interface{ Unwrap() []error }:
			children := x.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, c := range children {
				if !all(c) {
					return false
				}
			}
			return true
		case interface{ Unwrap() error }:
			return all(x.Unwrap())
		}
		return false
	}
	return all(err)
}
