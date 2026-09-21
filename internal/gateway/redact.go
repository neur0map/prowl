package gateway

import (
	"regexp"
	"strings"
)

// Credential redaction, ported from FreeLLMAPI
// (github.com/tashfeenahmed/freellmapi, MIT, v0.9.9 - see NOTICE.md;
// server/src/lib/log-redaction.ts, error-redaction.ts).
//
// A gateway holds a dozen provider credentials and relays their error bodies
// verbatim. Providers routinely echo the key back in a rejection message, and
// a relayed upstream error becomes a log line, a dashboard row and an HTTP
// response. Without this, a single 401 from one provider can print a working
// key from another into a log file that outlives the session.
//
// The patterns are grouped by key SHAPE rather than by vendor, so a new
// provider that adopts an existing convention is covered without an edit.
const redactedMarker = "[redacted-key]"

type redaction struct {
	pattern     *regexp.Regexp
	replacement string
}

// Ordered most specific first: prefixed provider keys, then bearer and
// header forms. A general high-entropy sweep is deliberately NOT included -
// it hits model ids, request ids and hashes, and a log that redacts the
// identifiers you need to debug with is a log nobody reads.
var redactions = []redaction{
	// OpenAI-style, shared by OpenRouter, SiliconFlow, Nara, NavyAI,
	// SEA-LION, Agnes and OpenCode.
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_\-/+=]{8,}`), redactedMarker},
	{regexp.MustCompile(`\bsk_[A-Za-z0-9_\-]{8,}`), redactedMarker},           // Pollinations
	{regexp.MustCompile(`\bgsk_[A-Za-z0-9_\-]{8,}`), redactedMarker},          // Groq
	{regexp.MustCompile(`\bcsk-[A-Za-z0-9_\-]{8,}`), redactedMarker},          // Cerebras
	{regexp.MustCompile(`\bnvapi-[A-Za-z0-9_\-]{8,}`), redactedMarker},        // NVIDIA
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{20,}`), redactedMarker},         // Google
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`), redactedMarker},    // GitHub fine-grained
	{regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}`), redactedMarker},            // GitHub classic
	{regexp.MustCompile(`\bhf_[A-Za-z0-9]{16,}`), redactedMarker},             // HuggingFace
	{regexp.MustCompile(`\bcfut_[A-Za-z0-9]{16,}`), redactedMarker},           // Cloudflare
	{regexp.MustCompile(`(?i)\brc-[0-9a-f]{48}\b`), redactedMarker},           // AMD Radeon Cloud
	{regexp.MustCompile(`\bvck_[A-Za-z0-9]{16,}`), redactedMarker},            // Vercel
	{regexp.MustCompile(`\bcpk_[A-Za-z0-9.]{16,}`), redactedMarker},           // Chutes
	{regexp.MustCompile(`\balv2_[A-Za-z0-9]{16,}`), redactedMarker},           // Aion Labs
	{regexp.MustCompile(`\brqsty-sk-[A-Za-z0-9_\-/+=]{16,}`), redactedMarker}, // Requesty

	// The credential this gateway itself issues.
	{regexp.MustCompile(`\bprowlag-[0-9a-f]{8,}`), redactedMarker},

	// Authorization headers, in prose or serialised objects. The negative
	// lookahead equivalent is handled by running these after the prefix
	// patterns and skipping an already-redacted value.
	{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/\-]+=*`), "Bearer " + redactedMarker},
	{regexp.MustCompile(`(?i)(\bx-api-key\b["']?\s*[:=]\s*["']?)[^"',\s}\]&]+`), "${1}" + redactedMarker},
	{regexp.MustCompile(`(?i)(\bx-goog-api-key\b["']?\s*[:=]\s*["']?)[^"',\s}\]&]+`), "${1}" + redactedMarker},
	{regexp.MustCompile(`(?i)(\bapi[_-]?key\b["']?\s*[:=]\s*["']?)[^"',\s}\]&]+`), "${1}" + redactedMarker},
}

// Redact removes credentials from text that is about to be logged, stored or
// returned. It is cheap enough to apply unconditionally: the alternative is
// deciding per call site whether a string might contain a key, and that
// judgement is wrong exactly once before a credential is on disk.
func Redact(text string) string {
	if text == "" {
		return text
	}
	for _, r := range redactions {
		if strings.Contains(text, redactedMarker) && r.replacement == redactedMarker {
			// Already-redacted text can still contain other secrets, so keep
			// going; this only avoids re-marking the marker itself.
			text = r.pattern.ReplaceAllStringFunc(text, func(match string) string {
				if strings.Contains(match, redactedMarker) {
					return match
				}
				return redactedMarker
			})
			continue
		}
		text = r.pattern.ReplaceAllString(text, r.replacement)
	}
	return text
}

// RedactWith removes the given secrets by exact match before applying the
// shape patterns.
//
// The patterns recognise fifteen key conventions, which does not cover
// forty-odd providers and cannot cover a custom endpoint whose key shape is
// whatever its operator chose. But at the moment a credential is used, the
// caller HOLDS it - so substituting that exact string is complete regardless
// of shape, and the patterns then catch keys belonging to other providers
// that an error body may also mention.
//
// Short secrets are skipped: a two-character "key" would redact half the
// message, and a credential that short is not one worth protecting.
func RedactWith(text string, secrets ...string) string {
	if text == "" {
		return text
	}
	for _, secret := range secrets {
		if len(secret) < 8 {
			continue
		}
		text = strings.ReplaceAll(text, secret, redactedMarker)
	}
	return Redact(text)
}

// RedactError applies Redact to an error's message, returning a plain error.
// A provider's rejection is the single most likely place for a key to appear,
// and that rejection is what the router logs and the dashboard shows.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	return Redact(err.Error())
}
