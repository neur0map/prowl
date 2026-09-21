package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// linkedTestEngine opens a throwaway engine with the catalogue seed skipped:
// these tests only exercise the vault.
func linkedTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := OpenEngine(context.Background(), t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

type fakeLogins struct {
	secret string
	known  bool
	err    error
	calls  int
}

func (f *fakeLogins) Credential(context.Context, string) (string, bool, error) {
	f.calls++
	return f.secret, f.known, f.err
}

func (f *fakeLogins) Linkable(context.Context) []LinkableProvider { return nil }

func (f *fakeLogins) Models(context.Context, string) []LinkedModel { return nil }

type managedLogin struct {
	enabled bool
}

func (m *managedLogin) Credential(context.Context, string) (string, bool, error) {
	return "live-token", true, nil
}

func (m *managedLogin) Linkable(context.Context) []LinkableProvider {
	return []LinkableProvider{
		{ID: "anthropic", Name: "Claude Pro / Max", PoolManaged: true, PoolEnabled: m.enabled},
		{ID: "openai", Name: "ChatGPT / Codex", PoolManaged: true, PoolEnabled: m.enabled},
	}
}

func (m *managedLogin) Models(_ context.Context, provider string) []LinkedModel {
	if provider == "openai" {
		return []LinkedModel{{ID: "gpt-current-codex", Name: "GPT Current Codex", Tools: true}}
	}
	return []LinkedModel{{ID: "claude-current", Name: "Claude Current", Tools: true}}
}

func TestManagedLoginPreferenceConvergesRoutingPool(t *testing.T) {
	eng := linkedTestEngine(t)
	login := &managedLogin{enabled: true}

	eng.SetCredentialSource(login)

	var keys, models int
	require.NoError(t, eng.DB().QueryRow(
		"SELECT COUNT(*) FROM api_keys WHERE platform IN ('anthropic', 'openai')").Scan(&keys))
	require.NoError(t, eng.DB().QueryRow(
		"SELECT COUNT(*) FROM models WHERE platform IN ('anthropic', 'openai') AND source = 'login'").Scan(&models))
	require.Equal(t, 2, keys)
	require.Equal(t, 2, models)

	login.enabled = false
	eng.ReconcileLoginModels(t.Context())
	require.NoError(t, eng.DB().QueryRow(
		"SELECT COUNT(*) FROM api_keys WHERE platform IN ('anthropic', 'openai')").Scan(&keys))
	require.NoError(t, eng.DB().QueryRow(
		"SELECT COUNT(*) FROM models WHERE platform IN ('anthropic', 'openai') AND source = 'login'").Scan(&models))
	require.Zero(t, keys)
	require.Zero(t, models)
}

// TestLinkedKeyResolvesLive covers the guarantee that makes enrolling a
// subscription safe: the pool row holds a reference, the secret is fetched on
// every read, and a login that has gone away produces an actionable error
// rather than a decrypt failure.
func TestLinkedKeyResolvesLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng := linkedTestEngine(t)
	logins := &fakeLogins{secret: "live-token-1", known: true}
	eng.SetCredentialSource(logins)

	id, err := eng.Vault().AddLinked(ctx, "copilot", "Prowl login")
	require.NoError(t, err)

	secret, err := eng.Vault().Reveal(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "live-token-1", secret)

	// A refreshed token must be picked up without touching the row: that is
	// the whole reason the secret is not copied.
	logins.secret = "live-token-2"
	secret, err = eng.Vault().Reveal(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "live-token-2", secret)
	require.Equal(t, 2, logins.calls, "each read must consult the login")

	// Nothing resembling a secret may be persisted.
	var stored string
	require.NoError(t, eng.DB().QueryRow(
		"SELECT encrypted_key FROM api_keys WHERE id = ?", id).Scan(&stored))
	require.Equal(t, "link:copilot", stored)

	// Signing out revokes the pool's access, and the message says what to do.
	logins.known = false
	_, err = eng.Vault().Reveal(ctx, id)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not logged in")

	logins.known = true
	logins.err = errors.New("refresh endpoint refused")
	_, err = eng.Vault().Reveal(ctx, id)
	require.ErrorContains(t, err, "refresh endpoint refused")
}

// TestLinkedKeyWithoutSourceFailsClearly is the standalone-gateway case: a
// linked row cannot be resolved with no harness attached, and the operator
// needs to read that rather than a crypto error.
func TestLinkedKeyWithoutSourceFailsClearly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng := linkedTestEngine(t)
	id, err := eng.Vault().AddLinked(ctx, "copilot", "")
	require.NoError(t, err)

	_, err = eng.Vault().Reveal(ctx, id)
	require.ErrorContains(t, err, "no login source is wired")
}
