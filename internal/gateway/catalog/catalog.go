// Package catalog is the built-in directory of LLM API providers the Prowl
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
)

//go:embed providers.json
var providersJSON []byte

// Compat identifies an endpoint's wire format. Everything the gateway proxies
type Compat string

const (
	CompatOpenAI     Compat = "openai"
	CompatGemini     Compat = "gemini"
	CompatCohere     Compat = "cohere"
	CompatCloudflare Compat = "cloudflare"
	CompatOllama     Compat = "ollama"
	CompatAI21       Compat = "ai21"
	CompatOther      Compat = "other"
)

// Tier describes how a provider gives access away.
const (
	TierPermanentFree = "permanent-free"
	TierRenewable     = "renewable-credits"
	TierPaid          = "paid-or-credits"
)

// Model is one model a provider advertises as free.
type Model struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Context int    `json:"context"`
	Limits  string `json:"limits"`
}

// Provider is one routable API endpoint plus what a user needs to sign up.
type Provider struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	BaseURL    string   `json:"base_url"`
	APIKeyURL  string   `json:"api_key_url"`
	Env        string   `json:"env"`
	Tier       string   `json:"tier"`
	Signup     string   `json:"signup"`
	FreeModels int      `json:"free_models"`
	MaxContext int      `json:"max_context"`
	Modalities []string `json:"modalities"`
	Models     []Model  `json:"models"`
	Source     string   `json:"source"`
	Compat     Compat   `json:"compat"`

	// RequiresVars names placeholders in BaseURL that a user must fill in
	// (Cloudflare's account id, for example). A provider with unfilled
	// placeholders cannot be routed to.
	RequiresVars []string `json:"requires_vars"`

	// Aliases records upstream names folded into this entry during
	// deduplication, so a search for the old name still finds it.
	Aliases []string `json:"aliases,omitempty"`

	// Friction is the normalised signup cost. It is the field users actually
	// decide on, and conflating phone verification with a payment card
	// misreports providers like NVIDIA NIM.
	Friction string `json:"friction"`
}

// Signup friction levels, cheapest first.
const (
	FrictionNone         = "none"
	FrictionRegistration = "registration"
	FrictionPhone        = "phone"
	FrictionCard         = "card"
)

// Routable reports whether the gateway can proxy to this provider as-is.
func (p Provider) Routable() bool {
	return p.Compat == CompatOpenAI && len(p.RequiresVars) == 0 && p.BaseURL != ""
}

// Free reports whether the provider has a standing free tier.
func (p Provider) Free() bool {
	return p.Tier == TierPermanentFree || p.Tier == TierRenewable
}

// NeedsCard reports whether signing up requires a payment card, the single
// most common reason a user skips a provider.
func (p Provider) NeedsCard() bool { return p.Friction == FrictionCard }

// NoSignupFriction reports whether a key needs nothing but an email.
func (p Provider) NoSignupFriction() bool { return p.Friction == FrictionNone }

var load = sync.OnceValues(func() ([]Provider, error) {
	var providers []Provider
	if err := json.Unmarshal(providersJSON, &providers); err != nil {
		return nil, fmt.Errorf("parse embedded provider catalog: %w", err)
	}
	return providers, nil
})

// All returns every catalog entry. The embedded data is generated, so a parse
// failure is a build-time mistake; it is returned rather than panicking so a
// broken catalog degrades the gateway instead of the whole binary.
func All() ([]Provider, error) {
	providers, err := load()
	if err != nil {
		return nil, err
	}
	return slices.Clone(providers), nil
}

// Routable returns only the providers the gateway can proxy to today.
func Routable() ([]Provider, error) {
	providers, err := load()
	if err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if p.Routable() {
			out = append(out, p)
		}
	}
	return out, nil
}

// Find returns a provider by id, matching folded-in aliases too.
func Find(id string) (Provider, bool) {
	providers, err := load()
	if err != nil {
		return Provider{}, false
	}
	for _, p := range providers {
		if p.ID == id || slices.Contains(p.Aliases, id) {
			return p, true
		}
	}
	return Provider{}, false
}

// adapterPlatformOverrides is the one canonical place a catalog provider id
// diverges from the registry platform its adapter is keyed under. Only the
// handwritten deviations live here: a name whose adapter carries a real wire
// difference (Google's Gemini validate wire, Cloudflare's compound key) or
// whose registry platform is simply a shorter token than the catalog id
// ("ovhcloud-ai-endpoints" is the adapter "ovh", which no prefix rule with a
// sane minimum length can infer). Every other routable provider is registered
// under its own id, so the mapping is identity there and needs no entry.
//
// Both the gateway's PlatformForCatalogID and the provider registry's
// auto-registration resolve through AdapterPlatform, so this table is the
// single source of truth for "which adapter serves this catalog id".
var adapterPlatformOverrides = map[string]string{
	"nvidia-nim":            "nvidia",
	"cloudflare-workers-ai": "cloudflare",
	"z-ai-zhipu-ai":         "zhipu",
	"llm7-io":               "llm7",
	"kilo-code":             "kilo",
	"ollama-cloud":          "ollama",
	"aion-labs":             "aion",
	"hugging-face":          "huggingface",
	"mistral-ai":            "mistral",
	"ovhcloud-ai-endpoints": "ovh",
	"opencode-zen":          "opencode",
	"agnes-ai":              "agnes",
}

// AdapterPlatform resolves a catalog provider id to the registry platform its
// adapter is registered under. It returns ok only when an adapter genuinely
// backs the id: an explicit handwritten override, or a routable provider that
// the registry auto-registers under its own id. A non-routable id with no
// handwritten adapter resolves to nothing, so a caller never claims an adapter
// that does not exist.
func AdapterPlatform(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if platform, ok := adapterPlatformOverrides[id]; ok {
		return platform, true
	}
	if p, ok := Find(id); ok && p.Routable() {
		return id, true
	}
	return "", false
}

// AdapterOverrides returns a copy of the handwritten catalog-id to adapter
// platform deviations, so a guard can assert every target is a registered
// platform without reaching into this package's internals.
func AdapterOverrides() map[string]string {
	out := make(map[string]string, len(adapterPlatformOverrides))
	for id, platform := range adapterPlatformOverrides {
		out[id] = platform
	}
	return out
}
