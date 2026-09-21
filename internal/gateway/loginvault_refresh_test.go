package gateway

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
	"github.com/stretchr/testify/require"
)

// TestLiveTokenPreservesRefreshTokenWhenExchangeOmitsRotation proves a login
// survives repeated refreshes even when the platform (Hyper) returns a fresh
// access token without rotating the refresh token: the working refresh token
// must be carried forward so the next refresh still has a grant.
func TestLiveTokenPreservesRefreshTokenWhenExchangeOmitsRotation(t *testing.T) {
	vault, err := OpenLoginVault(t.TempDir())
	require.NoError(t, err)

	// A stored Hyper login whose access token is already expired, so every
	// call drives the refresh path. It is committed to disk because refresh now
	// treats the on-disk state as authoritative.
	_, err = vault.commitLogin("hyper", &oauth.Token{
		AccessToken:  "access-0",
		RefreshToken: "refresh-keepme",
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}, "acme")
	require.NoError(t, err)

	// Hyper's /token/exchange hands back a fresh access token but omits the
	// refresh token (no rotation). Each refreshed token stays expired so the
	// following call refreshes again.
	var seen []string
	vault.refresh = func(_ context.Context, provider string, tok *oauth.Token) (*oauth.Token, error) {
		require.Equal(t, "hyper", provider)
		seen = append(seen, tok.RefreshToken)
		return &oauth.Token{AccessToken: "access-" + strconv.Itoa(len(seen))}, nil
	}

	for range 3 {
		_, err := vault.liveAccessToken(context.Background(), "hyper")
		require.NoError(t, err)
	}

	require.Equal(t, []string{"refresh-keepme", "refresh-keepme", "refresh-keepme"}, seen,
		"every refresh must reuse the preserved refresh token")

	stored, ok := vault.Get("hyper")
	require.True(t, ok)
	require.Equal(t, "refresh-keepme", stored.Token.RefreshToken,
		"the vault must keep the refresh token after a rotation-less refresh")
}
