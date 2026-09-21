package anthropic

// Pinned to the values the OAuth flow declares: the beta grant the token
// exchange and the subscription bearer both carry, and the Claude Code SDK
// version the exchange names as the client. Copied verbatim from the
// fingerprint header set the subscription transport uses, because the
// provider matches on them.
const (
	oauthBeta            = "oauth-2025-04-20"
	claudeCodeSDKVersion = "0.112.1"
)
