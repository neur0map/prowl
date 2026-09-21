# Provider catalog attribution

`providers.json` starts from generated upstream data, then receives a reviewed
compatibility pass for endpoint, authentication, and retirement changes. The
upstream merge combines two MIT-licensed sources and deduplicates them by API
endpoint.

## Sources

| Source | Used for | License |
|---|---|---|
| [open-free-llm-api/awesome-freellm-apis](https://github.com/open-free-llm-api/awesome-freellm-apis) | Free-tier providers: base URLs, API-key signup links, card/verification requirements, max context, modalities, and the curated best-free-model lists | MIT |
| [BerriAI/litellm](https://github.com/BerriAI/litellm) | Additional provider endpoints and their conventional API-key environment variable names | MIT |

Both upstreams are MIT licensed. Their copyright notices are reproduced in
`NOTICE.md` at the repository root.

## What the generation does

1. Parses the marker-delimited tables in the freellm `README.md`
   (`PERMANENT_FREE`, `RENEWABLE`, `QUICK_REF`, `BEST_MODELS`) into provider
   records with their free-model lists.
2. Parses litellm's `get_llm_provider_logic.py` for provider endpoints and
   `API_KEY` environment variable names, and adds any endpoint the freellm data
   does not already cover.
3. Drops entries with no endpoint (upstream ships a few placeholder rows that
   carry neither a base URL nor a key link, which cannot be routed to).
4. Drops providers whose authentication is SDK-specific rather than a bearer
   key - Bedrock, SageMaker, Vertex AI, Azure - because the gateway's premise
   is base URL plus API key. They are better served by their own SDKs.
5. Deduplicates by normalised base URL. Upstream lists some endpoints twice
   under different names (for example `xAI` and `Grok (xAI)` both resolve to
   `https://api.x.ai/v1`); the richer record wins, the other's models are
   folded in, and the discarded name is kept in `aliases` so a search for it
   still resolves.
6. Classifies each endpoint's wire format in `compat` and records any
   `{placeholder}` in the base URL as `requires_vars`. Only
   `compat: openai` entries with no unfilled placeholders are routable; the
   rest are listed in the dashboard with an explicit badge rather than offered
   as one-click setups that would fail on the first request.

The compatibility pass is intentionally conservative. On 2026-09-20 every
advertised generic endpoint was probed at both `/models` and
`/chat/completions`. Retired endpoints, unreachable TLS services, APIs that
require an OAuth exchange, and Responses-only APIs remain discoverable with a
reason in the provider directory but are not assigned a generic adapter.
Codestral is the one documented exception to model-list discovery: its
dedicated chat endpoint is authenticated with a no-charge nonexistent-model
probe and the documented `codestral-2508` model is registered.

## Current contents

- 45 providers, one per unique endpoint
- 26 with a standing free tier and at least one currently advertised free model
- 32 routable as OpenAI-compatible endpoints today
- 13 listed but not generically routable: Cloudflare Workers AI, Google Gemini,
  GitHub Models, Cohere, AI21 Labs, Glhf.chat, Anyscale, Empower, Galadriel,
  GigaChat, GitHub Models (legacy), Manus, and Soniox
- 81 curated free models carrying concrete model ids

## Refreshing

The upstream free-tier data changes often - rate limits move and free models
come and go. Re-run the generation against both upstreams and review the diff;
the dedup and classification steps above are deliberate and must be preserved.
Treat `compat` and `requires_vars` as reviewed fields: a new provider defaults
to non-routable until someone confirms its wire format.
