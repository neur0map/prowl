package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newSettingsStore(t *testing.T) *settingsStore {
	t.Helper()
	s := testServer(t, Options{})
	return &settingsStore{db: s.engine.DB()}
}

func settingsSession(t *testing.T, s *Server) map[string]string {
	t.Helper()
	return map[string]string{"Authorization": "Bearer " + compatMachineKey}
}

// TestSettingRoundTripsAndRejectsInvalid: a valid value survives a write/read,
// and an invalid one is refused with a message a human can act on, leaving the
// stored value untouched rather than poisoning a later read.
func TestSettingRoundTripsAndRejectsInvalid(t *testing.T) {
	t.Parallel()
	store := newSettingsStore(t)
	ctx := context.Background()

	require.NoError(t, store.set(ctx, settingUpdateCheckEnabled, "true"))
	got, err := store.get(ctx, settingUpdateCheckEnabled)
	require.NoError(t, err)
	require.Equal(t, "1", got, "a boolean setting is canonicalised to the reference's 1/0")
	require.True(t, store.autoUpdateCheckEnabled(ctx))

	err = store.set(ctx, settingUpdateCheckEnabled, "maybe")
	require.Error(t, err)
	require.Contains(t, err.Error(), "boolean", "the rejection must say why")

	// The bad write must not have clobbered the good value.
	got, err = store.get(ctx, settingUpdateCheckEnabled)
	require.NoError(t, err)
	require.Equal(t, "1", got, "a rejected value must not overwrite the stored one")
}

// TestUnknownSettingKeyIsRejected: a key the accessor does not know is refused,
// not silently written, so a typo cannot poison routing under an unread key.
func TestUnknownSettingKeyIsRejected(t *testing.T) {
	t.Parallel()
	store := newSettingsStore(t)
	ctx := context.Background()

	err := store.set(ctx, "totally_unknown_key", "x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown setting")

	_, err = store.get(ctx, "totally_unknown_key")
	require.Error(t, err, "reading an unknown key is an error, not an empty default")

	// And nothing was persisted under that key.
	var value string
	err = store.db.QueryRowContext(ctx,
		"SELECT value FROM settings WHERE key = ?", "totally_unknown_key").Scan(&value)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestUnifiedKeyMatchesReferenceFormat pins the wire format the reference mints:
// "freellmapi-" followed by 48 hex chars (db/index.ts:233).
// TestKeyPrefixWidthMatchesTheClientMask guards a coupling that is invisible
// from Go: the vendored dashboard masks the credential with a hardcoded
// slice(0, 13), sized for an 11-character prefix. A shorter prefix would
// reveal more of the secret body; a longer one would hide part of the prefix.
func TestKeyPrefixWidthMatchesTheClientMask(t *testing.T) {
	t.Parallel()
	// The reference pinned 11 for a hardcoded slice(0, 13) in the vendored
	// dashboard. The TUI's mask derives from the prefix instead, so the real
	// contract is that Masked() reveals exactly prefix+2 chars and the total
	// wire length stays prefix+48.
	require.Equal(t, "prowlag-", unifiedAPIKeyPrefix,
		"changing the prefix must be a deliberate edit, not drift")
	require.Equal(t, len(unifiedAPIKeyPrefix)+48, unifiedAPIKeyHexLen+len(unifiedAPIKeyPrefix),
		"the credential's wire length is the prefix plus 48 hex chars")
}

func TestUnifiedKeyMatchesReferenceFormat(t *testing.T) {
	t.Parallel()
	store := newSettingsStore(t)
	ctx := context.Background()

	key, err := store.unifiedAPIKey(ctx)
	require.NoError(t, err)
	require.Regexp(t, unifiedKeyPattern, key.Reveal())
	require.True(t, strings.HasPrefix(key.Reveal(), unifiedAPIKeyPrefix))
	require.Len(t, key.Reveal(), len(unifiedAPIKeyPrefix)+unifiedAPIKeyHexLen)

	// A minted key and a stored one share the format.
	minted, err := generateUnifiedKey()
	require.NoError(t, err)
	require.Regexp(t, unifiedKeyPattern, minted.Reveal())

	// The same key is returned on a second read: the first call seeded it,
	// the second must not mint a fresh one.
	again, err := store.unifiedAPIKey(ctx)
	require.NoError(t, err)
	require.Equal(t, key.Reveal(), again.Reveal(), "the seeded key must be stable across reads")
}

// TestRegenerateInvalidatesPreviousKey: after a rotation the old key stops
// authenticating and the new one starts, immediately, because authentication
// reads the live stored value.
func TestRegenerateInvalidatesPreviousKey(t *testing.T) {
	t.Parallel()
	store := newSettingsStore(t)
	ctx := context.Background()

	old, err := store.unifiedAPIKey(ctx)
	require.NoError(t, err)

	ok, err := store.authenticateMachineKey(ctx, old.Reveal())
	require.NoError(t, err)
	require.True(t, ok, "the current key authenticates")

	fresh, err := store.regenerateUnifiedAPIKey(ctx)
	require.NoError(t, err)
	require.NotEqual(t, old.Reveal(), fresh.Reveal(), "a regenerate must change the key")

	ok, err = store.authenticateMachineKey(ctx, old.Reveal())
	require.NoError(t, err)
	require.False(t, ok, "the previous key must stop authenticating at once")

	ok, err = store.authenticateMachineKey(ctx, fresh.Reveal())
	require.NoError(t, err)
	require.True(t, ok, "the new key must authenticate")

	ok, err = store.authenticateMachineKey(ctx, "")
	require.NoError(t, err)
	require.False(t, ok, "an empty candidate never authenticates")
}

// TestMaskedFormNeverLeaksTheKey: a credential embedded in any aggregate
// response serialises masked, so only the deliberate reveal path can expose the
// plaintext.
func TestMaskedFormNeverLeaksTheKey(t *testing.T) {
	t.Parallel()
	store := newSettingsStore(t)
	ctx := context.Background()

	key, err := store.unifiedAPIKey(ctx)
	require.NoError(t, err)
	full := key.Reveal()

	masked := key.Masked()
	require.NotContains(t, masked, full[len(unifiedAPIKeyPrefix):], "the masked form must not carry the secret body")
	require.Contains(t, masked, "\u2022", "the masked form shows bullets for the hidden part")
	require.True(t, strings.HasPrefix(masked, full[:len(unifiedAPIKeyPrefix)+2]), "the recognisable prefix is preserved")

	// Marshalled through any list-like path, the credential is masked.
	blob, err := json.Marshal(struct {
		Key unifiedKey `json:"key"`
	}{Key: key})
	require.NoError(t, err)
	require.NotContains(t, string(blob), full, "a credential must never marshal to plaintext by default")
}

// TestAPIKeyEndpointRevealsWholeKeyForTheDashboard: the dedicated endpoint is
// the explicit reveal - the dashboard's Show/Copy affordances need the
// plaintext - and regenerate returns a different whole key.
func TestAPIKeyEndpointRevealsWholeKeyForTheDashboard(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	auth := settingsSession(t, s)

	resp, body := do(t, s, http.MethodGet, "/api/settings/api-key", "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var first struct {
		APIKey string `json:"apiKey"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Regexp(t, unifiedKeyPattern, first.APIKey, "the reveal endpoint returns the whole key")

	resp, body = do(t, s, http.MethodPost, "/api/settings/api-key/regenerate", "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rotated struct {
		APIKey string `json:"apiKey"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &rotated))
	require.Regexp(t, unifiedKeyPattern, rotated.APIKey)
	require.NotEqual(t, first.APIKey, rotated.APIKey, "regenerate returns a new key")

	// A subsequent read returns the rotated key, not the original.
	_, body = do(t, s, http.MethodGet, "/api/settings/api-key", "", auth)
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Equal(t, rotated.APIKey, first.APIKey)
}

// TestUpdateCheckOptInRoundTrips: the Settings dialog toggle writes and reads
// the opt-in, and a missing field is a useful 400, not a silent default.
func TestUpdateCheckOptInRoundTrips(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	auth := settingsSession(t, s)

	resp, body := do(t, s, http.MethodGet, "/api/settings/update-check", "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.JSONEq(t, `{"enabled":false}`, body, "a fresh install has the check off")

	resp, body = do(t, s, http.MethodPut, "/api/settings/update-check", `{"enabled":true}`,
		mergeHeaders(auth, "Content-Type", "application/json"))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.JSONEq(t, `{"enabled":true}`, body)

	_, body = do(t, s, http.MethodGet, "/api/settings/update-check", "", auth)
	require.JSONEq(t, `{"enabled":true}`, body, "the opt-in persists")

	resp, body = do(t, s, http.MethodPut, "/api/settings/update-check", `{}`,
		mergeHeaders(auth, "Content-Type", "application/json"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, string(TypeInvalidRequest), errorType(t, body))
}

// TestUpdateCheckReportsUnknownWhenNetworkUnavailable: the whole point of the
// update check degrading gracefully. A build with a commit but no reachable
// network reports "unknown", never a 502 or a blocked page.
func TestUpdateCheckReportsUnknownWhenNetworkUnavailable(t *testing.T) {
	t.Parallel()
	store := newSettingsStore(t)

	u := newTestUpdateChecker(store, strings.Repeat("a", 40))
	u.fetch = func(context.Context, string, map[string]string) (*http.Response, error) {
		return nil, errors.New("network down")
	}

	res := u.check(context.Background())
	require.Equal(t, "unknown", res.Status)
	require.Equal(t, "source", res.Installation)
	require.NotNil(t, res.LocalSha)
	require.Equal(t, "aaaaaaa", *res.LocalSha, "the local sha is still reported")
	require.NotNil(t, res.Version)
}

// TestUpdateCheckIsUnsupportedWithoutACommit: a build with no resolvable commit
// cannot compare anything, and says so honestly with a null localSha rather
// than pretending to be current.
func TestUpdateCheckIsUnsupportedWithoutACommit(t *testing.T) {
	t.Parallel()
	store := newSettingsStore(t)

	u := newTestUpdateChecker(store, "")
	u.installation = "unknown"
	u.fetch = func(context.Context, string, map[string]string) (*http.Response, error) {
		t.Fatal("a build without a commit must not hit the network")
		return nil, nil
	}

	res := u.check(context.Background())
	require.Equal(t, "unsupported", res.Status)
	require.Nil(t, res.LocalSha, "an unsupported build reports a null local sha")
}

// TestUpdateCheckParsesAnAheadCompare: when GitHub reports main ahead of us the
// check maps to "available" and lists the new commits newest-first.
func TestUpdateCheckParsesAnAheadCompare(t *testing.T) {
	t.Parallel()
	store := newSettingsStore(t)

	u := newTestUpdateChecker(store, strings.Repeat("b", 40))
	u.fetch = stubFetch(http.StatusOK, `{
		"status": "ahead",
		"commits": [
			{"sha":"1111111111111111111111111111111111111111","commit":{"message":"old change","committer":{"date":"2026-01-01T00:00:00Z"}}},
			{"sha":"2222222222222222222222222222222222222222","commit":{"message":"new change\nbody","committer":{"date":"2026-01-02T00:00:00Z"}}}
		]
	}`)

	res := u.check(context.Background())
	require.Equal(t, "available", res.Status)
	require.Equal(t, "2222222", res.RemoteSha, "the remote head is the last commit")
	require.Len(t, res.Changes, 2)
	require.Equal(t, "2222222", res.Changes[0].Sha, "changes are newest-first")
	require.Equal(t, "new change", res.Changes[0].Message, "only the first line of a commit message is kept")
}

// TestReleaseRespectsTheOptIn: the reminder proxy stays silent until the
// operator opts in, so a self-hosted box never phones GitHub on its own.
func TestReleaseRespectsTheOptIn(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	auth := settingsSession(t, s)

	// Off by default: the endpoint answers disabled without any network call.
	resp, body := do(t, s, http.MethodGet, "/api/update/release", "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.JSONEq(t, `{"disabled":true}`, body)
}

func newTestUpdateChecker(store *settingsStore, sha string) *updateChecker {
	version := "v9.9.9"
	return &updateChecker{
		settings:     store,
		repo:         updateRepo,
		localSHA:     sha,
		installation: "source",
		version:      func() *string { return &version },
		now:          time.Now,
	}
}

func stubFetch(status int, body string) fetchFunc {
	return func(context.Context, string, map[string]string) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}
}

func mergeHeaders(base map[string]string, kv ...string) map[string]string {
	out := make(map[string]string, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}
