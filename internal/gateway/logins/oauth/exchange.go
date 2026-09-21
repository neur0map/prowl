package oauth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DecodeTokenResponse reads a bounded response without exposing credential-bearing
// error bodies. Providers may enrich the returned identity from JWT claims.
func DecodeTokenResponse(resp *http.Response) (*Token, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read OAuth response: %w", err)
	}
	if len(body) > 1<<20 {
		return nil, fmt.Errorf("OAuth response exceeds size limit")
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		var code string
		if json.Unmarshal(failure.Error, &code) != nil {
			var detail struct {
				Code string `json:"code"`
				Type string `json:"type"`
			}
			_ = json.Unmarshal(failure.Error, &detail)
			code = detail.Code
			if code == "" {
				code = detail.Type
			}
		}
		switch code {
		case "invalid_grant", "invalid_client", "access_denied", "unauthorized_client", "temporarily_unavailable", "invalid_request", "unsupported_grant_type":
		default:
			code = "authorization_failed"
		}
		return nil, &TokenExchangeError{StatusCode: resp.StatusCode, Body: code}
	}
	var wire struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		Account      struct {
			UUID string `json:"uuid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid OAuth token response")
	}
	if wire.AccessToken == "" || wire.ExpiresIn <= 0 || wire.ExpiresIn > 365*24*60*60 {
		return nil, fmt.Errorf("OAuth response is missing a valid access token or expiry")
	}
	token := &Token{AccessToken: wire.AccessToken, RefreshToken: wire.RefreshToken, IDToken: wire.IDToken, ExpiresIn: wire.ExpiresIn, AccountID: wire.Account.UUID}
	token.SetExpiresAt()
	return token, nil
}
