package catalog

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEndpointsAreUniqueAndValid is the dedup guarantee: upstream lists some
// endpoints twice under different names, and a duplicate would make the
// router treat one provider as two independent failover targets.
func TestEndpointsAreUniqueAndValid(t *testing.T) {
	t.Parallel()

	all, err := All()
	require.NoError(t, err)

	seen := make(map[string]string, len(all))
	ids := make(map[string]string, len(all))
	for _, p := range all {
		require.NotEmpty(t, p.ID)
		require.NotEmpty(t, p.Name)
		require.NotEmpty(t, p.BaseURL, "%s has no endpoint and cannot be routed to", p.ID)

		prev, dup := ids[p.ID]
		require.False(t, dup, "duplicate provider id %q (also %q)", p.ID, prev)
		ids[p.ID] = p.Name

		key := strings.ToLower(strings.TrimRight(p.BaseURL, "/"))
		other, clash := seen[key]
		require.False(t, clash, "%s and %s share endpoint %s; the router would double-count it", p.ID, other, key)
		seen[key] = p.ID

		parsed, err := url.Parse(p.BaseURL)
		require.NoError(t, err, "%s has an unparseable base url", p.ID)
		require.Equal(t, "https", parsed.Scheme, "%s must be https", p.ID)
	}
}

// TestRoutabilityIsHonest checks the classification the dashboard relies on:
// an endpoint that does not speak OpenAI chat completions, or that still has
// an unfilled placeholder, must not be offered as one-click.
func TestRoutabilityIsHonest(t *testing.T) {
	t.Parallel()

	routable, err := Routable()
	require.NoError(t, err)

	for _, p := range routable {
		require.Equal(t, CompatOpenAI, p.Compat)
		require.Empty(t, p.RequiresVars)
		require.NotContains(t, p.BaseURL, "{", "%s still has a template placeholder", p.ID)
	}

	// Known non-OpenAI wire formats must be excluded, not quietly included.
	for _, id := range []string{"google-gemini", "cohere", "ai21-labs"} {
		p, ok := Find(id)
		require.True(t, ok, "%s must still be listed for discovery", id)
		require.False(t, p.Routable(), "%s does not speak OpenAI chat completions", id)
	}
	for _, id := range []string{
		"github-models", "github", "anyscale", "glhf-chat",
		"empower", "galadriel", "gigachat", "manus",
	} {
		p, ok := Find(id)
		require.True(t, ok, "%s must remain visible with an explanation", id)
		require.False(t, p.Routable(), "%s must not advertise a broken generic adapter", id)
	}

	chutes, ok := Find("chutes-ai")
	require.True(t, ok)
	require.Equal(t, "https://llm.chutes.ai/v1", chutes.BaseURL)

	// Cloudflare's endpoint needs an account id before it resolves.
	cf, ok := Find("cloudflare-workers-ai")
	require.True(t, ok)
	require.Contains(t, cf.RequiresVars, "account_id")
	require.False(t, cf.Routable())
}

// TestAliasesResolve keeps deduplication from losing a name a user may search
// for: upstream's "Grok (xAI)" was folded into the xAI endpoint.
func TestAliasesResolve(t *testing.T) {
	t.Parallel()

	p, ok := Find("grok-xai")
	require.True(t, ok, "a folded-in upstream name must still resolve")
	require.Equal(t, "xai", p.ID)
	require.True(t, p.Routable())
}

// TestSignupFrictionIsRecorded covers the field users actually decide on.
func TestSignupFrictionIsRecorded(t *testing.T) {
	t.Parallel()

	groq, ok := Find("groq")
	require.True(t, ok)
	require.True(t, groq.NoSignupFriction(), "Groq is the email-only recommendation")
	require.False(t, groq.NeedsCard())
	require.NotEmpty(t, groq.APIKeyURL, "a one-click setup needs somewhere to send the user")
	require.NotEmpty(t, groq.Models, "curated model ids make the first request work")

	nvidia, ok := Find("nvidia-nim")
	require.True(t, ok)
	require.Equal(t, FrictionPhone, nvidia.Friction, "NVIDIA NIM needs a phone, not a card")
	require.False(t, nvidia.NeedsCard())
}
