// Package openai authenticates ChatGPT subscriptions against the Codex backend.
// Adapted from Charmbracelet Crush's OpenAI OAuth implementation (PR #3731).
package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway/logins/browserflow"
	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
)

const (
	ClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexBaseURL = "https://chatgpt.com/backend-api/codex"
	redirectURI  = "http://localhost:1455/auth/callback"
)

var (
	tokenEndpoint = "https://auth.openai.com/oauth/token"
	httpClient    = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
)

// Start opens the fixed loopback callback used by OpenAI's public OAuth client.
func Start(ctx context.Context) (*browserflow.Flow, error) {
	return browserflow.Start(ctx, browserflow.Config{
		AuthorizeURL: "https://auth.openai.com/oauth/authorize", ClientID: ClientID, RedirectURI: redirectURI,
		Scope: "openid profile email offline_access api.connectors.read api.connectors.invoke", Subject: "OpenAI (ChatGPT)",
		ExtraAuth: url.Values{"id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}, "originator": {"prowl"}},
		Exchange: func(ctx context.Context, code, redirect, verifier, _ string) (*oauth.Token, error) {
			token, err := exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {ClientID}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
			if err == nil && token.RefreshToken == "" {
				return nil, fmt.Errorf("OpenAI did not grant offline access; retry login")
			}
			return token, err
		},
	})
}

// RefreshToken rotates an OAuth grant, retaining a refresh token if not rotated.
func RefreshToken(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("OpenAI refresh token missing; run prowl login openai --force")
	}
	token, err := exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {ClientID}, "refresh_token": {refreshToken}})
	if err != nil {
		return nil, err
	}
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	return token, nil
}

func exchange(ctx context.Context, values url.Values) (*oauth.Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenAI token exchange: %w", err)
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	token, err := oauth.DecodeTokenResponse(resp)
	if err != nil {
		return nil, err
	}
	token.AccountID = accountID(token.AccessToken)
	if token.AccountID == "" {
		token.AccountID = accountID(token.IDToken)
	}
	return token, nil
}

// JWT claims are advisory routing metadata, not client-side authorization.
func accountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		AccountID string `json:"chatgpt_account_id"`
		Auth      struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.Auth.AccountID != "" {
		return claims.Auth.AccountID
	}
	return claims.AccountID
}

// AccountID extracts the advisory ChatGPT account id carried by an OAuth JWT.
// It is sent only to chatgpt.com by Transport; it is not an authorization
// decision and an absent claim is valid.
func AccountID(token string) string {
	return accountID(token)
}
