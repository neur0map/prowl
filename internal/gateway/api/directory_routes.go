package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/catalog"
)

func (s *Server) registerDirectoryRoutes() {
	s.mux.HandleFunc("GET /api/providers/directory", s.RequireKey(s.handleProviderDirectory))
	s.mux.HandleFunc("GET /api/providers/{id}/probe", s.RequireKey(s.handleProviderProbe))
}

type directoryRow struct {
	catalog.DirectoryEntry

	// Configured and KeyCount join the operator's own state onto the
	// directory, so one list answers both "what exists" and "what do I have".
	Configured bool `json:"configured"`
	KeyCount   int  `json:"keyCount"`
	// EnabledKeyCount, HealthyKeyCount and ErrorKeyCount break the credential
	// tally down so a row can say whether the keys it holds can actually
	// serve, not merely that some row exists.
	EnabledKeyCount int `json:"enabledKeyCount"`
	HealthyKeyCount int `json:"healthyKeyCount"`
	ErrorKeyCount   int `json:"errorKeyCount"`
	// ModelCount is how many catalogue models this provider actually serves
	// here across chat, embedding and media, which is what separates a
	// provider we can route to from a name.
	ModelCount int `json:"modelCount"`
	// Adapter reports whether the gateway has a wire adapter for it today.
	Adapter bool `json:"adapter"`
	// Platform is the id the key store and router use, which differs from the
	// directory id often enough that the client cannot derive it.
	Platform string `json:"platform"`
	// Keyless providers are served without a credential, so their add form
	// must not demand one.
	Keyless bool `json:"keyless"`
	// Ready is the honest "can route right now": an adapter, at least one
	// served model, and either a credential that passed a real health probe or
	// an enabled keyless sentinel. An unprobed key is connected, not ready.
	Ready bool `json:"ready"`
}

type directoryCounts struct {
	Free    int `json:"free"`
	Credits int `json:"credits"`
	Paid    int `json:"paid"`
	OAuth   int `json:"oauth"`
	Local   int `json:"local"`

	Total      int `json:"total"`
	Configured int `json:"configured"`
	// FreeModels counts only the free and credit classes. A subscription or a
	// paid account is not free capacity, and adding them to one total would
	// overstate what costs nothing.
	FreeModels int `json:"freeModels"`
	// Routable is how many the gateway can proxy to today.
	Routable int `json:"routable"`
	// Ready is Routable made honest: adapter plus a served model plus a usable
	// key. It is the count the storefront leads with, because Routable alone
	// overstates what an operator can actually send a request to.
	Ready int `json:"ready"`
}

// keyTally is the per-platform credential breakdown the directory needs to
// tell a routable provider from one that merely has a key on file.
type keyTally struct {
	total, enabled, healthy, errored int
}

func (s *Server) handleProviderDirectory(w http.ResponseWriter, r *http.Request) {
	entries, err := catalog.Directory()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the provider directory")
		return
	}
	sources, _ := catalog.DirectorySources()

	tally := map[string]keyTally{}
	if rows, err := s.engine.DB().QueryContext(r.Context(),
		`SELECT platform,
		        COUNT(*),
		        SUM(CASE WHEN enabled = 1 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN enabled = 1 AND status = 'healthy' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN enabled = 1 AND status = 'error' THEN 1 ELSE 0 END)
		   FROM api_keys GROUP BY platform`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var platform string
			var t keyTally
			if err := rows.Scan(&platform, &t.total, &t.enabled, &t.healthy, &t.errored); err == nil {
				tally[platform] = t
			}
		}
	}
	// Count every modality a provider serves - chat, embedding and media - so a
	// provider that serves only embeddings or media reads as having served
	// models, and can be ready, rather than looking empty.
	models := catalogModelTotalsByPlatform(r.Context(), s.engine.DB())

	out := make([]directoryRow, 0, len(entries))
	counts := directoryCounts{}
	// The setup surface is an action directory, not a wish list. Keep
	// non-compatible catalogue metadata out, require a live adapter, and show
	// each credential/routing platform once even when upstream source lists
	// carry several marketing names for it.
	seenPlatforms := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if !entry.Routable {
			continue
		}
		platform := directoryPlatform(entry.ID, s)
		if platform == "" {
			continue
		}
		if _, duplicate := seenPlatforms[platform]; duplicate {
			continue
		}
		seenPlatforms[platform] = struct{}{}
		kt := tally[platform]
		row := directoryRow{
			DirectoryEntry:  entry,
			KeyCount:        kt.total,
			EnabledKeyCount: kt.enabled,
			HealthyKeyCount: kt.healthy,
			ErrorKeyCount:   kt.errored,
			ModelCount:      models[platform],
			Adapter:         platform != "",
			Platform:        platform,
		}
		if platform != "" {
			if prov, ok := s.engine.Registry().Resolve(platform, ""); ok {
				row.Keyless = prov.Keyless()
			}
		}
		row.Configured = row.KeyCount > 0
		if row.Configured {
			counts.Configured++
		}
		counts.Total++
		counts.Routable++
		// Unknown credentials remain "connected": saving a key, or listing
		// public models with it, is not proof that inference accepts it. A real
		// health probe must have marked a credential healthy. Keyless adapters
		// are the exception because there is no credential to validate.
		credentialReady := kt.healthy > 0 || (row.Keyless && kt.enabled > 0)
		row.Ready = row.Adapter && row.ModelCount > 0 && credentialReady
		if row.Ready {
			counts.Ready++
		}
		switch entry.Class {
		case catalog.ClassFree:
			counts.Free++
			counts.FreeModels += entry.FreeModels
		case catalog.ClassCredits:
			counts.Credits++
			counts.FreeModels += entry.FreeModels
		case catalog.ClassPaid:
			counts.Paid++
		case catalog.ClassOAuth:
			counts.OAuth++
		case catalog.ClassLocal:
			counts.Local++
		}
		out = append(out, row)
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"providers": out,
		"counts":    counts,
		"sources":   sources,
	})
}

// directoryPlatform maps a directory id onto the gateway's platform name, or
// empty when no adapter serves it. The two vocabularies differ - the directory
// calls it "nvidia-nim" where the adapter is "nvidia" - so the match is made
// on a normalised form and on prefix, never by assuming they are equal.
func directoryPlatform(id string, s *Server) string {
	normalise := func(v string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(v) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	// The engine's own alias table is authoritative where it has an entry:
	// no length-bounded prefix rule can turn "ovhcloud-ai-endpoints" into the
	// three-letter adapter "ovh", and guessing wrong showed the provider as
	// unconfigured while its key was serving traffic.
	if platform, ok := gateway.PlatformForCatalogID(id); ok {
		if s.engine.Registry().Has(platform) {
			return platform
		}
		return ""
	}
	want := normalise(id)
	if want == "" {
		return ""
	}
	best := ""
	for _, p := range s.engine.Registry().All() {
		got := normalise(p.Platform())
		if got == want {
			return p.Platform()
		}
		// A directory id is usually the adapter's name plus a qualifier
		// ("google-gemini" for the "google" adapter), so a prefix match
		// resolves it - but only at a separator. Without that boundary
		// "hyperbolic" resolved to the unrelated "hyper" adapter and inherited
		// its models, latency and subscription access.
		if separatorBoundary(id, p.Platform()) && len(got) > len(normalise(best)) {
			best = p.Platform()
		}
	}
	return best
}

// separatorBoundary reports whether id is platform followed by a qualifier,
// rather than merely starting with the same letters.
func separatorBoundary(id, platform string) bool {
	low, want := strings.ToLower(id), strings.ToLower(platform)
	if len(want) < 4 || len(low) <= len(want) || !strings.HasPrefix(low, want) {
		return false
	}
	switch low[len(want)] {
	case '-', '_', '.', '/', ' ':
		return true
	default:
		return false
	}
}

// ── usage probe ─────────────────────────────────────────────────────────────

type probeResult struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	// Published reports whether the provider told us anything at all. False
	// with no error means the request succeeded and the provider simply
	// publishes no remaining-usage signal - which is an answer, not a failure.
	Published bool   `json:"published"`
	Message   string `json:"message"`

	Limit     *int64  `json:"limit"`
	Remaining *int64  `json:"remaining"`
	ResetAt   *string `json:"resetAt"`
	Window    string  `json:"window,omitempty"`
}

// handleProviderProbe asks a provider what quota is left.
//
// This exists because most free providers publish no quota anywhere, so the
// dashboard can only show "no published quota" - and an operator's real
// question is "how much do I have left right now". The honest answer differs
// per provider, so the result says which mechanism produced it and reports
// plainly when a provider publishes nothing rather than showing a zero.
func (s *Server) handleProviderProbe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, err := catalog.Directory()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the provider directory")
		return
	}
	var entry *catalog.DirectoryEntry
	for i := range entries {
		if strings.EqualFold(entries[i].ID, id) {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		WriteErrorCode(w, http.StatusNotFound, TypeNotFound, "provider_not_found",
			"no provider in the directory matches "+id)
		return
	}

	platform := directoryPlatform(entry.ID, s)
	if platform == "" {
		WriteJSON(w, http.StatusOK, probeResult{
			Provider: entry.ID, Kind: catalog.ProbeNone,
			Message: "this gateway has no adapter for " + entry.Name + " yet, so it cannot be probed",
		})
		return
	}

	if entry.Class == catalog.ClassOAuth || entry.Class == catalog.ClassLocal {
		WriteJSON(w, http.StatusOK, probeResult{
			Provider: entry.ID, Kind: catalog.ProbeNone,
			Message: "a " + entry.Class + " provider has no per-key quota to report",
		})
		return
	}

	// An observed reading from real traffic beats any probe: it came from this
	// provider's own response headers on a request we actually made.
	if observed, ok := s.observedQuota(r.Context(), platform); ok {
		observed.Provider = entry.ID
		WriteJSON(w, http.StatusOK, observed)
		return
	}

	WriteJSON(w, http.StatusOK, probeResult{
		Provider: entry.ID,
		Kind:     entry.Probe,
		Message:  probeGuidance(entry, platform, s),
	})
}

// observedQuota reads the newest reading the router recorded for a platform.
// It comes from the provider's own response headers, so it is a measurement
// rather than an estimate.
func (s *Server) observedQuota(ctx context.Context, platform string) (probeResult, bool) {
	row := s.engine.DB().QueryRowContext(ctx, `
		SELECT requests_limit, requests_remaining, tokens_limit, tokens_remaining, resets_at
		  FROM provider_quota_state
		 WHERE platform = ?
		 ORDER BY observed_at DESC LIMIT 1`, platform)

	var reqLimit, reqLeft, tokLimit, tokLeft, resetsAt *int64
	if err := row.Scan(&reqLimit, &reqLeft, &tokLimit, &tokLeft, &resetsAt); err != nil {
		return probeResult{}, false
	}

	out := probeResult{Kind: catalog.ProbeHeaders, Published: true,
		Message: "observed from this provider's own rate-limit headers"}
	// Tokens are the axis an operator plans against; requests are the one that
	// usually runs out first. Report whichever the provider actually
	// published, preferring tokens when both are present.
	switch {
	case tokLeft != nil:
		out.Limit, out.Remaining, out.Window = tokLimit, tokLeft, "tokens"
	case reqLeft != nil:
		out.Limit, out.Remaining, out.Window = reqLimit, reqLeft, "requests"
	default:
		return probeResult{}, false
	}
	if resetsAt != nil {
		reset := time.Unix(*resetsAt, 0).UTC().Format(time.RFC3339)
		out.ResetAt = &reset
	}
	return out, true
}

// probeGuidance explains what to do, which is more use than an empty reading.
func probeGuidance(entry *catalog.DirectoryEntry, platform string, s *Server) string {
	var configured int
	_ = s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM api_keys WHERE platform = ? AND enabled = 1`,
		platform).Scan(&configured)

	if configured == 0 {
		return "add a key for " + entry.Name + " first: quota is a property of a credential, not of the provider"
	}
	if entry.Probe == catalog.ProbeNone {
		return entry.Name + " publishes no remaining-usage signal, so no number can be shown without inventing one"
	}
	return entry.Name + " reports quota only in response headers, so a reading appears " +
		"once a request has been routed through this key"
}
