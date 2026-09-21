package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// The pool serves a provider Prowl is already signed in to by storing a
// reference, never a copy: a subscription token is refreshed by the login that
// owns it, so a copy would rot within the hour, and revoking the login must
// revoke the pool's access with it.

// linkedPrefix marks a vault row as a reference rather than ciphertext.
const linkedPrefix = "link:"

// LinkedRef is the provider a linked row points at.
func LinkedRef(stored string) (string, bool) {
	if !strings.HasPrefix(stored, linkedPrefix) {
		return "", false
	}
	ref := strings.TrimSpace(strings.TrimPrefix(stored, linkedPrefix))
	if ref == "" {
		return "", false
	}
	return ref, true
}

// MarkLinked builds the stored form for a linked provider.
func MarkLinked(provider string) string {
	return linkedPrefix + strings.ToLower(strings.TrimSpace(provider))
}

// CredentialSource hands out live credentials for providers Prowl is logged in
// to. The gateway holds the seam; the harness supplies it, so the engine never
// reaches into config or the OAuth flows itself.
type CredentialSource interface {
	// Credential returns a usable bearer token for the provider, refreshing
	// it when the login supports that. The bool reports whether this source
	// knows the provider at all.
	Credential(ctx context.Context, provider string) (string, bool, error)

	// Linkable lists the providers this source can currently serve, which is
	// what the dashboard offers to enroll.
	Linkable(ctx context.Context) []LinkableProvider

	// Models lists what the provider serves, so enrolling it puts real
	// models in the catalogue. Without this an enrolled login is inert: the
	// router has a credential and nothing to route to it.
	Models(ctx context.Context, provider string) []LinkedModel
}

// AuthoritativeModelSource is an optional CredentialSource capability. It
// reports whether a Models() result came from a live provider query - the only
// case in which a login's absent rows may be retired. A cached/fallback list or
// a failed discovery returns authoritative=false, so a transient catalogue
// outage never deletes models the provider still serves.
type AuthoritativeModelSource interface {
	ModelsAuthoritative(ctx context.Context, provider string) ([]LinkedModel, bool)
}

// discoverLoginModels asks the source for a provider's models and whether the
// answer is authoritative. A source that does not implement
// AuthoritativeModelSource is treated as non-authoritative, so retirement never
// fires on it.
func discoverLoginModels(ctx context.Context, src CredentialSource, provider string) ([]LinkedModel, bool) {
	if a, ok := src.(AuthoritativeModelSource); ok {
		return a.ModelsAuthoritative(ctx, provider)
	}
	return src.Models(ctx, provider), false
}

// LinkedModel is one model an enrolled login can serve, carried from the
// provider catalogue the harness already maintains.
type LinkedModel struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64

	// Published prices per million tokens. Zero means the provider lists no
	// price, which is how a subscription or free tier reads.
	InputPerM  float64
	OutputPerM float64

	CanReason   bool
	Attachments bool
	Tools       bool

	// Flagship and Small mark the provider's own default choices for its
	// large and small model, which is the vendor stating which end of its
	// range a model sits at.
	Flagship bool
	Small    bool
}

// LinkableProvider describes a login the pool can borrow. PoolManaged means
// the source owns a durable routing preference; unmanaged sources retain the
// explicit enrol/withdraw API contract.
type LinkableProvider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`   // "oauth" or "api_key"
	Detail      string `json:"detail"` // account or plan, when known
	PoolManaged bool   `json:"poolManaged,omitempty"`
	PoolEnabled bool   `json:"poolEnabled,omitempty"`
}

// SetCredentialSource installs the harness's credential source. The vault
// shares it: a linked row is resolved where the secret is read.
func (e *Engine) SetCredentialSource(src CredentialSource) {
	e.credentials = src
	if e.vault != nil {
		e.vault.credentials = src
	}
	e.ReconcileLoginModels(context.Background())
}

// ReconcileLoginModels converges durable login preferences with linked pool
// keys and their model rows. It is safe to call after startup and after a
// browser sign-in completes.
func (e *Engine) ReconcileLoginModels(ctx context.Context) {
	if e.credentials != nil {
		e.reconcileLoginModels(ctx, e.credentials)
	}
}

func (e *Engine) reconcileLoginModels(ctx context.Context, src CredentialSource) {
	type linked struct {
		id       int64
		platform string
	}

	rows, err := e.store.DB().QueryContext(ctx,
		"SELECT id, platform, encrypted_key FROM api_keys")
	if err != nil {
		return
	}
	byPlatform := map[string]linked{}
	for rows.Next() {
		var (
			id       int64
			platform string
			stored   string
		)
		if err := rows.Scan(&id, &platform, &stored); err != nil {
			break
		}
		if _, isLinked := LinkedRef(stored); isLinked {
			byPlatform[strings.ToLower(platform)] = linked{id: id, platform: platform}
		}
	}
	_ = rows.Close()

	linkable := src.Linkable(ctx)
	managed := map[string]bool{}
	for _, provider := range linkable {
		platform := strings.ToLower(strings.TrimSpace(provider.ID))
		if platform == "" {
			continue
		}
		entry, enrolled := byPlatform[platform]
		if enrolled && strings.TrimSpace(provider.Name) != "" {
			if _, err := e.store.DB().ExecContext(ctx,
				"UPDATE api_keys SET label = ? WHERE id = ? AND label <> ?",
				provider.Name, entry.id, provider.Name); err != nil {
				slog.Debug("Could not relabel a linked key", "platform", platform, "error", err)
			}
		}
		if !provider.PoolManaged {
			continue
		}
		managed[platform] = true
		if !provider.PoolEnabled {
			if enrolled {
				if err := e.vault.Delete(ctx, entry.id); err != nil {
					slog.Warn("Could not withdraw a disabled login", "platform", platform, "error", err)
				} else {
					delete(byPlatform, platform)
				}
			}
			continue
		}
		if !enrolled {
			secret, known, err := src.Credential(ctx, platform)
			if err != nil {
				slog.Warn("Could not activate a connected account", "platform", platform, "error", err)
				continue
			}
			if !known || strings.TrimSpace(secret) == "" {
				continue
			}
			keyID, err := e.vault.AddLinked(ctx, platform, provider.Name)
			if err != nil {
				slog.Warn("Could not add a connected account to routing", "platform", platform, "error", err)
				continue
			}
			entry = linked{id: keyID, platform: platform}
			byPlatform[platform] = entry
		}
		models, authoritative := discoverLoginModels(ctx, src, platform)
		// A NON-authoritative empty list is a fallback/failed discovery, not a
		// statement about what the provider serves, so it neither seeds nor
		// retires. An AUTHORITATIVE empty list IS meaningful - the provider now
		// serves nothing - and must retire every row below.
		if !authoritative && len(models) == 0 {
			continue
		}
		if len(models) > 0 {
			if _, err := e.SeedLoginModels(ctx, entry.id, platform, models); err != nil {
				slog.Warn("Could not refresh a connected account's models",
					"platform", platform, "error", err)
				continue
			}
		}
		// An authoritative discovery reconciles the login's rows to exactly what
		// the provider now serves - retiring absent rows, and retiring ALL rows
		// when the authoritative list is empty. A fallback/failed list never
		// retires; that safety lives in the source returning authoritative=false.
		if authoritative {
			if _, err := e.retireAbsentLoginModels(ctx, entry.id, models); err != nil {
				slog.Warn("Could not retire a connected account's stale models",
					"platform", platform, "error", err)
			}
		}
	}

	// Unmanaged sources preserve the previous explicit-enrol contract. They
	// still receive the historical repair for an already-linked key with no
	// model rows.
	for platform, entry := range byPlatform {
		if managed[platform] || e.LoginModelCount(ctx, entry.id) != 0 {
			continue
		}
		models := src.Models(ctx, platform)
		if len(models) == 0 {
			continue
		}
		if _, err := e.SeedLoginModels(ctx, entry.id, platform, models); err != nil {
			slog.Warn("Could not backfill an enrolled login's models",
				"platform", platform, "error", err)
		}
	}
}

// CredentialSource returns the installed source, or nil.
func (e *Engine) CredentialSource() CredentialSource {
	return e.credentials
}

// resolveLinked turns a stored reference into a live credential.
func (v *KeyVault) resolveLinked(ctx context.Context, ref string) (string, error) {
	if v.credentials == nil {
		return "", fmt.Errorf("provider %q is linked to a Prowl login, but no login source is wired", ref)
	}
	secret, known, err := v.credentials.Credential(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolving the %s login: %w", ref, err)
	}
	if !known {
		return "", fmt.Errorf("Prowl is not logged in to %q any more; sign in again or remove it from the pool", ref)
	}
	if secret == "" {
		return "", fmt.Errorf("the %s login returned no credential", ref)
	}
	return secret, nil
}
