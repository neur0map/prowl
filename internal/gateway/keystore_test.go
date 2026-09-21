package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestKeysAreNotReadableAsPlaintext is the property the store exists for: a
// credential must not be recoverable by reading the file, because config
// directories get copied, synced, and pasted into bug reports.
func TestKeysAreNotReadableAsPlaintext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := OpenKeyStore(dir)
	require.NoError(t, err)

	const secret = "gsk_live_do_not_leak_me_1234567890"
	require.NoError(t, store.Put("groq", secret, nil))

	blob, err := os.ReadFile(filepath.Join(dir, keyFileName))
	require.NoError(t, err)
	require.NotContains(t, string(blob), secret, "the secret must not appear on disk")
	require.NotContains(t, string(blob), "groq", "provider names leak which services a user has")
}

// TestKeyFilePermissions guards the other half: encryption is irrelevant if
// the master key sits world-readable next to it.
func TestKeyFilePermissions(t *testing.T) {
	t.Parallel()

	// Production points the store at a directory it creates itself, so the
	// test must too: handing it an existing directory would assert on
	// whatever permissions the caller happened to use.
	dir := filepath.Join(t.TempDir(), "gateway")
	store, err := OpenKeyStore(dir)
	require.NoError(t, err)
	require.NoError(t, store.Put("groq", "gsk_test_key_value", nil))

	for _, name := range []string{keyFileName, masterFileName} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "%s must be owner-only", name)
	}
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "the store directory must be owner-only")
}

// TestKeysSurviveReopen covers the ordinary lifecycle across restarts.
func TestKeysSurviveReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first, err := OpenKeyStore(dir)
	require.NoError(t, err)
	require.NoError(t, first.Put("groq", "gsk_one", nil))
	require.NoError(t, first.Put("cerebras", "csk_two", map[string]string{"region": "us"}))

	second, err := OpenKeyStore(dir)
	require.NoError(t, err)

	got, ok := second.Get("groq")
	require.True(t, ok)
	require.Equal(t, "gsk_one", got)
	require.Equal(t, map[string]string{"region": "us"}, second.Vars("cerebras"))

	require.NoError(t, second.Delete("groq"))
	third, err := OpenKeyStore(dir)
	require.NoError(t, err)
	_, ok = third.Get("groq")
	require.False(t, ok, "a deleted key must stay deleted")
	_, ok = third.Get("cerebras")
	require.True(t, ok, "deleting one key must not disturb another")
}

// TestWrongMasterKeyIsAnError is important for correctness of behaviour: a
// store that cannot be decrypted must fail loudly. Reporting "no keys" would
// make the gateway route nowhere and look like a provider outage.
func TestWrongMasterKeyIsAnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := OpenKeyStore(dir)
	require.NoError(t, err)
	require.NoError(t, store.Put("groq", "gsk_one", nil))

	// Simulate a restored backup that carries the sealed keys but a
	// different master key.
	replacement := make([]byte, masterKeySize)
	require.NoError(t, os.WriteFile(filepath.Join(dir, masterFileName), replacement, 0o600))

	_, err = OpenKeyStore(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "master key does not match")
}

// TestResolveFallsBackToEnvironment is the zero-setup path: a user who
// already exports a provider key should need no configuration at all.
func TestResolveFallsBackToEnvironment(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenKeyStore(dir)
	require.NoError(t, err)

	t.Setenv("GROQ_API_KEY", "gsk_from_env")
	key, source, ok := store.Resolve("groq", "GROQ_API_KEY")
	require.True(t, ok)
	require.Equal(t, "gsk_from_env", key)
	require.Equal(t, "env:GROQ_API_KEY", source)

	// A stored key is the explicit choice and must win over ambient env.
	require.NoError(t, store.Put("groq", "gsk_from_store", nil))
	key, source, ok = store.Resolve("groq", "GROQ_API_KEY")
	require.True(t, ok)
	require.Equal(t, "gsk_from_store", key)
	require.Equal(t, "store", source)

	_, _, ok = store.Resolve("unconfigured", "NOTHING_SET_HERE")
	require.False(t, ok)
}

// TestListNeverReturnsSecrets protects the dashboard payload.
func TestListNeverReturnsSecrets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := OpenKeyStore(dir)
	require.NoError(t, err)
	const secret = "gsk_live_abcdefghijklmnop"
	require.NoError(t, store.Put("groq", secret, nil))

	entries := store.List()
	require.Len(t, entries, 1)
	require.Equal(t, "groq", entries[0].Provider)
	require.NotContains(t, entries[0].Masked, "efghijkl", "the middle must stay hidden")
	require.Equal(t, "gsk_••••••mnop", entries[0].Masked,
		"a preview must be recognisable without being usable")
}

// TestPutRejectsEmptyInput keeps a blank form submission from storing a
// credential that would later fail every request with a confusing 401.
func TestPutRejectsEmptyInput(t *testing.T) {
	t.Parallel()

	store, err := OpenKeyStore(t.TempDir())
	require.NoError(t, err)

	require.Error(t, store.Put("", "key", nil))
	require.Error(t, store.Put("groq", "   ", nil))
	require.Empty(t, store.List())
}
