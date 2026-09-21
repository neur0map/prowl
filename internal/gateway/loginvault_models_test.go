package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
)

type loginModelsRoundTrip func(*http.Request) (*http.Response, error)

func (fn loginModelsRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestAnthropicLoginDiscoversMultipleAccountModels(t *testing.T) {
	original := loginModelsClient
	t.Cleanup(func() { loginModelsClient = original })
	loginModelsClient = &http.Client{Transport: loginModelsRoundTrip(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "https://api.anthropic.com/v1/models", req.URL.String())
		require.Equal(t, "Bearer sk-ant-oat-test", req.Header.Get("Authorization"))
		require.Empty(t, req.Header.Get("X-Api-Key"))
		require.Equal(t, "oauth-2025-04-20", req.Header.Get("anthropic-beta"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"data":[
				{"id":"claude-opus-5","display_name":"Claude Opus 5"},
				{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"},
				{"id":"claude-haiku-4-5","display_name":"Claude Haiku 4.5"}
			]}`)),
		}, nil
	})}
	vault := &LoginVault{logins: map[string]*StoredLogin{
		"anthropic": {
			Provider: "anthropic",
			Token: &oauth.Token{AccessToken: "sk-ant-oat-test", ExpiresIn: 3600,
				ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}}

	models := vault.Models(context.Background(), "anthropic")
	require.Len(t, models, 3)
	require.Equal(t, []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5"},
		[]string{models[0].ID, models[1].ID, models[2].ID})
	require.True(t, models[0].Flagship)
	require.True(t, models[0].CanReason)
	require.Equal(t, int64(1_000_000), models[1].ContextWindow)
	require.True(t, models[2].Small)
}

func TestAnthropicLoginUsesCurrentMultiModelFallbackOnDiscoveryFailure(t *testing.T) {
	original := loginModelsClient
	t.Cleanup(func() { loginModelsClient = original })
	loginModelsClient = &http.Client{Transport: loginModelsRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("down"))}, nil
	})}
	vault := &LoginVault{logins: map[string]*StoredLogin{
		"anthropic": {
			Provider: "anthropic",
			Token: &oauth.Token{AccessToken: "sk-ant-oat-test", ExpiresIn: 3600,
				ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}}

	models := vault.Models(context.Background(), "anthropic")
	require.Greater(t, len(models), 1)
	require.Equal(t, "claude-opus-5", models[0].ID)
	require.Equal(t, "claude-haiku-4-5", models[len(models)-1].ID)
}
