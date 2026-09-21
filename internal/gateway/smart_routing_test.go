package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyPromptFindsCodingToolsAndComplexity(t *testing.T) {
	profile := ClassifyPrompt(
		[]map[string]any{{
			"role":    "user",
			"content": "Debug this Go repository stack trace, identify the root cause, and implement the fix without changing the API.",
		}},
		map[string]any{"tools": []any{map[string]any{"type": "function"}}},
	)

	require.Equal(t, DomainCoding, profile.Domain)
	require.True(t, profile.UsesTools)
	require.True(t, profile.HasCode)
	require.Equal(t, "complex", profile.ComplexityLabel())
	require.Greater(t, profile.Confidence, 0.5)
}

func TestClassifyPromptSeparatesToolAvailabilityFromAgenticIntent(t *testing.T) {
	tools := map[string]any{"tools": []any{map[string]any{"type": "function"}}}
	mathProfile := ClassifyPrompt(
		[]map[string]any{{"role": "user", "content": "Derive the probability equation and prove the result."}},
		tools,
	)
	require.Equal(t, DomainMath, mathProfile.Domain,
		"merely offering tools must not erase the prompt's actual domain")
	require.True(t, mathProfile.UsesTools)

	agenticProfile := ClassifyPrompt(
		[]map[string]any{{"role": "user", "content": "Open the file, run the command, and use the tool to finish the workflow."}},
		tools,
	)
	require.Equal(t, DomainAgentic, agenticProfile.Domain)
}

func TestClassifyPromptIgnoresHarnessContextAndPrefersExplicitResearch(t *testing.T) {
	profile := ClassifyPrompt(
		[]map[string]any{
			{"role": "system", "content": strings.Repeat("repository codebase implement tools complex ", 4000)},
			{"role": "developer", "content": "Use every available tool and follow the repository workflow."},
			{"role": "user", "content": "Research the latest official documentation and cite sources."},
		},
		map[string]any{"tools": []any{map[string]any{"type": "function"}}},
	)

	require.Equal(t, DomainResearch, profile.Domain)
	require.Equal(t, "simple", profile.ComplexityLabel())
	require.True(t, profile.UsesTools, "tool support remains a capability gate")
}

func TestClassifyPromptUsesOnlyLatestUserTurn(t *testing.T) {
	profile := ClassifyPrompt(
		[]map[string]any{
			{"role": "user", "content": "Implement a complicated repository migration with many constraints."},
			{"role": "assistant", "content": "What should I check first?"},
			{"role": "user", "content": "Find the current official docs and sources."},
		},
		nil,
	)

	require.Equal(t, DomainResearch, profile.Domain)
	require.Equal(t, "simple", profile.ComplexityLabel())
}

func TestOrderSmartChainUsesDomainCapability(t *testing.T) {
	entries := []ChainEntry{
		{ModelDBID: 1, ModelID: "general-top", DisplayName: "General Top", Tier: TierLarge, IntelRank: 1, Enabled: true},
		{ModelDBID: 2, ModelID: "gpt-codex", DisplayName: "GPT Codex", Tier: TierLarge, IntelRank: 2, Enabled: true, SupportsTools: true},
	}
	benchmarks := map[int64]BenchmarkScores{
		1: {Intelligence: .99, Coding: .20, Agentic: .20, Source: benchmarkSource},
		2: {Intelligence: .70, Coding: .95, Agentic: .80, Source: benchmarkSource},
	}
	profile := PromptProfile{Domain: DomainCoding, Complexity: .8, UsesTools: true}

	ordered, scores := OrderSmartChain(entries, profile, benchmarks, false, fixedAxisScorer{})

	require.Equal(t, int64(2), ordered[0].ModelDBID)
	require.Greater(t, scores[2].Capability, scores[1].Capability)
	require.Equal(t, benchmarkSource, scores[2].Source)
}

func TestOrderSmartChainUsesEconomyForSmallWorkAndCapabilityForComplexWork(t *testing.T) {
	cheapIn, cheapOut := 0.10, 0.30
	frontierIn, frontierOut := 15.0, 60.0
	entries := []ChainEntry{
		{
			ModelDBID: 1, ModelID: "small-cheap", DisplayName: "Small Cheap", Tier: TierSmall,
			IntelRank: 1, Enabled: true, PaidInputPerM: &cheapIn, PaidOutputPerM: &cheapOut,
		},
		{
			ModelDBID: 2, ModelID: "frontier-expensive", DisplayName: "Frontier Expensive", Tier: TierFrontier,
			IntelRank: 1, Enabled: true, PaidInputPerM: &frontierIn, PaidOutputPerM: &frontierOut,
		},
	}
	benchmarks := map[int64]BenchmarkScores{
		1: {Intelligence: .58, Source: benchmarkSource},
		2: {Intelligence: .98, Source: benchmarkSource},
	}

	simple, simpleScores := OrderSmartChain(
		entries, PromptProfile{Domain: DomainGeneral, Complexity: .08}, benchmarks, false, fixedAxisScorer{})
	require.Equal(t, int64(1), simple[0].ModelDBID)
	require.Greater(t, simpleScores[1].Economy, simpleScores[2].Economy)

	complex, _ := OrderSmartChain(
		entries, PromptProfile{Domain: DomainCoding, Complexity: .92, Stakes: .2}, benchmarks, false, fixedAxisScorer{})
	require.Equal(t, int64(2), complex[0].ModelDBID)
}

type fixedAxisScorer struct{}

func (fixedAxisScorer) Axes(*ChainEntry, bool) Axes {
	return Axes{Reliability: .8, Speed: .6, Headroom: 1, RateLimit: 1}
}

func TestBenchmarkMatcherReordersVersionTokensButRejectsAmbiguity(t *testing.T) {
	matcher := newBenchmarkMatcher([]externalBenchmarkModel{
		{ID: "claude", Name: "Claude 4.5 Sonnet", Slug: "claude-4-5-sonnet"},
	})
	matched, ok := matcher.match("claude-sonnet-4-5")
	require.True(t, ok)
	require.Equal(t, "claude", matched.ID)

	ambiguous := newBenchmarkMatcher([]externalBenchmarkModel{
		{ID: "one", Name: "Model 2 Pro", Slug: "model-2-pro"},
		{ID: "two", Name: "Model Pro 2", Slug: "model-pro-2"},
	})
	_, ok = ambiguous.match("pro-model-2")
	require.False(t, ok, "an ambiguous external identity must fall back to catalogue priors")
}

func TestRefreshBenchmarksPersistsAndKeepsLastGoodScores(t *testing.T) {
	ctx := context.Background()
	engine, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	result, err := engine.DB().Exec(`
		INSERT INTO models(platform, model_id, display_name, enabled)
		VALUES ('anthropic', 'claude-sonnet-4-5', 'Claude Sonnet 4.5', 1)`)
	require.NoError(t, err)
	modelID, err := result.LastInsertId()
	require.NoError(t, err)

	statusCode := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "secret-aa-key", r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if statusCode != http.StatusOK {
			_, _ = fmt.Fprint(w, `{"error":"temporary outage"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{
			"intelligence_index_version": 4.3,
			"pagination": {"has_more": false, "total_pages": 1},
			"data": [{
				"id": "aa-claude", "name": "Claude 4.5 Sonnet", "slug": "claude-4-5-sonnet",
				"evaluations": {
					"artificial_analysis_intelligence_index": 82,
					"artificial_analysis_coding_index": 91,
					"artificial_analysis_agentic_index": 88
				}
			}]
		}`)
	}))
	defer server.Close()

	oldEndpoint, oldClient := benchmarkEndpoint, benchmarkHTTPClient
	benchmarkEndpoint, benchmarkHTTPClient = server.URL, server.Client()
	t.Cleanup(func() {
		benchmarkEndpoint, benchmarkHTTPClient = oldEndpoint, oldClient
	})
	t.Setenv(benchmarkAPIKeyEnv, "secret-aa-key")

	status, err := engine.RefreshBenchmarks(ctx)
	require.NoError(t, err)
	require.Equal(t, "current", status.Status)
	require.Equal(t, 1, status.Matched)
	require.Equal(t, 1, status.Available)
	require.NotNil(t, status.LastSuccess)

	scores := LoadBenchmarkScores(ctx, engine.DB(), []ChainEntry{{ModelDBID: modelID}})
	require.InDelta(t, .82, scores[modelID].Intelligence, .001)
	require.InDelta(t, .91, scores[modelID].Coding, .001)
	require.Equal(t, benchmarkSource, scores[modelID].Source)

	statusCode = http.StatusServiceUnavailable
	failedStatus, err := engine.RefreshBenchmarks(ctx)
	require.Error(t, err)
	require.Equal(t, "error", failedStatus.Status)
	require.NotNil(t, failedStatus.LastSuccess)

	cached := LoadBenchmarkScores(ctx, engine.DB(), []ChainEntry{{ModelDBID: modelID}})
	require.InDelta(t, .91, cached[modelID].Coding, .001,
		"a failed refresh must not erase the last good benchmark")
}
