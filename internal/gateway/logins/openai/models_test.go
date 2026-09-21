package openai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
)

type modelsRoundTrip func(*http.Request) (*http.Response, error)

func (fn modelsRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestFetchModelsReturnsEveryVisibleSubscriptionModel(t *testing.T) {
	originalEndpoint, originalClient := modelsEndpoint, httpClient
	t.Cleanup(func() { modelsEndpoint, httpClient = originalEndpoint, originalClient })
	modelsEndpoint = "https://chatgpt.com/backend-api/codex/models"
	httpClient = &http.Client{Transport: modelsRoundTrip(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, clientVersion, req.URL.Query().Get("client_version"))
		require.Equal(t, "Bearer access-token", req.Header.Get("Authorization"))
		require.Equal(t, "account-1", req.Header.Get("chatgpt-account-id"))
		require.Equal(t, clientVersion, req.Header.Get("version"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"models":[
				{"slug":"gpt-5.6-codex","display_name":"GPT-5.6 Codex","visibility":"list","context_window":400000,"max_output_tokens":128000,"default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]},
				{"slug":"gpt-5.5-codex-mini","display_name":"GPT-5.5 Codex Mini","visibility":"list","context_window":200000,"max_output_tokens":64000,"supported_reasoning_levels":[{"effort":"medium"}]},
				{"slug":"internal-shadow","display_name":"Internal","visibility":"hidden"}
			]}`)),
		}, nil
	})}

	models, err := FetchModels(context.Background(), &oauth.Token{AccessToken: "access-token", AccountID: "account-1"})
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.Equal(t, []string{"gpt-5.6-codex", "gpt-5.5-codex-mini"}, []string{models[0].ID, models[1].ID})
	require.True(t, models[0].CanReason)
	require.Equal(t, []string{"low", "high"}, models[0].ReasoningLevels)
	require.True(t, models[0].SupportsImages)
}

// TestFetchModelsReturnsEmptyForZeroVisibleCatalog proves a valid catalog whose
// visible set filters to zero is an authoritative empty result, not an error -
// so reconciliation retires the stale rows instead of treating it as an outage.
func TestFetchModelsReturnsEmptyForZeroVisibleCatalog(t *testing.T) {
	originalEndpoint, originalClient := modelsEndpoint, httpClient
	t.Cleanup(func() { modelsEndpoint, httpClient = originalEndpoint, originalClient })
	modelsEndpoint = "https://chatgpt.com/backend-api/codex/models"
	httpClient = &http.Client{Transport: modelsRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			// A valid catalog whose only entry is not listed: the visible set is empty.
			Body: io.NopCloser(strings.NewReader(`{"models":[
				{"slug":"internal-shadow","display_name":"Internal","visibility":"hidden"}
			]}`)),
		}, nil
	})}

	models, err := FetchModels(context.Background(), &oauth.Token{AccessToken: "access-token"})
	require.NoError(t, err, "a valid catalog that filters to zero visible models must not be an error")
	require.Empty(t, models, "a zero-visible catalog is an authoritative empty set")
}

// TestFetchModelsErrorsStayErrors proves the uncertain responses remain errors,
// so reconciliation never retires rows on a transport, status, or decode failure.
func TestFetchModelsErrorsStayErrors(t *testing.T) {
	originalEndpoint, originalClient := modelsEndpoint, httpClient
	t.Cleanup(func() { modelsEndpoint, httpClient = originalEndpoint, originalClient })
	modelsEndpoint = "https://chatgpt.com/backend-api/codex/models"

	cases := map[string]func(*http.Request) (*http.Response, error){
		"non-200": func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("down"))}, nil
		},
		"malformed body": func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("not json"))}, nil
		},
		"absent models array": func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`))}, nil
		},
		"wrong-typed models field": func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":{"slug":"x"}}`))}, nil
		},
	}
	for name, rt := range cases {
		t.Run(name, func(t *testing.T) {
			httpClient = &http.Client{Transport: modelsRoundTrip(rt)}
			_, err := FetchModels(context.Background(), &oauth.Token{AccessToken: "access-token"})
			require.Error(t, err, "an uncertain response must stay an error, never an authoritative empty set")
		})
	}
}
