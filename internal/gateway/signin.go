package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sign-in flows over HTTP. The TUI is a pure client of the gateway surface -
// attached to a daemon or driving an ephemeral service, the same endpoints
// answer - so a browser OAuth flow must be startable, pollable and cancellable
// through the API, not only inside the process that owns the vault.

// SignInState is where a started flow stands.
type SignInState string

const (
	SignInPending   SignInState = "pending"
	SignInComplete  SignInState = "complete"
	SignInFailed    SignInState = "failed"
	SignInCancelled SignInState = "cancelled"
)

// SignInSession is the API-visible view of a running flow.
type SignInSession struct {
	ID        string      `json:"id"`
	Provider  string      `json:"provider"`
	URL       string      `json:"url"`
	UserCode  string      `json:"user_code,omitempty"`
	Kind      string      `json:"kind"` // browser | device
	State     SignInState `json:"state"`
	Error     string      `json:"error,omitempty"`
	Account   string      `json:"account,omitempty"`
	ExpiresAt time.Time   `json:"expires_at"`
}

type signInFlow struct {
	session SignInSession
	pending *PendingLogin
	cancel  context.CancelFunc
	expiry  *time.Timer

	mu         sync.Mutex
	resolvedAt time.Time // zero while the flow still runs
}

func (f *signInFlow) resolved() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.resolvedAt.IsZero()
}

// SignInRegistry owns the flows in progress on this process.
type SignInRegistry struct {
	vault      *LoginVault
	startLogin func(context.Context, string) (*PendingLogin, error)
	onComplete func(string)
	// grace is the buffer beyond a flow's provider deadline before it is
	// force-cancelled to release its callback listener. A field so tests can
	// shrink it.
	grace time.Duration

	mu    sync.Mutex
	flows map[string]*signInFlow
}

// NewSignInRegistry builds the registry over a vault.
func NewSignInRegistry(v *LoginVault) *SignInRegistry {
	return &SignInRegistry{
		vault:      v,
		startLogin: v.StartLogin,
		flows:      map[string]*signInFlow{},
		grace:      10 * time.Minute,
	}
}

// SetOnComplete installs the convergence hook run after a login is safely
// persisted. The hook runs outside the flow lock.
func (r *SignInRegistry) SetOnComplete(fn func(string)) {
	r.mu.Lock()
	r.onComplete = fn
	r.mu.Unlock()
}

// Start launches the provider's interactive flow. The completion (token
// exchange and vault write) happens in a goroutine the caller never joins;
// Status reports the result, and the pool's backfill picks the login up the
// moment it lands.
func (r *SignInRegistry) Start(ctx context.Context, provider string) (SignInSession, error) {
	if err := ctx.Err(); err != nil {
		return SignInSession{}, err
	}
	// Release the listeners of flows that already expired before binding a
	// fresh loopback callback, so a stale browser flow cannot still hold the
	// port the new one needs.
	r.mu.Lock()
	r.pruneLocked()
	r.mu.Unlock()

	// The HTTP request only starts the flow. Its context is cancelled as soon
	// as the response is written, while the browser callback must remain alive
	// until the user completes or cancels it.
	flowCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	pending, err := r.startLogin(flowCtx, provider)
	if err != nil {
		cancel()
		return SignInSession{}, err
	}
	id := newFlowID()
	f := &signInFlow{
		session: SignInSession{
			ID: id, Provider: pending.Provider, URL: pending.URL, UserCode: pending.UserCode,
			Kind:  string(r.vault.platformKind(pending.Provider)),
			State: SignInPending, ExpiresAt: time.Now().Add(pending.Expiration + r.grace),
		},
		pending: pending,
		cancel:  cancel,
	}
	// Force the flow closed at its deadline so an abandoned browser flow
	// releases its callback listener even if no client ever polls it again.
	f.expiry = time.AfterFunc(time.Until(f.session.ExpiresAt), func() {
		f.cancel()
		f.pending.Cancel()
	})

	r.mu.Lock()
	r.flows[id] = f
	r.mu.Unlock()

	go func() {
		token, err := f.pending.complete(flowCtx)
		f.mu.Lock()
		f.resolvedAt = time.Now()
		if err != nil {
			if flowCtx.Err() != nil {
				f.session.State = SignInCancelled
			} else {
				f.session.State = SignInFailed
				f.session.Error = err.Error()
			}
			f.mu.Unlock()
			return
		}
		token.SetExpiresAt()
		account := accountFor(f.session.Provider, token)
		seq, err := f.pending.vault.commitLogin(f.session.Provider, token, account)
		if err != nil {
			f.session.State = SignInFailed
			f.session.Error = "signed in but could not persist the login: " + err.Error()
			slog.Error("Could not persist a completed login", "provider", f.session.Provider, "error", err)
			f.mu.Unlock()
			return
		}
		provider := f.session.Provider
		f.mu.Unlock()

		r.mu.Lock()
		onComplete := r.onComplete
		r.mu.Unlock()
		if onComplete != nil {
			onComplete(provider)
		}

		f.mu.Lock()
		// If another sign-in for the same provider replaced this login while we
		// reconciled, this completion is stale: report the supersession rather
		// than a success whose token is no longer the live one.
		if f.pending.vault.isCurrentLogin(provider, seq) {
			f.session.State = SignInComplete
			f.session.Account = account
		} else {
			f.session.State = SignInFailed
			f.session.Error = "a newer " + provider + " sign-in replaced this one before it completed"
		}
		f.mu.Unlock()
	}()
	return f.snapshot(), nil
}

func (f *signInFlow) snapshot() SignInSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.session
}

// Status returns a copy of the flow's current view. Unknown ids are an error:
// silently reporting "gone" would make a polling client think it won.
func (r *SignInRegistry) Status(id string) (SignInSession, error) {
	r.mu.Lock()
	f, ok := r.flows[id]
	r.mu.Unlock()
	if !ok {
		return SignInSession{}, fmt.Errorf("no sign-in flow %q is known to this gateway", id)
	}
	return f.snapshot(), nil
}

// Cancel aborts a pending flow and forgets it.
func (r *SignInRegistry) Cancel(id string) bool {
	r.mu.Lock()
	f, ok := r.flows[id]
	if ok {
		delete(r.flows, id)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	if f.expiry != nil {
		f.expiry.Stop()
	}
	f.cancel()
	f.pending.Cancel()
	return true
}

// Forget drops a stored login from the vault.
func (r *SignInRegistry) Forget(provider string) (bool, error) {
	return r.vault.Forget(provider)
}

// SetPoolEnabled persists whether this login should contribute routing
// capacity. Reconciliation applies the preference to the key and model pool.
func (r *SignInRegistry) SetPoolEnabled(provider string, enabled bool) error {
	return r.vault.SetPoolEnabled(provider, enabled)
}

// Signable lists every platform the gateway can sign in to, with whether a
// login is stored - the TUI renders one row per platform, signed or not.
func (r *SignInRegistry) Signable() []SignInPlatformRow {
	stored := map[string]StoredLogin{}
	for _, l := range r.vault.List() {
		stored[l.Provider] = l
	}
	var out []SignInPlatformRow
	for _, p := range LoginPlatforms() {
		row := SignInPlatformRow{
			ID: p.ID, Name: p.Name, Kind: string(p.Kind), RoutesTo: p.RoutesTo,
			AccountHint: p.AccountHint,
		}
		if l, ok := stored[p.ID]; ok {
			row.SignedIn = true
			row.Account = l.Account
			row.RefreshedAt = l.RefreshedAt
			if !l.TokenReadable {
				row.Broken = true
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SignInPlatformRow is one signable platform and its stored state.
type SignInPlatformRow struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	RoutesTo    string    `json:"routes_to"`
	AccountHint string    `json:"account_hint,omitempty"`
	SignedIn    bool      `json:"signed_in"`
	Broken      bool      `json:"broken,omitempty"`
	Account     string    `json:"account,omitempty"`
	RefreshedAt time.Time `json:"refreshed_at,omitempty"`
}

func (v *LoginVault) platformKind(provider string) LoginKind { return kindFor(provider) }

// Forget removes a stored login (the pool row is withdrawn separately). The
// deletion runs under the cross-process transaction so a sibling daemon's
// unrelated logins are preserved, and a failed persist leaves the login in
// place rather than half-forgotten.
func (v *LoginVault) Forget(provider string) (bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	existed := false
	_, err := v.transact(func(state map[string]*StoredLogin) (uint64, error) {
		if _, ok := state[provider]; !ok {
			return 0, errNoVaultChange
		}
		existed = true
		delete(state, provider)
		return 0, nil
	})
	if err != nil {
		return false, err
	}
	return existed, nil
}

// liveAccessToken is used by tests to confirm a refresh path without network.
func (v *LoginVault) liveAccessToken(ctx context.Context, provider string) (string, error) {
	t, err := v.liveToken(ctx, strings.ToLower(strings.TrimSpace(provider)))
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

func newFlowID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("flow-%d", time.Now().UnixNano())
	}
	return "f" + hex.EncodeToString(raw)
}

// pruneLocked drops flows that resolved more than five minutes ago - long
// enough for any client that started one to have seen its verdict - and
// cancels ones whose provider deadline has passed. Caller holds r.mu.
func (r *SignInRegistry) pruneLocked() {
	for id, f := range r.flows {
		f.mu.Lock()
		age := time.Duration(0)
		if !f.resolvedAt.IsZero() {
			age = time.Since(f.resolvedAt)
		}
		expired := time.Now().After(f.session.ExpiresAt)
		pending := f.session.State == SignInPending
		f.mu.Unlock()
		if age > 5*time.Minute {
			delete(r.flows, id)
			if f.expiry != nil {
				f.expiry.Stop()
			}
			f.cancel()
			f.pending.Cancel()
		} else if expired && pending {
			f.cancel()
			f.pending.Cancel()
		}
	}
}
