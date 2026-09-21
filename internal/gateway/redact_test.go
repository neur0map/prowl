package gateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRealProviderKeysAreRedacted is the case that motivates the whole file:
// providers echo the rejected key back in their error message, and the gateway
// relays that message into a log, a dashboard row and an HTTP response. One
// 401 must not print a working credential into a file that outlives the
// session.
func TestRealProviderKeysAreRedacted(t *testing.T) {
	t.Parallel()

	// Shaped like the real thing, with enough entropy to match the patterns.
	secrets := map[string]string{
		"openai-style":   "sk-or-v1-a4d8cf567d8af5572cf59f8b867494f4",
		"pollinations":   "sk_9f2b7c1d4e6a8b0c2d4e6f8a",
		"groq":           "gsk_7h3k9m2p5r8t1v4x6z9b2d5f",
		"cerebras":       "csk-4t7y2u9i6o3p1a8s5d2f7g",
		"nvidia":         "nvapi-8k3j6h9g2f5d8s1a4p7o",
		"google":         "AIzaSyD9x2K4m7P1q8R3t6V9y2B5e8H1k4N7p0Q",
		"github-fine":    "github_pat_11ABCDEFG0hIjKlMnOpQrS",
		"github-classic": "ghp_16CharsAndMore1234567890abc",
		"huggingface":    "hf_QwErTyUiOpAsDfGhJkLzXc",
		"cloudflare":     "cfut_1a2b3c4d5e6f7g8h9i0j",
		"vercel":         "vck_9z8y7x6w5v4u3t2s1r0q",
		"chutes":         "cpk_1.2a3b4c5d6e7f8g9h0i1j",
		"aion":           "alv2_1a2b3c4d5e6f7g8h9i0j",
		"requesty":       "rqsty-sk-1a2b3c4d5e6f7g8h9i0j",
		"ours":           "prowlag-0123456789abcdef0123456789abcdef0123456789abcdef",
	}

	for name, secret := range secrets {
		message := "provider rejected the request: invalid key " + secret + " (code 401)"
		out := Redact(message)
		require.NotContains(t, out, secret, "%s key survived redaction", name)
		require.Contains(t, out, redactedMarker, "%s should have been marked", name)
		require.Contains(t, out, "code 401", "%s: the useful part of the message must survive", name)
	}
}

// TestBearerAndHeaderFormsAreRedacted covers the other way a key reaches a
// log: serialised request headers in a debug dump.
func TestBearerAndHeaderFormsAreRedacted(t *testing.T) {
	t.Parallel()

	for name, text := range map[string]string{
		"bearer":         `upstream call failed; headers: {"Authorization":"Bearer abc123def456ghi789"}`,
		"x-api-key":      `headers: {"x-api-key":"abc123def456ghi789"}`,
		"x-goog-api-key": `headers: {"x-goog-api-key":"AIzaSyD9x2K4m7P1q8R3t6V9y2B5e"}`,
		"api_key field":  `body was {"api_key":"abc123def456ghi789","model":"m"}`,
	} {
		out := Redact(text)
		require.NotContains(t, out, "abc123def456ghi789", "%s leaked", name)
		require.Contains(t, out, redactedMarker, "%s not marked", name)
	}
}

// TestRedactionKeepsWhatDebuggingNeeds is the counterweight. A log that
// scrubs the identifiers you debug with is a log nobody reads, so model ids,
// request ids, hashes and ordinary prose must survive intact.
func TestRedactionKeepsWhatDebuggingNeeds(t *testing.T) {
	t.Parallel()

	for _, keep := range []string{
		"model=nvidia/nemotron-3.5-lightning:free",
		"request_id=1a2b3c4d5e6f",
		"attempt trail: custom/llama-3 key1: rate_limited",
		"sha256-4Mz/yZAENQGlTAAeE1WqXruXCripvlvl0s+Q9S1VS4A=",
		"every provider attempt failed; last status 502",
		"platform=groq model=llama-3.3-70b tokens=1843 latency_ms=412",
	} {
		require.Equal(t, keep, Redact(keep),
			"redaction must not touch text a reader needs: %q", keep)
	}
}

// TestRedactionIsIdempotent keeps a message that passes through two layers
// from accumulating markers.
func TestRedactionIsIdempotent(t *testing.T) {
	t.Parallel()

	once := Redact("invalid key gsk_7h3k9m2p5r8t1v4x6z9b2d5f")
	twice := Redact(once)
	require.Equal(t, once, twice)
	require.Equal(t, 1, strings.Count(twice, redactedMarker))
}

// TestRedactErrorHandlesNil keeps the helper usable on an error path without a
// nil check at every call site.
func TestRedactErrorHandlesNil(t *testing.T) {
	t.Parallel()
	require.Empty(t, RedactError(nil))
	require.Empty(t, Redact(""))
}
