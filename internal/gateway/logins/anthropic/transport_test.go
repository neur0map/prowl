package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestOAuthTransportAppliesClaudeCodeContractAndRestoresToolNames(t *testing.T) {
	var capturedHeader http.Header
	var capturedBody []byte
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedHeader = req.Header.Clone()
		var err error
		capturedBody, err = io.ReadAll(req.Body)
		require.NoError(t, err)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"type":"message","content":[{"type":"tool_use","id":"call-1","name":"_read_file","input":{"path":"main.go"}}]}`)),
		}, nil
	})
	client := &http.Client{Transport: &Transport{
		Base:  base,
		Token: &oauth.Token{AccessToken: "sk-ant-oat-test"},
	}}
	body := `{"model":"claude-sonnet-5","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"Inspect main.go carefully"}]}],"tools":[{"name":"read_file","input_schema":{"type":"object"}}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://api.anthropic.com/v1/messages", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", "must-not-leak")

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, "Bearer sk-ant-oat-test", capturedHeader.Get("Authorization"))
	require.Empty(t, capturedHeader.Get("X-Api-Key"))
	require.Contains(t, capturedHeader.Get("User-Agent"), "claude-cli/")
	require.Contains(t, capturedHeader.Get("anthropic-beta"), oauthBeta)
	require.NotEmpty(t, capturedHeader.Get("x-claude-code-session-id"))

	var sent struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(capturedBody, &sent))
	require.GreaterOrEqual(t, len(sent.System), 2)
	require.Contains(t, sent.System[0].Text, claudeBillingHeaderPrefix)
	require.NotContains(t, sent.System[0].Text, cchPlaceholderText)
	require.Equal(t, claudeCodeSystemInstruction, sent.System[1].Text)
	require.Equal(t, "_read_file", sent.Tools[0].Name)
	require.Contains(t, string(responseBody), `"name":"read_file"`)
	require.NotContains(t, string(responseBody), `"name":"_read_file"`)
}

func TestApplyAuthHeadersSeparatesAPIKeysAndOAuth(t *testing.T) {
	api := make(http.Header)
	ApplyAuthHeaders(api, "sk-ant-api-test")
	require.Equal(t, "sk-ant-api-test", api.Get("X-Api-Key"))
	require.Empty(t, api.Get("Authorization"))

	oauthHeader := make(http.Header)
	ApplyAuthHeaders(oauthHeader, "sk-ant-oat-test")
	require.Equal(t, "Bearer sk-ant-oat-test", oauthHeader.Get("Authorization"))
	require.Empty(t, oauthHeader.Get("X-Api-Key"))
	require.Equal(t, oauthBeta, oauthHeader.Get("anthropic-beta"))
}
