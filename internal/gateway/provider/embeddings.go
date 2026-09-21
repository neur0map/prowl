package provider

// The embedding wire, ported from FreeLLMAPI's server/src/services/embeddings.ts
// (github.com/tashfeenahmed/freellmapi, MIT - see NOTICE.md). Embeddings are a
// second modality on the same adapters that serve chat, so the capability is an
// OPTIONAL interface a route type-asserts rather than a fourth verb on Provider:
// most of the 46 platforms have no embeddings endpoint and would have to
// implement a method they cannot serve. The shared compat type implements it
// once, dispatching on the platform to the handful of embedding wire formats
// the reference actually speaks (openAiStyleEmbed, Cohere v2/embed, HuggingFace
// feature-extraction - embeddings.ts:131-319). WHICH platforms are
// embedding-capable is not decided here: it is a property of the catalog (an
// enabled embedding_models row), checked by the router before dispatch. This
// switch only knows how to speak each platform it recognises, and refuses the
// rest so a platform with a catalog row but no wire here is surfaced rather
// than sent to an endpoint that would 404.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Embedder is implemented by adapters that can serve an embedding request. It
// is the small optional capability interface the embeddings route
// type-asserts; the shared compat type is the only implementation.
type Embedder interface {
	Embeddings(ctx context.Context, apiKey string, req *EmbeddingRequest) (*EmbeddingResponse, error)
}

// EmbeddingRequest is the gateway's normalised embedding request. Input is
// always one or more texts - the /v1 surface expands a bare string into a
// one-element slice - and Dimensions is a client MRL override forwarded only to
// platforms that honour it, nil when the caller sent none.
type EmbeddingRequest struct {
	Model      string
	Input      []string
	Dimensions *int
}

// EmbeddingResponse is one provider's answer. Vectors is index-aligned with the
// request's Input; InputTokens is the provider-reported prompt-token count, nil
// when the provider does not report one so the caller estimates instead.
type EmbeddingResponse struct {
	Vectors     [][]float64
	InputTokens *int

	// Headers carries the successful upstream response headers so the API layer
	// can read the provider's rate-limit families for quota observation; remaining
	// quota is only ever published there. Nil when the wire returned none.
	Headers http.Header
}

// ErrEmbeddingsUnsupported is the typed refusal compat returns for a platform
// it has no embedding wire for. The router degrades honestly on it rather than
// panicking or sending a request to an endpoint that would 404; it also names
// the inconsistency of a catalog that ships an embedding row for such a
// platform.
var ErrEmbeddingsUnsupported = errors.New("provider does not serve embeddings")

// embedTimeout bounds an embedding call. The reference uses a flat 30s for
// every embedding provider (embeddings.ts:112), independent of the per-platform
// chat timeout, because an embedding request carries no generation.
const embedTimeout = 30_000_000_000 // 30s in nanoseconds; time.Duration below.

// Embeddings serves one embedding request, dispatching on the platform to the
// wire format the reference uses for it (callProvider, embeddings.ts:253-319).
// A platform with no case here is refused with ErrEmbeddingsUnsupported.
func (c *compat) Embeddings(ctx context.Context, apiKey string, req *EmbeddingRequest) (*EmbeddingResponse, error) {
	switch c.cfg.platform {
	case "custom":
		// An operator's OpenAI-compatible endpoint; the base URL is the key's,
		// resolved by endpoint(). (embeddings.ts:256-258.)
		return c.openAIEmbeddings(ctx, apiKey, req, "", nil, true)
	case "google":
		// Gemini's OpenAI-compatibility endpoint, distinct from the
		// generateContent base URL chat uses (embeddings.ts:259-260).
		return c.openAIEmbeddings(ctx, apiKey, req,
			"https://generativelanguage.googleapis.com/v1beta/openai/embeddings", nil, true)
	case "nvidia":
		// NeMo Retriever NIMs require input_type; 'query' is the symmetric-safe
		// choice for a gateway that cannot know index vs query time. MRL models
		// truncate to dimensions (embeddings.ts:261-265).
		return c.openAIEmbeddings(ctx, apiKey, req,
			"https://integrate.api.nvidia.com/v1/embeddings",
			map[string]any{"input_type": "query"}, true)
	case "openrouter":
		return c.openAIEmbeddings(ctx, apiKey, req,
			"https://openrouter.ai/api/v1/embeddings", nil, true)
	case "github":
		return c.openAIEmbeddings(ctx, apiKey, req,
			"https://models.github.ai/inference/embeddings", nil, true)
	case "sealion":
		return c.openAIEmbeddings(ctx, apiKey, req,
			"https://api.sea-lion.ai/v1/embeddings", nil, true)
	case "cloudflare":
		// The account id in the compound "account_id:token" credential builds
		// the URL; endpoint() splits it. Cloudflare's BGE endpoint ignores an
		// output dimension, so it is never forwarded (embeddings.ts:272-282).
		return c.openAIEmbeddings(ctx, apiKey, req, "", nil, false)
	case "cohere":
		return c.cohereEmbeddings(ctx, apiKey, req)
	case "huggingface":
		return c.huggingFaceEmbeddings(ctx, apiKey, req)
	default:
		return nil, ErrEmbeddingsUnsupported
	}
}

// openAIEmbeddings speaks the OpenAI /embeddings wire (openAiStyleEmbed,
// embeddings.ts:131-166). An empty url derives the endpoint from the
// credential's base URL - the custom relay's stored base, or Cloudflare's
// account-scoped host from the split key - so the one code path covers both the
// fixed-host platforms and the per-credential ones. The bearer is always the
// (possibly transformed) key: even Google's key rides as a Bearer on its
// OpenAI-compatibility endpoint, not as the x-goog-api-key header chat uses.
func (c *compat) openAIEmbeddings(
	ctx context.Context, apiKey string, req *EmbeddingRequest,
	url string, extra map[string]any, forwardDimensions bool,
) (*EmbeddingResponse, error) {
	effKey := apiKey
	if url == "" {
		base, k, err := c.endpoint(apiKey)
		if err != nil {
			return nil, err
		}
		url = strings.TrimRight(base, "/") + "/embeddings"
		effKey = k
	}

	body := map[string]any{"model": req.Model, "input": req.Input}
	for k, v := range extra {
		body[k] = v
	}
	if forwardDimensions && req.Dimensions != nil {
		body["dimensions"] = *req.Dimensions
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	status, respBody, header, err := c.httpCall(ctx, "POST", url,
		map[string]string{"Authorization": "Bearer " + effKey}, raw, embedTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}

	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens *int `json:"prompt_tokens"`
			TotalTokens  *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("%s: parse embeddings response: %w", c.cfg.name, err)
	}
	// Providers may return data out of order; the caller relies on index
	// alignment with Input (embeddings.ts:161).
	sort.Slice(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
	vectors := make([][]float64, len(parsed.Data))
	for i, d := range parsed.Data {
		vectors[i] = d.Embedding
	}
	tokens := parsed.Usage.PromptTokens
	if tokens == nil {
		tokens = parsed.Usage.TotalTokens
	}
	return &EmbeddingResponse{Vectors: vectors, InputTokens: tokens, Headers: header}, nil
}

// cohereEmbeddings speaks Cohere's v2/embed wire, which is not OpenAI-shaped:
// texts rather than input, an input_type, and a float embedding type
// (embeddings.ts:300-315).
func (c *compat) cohereEmbeddings(ctx context.Context, apiKey string, req *EmbeddingRequest) (*EmbeddingResponse, error) {
	body, err := json.Marshal(map[string]any{
		"model":           req.Model,
		"texts":           req.Input,
		"input_type":      "search_document",
		"embedding_types": []string{"float"},
	})
	if err != nil {
		return nil, err
	}
	status, respBody, header, err := c.httpCall(ctx, "POST", "https://api.cohere.com/v2/embed",
		map[string]string{"Authorization": "Bearer " + apiKey}, body, embedTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}
	var parsed struct {
		Embeddings struct {
			Float [][]float64 `json:"float"`
		} `json:"embeddings"`
		Meta struct {
			BilledUnits struct {
				InputTokens *int `json:"input_tokens"`
			} `json:"billed_units"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("%s: parse embeddings response: %w", c.cfg.name, err)
	}
	return &EmbeddingResponse{
		Vectors:     parsed.Embeddings.Float,
		InputTokens: parsed.Meta.BilledUnits.InputTokens,
		Headers:     header,
	}, nil
}

// huggingFaceEmbeddings speaks HuggingFace's feature-extraction pipeline, which
// is not /v1/embeddings at all: the model id is in the path, the body is a bare
// {inputs}, and the response is number[][] (or number[] for a single input,
// which is wrapped) with no usage block (embeddings.ts:283-298).
func (c *compat) huggingFaceEmbeddings(ctx context.Context, apiKey string, req *EmbeddingRequest) (*EmbeddingResponse, error) {
	url := "https://router.huggingface.co/hf-inference/models/" + req.Model + "/pipeline/feature-extraction"
	body, err := json.Marshal(map[string]any{"inputs": req.Input})
	if err != nil {
		return nil, err
	}
	status, respBody, header, err := c.httpCall(ctx, "POST", url,
		map[string]string{"Authorization": "Bearer " + apiKey}, body, embedTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}
	// A single input can come back as a flat vector rather than a list of one.
	var nested [][]float64
	if err := json.Unmarshal(respBody, &nested); err != nil {
		var flat []float64
		if err2 := json.Unmarshal(respBody, &flat); err2 != nil {
			return nil, fmt.Errorf("%s: parse embeddings response: %w", c.cfg.name, err)
		}
		nested = [][]float64{flat}
	}
	return &EmbeddingResponse{Vectors: nested, InputTokens: nil, Headers: header}, nil
}
