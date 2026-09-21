package gateway

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/neur0map/prowl/internal/gateway/logins/anthropic"
	"github.com/neur0map/prowl/internal/gateway/logins/browserflow"
	"github.com/neur0map/prowl/internal/gateway/logins/copilot"
	"github.com/neur0map/prowl/internal/gateway/logins/hyper"
	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
	openaiflow "github.com/neur0map/prowl/internal/gateway/logins/openai"
)

// The gateway is its own harness here: unlike Prowl Legacy, which borrowed a
// CredentialSource from the running agent's config store, Prowl holds
// subscription logins itself. This file is that vault - logins.json, encrypted
// with the same master key as the API-key vault, refreshed on demand at
// dispatch time so a rotated token is picked up mid-request and a revoked login
// stops serving traffic immediately.

// LoginKind is how a login is acquired: an interactive browser PKCE flow, a
// device-code flow, or a pasted credential.
type LoginKind string

const (
	LoginBrowser LoginKind = "browser"
	LoginDevice  LoginKind = "device"
	LoginManual  LoginKind = "manual"
)

// LoginPlatform is one provider the TUI can sign in to, and what happens to
// the credential afterwards.
type LoginPlatform struct {
	// ID is the vault key and, for routable platforms, the gateway platform
	// the adapter registry keys on.
	ID string
	// Name is the display label.
	Name string
	// Kind is the acquisition flow.
	Kind LoginKind
	// RoutesTo is the gateway platform the credential is dispatched as, or
	// "" when no wire adapter can spend it. A login that cannot route is
	// still stored (and shown) because re-enrolling after an adapter lands
	// should not require another browser dance - but the UI says so out loud
	// rather than enrolling a pool member that fails its first request.
	RoutesTo string
	// AccountHint tells the user which account this spends (a Claude
	// subscription, a Charm Hyper seat), since "logged in" without an owner is
	// ambiguous on a shared machine.
	AccountHint string
	// BaseModelsURL is the OpenAI-shaped /models endpoint probed after login
	// to seed the catalogue, when the route serves one.
	BaseModelsURL string
}

// LoginPlatforms is the full subscription/OAuth set the tool supports: the
// four interactive flows Prowl's harness offers, plus pasted-key platforms
// that behave like subscriptions but authenticate with a static token.
//
// anthropic and hyper route through the registry's compat adapters. OpenAI
// routes through the Codex Responses adapter, which also discovers the exact
// model set offered to the signed-in ChatGPT subscription. Copilot is stored
// but not enrolled until its editor-agent wire contract is implemented.
func LoginPlatforms() []LoginPlatform {
	return []LoginPlatform{
		{ID: "anthropic", Name: "Claude Pro / Max", Kind: LoginBrowser, RoutesTo: "anthropic",
			AccountHint: "your Anthropic subscription", BaseModelsURL: "https://api.anthropic.com/v1/models"},
		{ID: "hyper", Name: "Charm Hyper", Kind: LoginDevice, RoutesTo: "hyper",
			AccountHint: "your Charm Hyper seat", BaseModelsURL: "https://hyper.charm.land/v1/models"},
		{ID: "openai", Name: "ChatGPT (Codex)", Kind: LoginBrowser, RoutesTo: "openai",
			AccountHint: "your ChatGPT subscription"},
		{ID: "copilot", Name: "GitHub Copilot", Kind: LoginDevice, RoutesTo: "",
			AccountHint: "your Copilot plan"},
	}
}

// loginFile is the vault's on-disk name inside the gateway state dir.
const loginFile = "logins.json"

// StoredLogin is one acquired login. The token is stored encrypted; the rest
// is metadata safe to show a terminal.
type StoredLogin struct {
	Provider      string          `json:"provider"`
	Account       string          `json:"account,omitempty"`
	Kind          LoginKind       `json:"kind"`
	CreatedAt     time.Time       `json:"created_at"`
	RefreshedAt   time.Time       `json:"refreshed_at,omitempty"`
	PoolEnabled   *bool           `json:"pool_enabled,omitempty"`
	Token         *oauth.Token    `json:"-"`
	TokenReadable bool            `json:"-"`
	TokenJSON     json.RawMessage `json:"token,omitempty"` // ciphertext form
	// seq is the in-memory identity generation, bumped on every replacement,
	// so a superseded sign-in completion can tell it is no longer the live
	// login. It is never persisted.
	seq uint64
}

// LoginVault stores and refreshes subscription logins, and is the engine's
// CredentialSource. It satisfies gateway.CredentialSource structurally; the
// compile-time assertion is at the bottom of this file.
type LoginVault struct {
	dir string
	// key is the gateway master key, shared with the API-key vault so one
	// file compromise and one 0600 policy cover both credential kinds.
	key []byte

	mu     sync.Mutex
	logins map[string]*StoredLogin

	// now is injected in tests.
	now func() time.Time
	// refresh is the platform refresh seam, overridden in tests.
	refresh func(ctx context.Context, provider string, token *oauth.Token) (*oauth.Token, error)
	// seq issues monotonic identity generations for stored logins so a
	// completion can recognise a later replacement of the same provider.
	seq uint64
	// writeState persists the encoded file. It is the atomic temp+rename
	// writer in production and a fault-injection seam in tests.
	writeState func(path string, blob []byte) error
}

// OpenLoginVault loads (or initializes) logins.json under dir using the
// gateway master key resolved the same way the key vault resolves it.
func OpenLoginVault(dir string) (*LoginVault, error) {
	key, _, err := resolveVaultMasterKey(dir)
	if err != nil {
		return nil, fmt.Errorf("login vault needs the gateway master key: %w", err)
	}
	v := &LoginVault{
		dir:    dir,
		key:    key,
		logins: map[string]*StoredLogin{},
		now:    time.Now,
	}
	v.refresh = refreshLogin
	state, err := v.loadStateLocked()
	if err != nil {
		return nil, err
	}
	v.logins = state
	return v, nil
}

// errNoVaultChange lets a transaction body report "nothing to persist" so the
// vault neither rewrites the file nor treats the no-op as a failure.
var errNoVaultChange = errors.New("login vault: no change")

// loginLockTimeout bounds how long a mutation waits for a sibling process to
// finish its own vault write before giving up.
const loginLockTimeout = 5 * time.Second

func (v *LoginVault) nowFn() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

// loadStateLocked reads and decrypts the on-disk logins into a fresh map. The
// caller holds v.mu (or is still constructing the vault). Every object is
// freshly allocated so it never aliases the live map.
func (v *LoginVault) loadStateLocked() (map[string]*StoredLogin, error) {
	state := map[string]*StoredLogin{}
	if v.dir == "" {
		return state, nil
	}
	blob, err := os.ReadFile(filepath.Join(v.dir, loginFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return state, nil
	case err != nil:
		return nil, fmt.Errorf("read logins: %w", err)
	}
	var disk []StoredLogin
	if err := json.Unmarshal(blob, &disk); err != nil {
		return nil, fmt.Errorf("parse logins: %w", err)
	}
	for i := range disk {
		l := disk[i]
		if len(l.TokenJSON) > 0 {
			tok, err := v.decryptToken(l.TokenJSON)
			if err != nil {
				// A login that cannot be decrypted is not dropped: the
				// operator sees it as an error row and re-authenticates,
				// rather than silently losing the account reference.
				slog.Error("Could not decrypt a stored login", "provider", l.Provider, "error", err)
				l.Token = nil
			} else {
				l.Token = tok
			}
		}
		state[l.Provider] = &l
	}
	return state, nil
}

// saveStateLocked encrypts and atomically writes state. The caller holds v.mu.
func (v *LoginVault) saveStateLocked(state map[string]*StoredLogin) error {
	out := make([]StoredLogin, 0, len(state))
	for _, l := range state {
		cp := *l
		if l.Token != nil {
			blob, err := v.encryptToken(l.Token)
			if err != nil {
				return err
			}
			cp.TokenJSON = blob
		} else {
			cp.TokenJSON = nil
		}
		cp.Token = nil
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return v.writeFileAtomic(filepath.Join(v.dir, loginFile), blob)
}

// writeFileAtomic replaces the file in one rename. The writeState seam lets a
// test force a persistence failure to prove rollback; a dir-less vault (unit
// tests that never persist) is a no-op.
func (v *LoginVault) writeFileAtomic(path string, blob []byte) error {
	if v.writeState != nil {
		return v.writeState(path, blob)
	}
	if v.dir == "" {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// acquireFileLock takes the cross-process exclusive lock guarding this vault's
// directory so two port daemons cannot interleave read-modify-write cycles on
// logins.json. It reuses the gofrs/flock convention already used for the pid
// record and index-refresh lock. A dir-less vault has nothing to guard.
func (v *LoginVault) acquireFileLock() (func(), error) {
	if v.dir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create login vault dir: %w", err)
	}
	lock := flock.New(filepath.Join(v.dir, loginFile) + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), loginLockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("login vault lock was not acquired")
	}
	return func() { _ = lock.Unlock() }, nil
}

// transact serializes a single provider mutation across every gateway process
// sharing this directory. It reloads the latest disk state so a sibling
// daemon's writes to unrelated providers survive, applies exactly one change,
// atomically replaces the file, and only then swaps the in-memory map. A
// persistence failure leaves the prior in-memory state intact - the caller's
// prior live login is never lost to a failed write.
func (v *LoginVault) transact(mutate func(state map[string]*StoredLogin) (uint64, error)) (uint64, error) {
	release, err := v.acquireFileLock()
	if err != nil {
		return 0, err
	}
	defer release()

	v.mu.Lock()
	defer v.mu.Unlock()

	merged, err := v.loadStateLocked()
	if err != nil {
		return 0, err
	}
	// Disk is authoritative: a provider absent from the reloaded file was
	// forgotten (possibly by another process) and must not be resurrected. Only
	// the identity generation of a provider that survived the reload is carried
	// forward, so a completion's supersession check stays stable across reloads.
	for p, cur := range v.logins {
		if d, ok := merged[p]; ok {
			d.seq = cur.seq
		}
	}
	seq, err := mutate(merged)
	if err != nil {
		if errors.Is(err, errNoVaultChange) {
			return 0, nil
		}
		return 0, err
	}
	if err := v.saveStateLocked(merged); err != nil {
		return 0, err // rollback: v.logins is left untouched
	}
	v.logins = merged
	return seq, nil
}

// sameStoredToken reports whether two tokens are the same stored credential -
// the same access and refresh grant. A refresh compares the login it started
// from against the currently stored one to detect a replacement it must not
// overwrite.
func sameStoredToken(a, b *oauth.Token) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken
}

// persistRefresh durably applies a refreshed token as a compare-and-swap. It
// reloads the authoritative disk state and overwrites the login only while the
// stored credential is still the one that was refreshed. A newer sign-in that
// replaced it is adopted and its token returned rather than clobbered; a
// forgotten login is not resurrected. Memory is swapped only after a durable
// write, or when adopting authoritative disk state that needs no write.
func (v *LoginVault) persistRefresh(provider string, before, fresh *oauth.Token) (*oauth.Token, error) {
	release, err := v.acquireFileLock()
	if err != nil {
		return fresh, err
	}
	defer release()

	v.mu.Lock()
	defer v.mu.Unlock()

	return v.persistRefreshLocked(provider, before, fresh)
}

// persistRefreshLocked is persistRefresh's body once both locks are held: the
// caller MUST already hold the cross-process file lock and v.mu. It exists so
// the refresh path can persist a rotation under the SAME file lock it held for
// the network exchange. acquireFileLock uses gofrs/flock, which contends across
// open file descriptions even within one process, so a second acquire from the
// same goroutine would block until it timed out; callers not already under the
// lock go through the persistRefresh wrapper instead.
func (v *LoginVault) persistRefreshLocked(provider string, before, fresh *oauth.Token) (*oauth.Token, error) {
	merged, err := v.loadStateLocked()
	if err != nil {
		return fresh, err
	}
	for p, cur := range v.logins {
		if d, ok := merged[p]; ok {
			d.seq = cur.seq
		}
	}
	cur, ok := merged[provider]
	switch {
	case !ok:
		// Forgotten while we refreshed; converge memory to disk without
		// resurrecting it. Serve the freshly minted token to this request.
		v.logins = merged
		return fresh, nil
	case !sameStoredToken(cur.Token, before):
		// A newer sign-in replaced the login we refreshed. Adopt the current
		// stored login and serve its token; never overwrite it with our stale
		// refresh.
		v.logins = merged
		return cur.Token, nil
	default:
		cur.Token = fresh
		cur.RefreshedAt = v.nowFn()
		if err := v.saveStateLocked(merged); err != nil {
			// Rollback: leave the prior in-memory state untouched; the request
			// still receives the fresh token and the next dispatch retries.
			return fresh, err
		}
		v.logins = merged
		return fresh, nil
	}
}

// commitLogin atomically stores (or replaces) a provider's login and persists
// it under the cross-process lock. It returns the login's identity generation
// so a sign-in completion can later tell whether its write is still the live
// one. A persistence failure restores the prior login.
func (v *LoginVault) commitLogin(provider string, token *oauth.Token, account string) (uint64, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	enabled := true
	return v.transact(func(state map[string]*StoredLogin) (uint64, error) {
		v.seq++
		state[provider] = &StoredLogin{
			Provider: provider, Account: account, Kind: kindFor(provider),
			CreatedAt: v.nowFn(), PoolEnabled: &enabled, Token: token, seq: v.seq,
		}
		return v.seq, nil
	})
}

// isCurrentLogin reports whether the stored login for provider is still the one
// identified by seq. A later replacement bumps the identity generation, so a
// superseded sign-in completion learns it must not report stale success.
func (v *LoginVault) isCurrentLogin(provider string, seq uint64) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	v.mu.Lock()
	defer v.mu.Unlock()
	l, ok := v.logins[provider]
	return ok && l.seq == seq
}

func (v *LoginVault) aead() (cipher.AEAD, error) { return newVaultAEAD(v.key) }

func (v *LoginVault) encryptToken(t *oauth.Token) (json.RawMessage, error) {
	plain, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	aead, err := v.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, plain, []byte(loginFile))
	return json.Marshal(base64.RawStdEncoding.EncodeToString(append(nonce, sealed...)))
}

func (v *LoginVault) decryptToken(blob json.RawMessage) (*oauth.Token, error) {
	var enc string
	if err := json.Unmarshal(blob, &enc); err != nil {
		return nil, err
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	aead, err := v.aead()
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("stored login token is truncated")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(loginFile))
	if err != nil {
		return nil, err
	}
	var t oauth.Token
	if err := json.Unmarshal(plain, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Put stages (or replaces) a login's token in memory. It does not persist: the
// sign-in flow commits atomically through commitLogin, and the refresh path
// persists rotations. Callers that need durability must go through those paths.
func (v *LoginVault) Put(provider string, token *oauth.Token, account string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	enabled := true
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seq++
	v.logins[provider] = &StoredLogin{
		Provider: provider, Account: account, Kind: kindFor(provider),
		CreatedAt: v.now(), PoolEnabled: &enabled, Token: token, seq: v.seq,
	}
}

// Remove deletes a login. Missing is not an error - the point is that it is
// gone.
func (v *LoginVault) Remove(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.logins[provider]; !ok {
		return false
	}
	delete(v.logins, provider)
	return true
}

// SetPoolEnabled remembers whether a signed-in account should be routable.
// Keeping this preference with the login prevents an explicit withdrawal from
// being undone on the next gateway start. The change is applied under the same
// cross-process transaction as a sign-in, so a sibling daemon's writes survive
// and a failed persist does not leave a half-applied preference.
func (v *LoginVault) SetPoolEnabled(provider string, enabled bool) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	_, err := v.transact(func(state map[string]*StoredLogin) (uint64, error) {
		login, ok := state[provider]
		if !ok {
			return 0, fmt.Errorf("no %s login is stored", provider)
		}
		login.PoolEnabled = &enabled
		return login.seq, nil
	})
	return err
}

// Get returns the stored (possibly expired) login without refreshing.
func (v *LoginVault) Get(provider string) (StoredLogin, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	v.mu.Lock()
	defer v.mu.Unlock()
	l, ok := v.logins[provider]
	if !ok {
		return StoredLogin{}, false
	}
	cp := *l
	return cp, true
}

// List returns every stored login's safe metadata. TokenReadable records
// whether the encrypted token loaded successfully without returning the token.
func (v *LoginVault) List() []StoredLogin {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]StoredLogin, 0, len(v.logins))
	for _, l := range v.logins {
		cp := *l
		cp.TokenReadable = l.Token != nil
		cp.Token = nil
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

func kindFor(provider string) LoginKind {
	for _, p := range LoginPlatforms() {
		if p.ID == provider {
			return p.Kind
		}
	}
	return LoginManual
}

// authoritativeLogin reloads the shared on-disk state under the cross-process
// lock and converges the in-memory map to it before returning the stored token
// for provider. Disk is authoritative: a login a sibling process forgot is gone
// here too, so a cached credential is never served after another process
// invalidated it. Surviving providers keep their in-memory identity generation,
// matching transact and persistRefresh. A dir-less vault has no shared state, so
// its in-memory copy is authoritative and returned as-is. A disk-backed vault
// that cannot take the lock or reload the file fails closed - reporting the
// login missing - because it cannot confirm a sibling has not forgotten it, and
// serving a possibly-invalidated credential is worse than a transient miss.
func (v *LoginVault) authoritativeLogin(provider string) (*oauth.Token, bool) {
	if v.dir == "" {
		return v.cachedToken(provider)
	}
	release, err := v.acquireFileLock()
	if err != nil {
		// Fail closed: without the cross-process lock the reconcile cannot run,
		// so a forget by a sibling may be invisible. Report the login missing
		// rather than serve a credential we can no longer vouch for.
		slog.Warn("Could not lock the login vault to reconcile; reporting the login missing", "provider", provider, "error", err)
		return nil, false
	}
	defer release()

	v.mu.Lock()
	defer v.mu.Unlock()

	return v.authoritativeLoginLocked(provider)
}

// authoritativeLoginLocked is authoritativeLogin's reconcile body once both
// locks are held: the caller MUST already hold the cross-process file lock and
// v.mu. The refresh path uses it to re-read the shared state under the file lock
// it already took for the exchange, without re-entering acquireFileLock (flock
// is not re-entrant across open file descriptions, even in one process). A
// dir-less vault has no shared state, so its in-memory copy is authoritative.
func (v *LoginVault) authoritativeLoginLocked(provider string) (*oauth.Token, bool) {
	if v.dir == "" {
		if l, ok := v.logins[provider]; ok {
			return l.Token, true
		}
		return nil, false
	}
	merged, err := v.loadStateLocked()
	if err != nil {
		// Fail closed: a reload failure cannot distinguish a live login from one
		// a sibling forgot, so do not serve the stale in-memory cache.
		slog.Warn("Could not reload the login vault to reconcile; reporting the login missing", "provider", provider, "error", err)
		return nil, false
	}
	for p, cur := range v.logins {
		if d, ok := merged[p]; ok {
			d.seq = cur.seq
		}
	}
	v.logins = merged
	if l, ok := merged[provider]; ok {
		return l.Token, true
	}
	return nil, false
}

// cachedToken returns the in-memory token for provider without touching disk.
func (v *LoginVault) cachedToken(provider string) (*oauth.Token, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if l, ok := v.logins[provider]; ok {
		return l.Token, true
	}
	return nil, false
}

// liveToken returns a fresh, unexpired token, refreshing through the platform's
// own endpoint when needed. The fast path reconciles against the
// disk-authoritative shared state and serves an unexpired token cheaply. When a
// refresh is required it serializes the WHOLE exchange - re-check, network
// rotation, and persist - under the cross-process file lock, because a rotating
// single-use refresh token (Charm Hyper) is invalidated by its first exchange:
// two gateway processes sharing this vault must not both rotate the same grant,
// or the loser's chain breaks until the user signs in again. The in-process
// single-flight collapses same-process refreshers before they queue on the file
// lock. v.mu is taken only for the two brief in-memory steps and is never held
// across the network exchange, so other vault readers are not blocked for the
// exchange timeout.
func (v *LoginVault) liveToken(ctx context.Context, provider string) (*oauth.Token, error) {
	// Fast path: reconcile against disk and serve an unexpired token without a
	// refresh, keeping the common dispatch cost a single lock+reload.
	tok, ok := v.authoritativeLogin(provider)
	if !ok || tok == nil {
		return nil, fmt.Errorf("no %s login is stored", provider)
	}
	if !tok.IsExpired() {
		return tok, nil
	}

	// Slow path. Collapse same-process concurrent refreshers first so they queue
	// here instead of all contending on the cross-process file lock.
	flight := v.flightFor(provider)
	flight.mu.Lock()
	defer flight.mu.Unlock()

	// Serialize the exchange across every process sharing this vault. Fail closed
	// on a lock error: exchanging without the lock is exactly the concurrent
	// double-rotation this guards against. The lock spans the re-check, the
	// network exchange, and the persist below.
	release, err := v.acquireFileLock()
	if err != nil {
		return nil, err
	}
	defer release()

	// (a) Re-read authoritative disk state under the lock. A sibling that
	// refreshed while we waited for the lock has already written a fresh token;
	// adopt it and skip the exchange entirely, never rotating a grant twice. Hold
	// v.mu only for this in-memory step, releasing it before the network call.
	v.mu.Lock()
	tok, ok = v.authoritativeLoginLocked(provider)
	if !ok || tok == nil {
		v.mu.Unlock()
		return nil, fmt.Errorf("no %s login is stored", provider)
	}
	before := tok
	expired := tok.IsExpired()
	v.mu.Unlock()
	if !expired {
		return before, nil
	}

	// (b) Network exchange with NO v.mu held, but still under the file lock, so
	// no sibling process can rotate the same grant concurrently.
	fresh, err := v.refresh(ctx, provider, before)
	if err != nil {
		return nil, err
	}
	// A provider that does not rotate on refresh returns a token with no refresh
	// grant (Hyper's /token/exchange omits it). Keep the working one so the next
	// refresh still has a grant; a provider that does rotate supplies a new one
	// that overrides it here.
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = before.RefreshToken
	}
	fresh.SetExpiresAt()

	// (c) Persist the rotation as a compare-and-swap under the same file lock,
	// re-taking v.mu only for the durable write. persistRefreshLocked does not
	// re-acquire the file lock, which this goroutine already holds.
	v.mu.Lock()
	token, err := v.persistRefreshLocked(provider, before, fresh)
	v.mu.Unlock()
	if err != nil {
		slog.Warn("Refreshed a login but could not persist it", "provider", provider, "error", err)
	}
	return token, nil
}

type flight struct {
	mu sync.Mutex
}

var (
	flightsMu sync.Mutex
	flights   = map[string]*flight{} // dir\x00provider -> single-flight
)

func (v *LoginVault) flightFor(provider string) *flight {
	flightsMu.Lock()
	defer flightsMu.Unlock()
	key := v.dir + "\x00" + provider
	f, ok := flights[key]
	if !ok {
		f = &flight{}
		flights[key] = f
	}
	return f
}

// refreshLogin rotates a stored token through the platform's refresh path.
func refreshLogin(ctx context.Context, provider string, token *oauth.Token) (*oauth.Token, error) {
	switch provider {
	case "anthropic":
		return anthropic.RefreshToken(ctx, token.RefreshToken)
	case "openai":
		return openaiflow.RefreshToken(ctx, token.RefreshToken)
	case "hyper":
		return hyper.ExchangeToken(ctx, token.RefreshToken)
	case "copilot":
		return copilot.RefreshToken(ctx, token.RefreshToken)
	}
	return nil, fmt.Errorf("no refresh flow is known for %s; sign in again", provider)
}

// Credential resolves the live bearer for a linked pool row. It is the
// gateway.CredentialSource contract, implemented by the vault this binary owns.
func (v *LoginVault) Credential(ctx context.Context, provider string) (string, bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	platform, known := platformByID(provider)
	if !known {
		return "", false, nil
	}
	route := platform.RoutesTo
	if route == "" {
		// Known but not spendable: report it as unknown capacity rather than
		// handing out a bearer no adapter can use.
		return "", false, nil
	}
	token, err := v.liveToken(ctx, provider)
	if err != nil {
		return "", true, err
	}
	if token.AccessToken == "" {
		return "", true, fmt.Errorf("the %s login holds no access token; sign in again", provider)
	}
	return token.AccessToken, true, nil
}

// Linkable lists logins the pool can borrow.
func (v *LoginVault) Linkable(_ context.Context) []LinkableProvider {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]LinkableProvider, 0, len(v.logins))
	for _, l := range v.logins {
		platform, known := platformByID(l.Provider)
		if !known {
			continue
		}
		detail := l.Account
		if detail == "" {
			detail = platform.AccountHint
		}
		if l.Token == nil {
			detail = "stored but unreadable - sign in again"
		}
		poolEnabled := l.PoolEnabled == nil || *l.PoolEnabled
		out = append(out, LinkableProvider{
			ID: platform.ID, Name: platform.Name, Kind: "oauth", Detail: detail,
			PoolManaged: true, PoolEnabled: poolEnabled,
		})
	}
	return out
}

var loginModelsClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Models lists every model the signed-in account can actually serve, using the
// curated fallback when live discovery is unavailable. It is the
// non-authoritative wrapper over discoverModels.
func (v *LoginVault) Models(ctx context.Context, provider string) []LinkedModel {
	models, _ := v.discoverModels(ctx, provider)
	return models
}

// ModelsAuthoritative reports the account's model set together with whether that
// set is authoritative - a real provider discovery (OpenAI's Codex catalogue or
// the OpenAI-shaped /v1/models list) succeeded - a valid empty catalog counts,
// so a subscription that stopped offering models is retired. It is
// false whenever discovery fell back to the curated set or hit an error, so a
// caller reconciling stored rows never retires a model on a transient outage.
func (v *LoginVault) ModelsAuthoritative(ctx context.Context, provider string) ([]LinkedModel, bool) {
	return v.discoverModels(ctx, provider)
}

// discoverModels performs live model discovery for a signed-in platform. The
// second return is true only when a real discovery succeeded; every fallback and
// error path returns false so retirement logic can distinguish "the account no
// longer offers this model" from "discovery was briefly unavailable".
func (v *LoginVault) discoverModels(ctx context.Context, provider string) ([]LinkedModel, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	platform, known := platformByID(provider)
	if !known {
		return nil, false
	}
	route, err := v.liveToken(ctx, provider)
	if err != nil {
		slog.Warn("Could not resolve a login credential for model discovery", "provider", provider, "error", err)
		return nil, false
	}
	if provider == "openai" {
		models, err := openaiflow.FetchModels(ctx, route)
		if err != nil {
			slog.Warn("ChatGPT model discovery failed", "error", err)
			return nil, false
		}
		out := make([]LinkedModel, 0, len(models))
		for _, model := range models {
			out = append(out, LinkedModel{
				ID: model.ID, Name: model.Name,
				ContextWindow: model.ContextWindow,
				MaxTokens:     model.DefaultMaxTokens,
				CanReason:     model.CanReason,
				Attachments:   model.SupportsImages,
				Tools:         true,
			})
		}
		return out, true
	}
	if platform.BaseModelsURL == "" {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, platform.BaseModelsURL, nil)
	if err != nil {
		return nil, false
	}
	if provider == "anthropic" {
		anthropic.ApplyAuthHeaders(req.Header, route.AccessToken)
	} else {
		req.Header.Set("Authorization", "Bearer "+route.AccessToken)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := loginModelsClient.Do(req)
	if err != nil {
		slog.Warn("Login model discovery failed", "provider", provider, "error", err)
		return fallbackLoginModels(provider), false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("Login model discovery returned a non-200", "provider", provider, "status", resp.StatusCode)
		return fallbackLoginModels(provider), false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fallbackLoginModels(provider), false
	}
	models, parsed := parseLoginModelList(body)
	if !parsed {
		return fallbackLoginModels(provider), false
	}
	if provider == "anthropic" {
		models = enrichAnthropicModels(models)
	}
	// A valid 200 that parses to an empty list is authoritative: the account
	// serves nothing now, and the reconcile retires its rows. Only an uncertain
	// response (transport, non-200, read or decode failure) falls back.
	return models, true
}

// parseLoginModelList reads the OpenAI list shape both routed platforms serve.
// The bool reports whether the response is an AUTHORITATIVE catalogue: a
// present, valid `data` array - even empty - is authoritative, so reconciliation
// may retire absent rows. A body that is not valid JSON, or whose required
// `data` field is absent or wrong-typed (an error payload, a schema change), is
// NOT authoritative and must never retire the login's models.
func parseLoginModelList(body []byte) ([]LinkedModel, bool) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false
	}
	if !isJSONArray(envelope.Data) {
		return nil, false
	}
	var items []struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		DisplayName   string `json:"display_name"`
		ContextWindow int64  `json:"context_window"`
		MaxTokens     int64  `json:"max_tokens"`
	}
	if err := json.Unmarshal(envelope.Data, &items); err != nil {
		return nil, false
	}
	out := make([]LinkedModel, 0, len(items))
	for _, m := range items {
		if m.ID == "" {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		lm := LinkedModel{
			ID: m.ID, Name: name,
			ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens,
			// A subscription's tokens are already paid for: price zero reads
			// as free-to-route, which is exactly what it is.
			Tools: true,
		}
		if strings.Contains(m.ID, "thinking") || strings.Contains(m.ID, "reason") ||
			strings.Contains(m.ID, "o1") || strings.Contains(m.ID, "o3") ||
			strings.Contains(m.ID, "o4") {
			lm.CanReason = true
		}
		out = append(out, lm)
	}
	return out, true
}

// isJSONArray reports whether raw is a present JSON array. An absent field
// (nil), JSON null, or any non-array value all fail, so a required catalogue
// array the response omits or mistypes is never read as an authoritative empty
// set that would retire the login's models.
func isJSONArray(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

// This curated September 2026 fallback is used only when Anthropic's
// account-scoped /v1/models discovery is unavailable. Discovery remains
// authoritative; the fallback prevents a transient catalogue outage from
// enrolling a credential with zero routable models.
var anthropicFallbackModels = []LinkedModel{
	{ID: "claude-opus-5", Name: "Claude Opus 5", ContextWindow: 1_000_000, MaxTokens: 128_000, CanReason: true, Attachments: true, Tools: true, Flagship: true},
	{ID: "claude-fable-5", Name: "Claude Fable 5", ContextWindow: 1_000_000, MaxTokens: 128_000, CanReason: true, Attachments: true, Tools: true},
	{ID: "claude-mythos-5", Name: "Claude Mythos 5", ContextWindow: 1_000_000, MaxTokens: 128_000, CanReason: true, Attachments: true, Tools: true},
	{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", ContextWindow: 1_000_000, MaxTokens: 128_000, CanReason: true, Attachments: true, Tools: true},
	{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5", ContextWindow: 200_000, MaxTokens: 64_000, CanReason: true, Attachments: true, Tools: true, Small: true},
}

func fallbackLoginModels(provider string) []LinkedModel {
	if provider != "anthropic" {
		return nil
	}
	return append([]LinkedModel(nil), anthropicFallbackModels...)
}

func enrichAnthropicModels(models []LinkedModel) []LinkedModel {
	known := make(map[string]LinkedModel, len(anthropicFallbackModels))
	for _, model := range anthropicFallbackModels {
		known[model.ID] = model
	}
	for i := range models {
		if metadata, ok := known[models[i].ID]; ok {
			name := models[i].Name
			models[i] = metadata
			if name != "" && name != models[i].ID {
				models[i].Name = name
			}
			continue
		}
		models[i].Tools = true
		models[i].Attachments = true
		// Every modern Claude model supports extended thinking; discovery does
		// not advertise it, so a model outside the curated set would otherwise
		// read as non-reasoning and have its reasoning request stripped before
		// the Anthropic transform could turn it into a thinking budget - the
		// "opus is far dumber through the gateway" report.
		models[i].CanReason = true
	}
	return models
}

func platformByID(id string) (LoginPlatform, bool) {
	for _, p := range LoginPlatforms() {
		if p.ID == id {
			return p, true
		}
	}
	return LoginPlatform{}, false
}

// PendingLogin is an interactive flow in progress: the browser URL the user
// must open, and the wait that completes it into a stored login.
type PendingLogin struct {
	Provider   string
	URL        string
	UserCode   string // device flows only
	Expiration time.Duration

	vault    *LoginVault
	complete func(ctx context.Context) (*oauth.Token, error)
	flow     *browserflow.Flow
}

// Wait finishes the flow and stores the resulting login atomically.
func (p *PendingLogin) Wait(ctx context.Context) error {
	token, err := p.complete(ctx)
	if err != nil {
		return err
	}
	token.SetExpiresAt()
	_, err = p.vault.commitLogin(p.Provider, token, accountFor(p.Provider, token))
	return err
}

// Cancel releases the flow's resources (the loopback listener for browser
// flows).
func (p *PendingLogin) Cancel() {
	if p.flow != nil {
		_ = p.flow.Close()
	}
}

// StartLogin begins the interactive flow for a platform. Browser flows bind
// their loopback callback now; device flows have already fetched their code.
func (v *LoginVault) StartLogin(ctx context.Context, provider string) (*PendingLogin, error) {
	platform, known := platformByID(provider)
	if !known {
		return nil, fmt.Errorf("unknown login platform %q", provider)
	}
	switch platform.ID {
	case "anthropic":
		flow, err := anthropic.Start(ctx)
		if err != nil {
			return nil, err
		}
		return &PendingLogin{Provider: platform.ID, URL: flow.URL, vault: v,
			complete: func(ctx context.Context) (*oauth.Token, error) { return flow.Wait(ctx) },
			flow:     flow}, nil
	case "openai":
		flow, err := openaiflow.Start(ctx)
		if err != nil {
			return nil, err
		}
		return &PendingLogin{Provider: platform.ID, URL: flow.URL, vault: v,
			complete: func(ctx context.Context) (*oauth.Token, error) { return flow.Wait(ctx) },
			flow:     flow}, nil
	case "hyper":
		auth, err := hyper.InitiateDeviceAuth(ctx)
		if err != nil {
			return nil, err
		}
		return &PendingLogin{Provider: platform.ID, URL: auth.VerificationURL,
			UserCode: auth.UserCode, Expiration: time.Duration(auth.ExpiresIn) * time.Second, vault: v,
			complete: func(ctx context.Context) (*oauth.Token, error) {
				refreshToken, err := hyper.PollForToken(ctx, auth.DeviceCode, auth.ExpiresIn)
				if err != nil {
					return nil, err
				}
				token, err := hyper.ExchangeToken(ctx, refreshToken)
				if err != nil {
					return nil, err
				}
				check, err := hyper.IntrospectToken(ctx, token.AccessToken)
				if err != nil {
					return nil, fmt.Errorf("token introspection failed: %w", err)
				}
				if !check.Active {
					return nil, errors.New("hyper says the access token is not active")
				}
				token.RefreshToken = refreshToken
				return token, nil
			}}, nil
	case "copilot":
		dc, err := copilot.RequestDeviceCode(ctx)
		if err != nil {
			return nil, err
		}
		return &PendingLogin{Provider: platform.ID, URL: dc.VerificationURI,
			UserCode: dc.UserCode, Expiration: time.Duration(dc.ExpiresIn) * time.Second, vault: v,
			complete: func(ctx context.Context) (*oauth.Token, error) {
				return copilot.PollForToken(ctx, dc)
			}}, nil
	}
	return nil, fmt.Errorf("no flow is implemented for %s", platform.ID)
}

// accountFor extracts a human label from a token's fields where the provider
// put one there (Charm Hyper's introspection names the org; Claude's account id
// is an opaque suffix worth showing).
func accountFor(provider string, token *oauth.Token) string {
	switch provider {
	case "openai":
		if email := emailFromIDToken(token.IDToken); email != "" {
			return email
		}
	}
	if token.AccountID != "" {
		return "account " + short(token.AccountID)
	}
	return ""
}

// emailFromIDToken reads the email claim out of an OIDC id_token without
// verifying anything - it is display sugar for the person who just completed
// the flow in their own browser on their own machine.
func emailFromIDToken(idToken string) string {
	if idToken == "" {
		return ""
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Email
}

func short(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

var _ CredentialSource = (*LoginVault)(nil)
