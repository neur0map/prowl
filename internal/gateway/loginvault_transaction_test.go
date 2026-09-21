package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
	"github.com/stretchr/testify/require"
)

// TestFailedPersistRestoresPriorLogin proves a replacement whose disk write
// fails does not corrupt the live login: the previously stored token keeps
// serving, in memory and on disk.
func TestFailedPersistRestoresPriorLogin(t *testing.T) {
	dir := t.TempDir()
	vault, err := OpenLoginVault(dir)
	require.NoError(t, err)

	_, err = vault.commitLogin("anthropic", &oauth.Token{AccessToken: "first"}, "acct-first")
	require.NoError(t, err)

	// The next replacement's persistence fails.
	boom := errors.New("disk is full")
	vault.writeState = func(string, []byte) error { return boom }

	_, err = vault.commitLogin("anthropic", &oauth.Token{AccessToken: "second"}, "acct-second")
	require.ErrorIs(t, err, boom)

	// In-memory state rolled back to the prior live login.
	live, ok := vault.Get("anthropic")
	require.True(t, ok)
	require.Equal(t, "first", live.Token.AccessToken, "a failed replace must not evict the prior live login")
	require.Equal(t, "acct-first", live.Account)

	// The file was never touched, so a fresh open sees the prior login too.
	vault.writeState = nil
	reopened, err := OpenLoginVault(dir)
	require.NoError(t, err)
	fromDisk, ok := reopened.Get("anthropic")
	require.True(t, ok)
	require.Equal(t, "first", fromDisk.Token.AccessToken)
}

// TestConcurrentDistinctProviderWritersPreserveBoth proves two daemons sharing
// one vault directory, each signing in a different provider at the same time,
// both survive: neither whole-map write clobbers the other's provider.
func TestConcurrentDistinctProviderWritersPreserveBoth(t *testing.T) {
	dir := t.TempDir()

	daemonA, err := OpenLoginVault(dir) // creates master.key
	require.NoError(t, err)
	daemonB, err := OpenLoginVault(dir) // reads the same key
	require.NoError(t, err)

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = daemonA.commitLogin("anthropic", &oauth.Token{AccessToken: "anthropic-tok"}, "acct-a")
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errs[1] = daemonB.commitLogin("openai", &oauth.Token{AccessToken: "openai-tok"}, "acct-b")
	}()
	close(start)
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	// A third daemon reading from disk must see both logins.
	fresh, err := OpenLoginVault(dir)
	require.NoError(t, err)
	anthropic, ok := fresh.Get("anthropic")
	require.True(t, ok, "the anthropic writer must not be clobbered by the openai writer")
	require.Equal(t, "anthropic-tok", anthropic.Token.AccessToken)
	openai, ok := fresh.Get("openai")
	require.True(t, ok, "the openai writer must not be clobbered by the anthropic writer")
	require.Equal(t, "openai-tok", openai.Token.AccessToken)
}

// TestSameProviderRaceHasOneTruthfulFinalState proves concurrent sign-ins for
// the same provider converge on exactly one live login, and that only the
// final writer's identity generation reports itself as current.
func TestSameProviderRaceHasOneTruthfulFinalState(t *testing.T) {
	vault, err := OpenLoginVault(t.TempDir())
	require.NoError(t, err)

	const writers = 8
	type result struct {
		seq uint64
		tok *oauth.Token
		err error
	}
	var mu sync.Mutex
	var results []result

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func(i int) {
			defer wg.Done()
			<-start
			tok := &oauth.Token{AccessToken: "tok-" + string(rune('A'+i))}
			seq, e := vault.commitLogin("anthropic", tok, "acct")
			mu.Lock()
			results = append(results, result{seq: seq, tok: tok, err: e})
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	// Serialized commits issue unique, increasing generations; the highest is
	// the winner whose token is the one truthful final state.
	require.Len(t, results, writers)
	seen := map[uint64]bool{}
	var winner result
	for _, r := range results {
		require.NoError(t, r.err)
		require.Falsef(t, seen[r.seq], "generation %d was issued twice", r.seq)
		seen[r.seq] = true
		if r.seq > winner.seq {
			winner = r
		}
	}
	live, ok := vault.Get("anthropic")
	require.True(t, ok)
	require.Equal(t, winner.tok.AccessToken, live.Token.AccessToken, "the final state must be the last writer's login")
	require.True(t, vault.isCurrentLogin("anthropic", winner.seq), "the winning generation must report itself current")
	for _, r := range results {
		if r.seq == winner.seq {
			continue
		}
		require.False(t, vault.isCurrentLogin("anthropic", r.seq), "a superseded generation must not report itself current")
	}
}

// TestSupersededSignInReportsReplacementNotStaleSuccess drives the sign-in
// completion path: a login replaced (by a competing sign-in) between commit and
// verdict reports the supersession instead of a stale success.
func TestSupersededSignInReportsReplacementNotStaleSuccess(t *testing.T) {
	vault, err := OpenLoginVault(t.TempDir())
	require.NoError(t, err)
	registry := NewSignInRegistry(vault)
	registry.startLogin = func(_ context.Context, provider string) (*PendingLogin, error) {
		return &PendingLogin{
			Provider: provider,
			URL:      "https://example.com/authorize",
			vault:    vault,
			complete: func(context.Context) (*oauth.Token, error) {
				return &oauth.Token{AccessToken: "first-token"}, nil
			},
		}, nil
	}
	// A competing sign-in for the same provider lands after this flow persisted
	// but before it reports, replacing the login it just wrote.
	registry.SetOnComplete(func(provider string) {
		_, _ = vault.commitLogin(provider, &oauth.Token{AccessToken: "replacement-token"}, "acct-replacement")
	})

	session, err := registry.Start(t.Context(), "anthropic")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		cur, e := registry.Status(session.ID)
		return e == nil && cur.State != SignInPending
	}, time.Second, time.Millisecond)

	final, err := registry.Status(session.ID)
	require.NoError(t, err)
	require.Equal(t, SignInFailed, final.State, "a superseded completion must not report success")
	require.Contains(t, final.Error, "replaced")

	// The vault's final state is the replacement, the one truthful login.
	live, ok := vault.Get("anthropic")
	require.True(t, ok)
	require.Equal(t, "replacement-token", live.Token.AccessToken)
}

// TestForgetPreservesSiblingWriteAndRollsBack proves a forget obeys the same
// transaction: it reloads a sibling daemon's unrelated login before deleting,
// and a failed persist leaves the login in place rather than half-forgotten.
func TestForgetPreservesSiblingWriteAndRollsBack(t *testing.T) {
	dir := t.TempDir()
	daemonA, err := OpenLoginVault(dir)
	require.NoError(t, err)
	daemonB, err := OpenLoginVault(dir)
	require.NoError(t, err)

	// A signs in anthropic; both daemons now know it.
	_, err = daemonA.commitLogin("anthropic", &oauth.Token{AccessToken: "anthropic-tok"}, "acct-a")
	require.NoError(t, err)
	// A also signs in openai - B never learns this in memory.
	_, err = daemonA.commitLogin("openai", &oauth.Token{AccessToken: "openai-tok"}, "acct-a")
	require.NoError(t, err)

	// B forgets anthropic. Its transaction must reload openai from disk and keep it.
	forgotten, err := daemonB.Forget("anthropic")
	require.NoError(t, err)
	require.True(t, forgotten)

	fresh, err := OpenLoginVault(dir)
	require.NoError(t, err)
	_, gone := fresh.Get("anthropic")
	require.False(t, gone, "the forgotten login must be gone")
	openai, kept := fresh.Get("openai")
	require.True(t, kept, "forget must not clobber a sibling daemon's unrelated login")
	require.Equal(t, "openai-tok", openai.Token.AccessToken)

	// A failed persist leaves the login in place.
	boom := errors.New("disk is full")
	fresh.writeState = func(string, []byte) error { return boom }
	_, err = fresh.Forget("openai")
	require.ErrorIs(t, err, boom)
	stillThere, ok := fresh.Get("openai")
	require.True(t, ok, "a failed forget must not evict the login")
	require.Equal(t, "openai-tok", stillThere.Token.AccessToken)
}

// TestSetPoolEnabledRollsBackOnPersistFailure proves the pool-enabled toggle
// obeys the same transaction: a failed persist leaves the preference untouched.
func TestSetPoolEnabledRollsBackOnPersistFailure(t *testing.T) {
	vault, err := OpenLoginVault(t.TempDir())
	require.NoError(t, err)
	_, err = vault.commitLogin("anthropic", &oauth.Token{AccessToken: "tok"}, "acct")
	require.NoError(t, err)

	// A fresh commit defaults pool_enabled to true.
	before, ok := vault.Get("anthropic")
	require.True(t, ok)
	require.NotNil(t, before.PoolEnabled)
	require.True(t, *before.PoolEnabled)

	boom := errors.New("disk is full")
	vault.writeState = func(string, []byte) error { return boom }
	require.ErrorIs(t, vault.SetPoolEnabled("anthropic", false), boom)

	after, ok := vault.Get("anthropic")
	require.True(t, ok)
	require.NotNil(t, after.PoolEnabled)
	require.True(t, *after.PoolEnabled, "a failed pool-enabled write must not flip the live preference")

	// Once persistence works, the toggle applies.
	vault.writeState = nil
	require.NoError(t, vault.SetPoolEnabled("anthropic", false))
	applied, ok := vault.Get("anthropic")
	require.True(t, ok)
	require.NotNil(t, applied.PoolEnabled)
	require.False(t, *applied.PoolEnabled)
}

// TestTransactDoesNotResurrectForgottenLogin proves disk is authoritative: a
// login another process forgot is not re-added by a stale daemon that still
// holds it in memory when that daemon next writes.
func TestTransactDoesNotResurrectForgottenLogin(t *testing.T) {
	dir := t.TempDir()
	daemonA, err := OpenLoginVault(dir)
	require.NoError(t, err)

	_, err = daemonA.commitLogin("anthropic", &oauth.Token{AccessToken: "anthropic-tok"}, "acct-a")
	require.NoError(t, err)

	// daemonB opens after the commit, so it holds anthropic in memory.
	daemonB, err := OpenLoginVault(dir)
	require.NoError(t, err)
	_, held := daemonB.Get("anthropic")
	require.True(t, held, "precondition: the stale daemon must hold the login in memory")

	// daemonA forgets it; disk no longer has anthropic.
	forgotten, err := daemonA.Forget("anthropic")
	require.NoError(t, err)
	require.True(t, forgotten)

	// daemonB's next transactional write must reload authoritative disk and must
	// NOT resurrect the forgotten login from its stale memory.
	_, err = daemonB.commitLogin("openai", &oauth.Token{AccessToken: "openai-tok"}, "acct-b")
	require.NoError(t, err)

	_, revived := daemonB.Get("anthropic")
	require.False(t, revived, "a forgotten login must not be resurrected from stale memory")

	fresh, err := OpenLoginVault(dir)
	require.NoError(t, err)
	_, onDisk := fresh.Get("anthropic")
	require.False(t, onDisk, "the forgotten login must stay gone on disk")
	openai, ok := fresh.Get("openai")
	require.True(t, ok)
	require.Equal(t, "openai-tok", openai.Token.AccessToken)
}

// TestRefreshPersistFailureRollsBackToPriorToken proves a refresh whose durable
// write fails serves the fresh token to the in-flight request but leaves the
// stored login on its prior token - memory is swapped only after a durable write.
func TestRefreshPersistFailureRollsBackToPriorToken(t *testing.T) {
	vault, err := OpenLoginVault(t.TempDir())
	require.NoError(t, err)
	_, err = vault.commitLogin("hyper", &oauth.Token{
		AccessToken: "access-old", RefreshToken: "refresh-old",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}, "acct")
	require.NoError(t, err)

	vault.refresh = func(context.Context, string, *oauth.Token) (*oauth.Token, error) {
		return &oauth.Token{AccessToken: "access-new", RefreshToken: "refresh-new"}, nil
	}
	vault.writeState = func(string, []byte) error { return errors.New("disk is full") }

	// The in-flight request still receives the freshly minted token.
	got, err := vault.liveAccessToken(context.Background(), "hyper")
	require.NoError(t, err)
	require.Equal(t, "access-new", got)

	// But the durable write failed, so the stored login stays on the prior token.
	stored, ok := vault.Get("hyper")
	require.True(t, ok)
	require.Equal(t, "access-old", stored.Token.AccessToken, "a failed refresh persist must not swap the stored token")
	require.Equal(t, "refresh-old", stored.Token.RefreshToken)
}

// TestStaleRefreshDoesNotClobberNewerSignIn proves a refresh that finishes after
// a newer sign-in replaced the same provider serves and keeps the newer login,
// never overwriting it with the stale refreshed token.
func TestStaleRefreshDoesNotClobberNewerSignIn(t *testing.T) {
	vault, err := OpenLoginVault(t.TempDir())
	require.NoError(t, err)
	_, err = vault.commitLogin("anthropic", &oauth.Token{
		AccessToken: "access-old", RefreshToken: "refresh-old",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}, "acct-old")
	require.NoError(t, err)

	// Model a slow refresh: a newer sign-in for the same provider lands (and
	// persists) before the refresh returns its now-stale token.
	var replaced error
	vault.refresh = func(_ context.Context, provider string, _ *oauth.Token) (*oauth.Token, error) {
		_, replaced = vault.commitLogin(provider, &oauth.Token{
			AccessToken: "access-new", RefreshToken: "refresh-new",
		}, "acct-new")
		return &oauth.Token{AccessToken: "access-stale", RefreshToken: "refresh-stale"}, nil
	}

	got, err := vault.liveAccessToken(context.Background(), "anthropic")
	require.NoError(t, err)
	require.NoError(t, replaced)
	require.Equal(t, "access-new", got, "a superseded refresh must serve the newer login's token")

	stored, ok := vault.Get("anthropic")
	require.True(t, ok)
	require.Equal(t, "access-new", stored.Token.AccessToken, "a stale refresh must not overwrite a newer sign-in")
	require.Equal(t, "refresh-new", stored.Token.RefreshToken)
}

// TestLiveTokenRefusesTokenForgottenBySibling proves the disk is consulted on
// the read fast path: a still-unexpired token cached by one daemon must not be
// served after a sibling daemon forgets the login. Without the reconcile the
// cached, non-expired token would be handed out even though the shared vault no
// longer holds it.
func TestLiveTokenRefusesTokenForgottenBySibling(t *testing.T) {
	dir := t.TempDir()
	daemonA, err := OpenLoginVault(dir)
	require.NoError(t, err)

	// A non-expired token so the read never touches the refresh path: the only
	// way daemonA can stop serving it is by consulting authoritative disk state.
	_, err = daemonA.commitLogin("anthropic", &oauth.Token{
		AccessToken: "anthropic-tok", RefreshToken: "refresh-tok",
		ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, "acct-a")
	require.NoError(t, err)

	// daemonB opens after the commit, so it holds anthropic in memory too.
	daemonB, err := OpenLoginVault(dir)
	require.NoError(t, err)

	// Precondition: daemonA serves the cached, unexpired token.
	got, err := daemonA.liveAccessToken(context.Background(), "anthropic")
	require.NoError(t, err)
	require.Equal(t, "anthropic-tok", got)

	// A sibling process forgets the login, persisting the deletion to disk.
	forgotten, err := daemonB.Forget("anthropic")
	require.NoError(t, err)
	require.True(t, forgotten)

	// daemonA must now report the login missing rather than serving its cache.
	_, err = daemonA.liveAccessToken(context.Background(), "anthropic")
	require.Error(t, err, "a forgotten login must not be served from a sibling's stale cache")

	// The CredentialSource contract reports it as known-but-unresolvable, never
	// handing back the forgotten bearer.
	bearer, spendable, err := daemonA.Credential(context.Background(), "anthropic")
	require.Error(t, err)
	require.True(t, spendable, "anthropic is a known spendable platform")
	require.Empty(t, bearer)
}

// TestConcurrentRefreshAndForgetDoesNotResurrect proves a refresh that races a
// sibling's forget never resurrects the login: the forget stays authoritative
// on disk, and neither process serves the credential on the next read.
func TestConcurrentRefreshAndForgetDoesNotResurrect(t *testing.T) {
	dir := t.TempDir()
	daemonA, err := OpenLoginVault(dir)
	require.NoError(t, err)

	// An expired token so the read drives the refresh path.
	_, err = daemonA.commitLogin("hyper", &oauth.Token{
		AccessToken: "hyper-old", RefreshToken: "refresh-old",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}, "acct-a")
	require.NoError(t, err)

	daemonB, err := OpenLoginVault(dir)
	require.NoError(t, err)

	// Model the race: the forget lands (and persists) while the refresh is in
	// flight, before it returns its freshly minted token.
	var forgotten bool
	var forgetErr error
	daemonA.refresh = func(_ context.Context, provider string, _ *oauth.Token) (*oauth.Token, error) {
		forgotten, forgetErr = daemonB.Forget(provider)
		return &oauth.Token{AccessToken: "hyper-new", RefreshToken: "refresh-new"}, nil
	}

	// The in-flight request still receives its freshly minted token, but the
	// persist is a no-op that must not re-add the forgotten login.
	got, err := daemonA.liveAccessToken(context.Background(), "hyper")
	require.NoError(t, err)
	require.NoError(t, forgetErr)
	require.True(t, forgotten)
	require.Equal(t, "hyper-new", got)

	// The forget stays authoritative on disk.
	onDisk, err := OpenLoginVault(dir)
	require.NoError(t, err)
	_, revived := onDisk.Get("hyper")
	require.False(t, revived, "a concurrent refresh must not resurrect a forgotten login on disk")

	// And neither process serves it on the next read.
	_, err = daemonA.liveAccessToken(context.Background(), "hyper")
	require.Error(t, err, "the refreshing process must not serve a login a sibling forgot")
	_, err = daemonB.liveAccessToken(context.Background(), "hyper")
	require.Error(t, err)
}

// TestLiveTokenFailsClosedWhenSharedStateUnreadable proves a disk-backed vault
// does not serve a cached credential when it cannot reconcile against shared
// state. A corrupt logins.json makes the reload fail; since the vault cannot
// confirm a sibling has not forgotten the login, it must report it missing
// rather than serve the stale in-memory cache - even for an unexpired token.
func TestLiveTokenFailsClosedWhenSharedStateUnreadable(t *testing.T) {
	dir := t.TempDir()
	vault, err := OpenLoginVault(dir)
	require.NoError(t, err)

	// A non-expired token: the only reason not to serve it is the fail-closed
	// reconcile, not expiry.
	_, err = vault.commitLogin("anthropic", &oauth.Token{
		AccessToken: "anthropic-tok", RefreshToken: "refresh-tok",
		ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, "acct")
	require.NoError(t, err)

	// Precondition: the token is served while the shared file is readable.
	got, err := vault.liveAccessToken(context.Background(), "anthropic")
	require.NoError(t, err)
	require.Equal(t, "anthropic-tok", got)

	// Corrupt the shared file so the reconcile reload fails.
	require.NoError(t, os.WriteFile(filepath.Join(dir, loginFile), []byte("{not json"), 0o600))

	// Fail closed: report the login missing rather than serve from cache.
	_, err = vault.liveAccessToken(context.Background(), "anthropic")
	require.Error(t, err, "an unreadable shared vault must not serve a cached credential")
}
