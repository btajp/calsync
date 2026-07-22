// Package auth はトークンの永続化と OAuth フロー(ループバック+PKCE / Device Code)を提供する。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"
)

// TokenStore は <Dir>/tokens/<accountID>.json にトークンを保存する。
// Dir は SQLite と同じデータディレクトリ(Docker ボリューム。仕様書7章)。
type TokenStore struct {
	Dir string
}

func (t *TokenStore) path(accountID string) string {
	return filepath.Join(t.Dir, "tokens", accountID+".json")
}

// ValidateAccountID は設定由来の accountID によるパストラバーサルを防ぐ。
// TokenStore の内部呼び出し(Save/Load/Delete)に加え、appserver の
// POST /api/auth/start でもブラウザ往復前の事前検証として使う(仕様書 F4)。
func ValidateAccountID(accountID string) error {
	if accountID == "" || accountID == "." || accountID == ".." || strings.ContainsAny(accountID, `/\`) {
		return fmt.Errorf("auth: invalid account id %q", accountID)
	}
	return nil
}

// Save はトークンを JSON で保存する(ディレクトリ 0700・ファイル 0600)。
// 一時ファイル + rename で、クラッシュしても書きかけの JSON を残さない。
func (t *TokenStore) Save(accountID string, tok *oauth2.Token) error {
	if err := ValidateAccountID(accountID); err != nil {
		return err
	}
	if tok == nil {
		return errors.New("auth: token is nil")
	}
	dir := filepath.Join(t.Dir, "tokens")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	data, err := json.Marshal(tok)
	if err != nil {
		return fmt.Errorf("marshal token for %s: %w", accountID, err)
	}
	path := t.path(accountID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write token file for %s: %w", accountID, err)
	}
	// 一時ファイルが過去のクラッシュで別権限のまま残っていた場合にも 0600 を保証する
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("chmod token file for %s: %w", accountID, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace token file for %s: %w", accountID, err)
	}
	return nil
}

// Load は保存済みトークンを返す。ファイルが無ければエラー(fs.ErrNotExist を包含)。
func (t *TokenStore) Load(accountID string) (*oauth2.Token, error) {
	if err := ValidateAccountID(accountID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(t.path(accountID))
	if err != nil {
		return nil, fmt.Errorf("load token for %s: %w", accountID, err)
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("parse token file for %s: %w", accountID, err)
	}
	return &tok, nil
}

// Delete はトークンファイルを削除する。既に無い場合も成功扱い(冪等)。
func (t *TokenStore) Delete(accountID string) error {
	if err := ValidateAccountID(accountID); err != nil {
		return err
	}
	if err := os.Remove(t.path(accountID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete token for %s: %w", accountID, err)
	}
	return nil
}

type persistingTokenSource struct {
	accountID string
	store     *TokenStore
	base      oauth2.TokenSource

	mu   sync.Mutex
	last *oauth2.Token // 最後に永続化を確認したトークン
}

// PersistingTokenSource は base が返すトークンが前回から変化していたら
// TokenStore へ保存する TokenSource を返す。Microsoft の refresh token は
// 更新のたびにローテーションするため、毎回の永続化が必須(仕様書9.3)。
// 比較基準は保存済みトークンで初期化するため、保存済みと同一のトークンを
// 返し続ける base は一度も書き込みを発生させない。
func PersistingTokenSource(accountID string, ts *TokenStore, base oauth2.TokenSource) oauth2.TokenSource {
	p := &persistingTokenSource{accountID: accountID, store: ts, base: base}
	if tok, err := ts.Load(accountID); err == nil {
		p.last = tok
	}
	return p
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if p.last != nil && p.last.AccessToken == tok.AccessToken && p.last.RefreshToken == tok.RefreshToken {
		return tok, nil // 変化なし → 保存しない
	}
	if err := p.store.Save(p.accountID, tok); err != nil {
		return nil, fmt.Errorf("persist refreshed token for %s: %w", p.accountID, err)
	}
	p.last = tok
	return tok, nil
}
