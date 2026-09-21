package gateway

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/gateway/catalog"
	"github.com/neur0map/prowl/internal/gateway/provider"
	"github.com/neur0map/prowl/internal/gateway/store"
)

// stubValidator scripts a validation verdict per key (or a default), so the
// health logic can be tested without any real HTTP.
type stubValidator struct {
	mu      sync.Mutex
	def     provider.KeyValidationResult
	byKey   map[string]provider.KeyValidationResult
	calls   int
	lastKey string
}

func (s *stubValidator) ValidateKey(_ context.Context, _, _, apiKey string) provider.KeyValidationResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastKey = apiKey
	if r, ok := s.byKey[apiKey]; ok {
		return r
	}
	return s.def
}

func newVault(t *testing.T, v Validator) (*KeyVault, string, *sql.DB) {
	t.Helper()
	t.Setenv(encryptionKeyEnv, "") // force the file master key, ignore any ambient env
	dir := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	vault, err := OpenKeyVault(ctx, st.DB(), dir, v)
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}
	return vault, dir, st.DB()
}

func TestVaultEncryptRoundTripAndMasking(t *testing.T) {
	v, _, _ := newVault(t, nil)
	ctx := context.Background()
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"

	id, err := v.Add("groq", secret, AddOptions{Label: "main"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Reveal(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("round trip = %q, want %q", got, secret)
	}

	rows, err := v.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("listed %d rows, want 1", len(rows))
	}
	row := rows[0]

	// The masked preview must not be the secret, and marshalling the row must
	// not leak it - the property that stops a keys handler from spilling every
	// credential.
	if row.Masked == secret || !strings.Contains(row.Masked, "...") {
		t.Fatalf("masked = %q", row.Masked)
	}
	blob, _ := json.Marshal(row)
	if strings.Contains(string(blob), secret) {
		t.Fatalf("marshalled row leaks the plaintext: %s", blob)
	}
	// A recognisable-but-small tail, never a reconstructable share.
	if !strings.HasPrefix(row.Masked, "sk-a") || !strings.HasSuffix(row.Masked, "6789") {
		t.Fatalf("masked preview shape wrong: %q", row.Masked)
	}
}

func TestVaultWrongMasterKeyFailsLoudly(t *testing.T) {
	t.Setenv(encryptionKeyEnv, "")
	dir := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	v, err := OpenKeyVault(ctx, st.DB(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Add("groq", "sk-longenoughsecret", AddOptions{}); err != nil {
		t.Fatal(err)
	}

	// Swap the master key for a different one: reopening must fail loudly and
	// name the master.key, not return decrypt garbage or "no keys".
	newKey := make([]byte, masterKeySize)
	crand.Read(newKey)
	if err := os.WriteFile(filepath.Join(dir, masterFileName), newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = OpenKeyVault(ctx, st.DB(), dir, nil)
	if err == nil {
		t.Fatal("reopen with a changed master key must fail")
	}
	if !strings.Contains(err.Error(), "master key does not match") || !strings.Contains(err.Error(), "master.key") {
		t.Fatalf("error should name the master.key cause, got: %v", err)
	}
}

func TestVaultTamperedCiphertextFailsAuthTag(t *testing.T) {
	v, _, db := newVault(t, nil)
	ctx := context.Background()
	id, err := v.Add("groq", "sk-secret-abcdefghij", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var enc, tag string
	if err := db.QueryRow("SELECT encrypted_key, auth_tag FROM api_keys WHERE id = ?", id).Scan(&enc, &tag); err != nil {
		t.Fatal(err)
	}

	// Flip one ciphertext nibble: GCM authentication must reject it.
	if _, err := db.Exec("UPDATE api_keys SET encrypted_key = ? WHERE id = ?", flipFirstHex(enc), id); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Reveal(ctx, id); err == nil {
		t.Fatal("tampered ciphertext must fail the auth tag")
	}

	// Restore ciphertext, tamper the tag instead: same rejection.
	if _, err := db.Exec("UPDATE api_keys SET encrypted_key = ?, auth_tag = ? WHERE id = ?", enc, flipFirstHex(tag), id); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Reveal(ctx, id); err == nil {
		t.Fatal("tampered auth tag must fail")
	}
}

func TestHealthInconclusiveDoesNotDisable(t *testing.T) {
	sv := &stubValidator{def: provider.Inconclusive("network down")}
	v, _, _ := newVault(t, sv)
	ctx := context.Background()
	id, _ := v.Add("groq", "sk-key-1234567890", AddOptions{})

	for range 5 {
		if _, err := v.CheckKey(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	row, _, _ := v.Get(ctx, id)
	if !row.Enabled {
		t.Fatal("a flaky network must never disable a key")
	}
	if row.Status != StatusUnknown {
		t.Fatalf("status = %q, want unchanged (unknown)", row.Status)
	}
	if row.ConsecutiveFailures != 0 {
		t.Fatalf("inconclusive must not count toward auto-disable, got %d", row.ConsecutiveFailures)
	}
	if row.LastHealthError == "" {
		t.Fatal("inconclusive should still record a diagnostic")
	}
}

func TestHealthThreeInvalidsDisableTwoDont(t *testing.T) {
	sv := &stubValidator{def: provider.Invalid("bad key")}
	v, _, _ := newVault(t, sv)
	ctx := context.Background()
	a, _ := v.Add("groq", "sk-a-aaaaaaaaaa", AddOptions{})
	b, _ := v.Add("cerebras", "sk-b-bbbbbbbbbb", AddOptions{})

	for range 3 {
		v.CheckKey(ctx, a)
	}
	for range 2 {
		v.CheckKey(ctx, b)
	}

	ra, _, _ := v.Get(ctx, a)
	rb, _, _ := v.Get(ctx, b)
	if ra.Enabled {
		t.Fatal("three consecutive invalids must disable")
	}
	if ra.Status != StatusError || ra.ConsecutiveFailures < 3 {
		t.Fatalf("disabled key: status=%q failures=%d", ra.Status, ra.ConsecutiveFailures)
	}
	if !rb.Enabled {
		t.Fatal("two invalids must not disable")
	}
	if rb.ConsecutiveFailures != 2 {
		t.Fatalf("two invalids => failures=%d, want 2", rb.ConsecutiveFailures)
	}
}

func TestHealthSuccessResetsAndPromotes(t *testing.T) {
	ctx := context.Background()

	// A live request promotes a confirmed-bad key back to healthy.
	sv := &stubValidator{def: provider.Invalid("bad")}
	v, _, _ := newVault(t, sv)
	id, _ := v.Add("groq", "sk-x-xxxxxxxxxx", AddOptions{})
	v.CheckKey(ctx, id)
	if r, _, _ := v.Get(ctx, id); r.Status != StatusError {
		t.Fatalf("after one invalid, status = %q, want error", r.Status)
	}
	if err := v.MarkHealthyFromRequest(ctx, id); err != nil {
		t.Fatal(err)
	}
	r, _, _ := v.Get(ctx, id)
	if r.Status != StatusHealthy || r.ConsecutiveFailures != 0 {
		t.Fatalf("live success should promote error->healthy and reset: status=%q failures=%d", r.Status, r.ConsecutiveFailures)
	}

	// A subsequent valid probe also resets the counter after invalids.
	sv2 := &stubValidator{def: provider.Invalid("bad")}
	v2, _, _ := newVault(t, sv2)
	id2, _ := v2.Add("groq", "sk-y-yyyyyyyyyy", AddOptions{})
	v2.CheckKey(ctx, id2)
	v2.CheckKey(ctx, id2)
	sv2.mu.Lock()
	sv2.def = provider.Valid()
	sv2.mu.Unlock()
	v2.CheckKey(ctx, id2)
	r2, _, _ := v2.Get(ctx, id2)
	if r2.Status != StatusHealthy || r2.ConsecutiveFailures != 0 {
		t.Fatalf("valid probe after invalids: status=%q failures=%d", r2.Status, r2.ConsecutiveFailures)
	}
}

func TestVaultMultipleKeysIndependentStatus(t *testing.T) {
	sv := &stubValidator{byKey: map[string]provider.KeyValidationResult{
		"sk-good-11111111": provider.Valid(),
		"sk-bad-222222222": provider.Invalid("nope"),
		"sk-net-333333333": provider.Inconclusive("timeout"),
	}}
	v, _, _ := newVault(t, sv)
	ctx := context.Background()

	good, _ := v.Add("groq", "sk-good-11111111", AddOptions{})
	bad, _ := v.Add("groq", "sk-bad-222222222", AddOptions{})
	net, _ := v.Add("groq", "sk-net-333333333", AddOptions{})

	v.CheckKey(ctx, good)
	v.CheckKey(ctx, bad)
	v.CheckKey(ctx, net)

	rg, _, _ := v.Get(ctx, good)
	rb, _, _ := v.Get(ctx, bad)
	rn, _, _ := v.Get(ctx, net)
	if rg.Status != StatusHealthy {
		t.Fatalf("good key status = %q", rg.Status)
	}
	if rb.Status != StatusError {
		t.Fatalf("bad key status = %q", rb.Status)
	}
	if rn.Status != StatusUnknown {
		t.Fatalf("inconclusive key status = %q, want unchanged", rn.Status)
	}
	if !rg.Enabled || !rb.Enabled || !rn.Enabled {
		t.Fatal("a single check must not disable any of them")
	}
	if rows, _ := v.List(ctx); len(rows) != 3 {
		t.Fatalf("three keys must coexist for one platform, got %d", len(rows))
	}
}

func TestLegacyImportOneRowPerProviderAndIdempotent(t *testing.T) {
	t.Setenv(encryptionKeyEnv, "")
	dir := t.TempDir()
	ctx := context.Background()

	// Seed the pre-port single-key store.
	old, err := OpenKeyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(old.Put("google-gemini", "AIzaSyExampleLongKey1234567890", nil))
	must(old.Put("groq", "gsk_exampleLongKey1234567890", nil))
	must(old.Put("mystery-provider", "secretLongKey1234567890", nil))

	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	v, err := OpenKeyVault(ctx, st.DB(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := v.List(ctx)
	if len(rows) != 3 {
		t.Fatalf("import produced %d rows, want exactly one per provider (3)", len(rows))
	}
	byPlatform := map[string]KeyRow{}
	for _, r := range rows {
		byPlatform[r.Platform] = r
	}
	// A provider whose production wire was retired keeps its secret for
	// operator review, but must not be silently enabled or remapped onto a
	// nonexistent adapter.
	google, ok := byPlatform["google-gemini"]
	if !ok {
		t.Fatal("retired google-gemini credential must be preserved for review")
	}
	if google.Enabled {
		t.Fatal("retired google-gemini credential must be imported disabled")
	}
	if _, ok := byPlatform["groq"]; !ok {
		t.Fatal("groq must import unchanged")
	}
	// An unrecognised id is preserved under its own id, DISABLED, never dropped.
	m, ok := byPlatform["mystery-provider"]
	if !ok {
		t.Fatal("unrecognised provider must not be dropped on import")
	}
	if m.Enabled {
		t.Fatal("unrecognised provider must be imported disabled for review")
	}

	// The secret survives the migration intact even though its old provider is
	// no longer advertised.
	secret, err := v.Reveal(ctx, google.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secret != "AIzaSyExampleLongKey1234567890" {
		t.Fatalf("imported secret = %q", secret)
	}

	// A second open adds nothing (idempotent), and the file is left in place.
	if _, err := os.Stat(filepath.Join(dir, "keys.enc")); err != nil {
		t.Fatalf("keys.enc should remain: %v", err)
	}
	v2, err := OpenKeyVault(ctx, st.DB(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows2, _ := v2.List(ctx); len(rows2) != 3 {
		t.Fatalf("second open changed the row count to %d", len(rows2))
	}
}

func TestCatalogAdapterOverrideTargetsAreKnown(t *testing.T) {
	for catalogID, platform := range catalog.AdapterOverrides() {
		if !provider.Known(platform) {
			t.Errorf("override %q -> %q is not a platform the registry knows", catalogID, platform)
		}
	}
}

func TestNextHealthCheckDelayBounds(t *testing.T) {
	if d := NextHealthCheckDelay(func() float64 { return 0 }); d != 4*time.Minute {
		t.Fatalf("jitter 0 => %v, want 4m (interval -20%%)", d)
	}
	if d := NextHealthCheckDelay(func() float64 { return 1 }); d != 6*time.Minute {
		t.Fatalf("jitter 1 => %v, want 6m (interval +20%%)", d)
	}
}

func TestCheckAllKeysSkipsRecent(t *testing.T) {
	sv := &stubValidator{def: provider.Valid()}
	v, _, db := newVault(t, sv)
	ctx := context.Background()

	recent, _ := v.Add("groq", "sk-recent-12345678", AddOptions{})
	stale, _ := v.Add("cerebras", "sk-stale-12345678", AddOptions{})
	db.Exec("UPDATE api_keys SET last_checked_at = ? WHERE id = ?", time.Now().Unix(), recent)
	db.Exec("UPDATE api_keys SET last_checked_at = ? WHERE id = ?", time.Now().Add(-time.Hour).Unix(), stale)

	res, err := v.CheckAllKeys(ctx, HealthPassOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.Skipped, recent) {
		t.Fatalf("a recently checked key must be skipped: %+v", res)
	}
	if !contains(res.Checked, stale) {
		t.Fatalf("a stale key must be checked: %+v", res)
	}

	forced, err := v.CheckAllKeys(ctx, HealthPassOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(forced.Checked, recent) || !contains(forced.Checked, stale) {
		t.Fatalf("force must check every enabled key: %+v", forced)
	}
}

func contains(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func flipFirstHex(s string) string {
	if s == "" {
		return "0"
	}
	b := []byte(s)
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}

// TestHealthProbesLinkedTokenNotMalformedIV proves the health verbs resolve a
// linked row's credential through the login source instead of trying to decrypt
// its empty IV/tag. Before the fix, CheckKey ran decryptSecret on the empty IV,
// recorded a "stored iv is malformed" inconclusive and never probed the real,
// refreshed subscription token - so a dead login stayed green.
func TestHealthProbesLinkedTokenNotMalformedIV(t *testing.T) {
	ctx := context.Background()

	// The provider approves only the live refreshed token. There is no
	// malformed-IV short-circuit that could reach this stub, so the sole way
	// the key goes healthy is the resolved token actually being probed.
	sv := &stubValidator{
		def:   provider.Invalid("probed something other than the live token"),
		byKey: map[string]provider.KeyValidationResult{"live-refreshed-token": provider.Valid()},
	}
	v, _, db := newVault(t, sv)
	logins := &fakeLogins{secret: "live-refreshed-token", known: true}
	v.credentials = logins

	id, err := v.AddLinked(ctx, "copilot", "Prowl login")
	if err != nil {
		t.Fatalf("add linked: %v", err)
	}

	status, err := v.CheckKey(ctx, id)
	if err != nil {
		t.Fatalf("check key: %v", err)
	}
	if status != StatusHealthy {
		t.Fatalf("linked key status = %q, want %q: the real token was never probed", status, StatusHealthy)
	}
	if sv.calls != 1 {
		t.Fatalf("validator calls = %d, want 1: the probe skipped the provider", sv.calls)
	}
	if sv.lastKey != "live-refreshed-token" {
		t.Fatalf("validator saw key %q, want the resolved live token", sv.lastKey)
	}
	if logins.calls == 0 {
		t.Fatal("health probe never consulted the login source")
	}

	// A healthy probe leaves no diagnostic at all, and certainly not the
	// malformed-IV one the empty-tag decrypt path would have produced.
	var healthErr sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT last_health_error FROM api_keys WHERE id = ?", id).Scan(&healthErr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(healthErr.String, "iv is malformed") {
		t.Fatalf("recorded malformed-IV diagnostic %q instead of probing the token", healthErr.String)
	}

	// CheckAllKeys shares the same resolution path and picks up a rotated token.
	logins.secret = "rotated-live-token"
	sv.byKey["rotated-live-token"] = provider.Valid()
	res, err := v.CheckAllKeys(ctx, HealthPassOptions{Force: true})
	if err != nil {
		t.Fatalf("check all: %v", err)
	}
	if len(res.Checked) != 1 || res.Checked[0] != id {
		t.Fatalf("CheckAllKeys checked %v, want [%d]", res.Checked, id)
	}
	if sv.lastKey != "rotated-live-token" {
		t.Fatalf("CheckAllKeys probed %q, want the rotated live token", sv.lastKey)
	}
}

// TestHealthLinkedFailureAttribution proves the fix preserves how failures are
// attributed: a provider's invalid verdict on the resolved token still demotes
// and redacts, while a login that cannot be resolved is transport-class and
// leaves the status untouched rather than blaming the provider.
func TestHealthLinkedFailureAttribution(t *testing.T) {
	ctx := context.Background()

	sv := &stubValidator{def: provider.Invalid("subscription rejected key live-refreshed-token")}
	v, _, db := newVault(t, sv)
	logins := &fakeLogins{secret: "live-refreshed-token", known: true}
	v.credentials = logins

	bad, err := v.AddLinked(ctx, "copilot", "Prowl login")
	if err != nil {
		t.Fatalf("add linked: %v", err)
	}

	// A confirmed-invalid verdict on the resolved token parks the key at error
	// with the reason recorded - and the token redacted out of it.
	status, err := v.CheckKey(ctx, bad)
	if err != nil {
		t.Fatalf("check key: %v", err)
	}
	if status != StatusError {
		t.Fatalf("invalid linked key status = %q, want %q", status, StatusError)
	}
	var reason sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT last_health_error FROM api_keys WHERE id = ?", bad).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason.String == "" {
		t.Fatal("invalid verdict recorded no reason")
	}
	if strings.Contains(reason.String, "live-refreshed-token") {
		t.Fatalf("resolved token leaked into recorded reason %q", reason.String)
	}

	// A login that has gone away is not a provider verdict. The status must not
	// flip to error on the provider's behalf, and the validator is never asked.
	callsBefore := sv.calls
	gone, err := v.AddLinked(ctx, "copilot", "Prowl login")
	if err != nil {
		t.Fatalf("add linked: %v", err)
	}
	logins.known = false
	status, err = v.CheckKey(ctx, gone)
	if err != nil {
		t.Fatalf("check key (gone login): %v", err)
	}
	if status != StatusUnknown {
		t.Fatalf("unresolvable linked key status = %q, want %q (untouched)", status, StatusUnknown)
	}
	if sv.calls != callsBefore {
		t.Fatalf("validator was called %d times for an unresolvable login, want 0", sv.calls-callsBefore)
	}
	if err := db.QueryRowContext(ctx, "SELECT last_health_error FROM api_keys WHERE id = ?", gone).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason.String, "not logged in") {
		t.Fatalf("recorded reason %q does not explain the unresolved login", reason.String)
	}
}

// TestResolveVaultMasterKeyConcurrentCreatorsAgree proves the file master key is
// created first-writer-wins: concurrent openers converge on one key (a divergent
// second key would fail every stored credential's fingerprint check) and that
// key is usable - it round-trips a secret through the vault AEAD and yields a
// stable fingerprint, so a vault initialised by any creator verifies against any
// other.
func TestResolveVaultMasterKeyConcurrentCreatorsAgree(t *testing.T) {
	t.Setenv(encryptionKeyEnv, "") // force the file master key, ignore ambient env
	dir := t.TempDir()

	const n = 16
	keys := make([][]byte, n)
	sources := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			<-start
			keys[i], sources[i], errs[i] = resolveVaultMasterKey(dir)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("creator %d: %v", i, errs[i])
		}
		if len(keys[i]) != masterKeySize {
			t.Fatalf("creator %d got a %d-byte key, want %d", i, len(keys[i]), masterKeySize)
		}
		if sources[i] != "file" {
			t.Fatalf("creator %d source = %q, want file", i, sources[i])
		}
	}
	for i := 1; i < n; i++ {
		if string(keys[i]) != string(keys[0]) {
			t.Fatalf("creator %d diverged from the winning master key", i)
		}
	}

	onDisk, err := os.ReadFile(filepath.Join(dir, masterFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(keys[0]) {
		t.Fatal("the persisted master key differs from the one creators observed")
	}

	// Fingerprint usability: the winning key encrypts and decrypts through the
	// vault AEAD, so it is real cipher material and not, say, a truncated write.
	aead, err := newVaultAEAD(keys[0])
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	sealed := aead.Seal(nil, nonce, []byte("provider-secret"), nil)
	opened, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		t.Fatalf("winning master key cannot decrypt what it encrypted: %v", err)
	}
	if string(opened) != "provider-secret" {
		t.Fatalf("round trip = %q, want provider-secret", opened)
	}

	// And the real vault path accepts it: opening initialises the stored
	// fingerprint, and re-opening verifies against it. With a divergent second
	// master key this reopen would fail loudly, so a clean reopen is the
	// end-to-end proof that the winner's fingerprint is stable and usable.
	ctx := context.Background()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if _, err := OpenKeyVault(ctx, st.DB(), dir, nil); err != nil {
		t.Fatalf("first vault open with the winning master key: %v", err)
	}
	if _, err := OpenKeyVault(ctx, st.DB(), dir, nil); err != nil {
		t.Fatalf("reopen must verify against the stored fingerprint, got: %v", err)
	}
}
