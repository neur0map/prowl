package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/gateway/provider"
)

// TestProviderTransportRejectsPrivateConnectAddress proves the connect-time
// SSRF guard the gateway installs (via init, provider.DialGuard) actually fires
// on the routed inference transport: a custom endpoint whose dialled address
// classifies as metadata (always) or private (when blocking is enabled) is
// refused at connect time on the registry's own client, before the stored
// credential can leave the process. This is the DNS-rebinding half of
// AssessProviderURL, so it must live on the real transport, not just the check.
func TestProviderTransportRejectsPrivateConnectAddress(t *testing.T) {
	if provider.DialGuard == nil {
		t.Fatal("gateway init did not install provider.DialGuard on the provider transport")
	}

	t.Run("metadata address always refused", func(t *testing.T) {
		p, ok := provider.NewRegistry().Resolve("custom", "http://169.254.169.254/v1")
		if !ok {
			t.Fatal("custom did not resolve")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := p.ChatCompletion(ctx, "k", &provider.ChatRequest{Model: "m"})
		if err == nil {
			t.Fatal("dial to a cloud-metadata address must be refused")
		}
		if !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("want a metadata refusal, got %v", err)
		}
	})

	t.Run("private address refused when blocking enabled", func(t *testing.T) {
		t.Setenv("PROWL_BLOCK_PRIVATE_PROVIDER_URLS", "1")
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("guard let a private-address dial reach the upstream")
		}))
		defer srv.Close()
		p, ok := provider.NewRegistry().Resolve("custom", srv.URL)
		if !ok {
			t.Fatal("custom did not resolve")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := p.ChatCompletion(ctx, "k", &provider.ChatRequest{Model: "m"})
		if err == nil {
			t.Fatal("dial to a loopback address with blocking enabled must be refused")
		}
		if !strings.Contains(err.Error(), "private") {
			t.Fatalf("want a private-address refusal, got %v", err)
		}
	})
}
