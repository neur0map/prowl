package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func catalogServer(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s, headers := keyedServer(t)
	return s, headers
}

type catSpec struct {
	platform, modelID, name, sizeLabel, source string
	priceIn, priceOut                          *float64
	contextWindow                              *int64
	vision, tools, reasoning                   bool
	intelRank                                  int
}

func seedCat(t *testing.T, db *sql.DB, spec catSpec) int64 {
	t.Helper()
	if spec.source == "" {
		spec.source = "catalog"
	}
	res, err := db.Exec(`INSERT INTO models
		(platform, model_id, display_name, size_label, source,
		 paid_input_per_m, paid_output_per_m, supports_vision, supports_tools,
		 supports_reasoning, context_window, intelligence_rank, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		spec.platform, spec.modelID, spec.name, spec.sizeLabel, spec.source,
		spec.priceIn, spec.priceOut, b2i(spec.vision), b2i(spec.tools),
		b2i(spec.reasoning), spec.contextWindow, spec.intelRank)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

func seedCatKey(t *testing.T, db *sql.DB, platform, status string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO api_keys
		(platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES (?, 'test', 'x', 'y', 'z', ?, 1, 0)`, platform, status)
	require.NoError(t, err)
}

func seedCatRequest(t *testing.T, db *sql.DB, platform, modelID, outcome string, latencyMs, outTokens int) {
	t.Helper()
	status := 200
	if outcome != "success" {
		status = 500
	}
	_, err := db.Exec(`INSERT INTO requests
		(created_at, platform, model_id, status, outcome, latency_ms, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), platform, modelID, status, outcome, latencyMs, outTokens)
	require.NoError(t, err)
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

func getCatalog(t *testing.T, s *Server, auth map[string]string, modality string) catalogModelsResponse {
	t.Helper()
	resp, body := do(t, s, http.MethodGet, "/api/catalog/models?modality="+modality, "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)
	var out catalogModelsResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %q", body)
	return out
}

func findCatModel(t *testing.T, models []catalogModelOut, name string) catalogModelOut {
	t.Helper()
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("model %q not in catalog (%d models)", name, len(models))
	return catalogModelOut{}
}

// TestCatalogUnifiesProvidersByDisplayName: a model served by two platforms is
// one logical entry carrying both providers, not two rows.
func TestCatalogUnifiesProvidersByDisplayName(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()

	seedCatKey(t, db, "groq", "healthy")
	seedCatKey(t, db, "cerebras", "healthy")
	seedCat(t, db, catSpec{platform: "groq", modelID: "openai/gpt-oss-120b", name: "GPT-OSS 120B", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "cerebras", modelID: "gpt-oss-120b", name: "GPT-OSS 120B", sizeLabel: "Large"})

	resp := getCatalog(t, s, auth, "chat")
	require.Equal(t, 1, resp.Total, "the two providers must collapse into one model")
	m := findCatModel(t, resp.Models, "GPT-OSS 120B")
	require.Len(t, m.Providers, 2)
	require.True(t, m.Routable)
	// canonicalId prefers the author-qualified id, so the author is its prefix.
	require.Equal(t, "openai/gpt-oss-120b", m.CanonicalID)
	require.Equal(t, "openai", m.Author)
}

// TestCatalogTierClassification: subscription wins even with a price, a priced
// catalogue row is paid, an unpriced one is free.
func TestCatalogTierClassification(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()

	seedCatKey(t, db, "anthropic", "healthy")
	seedCatKey(t, db, "openrouter", "healthy")
	seedCatKey(t, db, "groq", "healthy")
	seedCat(t, db, catSpec{platform: "anthropic", modelID: "claude", name: "Claude Sub",
		sizeLabel: "Frontier", source: "login", priceIn: new(3.0), priceOut: new(15.0)})
	seedCat(t, db, catSpec{platform: "openrouter", modelID: "gpt-4o", name: "GPT Paid",
		sizeLabel: "Large", priceIn: new(2.5), priceOut: new(10.0)})
	seedCat(t, db, catSpec{platform: "groq", modelID: "llama", name: "Llama Free", sizeLabel: "Medium"})

	resp := getCatalog(t, s, auth, "chat")
	require.Equal(t, "subscription", findCatModel(t, resp.Models, "Claude Sub").Tier)
	require.Equal(t, "paid", findCatModel(t, resp.Models, "GPT Paid").Tier)
	require.Equal(t, "free", findCatModel(t, resp.Models, "Llama Free").Tier)
}

// TestCatalogFacetsMatchFilters: every facet count equals the number of models
// the same filter would return over the list.
func TestCatalogFacetsMatchFilters(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()

	seedCatKey(t, db, "groq", "healthy")
	seedCatKey(t, db, "openrouter", "healthy")
	// Two free (one with tools), one paid, and one that has no key at all.
	seedCat(t, db, catSpec{platform: "groq", modelID: "a", name: "Free A", sizeLabel: "Medium", tools: true})
	seedCat(t, db, catSpec{platform: "groq", modelID: "b", name: "Free B", sizeLabel: "Small"})
	seedCat(t, db, catSpec{platform: "openrouter", modelID: "c", name: "Paid C", sizeLabel: "Large", priceIn: new(1.0), priceOut: new(2.0)})
	seedCat(t, db, catSpec{platform: "nokeyplat", modelID: "d", name: "Orphan D", sizeLabel: "Small"})

	resp := getCatalog(t, s, auth, "chat")
	require.Equal(t, 4, resp.Total)

	// Recompute each facet from the list itself: the server's count must agree.
	var free, paid, sub, tools, routable, needsKey int
	for _, m := range resp.Models {
		switch m.Tier {
		case "free":
			free++
		case "paid":
			paid++
		case "subscription":
			sub++
		}
		if m.Caps.Tools {
			tools++
		}
		if m.Routable {
			routable++
		} else {
			needsKey++
		}
	}
	require.Equal(t, free, resp.Facets.Tier["free"])
	require.Equal(t, paid, resp.Facets.Tier["paid"])
	require.Equal(t, sub, resp.Facets.Tier["subscription"])
	require.Equal(t, tools, resp.Facets.Caps["tools"])
	require.Equal(t, routable, resp.Facets.Availability.Routable)
	require.Equal(t, needsKey, resp.Facets.Availability.NeedsKey)
	require.Equal(t, resp.Total, resp.Facets.Availability.Routable+resp.Facets.Availability.NeedsKey)
	require.Equal(t, 4, resp.Facets.Modalities["chat"])
}

// TestCatalogUseFallbackIdempotent: enabling twice leaves one enabled row and
// does not renumber untouched members; a new member appends; disabling clears.
func TestCatalogUseFallbackIdempotent(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	seedCatKey(t, db, "groq", "healthy")

	a := seedCat(t, db, catSpec{platform: "groq", modelID: "a", name: "A", sizeLabel: "Small"})
	b := seedCat(t, db, catSpec{platform: "groq", modelID: "b", name: "B", sizeLabel: "Small"})
	c := seedCat(t, db, catSpec{platform: "groq", modelID: "c", name: "C", sizeLabel: "Small"})
	d := seedCat(t, db, catSpec{platform: "groq", modelID: "d", name: "D", sizeLabel: "Small"})
	seedFallback(t, db, a, 0, true)
	seedFallback(t, db, b, 1, true)
	seedFallback(t, db, c, 2, false)

	body := fmt.Sprintf(`{"modelDbIds":[%d,%d],"use":true}`, c, d)
	resp, respBody := do(t, s, http.MethodPut, "/api/catalog/use", body, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", respBody)
	require.Equal(t, 4, inGatewayField(t, respBody))

	require.Equal(t, int64(0), fallbackPos(t, db, a))
	require.Equal(t, int64(1), fallbackPos(t, db, b))
	require.Equal(t, int64(2), fallbackPos(t, db, c), "an enabled member keeps its position")
	require.Equal(t, int64(3), fallbackPos(t, db, d), "a new member appends after the last position")

	// Re-enabling is a no-op: still one enabled row for C, count unchanged.
	resp, respBody = do(t, s, http.MethodPut, "/api/catalog/use", fmt.Sprintf(`{"modelDbIds":[%d],"use":true}`, c), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 4, inGatewayField(t, respBody))
	require.Equal(t, int64(2), fallbackPos(t, db, c))

	resp, respBody = do(t, s, http.MethodPut, "/api/catalog/use", fmt.Sprintf(`{"modelDbIds":[%d],"use":false}`, c), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 3, inGatewayField(t, respBody))
	require.Equal(t, 0, fallbackEnabled(t, db, c))
}

// TestCatalogUseProfileAppendsWithoutRenumber: into an active profile, a new
// member appends after the last position and existing members keep theirs;
// disabling removes only the toggled row.
func TestCatalogUseProfileAppendsWithoutRenumber(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	seedCatKey(t, db, "groq", "healthy")

	a := seedCat(t, db, catSpec{platform: "groq", modelID: "a", name: "A", sizeLabel: "Small"})
	b := seedCat(t, db, catSpec{platform: "groq", modelID: "b", name: "B", sizeLabel: "Small"})
	c := seedCat(t, db, catSpec{platform: "groq", modelID: "c", name: "C", sizeLabel: "Small"})
	pid := seedProfile(t, db, "work")
	_, err := db.Exec(`INSERT INTO profile_models (profile_id, model_db_id, position) VALUES (?, ?, 0), (?, ?, 1)`, pid, a, pid, b)
	require.NoError(t, err)
	resp, _ := do(t, s, http.MethodPost, "/api/profiles/active", fmt.Sprintf(`{"profileId":%d}`, pid), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, respBody := do(t, s, http.MethodPut, "/api/catalog/use", fmt.Sprintf(`{"modelDbIds":[%d],"use":true}`, c), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", respBody)
	require.Equal(t, 3, inGatewayField(t, respBody))
	require.Equal(t, int64(0), profilePos(t, db, pid, a))
	require.Equal(t, int64(1), profilePos(t, db, pid, b))
	require.Equal(t, int64(2), profilePos(t, db, pid, c))

	// Idempotent add, then remove.
	_, respBody = do(t, s, http.MethodPut, "/api/catalog/use", fmt.Sprintf(`{"modelDbIds":[%d],"use":true}`, c), auth)
	require.Equal(t, 3, inGatewayField(t, respBody))
	require.Equal(t, 1, profileMembershipCount(t, db, pid, c))

	_, respBody = do(t, s, http.MethodPut, "/api/catalog/use", fmt.Sprintf(`{"modelDbIds":[%d],"use":false}`, c), auth)
	require.Equal(t, 2, inGatewayField(t, respBody))
	require.Equal(t, 0, profileMembershipCount(t, db, pid, c))
	require.Equal(t, int64(0), profilePos(t, db, pid, a), "removing C must not renumber A")
}

// TestCatalogModelWithoutUsableKeyIsNotRoutable: a model whose platform has no
// enabled key reports routable:false and its provider keyStatus "none".
func TestCatalogModelWithoutUsableKeyIsNotRoutable(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	seedCat(t, db, catSpec{platform: "lonelyplat", modelID: "m", name: "No Key Model", sizeLabel: "Medium"})

	m := findCatModel(t, getCatalog(t, s, auth, "chat").Models, "No Key Model")
	require.False(t, m.Routable)
	require.Len(t, m.Providers, 1)
	require.False(t, m.Providers[0].HasKey)
	require.Equal(t, "none", m.Providers[0].KeyStatus)
}

// TestCatalogUnmeasuredScoreIsNull: a model with no request history reports
// null score/axes, never a fabricated zero; one with history reports a number.
func TestCatalogUnmeasuredScoreIsNull(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	seedCatKey(t, db, "groq", "healthy")
	seedCat(t, db, catSpec{platform: "groq", modelID: "quiet", name: "Quiet", sizeLabel: "Medium"})
	seedCat(t, db, catSpec{platform: "groq", modelID: "busy", name: "Busy", sizeLabel: "Medium"})
	for range 5 {
		seedCatRequest(t, db, "groq", "busy", "success", 200, 100)
	}

	models := getCatalog(t, s, auth, "chat").Models
	quiet := findCatModel(t, models, "Quiet")
	require.Nil(t, quiet.Score)
	require.Nil(t, quiet.Reliability)
	require.Nil(t, quiet.Speed)
	require.Nil(t, quiet.Intelligence)
	require.Nil(t, quiet.Providers[0].Requests, "an untried provider reports no request count")

	busy := findCatModel(t, models, "Busy")
	require.NotNil(t, busy.Score)
	require.NotNil(t, busy.Reliability)
	require.NotNil(t, busy.Providers[0].Requests)
	require.Equal(t, 5, *busy.Providers[0].Requests)
}

// TestCatalogCanonicalIDStableAcrossScoreShift: the group's best provider (and
// so the headline id) may flip with traffic, but canonicalId - the detail-page
// URL - must not.
func TestCatalogCanonicalIDStableAcrossScoreShift(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	seedCatKey(t, db, "pa", "healthy")
	seedCatKey(t, db, "pb", "healthy")
	paID := seedCat(t, db, catSpec{platform: "pa", modelID: "pa/thing", name: "Thing", sizeLabel: "Large"})
	pbID := seedCat(t, db, catSpec{platform: "pb", modelID: "thing", name: "Thing", sizeLabel: "Large"})

	// First: only pb has traffic, so pb is the best (measured) provider.
	for range 4 {
		seedCatRequest(t, db, "pb", "thing", "success", 200, 100)
	}
	first := findCatModel(t, getCatalog(t, s, auth, "chat").Models, "Thing")
	require.Equal(t, pbID, first.ID)
	require.Equal(t, "pa/thing", first.CanonicalID)

	// Then: pa gets strong reliability and pb fails, so pa becomes the best.
	for range 30 {
		seedCatRequest(t, db, "pa", "pa/thing", "success", 200, 100)
	}
	for range 8 {
		seedCatRequest(t, db, "pb", "thing", "error", 200, 0)
	}
	second := findCatModel(t, getCatalog(t, s, auth, "chat").Models, "Thing")
	require.Equal(t, paID, second.ID, "the best provider should have flipped")
	require.Equal(t, "pa/thing", second.CanonicalID, "canonicalId must survive a score shift")
}

// TestCatalogProvidersDirectory: the provider directory serves the full catalog
// with access classes and links, and joins the operator's key state onto it.
func TestCatalogProvidersDirectory(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	seedCatKey(t, db, "groq", "healthy")

	resp, body := do(t, s, http.MethodGet, "/api/catalog/providers", "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)
	var out struct {
		Total     int                       `json:"total"`
		Providers []catalogProviderDirEntry `json:"providers"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Greater(t, out.Total, 100, "the directory is the whole provider catalog")
	require.Equal(t, out.Total, len(out.Providers))

	byPlatform := map[string]catalogProviderDirEntry{}
	for _, p := range out.Providers {
		byPlatform[p.Platform] = p
	}
	groq, ok := byPlatform["groq"]
	require.True(t, ok, "groq must be listed")
	require.True(t, groq.HasKey)
	require.Equal(t, "healthy", groq.KeyStatus)
	require.Equal(t, "free", groq.Access)

	anthropic, ok := byPlatform["anthropic"]
	require.True(t, ok)
	require.Equal(t, "subscription", anthropic.Access, "an OAuth provider is a subscription")

	// Card-gated paid providers surface the requiresCard flag.
	var sawCard bool
	for _, p := range out.Providers {
		if p.RequiresCard {
			sawCard = true
			break
		}
	}
	require.True(t, sawCard, "at least one provider requires a card")
}

func inGatewayField(t *testing.T, body string) int {
	t.Helper()
	var parsed struct {
		InGateway int `json:"inGateway"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed), "body was %q", body)
	return parsed.InGateway
}

func fallbackPos(t *testing.T, db *sql.DB, modelDBID int64) int64 {
	t.Helper()
	var pos int64
	require.NoError(t, db.QueryRow(`SELECT position FROM fallback_config WHERE model_db_id = ?`, modelDBID).Scan(&pos))
	return pos
}

func fallbackEnabled(t *testing.T, db *sql.DB, modelDBID int64) int {
	t.Helper()
	var e int
	require.NoError(t, db.QueryRow(`SELECT enabled FROM fallback_config WHERE model_db_id = ?`, modelDBID).Scan(&e))
	return e
}

func profilePos(t *testing.T, db *sql.DB, profileID, modelDBID int64) int64 {
	t.Helper()
	var pos int64
	require.NoError(t, db.QueryRow(`SELECT position FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, profileID, modelDBID).Scan(&pos))
	return pos
}

func profileMembershipCount(t *testing.T, db *sql.DB, profileID, modelDBID int64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, profileID, modelDBID).Scan(&n))
	return n
}

// TestCatalogUnifiesSuffixedProviderRows: the shipped catalogue tags each row
// with its provider ("(Groq)", "(CF)"), so the group key must be the name with
// that trailing parenthetical stripped - five suffixed rows are one model.
func TestCatalogUnifiesSuffixedProviderRows(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	for _, p := range []string{"cloudflare", "groq", "huggingface", "ovh", "ollama"} {
		seedCatKey(t, db, p, "healthy")
	}
	seedCat(t, db, catSpec{platform: "cloudflare", modelID: "@cf/openai/gpt-oss-120b", name: "GPT-OSS 120B (CF)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "groq", modelID: "openai/gpt-oss-120b", name: "GPT-OSS 120B (Groq)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "huggingface", modelID: "openai/gpt-oss-120b", name: "GPT-OSS 120B (HF)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "ovh", modelID: "gpt-oss-120b", name: "GPT-OSS 120B (OVH)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "ollama", modelID: "gpt-oss:120b", name: "GPT-OSS 120B (Ollama)", sizeLabel: "Large"})

	resp := getCatalog(t, s, auth, "chat")
	require.Equal(t, 1, resp.Total, "five suffixed rows must collapse to one model")
	require.Equal(t, "GPT-OSS 120B", resp.Models[0].Name, "the headline name drops the provider suffix")
	require.Len(t, resp.Models[0].Providers, 5)
	require.Equal(t, 1, resp.Facets.Modalities["chat"])
}

// TestCatalogFreeSuffixMergesWithSiblings: a "(free)" row belongs with its
// abbreviation-suffixed siblings, not in a group of its own.
func TestCatalogFreeSuffixMergesWithSiblings(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	for _, p := range []string{"kilo", "nvidia", "requesty", "openrouter"} {
		seedCatKey(t, db, p, "healthy")
	}
	seedCat(t, db, catSpec{platform: "kilo", modelID: "nemotron", name: "Nemotron 3 Super 120B (Kilo)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "nvidia", modelID: "nvidia/nemotron-3", name: "Nemotron 3 Super 120B (NV)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "requesty", modelID: "nemotron-super", name: "Nemotron 3 Super 120B (Requesty)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "openrouter", modelID: "nvidia/nemotron-3-super-120b", name: "Nemotron 3 Super 120B (free)", sizeLabel: "Large"})

	resp := getCatalog(t, s, auth, "chat")
	require.Equal(t, 1, resp.Total)
	require.Equal(t, "Nemotron 3 Super 120B", resp.Models[0].Name)
	require.Len(t, resp.Models[0].Providers, 4, "the (free) row joins its siblings")
}

// TestCatalogDistinctModelsDoNotMerge: two genuinely different models keep
// their own entries even when both are provider-suffixed.
func TestCatalogDistinctModelsDoNotMerge(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	seedCatKey(t, db, "groq", "healthy")
	seedCat(t, db, catSpec{platform: "groq", modelID: "l31", name: "Llama 3.1 8B (Groq)", sizeLabel: "Small"})
	seedCat(t, db, catSpec{platform: "groq", modelID: "l33", name: "Llama 3.3 70B (Groq)", sizeLabel: "Large"})

	resp := getCatalog(t, s, auth, "chat")
	require.Equal(t, 2, resp.Total)
}

// TestCatalogFacetsMatchAfterRegroup: after suffix regrouping, every facet
// count still equals the number of models the same filter returns.
func TestCatalogFacetsMatchAfterRegroup(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	seedCatKey(t, db, "groq", "healthy")
	seedCatKey(t, db, "cloudflare", "healthy")
	seedCatKey(t, db, "openrouter", "healthy")
	seedCat(t, db, catSpec{platform: "groq", modelID: "m1g", name: "Model One (Groq)", sizeLabel: "Large", tools: true})
	seedCat(t, db, catSpec{platform: "cloudflare", modelID: "m1c", name: "Model One (CF)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "openrouter", modelID: "m2", name: "Model Two (free)", sizeLabel: "Large", priceIn: new(1.0), priceOut: new(2.0)})

	resp := getCatalog(t, s, auth, "chat")
	require.Equal(t, 2, resp.Total, "Model One's two providers collapse to one entry")

	var free, paid, tools, routable int
	for _, m := range resp.Models {
		switch m.Tier {
		case "free":
			free++
		case "paid":
			paid++
		}
		if m.Caps.Tools {
			tools++
		}
		if m.Routable {
			routable++
		}
	}
	require.Equal(t, free, resp.Facets.Tier["free"])
	require.Equal(t, paid, resp.Facets.Tier["paid"])
	require.Equal(t, tools, resp.Facets.Caps["tools"])
	require.Equal(t, routable, resp.Facets.Availability.Routable)
	require.Equal(t, 2, resp.Facets.Modalities["chat"])
}

// TestCatalogAuthorDerivation: the family author skips a vendor-route prefix
// (Cloudflare "@cf/…"), keeps the normal "author/model" path, and falls back
// to the platform for an unqualified id.
func TestCatalogAuthorDerivation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		canonicalID, platform, want string
	}{
		{"@cf/openai/gpt-oss-120b", "cloudflare", "openai"},
		{"openai/gpt-oss-120b", "openrouter", "openai"},
		{"gpt-oss-120b", "groq", "groq"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, catalogAuthor(c.canonicalID, c.platform), "id %q", c.canonicalID)
	}
}

// TestCatalogCanonicalIDPrefersCleanVendorID: when a group mixes a
// Cloudflare-route id, a portable "author/model" id and bare ids, the
// canonicalId (the id a client sends, and the detail-page URL) is the portable
// one, not the route-specific "@cf/…" that only one provider understands.
func TestCatalogCanonicalIDPrefersCleanVendorID(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	for _, p := range []string{"cloudflare", "openrouter", "ollama", "groq"} {
		seedCatKey(t, db, p, "healthy")
	}
	seedCat(t, db, catSpec{platform: "cloudflare", modelID: "@cf/openai/gpt-oss-120b", name: "GPT-OSS 120B (CF)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "openrouter", modelID: "openai/gpt-oss-120b", name: "GPT-OSS 120B (free)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "ollama", modelID: "gpt-oss:120b", name: "GPT-OSS 120B (Ollama)", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "groq", modelID: "gpt-oss-120b", name: "GPT-OSS 120B (Groq)", sizeLabel: "Large"})

	resp := getCatalog(t, s, auth, "chat")
	require.Equal(t, 1, resp.Total)
	require.Equal(t, "openai/gpt-oss-120b", resp.Models[0].CanonicalID)
	require.Equal(t, "openai", resp.Models[0].Author)
}

// TestCatalogGroupKeyMergesPunctuationAndCaseButNotSize: casing and
// punctuation must not fork a logical model, but a different size must stay its
// own model so a future normalisation change cannot collapse 120B into 20B.
func TestCatalogGroupKeyMergesPunctuationAndCaseButNotSize(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	for _, p := range []string{"groq", "navyai", "ollama", "cerebras"} {
		seedCatKey(t, db, p, "healthy")
	}
	seedCat(t, db, catSpec{platform: "groq", modelID: "gpt-oss-120b", name: "GPT-OSS 120B", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "navyai", modelID: "gpt-oss-120b-navy", name: "GPT Oss 120B", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "ollama", modelID: "gpt-oss:120b", name: "gpt oss 120b", sizeLabel: "Large"})
	seedCat(t, db, catSpec{platform: "cerebras", modelID: "gpt-oss-20b", name: "GPT-OSS 20B", sizeLabel: "Medium"})

	resp := getCatalog(t, s, auth, "chat")
	require.Equal(t, 2, resp.Total, "the three 120B spellings merge; 20B stays separate")
	var m120, m20 catalogModelOut
	for _, m := range resp.Models {
		switch len(m.Providers) {
		case 3:
			m120 = m
		case 1:
			m20 = m
		}
	}
	require.Len(t, m120.Providers, 3, "casing and punctuation variants are one model")
	require.Contains(t, []string{"GPT-OSS 120B", "GPT Oss 120B", "gpt oss 120b"}, m120.Name,
		"the headline is a real spelling, not the stripped key")
	require.Equal(t, "GPT-OSS 20B", m20.Name)
}

func seedEmbeddingModel(t *testing.T, db *sql.DB, platform, modelID, name string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO embedding_models(family, platform, model_id, display_name)
		VALUES (?, ?, ?, ?)`, name, platform, modelID, name)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

func seedMediaModel(t *testing.T, db *sql.DB, platform, modelID, name, modality string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO media_models(platform, model_id, display_name, modality)
		VALUES (?, ?, ?, ?)`, platform, modelID, name, modality)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// TestCatalogProvidersRoutableCountsModalityModels proves the provider
// directory's routable and total counts include embedding and media models,
// while the in-gateway count stays chat-only (the routing chain references chat
// models alone). A provider serving only an embedding and a media model, with a
// healthy key, reports two routable and two total models and zero in-gateway.
func TestCatalogProvidersRoutableCountsModalityModels(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()
	seedCatKey(t, db, "groq", "healthy")
	seedEmbeddingModel(t, db, "groq", "emb-1", "Embed One")
	seedMediaModel(t, db, "groq", "img-1", "Image One", "image")

	resp, body := do(t, s, http.MethodGet, "/api/catalog/providers", "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)
	var out struct {
		Providers []catalogProviderDirEntry `json:"providers"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))

	var groq catalogProviderDirEntry
	for _, p := range out.Providers {
		if p.Platform == "groq" {
			groq = p
			break
		}
	}
	require.Equal(t, "groq", groq.Platform, "groq must be listed")
	require.Equal(t, 2, groq.Models, "embedding + media models both count in the total")
	require.Equal(t, 2, groq.RoutableModels, "both modality models are routable with a healthy key")
	require.Equal(t, 0, groq.InGatewayModels, "modality models are never in the chat routing chain")
}

// TestCatalogLinkedOnlyPlatformDoesNotRouteUnrelatedModels proves catalog
// availability obeys the router's chain-expansion rules: a linked-login
// credential (a subscription) is authorised only for the models it seeded, so a
// shipped catalogue model on a platform whose ONLY key is that subscription is
// NOT routable, while the login's own key-bound model IS. Without this the
// catalog would claim a linked-only route can serve unrelated models the router
// never would.
func TestCatalogLinkedOnlyPlatformDoesNotRouteUnrelatedModels(t *testing.T) {
	t.Parallel()
	s, auth := catalogServer(t)
	db := s.engine.DB()

	// A ChatGPT subscription: a linked (link:openai) credential, not an API key.
	res, err := db.Exec(`INSERT INTO api_keys
		(platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES ('openai', 'ChatGPT', 'link:openai', '', '', 'healthy', 1, 0)`)
	require.NoError(t, err)
	linkedKeyID, err := res.LastInsertId()
	require.NoError(t, err)

	// A shipped catalogue model bound to no credential.
	seedCat(t, db, catSpec{platform: "openai", modelID: "gpt-shipped", name: "GPT Shipped", sizeLabel: "Large"})
	// A login model seeded from the subscription, bound to the linked key.
	_, err = db.Exec(`INSERT INTO models
		(platform, model_id, display_name, size_label, source, key_id, endpoint_scope,
		 supports_tools, intelligence_rank, enabled)
		VALUES ('openai', 'gpt-login', 'GPT Login', 'Large', 'login', ?, 'linked:openai', 1, 5, 1)`, linkedKeyID)
	require.NoError(t, err)

	models := getCatalog(t, s, auth, "chat").Models

	shipped := findCatModel(t, models, "GPT Shipped")
	require.False(t, shipped.Routable,
		"a shipped model must not be routable through a linked-only subscription")
	require.Len(t, shipped.Providers, 1)
	require.False(t, shipped.Providers[0].HasKey,
		"a subscription is not a key for an unrelated catalogue model")
	require.Equal(t, "none", shipped.Providers[0].KeyStatus)

	login := findCatModel(t, models, "GPT Login")
	require.True(t, login.Routable,
		"a login model IS routable through its own bound subscription")
	require.True(t, login.Providers[0].HasKey)
	require.Equal(t, "subscription", login.Tier)

	// The provider directory counts only the login's own bound model as routable.
	presp, pbody := do(t, s, http.MethodGet, "/api/catalog/providers", "", auth)
	require.Equal(t, http.StatusOK, presp.StatusCode, "body was %q", pbody)
	var pd struct {
		Providers []catalogProviderDirEntry `json:"providers"`
	}
	require.NoError(t, json.Unmarshal([]byte(pbody), &pd))
	var openai catalogProviderDirEntry
	for _, p := range pd.Providers {
		if p.Platform == "openai" {
			openai = p
			break
		}
	}
	require.Equal(t, "openai", openai.Platform, "openai must be listed")
	require.Equal(t, 1, openai.RoutableModels,
		"only the login's own bound model is routable, never the shipped model")
}
