package oauth

import (
	"strings"
	"testing"
)

func TestTokenExchangeErrorDoesNotExposeUpstreamBody(t *testing.T) {
	body := `{"error":"refresh token revoked","secret":"do-not-render"}`
	err := (&TokenExchangeError{StatusCode: 401, Body: body}).Error()

	if strings.Contains(err, body) || strings.Contains(err, "do-not-render") {
		t.Fatalf("error exposed upstream response body: %q", err)
	}
	if err != "OAuth session expired or was revoked; sign in again" {
		t.Fatalf("error = %q", err)
	}
}
