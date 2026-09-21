// Package anthropic implements the native Claude subscription flow used by OMP.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/neur0map/prowl/internal/gateway/logins/browserflow"
	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
)

const (
	BaseURL  = "https://api.anthropic.com"
	clientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

var (
	tokenEndpoint = BaseURL + "/v1/oauth/token"
	httpClient    = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
)

// Start opens a Claude browser authorization flow, retaining Prowl's agent loop.
func Start(ctx context.Context) (*browserflow.Flow, error) {
	return browserflow.Start(ctx, browserflow.Config{
		AuthorizeURL: "https://claude.ai/oauth/authorize", ClientID: clientID, RedirectURI: "http://localhost:54545/callback", Subject: "Anthropic (Claude)",
		Scope:     "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload",
		ExtraAuth: url.Values{"code": {"true"}},
		Exchange: func(ctx context.Context, code, redirect, verifier, state string) (*oauth.Token, error) {
			token, err := exchange(ctx, map[string]string{"grant_type": "authorization_code", "client_id": clientID, "code": code, "redirect_uri": redirect, "code_verifier": verifier, "state": state}, false)
			if err == nil && token.RefreshToken == "" {
				return nil, fmt.Errorf("claude did not grant a refresh token; retry login")
			}
			return token, err
		},
	})
}

// RefreshToken rotates the subscription grant without reopening the browser.
func RefreshToken(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("claude refresh token missing; run prowl login anthropic --force")
	}
	token, err := exchange(ctx, map[string]string{"grant_type": "refresh_token", "client_id": clientID, "refresh_token": refreshToken}, true)
	if err != nil {
		return nil, err
	}
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	return token, nil
}

func exchange(ctx context.Context, values map[string]string, refresh bool) (*oauth.Token, error) {
	body, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if refresh {
		req.Header.Set("anthropic-beta", oauthBeta)
		req.Header.Set("User-Agent", "anthropic-sdk-typescript/"+claudeCodeSDKVersion+" userOAuthProvider")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude token exchange: %w", err)
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	return oauth.DecodeTokenResponse(resp)
}
