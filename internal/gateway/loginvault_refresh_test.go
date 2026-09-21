package gateway

import (
	"context"
	"os"
	"path/filepath"
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

// TestLiveTokenRefreshesExpiredOnce proves an expired login with a refresh grant
// is exchanged exactly once and the rotated token persists durably: a freshly
// opened vault reads the new access and refresh grant, and a second read is
// served from the fresh token without another exchange.
func TestLiveTokenRefreshesExpiredOnce(t *testing.T) {
	dir := t.TempDir()
	vault, err := OpenLoginVault(dir)
	require.NoError(t, err)
	_, err = vault.commitLogin("hyper", &oauth.Token{
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}, "acme")
	require.NoError(t, err)

	var calls int
	vault.refresh = func(_ context.Context, provider string, tok *oauth.Token) (*oauth.Token, error) {
		require.Equal(t, "hyper", provider)
		require.Equal(t, "refresh-old", tok.RefreshToken, "the exchange must use the stored refresh grant")
		calls++
		// A rotating provider returns a new access AND a new refresh grant,
		// valid an hour out so the next read does not refresh again.
		return &oauth.Token{
			AccessToken: "access-new", RefreshToken: "refresh-new", ExpiresIn: 3600,
		}, nil
	}

	got, err := vault.liveAccessToken(context.Background(), "hyper")
	require.NoError(t, err)
	require.Equal(t, "access-new", got)
	require.Equal(t, 1, calls, "an expired token must be exchanged exactly once")

	// The now-fresh token is served without another exchange.
	got, err = vault.liveAccessToken(context.Background(), "hyper")
	require.NoError(t, err)
	require.Equal(t, "access-new", got)
	require.Equal(t, 1, calls, "a still-fresh token must not be exchanged again")

	// The rotation is durable: a freshly opened vault reads the new grant.
	reopened, err := OpenLoginVault(dir)
	require.NoError(t, err)
	stored, ok := reopened.Get("hyper")
	require.True(t, ok)
	require.Equal(t, "access-new", stored.Token.AccessToken)
	require.Equal(t, "refresh-new", stored.Token.RefreshToken, "the rotated refresh token must persist to disk")
}

// TestLiveTokenSkipsExchangeWhenSiblingRefreshed proves the fix for the
// single-use rotating refresh token: when a sibling process has already
// refreshed the login (a fresh token is authoritative on disk), this process
// adopts it and does NOT run its own exchange, even though its in-memory copy is
// still the stale expired one. Rotating the same grant twice would invalidate
// the sibling's token and break the chain until the user signs in again.
func TestLiveTokenSkipsExchangeWhenSiblingRefreshed(t *testing.T) {
	dir := t.TempDir()
	daemonA, err := OpenLoginVault(dir)
	require.NoError(t, err)
	_, err = daemonA.commitLogin("hyper", &oauth.Token{
		AccessToken: "access-stale", RefreshToken: "refresh-stale",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}, "acme")
	require.NoError(t, err)

	// A sibling process refreshes the login and persists a fresh token to the
	// shared file. daemonA's in-memory copy stays the stale expired one.
	daemonB, err := OpenLoginVault(dir)
	require.NoError(t, err)
	_, err = daemonB.commitLogin("hyper", &oauth.Token{
		AccessToken: "access-sibling", RefreshToken: "refresh-sibling",
		ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, "acme")
	require.NoError(t, err)

	// daemonA must never run its own exchange: doing so would rotate the same
	// single-use grant a second time and revoke the sibling's fresh token.
	var calls int
	daemonA.refresh = func(context.Context, string, *oauth.Token) (*oauth.Token, error) {
		calls++
		return &oauth.Token{AccessToken: "access-double-rotated"}, nil
	}

	got, err := daemonA.liveAccessToken(context.Background(), "hyper")
	require.NoError(t, err)
	require.Equal(t, "access-sibling", got, "the sibling's fresh token must be adopted")
	require.Equal(t, 0, calls, "a sibling's fresh token must skip the exchange, never double-rotating the grant")
}

// TestLiveTokenFailsClosedWithoutLock proves a refresh never runs the exchange
// when the cross-process lock cannot be taken: without the lock this process
// cannot serialize against a sibling, so exchanging would risk the concurrent
// double-rotation the lock exists to prevent. It must report an error and leave
// the grant untouched rather than exchange unlocked.
func TestLiveTokenFailsClosedWithoutLock(t *testing.T) {
	dir := t.TempDir()
	vault, err := OpenLoginVault(dir)
	require.NoError(t, err)
	_, err = vault.commitLogin("hyper", &oauth.Token{
		AccessToken: "access-old", RefreshToken: "refresh-old",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}, "acme")
	require.NoError(t, err)

	// Make the lock unobtainable: point the vault at a path that is a regular
	// file, so acquireFileLock's MkdirAll fails and no exclusive lock can form.
	notADir := filepath.Join(dir, "not-a-dir")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o600))
	vault.dir = notADir

	var calls int
	vault.refresh = func(context.Context, string, *oauth.Token) (*oauth.Token, error) {
		calls++
		return &oauth.Token{AccessToken: "access-new"}, nil
	}

	_, err = vault.liveAccessToken(context.Background(), "hyper")
	require.Error(t, err, "a refresh must fail closed when the cross-process lock is unavailable")
	require.Equal(t, 0, calls, "the exchange must never run without the cross-process lock")
}
