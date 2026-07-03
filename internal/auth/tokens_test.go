package auth

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestTokenStoreSaveLoadRoundTrip(t *testing.T) {
	expiry := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		accountID string
		tok       *oauth2.Token
	}{
		{
			name:      "full token",
			accountID: "work-ms",
			tok: &oauth2.Token{
				AccessToken:  "at-1",
				TokenType:    "Bearer",
				RefreshToken: "rt-1",
				Expiry:       expiry,
			},
		},
		{
			name:      "no refresh token",
			accountID: "personal",
			tok: &oauth2.Token{
				AccessToken: "at-2",
				TokenType:   "Bearer",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := &TokenStore{Dir: t.TempDir()}
			require.NoError(t, ts.Save(tc.accountID, tc.tok))

			// ファイルは <Dir>/tokens/<accountID>.json、権限 0600(仕様書7章)
			path := filepath.Join(ts.Dir, "tokens", tc.accountID+".json")
			info, err := os.Stat(path)
			require.NoError(t, err)
			if runtime.GOOS != "windows" {
				require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}

			got, err := ts.Load(tc.accountID)
			require.NoError(t, err)
			require.Equal(t, tc.tok.AccessToken, got.AccessToken)
			require.Equal(t, tc.tok.TokenType, got.TokenType)
			require.Equal(t, tc.tok.RefreshToken, got.RefreshToken)
			require.True(t, tc.tok.Expiry.Equal(got.Expiry),
				"expiry round-trip: want %v got %v", tc.tok.Expiry, got.Expiry)
		})
	}
}

func TestTokenStoreLoadMissing(t *testing.T) {
	ts := &TokenStore{Dir: t.TempDir()}
	tok, err := ts.Load("nope")
	require.Error(t, err)
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Nil(t, tok)
}

func TestTokenStoreDelete(t *testing.T) {
	ts := &TokenStore{Dir: t.TempDir()}
	require.NoError(t, ts.Save("acc", &oauth2.Token{AccessToken: "at"}))

	require.NoError(t, ts.Delete("acc"))
	_, err := ts.Load("acc")
	require.Error(t, err)

	// Delete は冪等: 既に無くてもエラーにしない(accounts remove の再実行安全性)
	require.NoError(t, ts.Delete("acc"))
}

func TestTokenStoreRejectsInvalidAccountID(t *testing.T) {
	ts := &TokenStore{Dir: t.TempDir()}
	tests := []struct {
		name string
		id   string
	}{
		{name: "empty", id: ""},
		{name: "dot", id: "."},
		{name: "dotdot", id: ".."},
		{name: "slash", id: "a/b"},
		{name: "backslash", id: `a\b`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, ts.Save(tc.id, &oauth2.Token{AccessToken: "x"}))
			_, err := ts.Load(tc.id)
			require.Error(t, err)
			require.Error(t, ts.Delete(tc.id))
		})
	}
}

// fakeTokenSource はキューに積んだトークンを順に返す(最後の1件は繰り返す)。
type fakeTokenSource struct {
	mu    sync.Mutex
	toks  []*oauth2.Token
	err   error // 非 nil なら常にこのエラーを返す
	calls int
}

func (f *fakeTokenSource) Token() (*oauth2.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.toks) == 0 {
		return nil, errors.New("fakeTokenSource: no tokens queued")
	}
	tok := f.toks[0]
	if len(f.toks) > 1 {
		f.toks = f.toks[1:]
	}
	return tok, nil
}

func TestPersistingTokenSource(t *testing.T) {
	dir := t.TempDir()
	store := &TokenStore{Dir: dir}
	t1 := &oauth2.Token{AccessToken: "at-1", TokenType: "Bearer", RefreshToken: "rt-1"}
	t2 := &oauth2.Token{AccessToken: "at-2", TokenType: "Bearer", RefreshToken: "rt-2"}

	// 事前に t1 を保存(auth add 済みの状態)。コンストラクタがこれを読み込む
	require.NoError(t, store.Save("acc", t1))
	base := &fakeTokenSource{toks: []*oauth2.Token{t1, t1, t2}}
	src := PersistingTokenSource("acc", store, base)

	// Save が呼ばれたかを「ファイルの再出現」で観測するため、いったん消す
	tokenPath := filepath.Join(dir, "tokens", "acc.json")
	require.NoError(t, os.Remove(tokenPath))

	// 1回目: base は保存済みと同一のトークンを返す → Save されない
	got, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, "at-1", got.AccessToken)
	_, statErr := os.Stat(tokenPath)
	require.ErrorIs(t, statErr, fs.ErrNotExist, "identical token must not be re-saved")

	// 2回目: まだ同一 → Save されない
	_, err = src.Token()
	require.NoError(t, err)
	_, statErr = os.Stat(tokenPath)
	require.ErrorIs(t, statErr, fs.ErrNotExist, "identical token must not be re-saved")

	// 3回目: RefreshToken がローテーション → Save される(仕様書9.3)
	got, err = src.Token()
	require.NoError(t, err)
	require.Equal(t, "rt-2", got.RefreshToken)
	saved, err := store.Load("acc")
	require.NoError(t, err)
	require.Equal(t, "at-2", saved.AccessToken)
	require.Equal(t, "rt-2", saved.RefreshToken)
	require.Equal(t, 3, base.calls)
}

func TestPersistingTokenSourceSavesFirstTokenWhenNoneStored(t *testing.T) {
	store := &TokenStore{Dir: t.TempDir()}
	t1 := &oauth2.Token{AccessToken: "at-1", RefreshToken: "rt-1"}
	src := PersistingTokenSource("acc", store, &fakeTokenSource{toks: []*oauth2.Token{t1}})

	_, err := src.Token()
	require.NoError(t, err)

	saved, err := store.Load("acc")
	require.NoError(t, err)
	require.Equal(t, "rt-1", saved.RefreshToken)
}

func TestPersistingTokenSourcePropagatesBaseError(t *testing.T) {
	store := &TokenStore{Dir: t.TempDir()}
	baseErr := errors.New("refresh failed")
	src := PersistingTokenSource("acc", store, &fakeTokenSource{err: baseErr})

	tok, err := src.Token()
	require.ErrorIs(t, err, baseErr)
	require.Nil(t, tok)
}
