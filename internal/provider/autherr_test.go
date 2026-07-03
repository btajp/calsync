package provider

import (
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestNormalizeAuthErr(t *testing.T) {
	t.Run("invalid_grant は *url.Error にネストしていても ErrAuthExpired になる", func(t *testing.T) {
		rerr := &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: "Token has been expired or revoked"}
		wrapped := &url.Error{Op: "Post", URL: "https://oauth2.googleapis.com/token", Err: rerr}

		got := NormalizeAuthErr(wrapped)

		require.ErrorIs(t, got, ErrAuthExpired)
		// 元エラーは %w で保持されるため、RetrieveError までたどり着ける。
		var got2 *oauth2.RetrieveError
		require.ErrorAs(t, got, &got2)
		require.Equal(t, "invalid_grant", got2.ErrorCode)
	})

	t.Run("interaction_required も ErrAuthExpired になる", func(t *testing.T) {
		rerr := &oauth2.RetrieveError{ErrorCode: "interaction_required"}
		wrapped := &url.Error{Op: "Post", URL: "https://login.microsoftonline.com/token", Err: rerr}

		got := NormalizeAuthErr(wrapped)

		require.ErrorIs(t, got, ErrAuthExpired)
	})

	t.Run("無関係なエラーはそのまま返す", func(t *testing.T) {
		orig := errors.New("boom")

		got := NormalizeAuthErr(orig)

		require.Same(t, orig, got)
		require.False(t, errors.Is(got, ErrAuthExpired))
	})

	t.Run("invalid_grant/interaction_required 以外の RetrieveError はそのまま返す", func(t *testing.T) {
		rerr := &oauth2.RetrieveError{ErrorCode: "invalid_client"}

		got := NormalizeAuthErr(rerr)

		require.Same(t, error(rerr), got)
		require.False(t, errors.Is(got, ErrAuthExpired))
	})
}
