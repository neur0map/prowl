package browserflow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
	"github.com/stretchr/testify/require"
)

func callbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

func dualStackPort(t *testing.T) string {
	t.Helper()
	ipv4, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(ipv4.Addr().String())
	require.NoError(t, err)
	ipv6, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp6", net.JoinHostPort("::1", port))
	if err != nil {
		_ = ipv4.Close()
		t.Skip("IPv6 loopback is unavailable")
	}
	require.NoError(t, ipv6.Close())
	require.NoError(t, ipv4.Close())
	return port
}

func TestLocalhostCallbackAcceptsIPv6(t *testing.T) {
	port := dualStackPort(t)
	redirect := "http://localhost:" + port + "/callback"
	flow, err := Start(t.Context(), Config{
		AuthorizeURL: "https://example.com/authorize",
		ClientID:     "public-client",
		RedirectURI:  redirect,
		Exchange: func(_ context.Context, code, redirectURI, _, _ string) (*oauth.Token, error) {
			require.Equal(t, "valid", code)
			require.Equal(t, redirect, redirectURI)
			return &oauth.Token{AccessToken: "accepted"}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Close() })
	authorize, err := url.Parse(flow.URL)
	require.NoError(t, err)
	callback := "http://" + net.JoinHostPort("::1", port) + "/callback?" + url.Values{
		"state": {authorize.Query().Get("state")},
		"code":  {"valid"},
	}.Encode()
	client := &http.Client{
		Timeout:   time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Get(callback)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	token, err := flow.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "accepted", token.AccessToken)
}

func TestCallbackRejectsUnboundStateWithoutConsumingGrant(t *testing.T) {
	t.Parallel()
	address := callbackAddress(t)
	redirect := "http://" + address + "/callback"
	var verifier, exchangedState string
	calls := 0
	flow, err := Start(t.Context(), Config{AuthorizeURL: "https://example.com/authorize", ClientID: "public-client", RedirectURI: redirect, Exchange: func(_ context.Context, code, redirectURI, proof, state string) (*oauth.Token, error) {
		calls++
		if code != "valid" || redirectURI != redirect {
			return nil, context.Canceled
		}
		verifier, exchangedState = proof, state
		return &oauth.Token{AccessToken: "accepted"}, nil
	}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Close() })
	authorize, err := url.Parse(flow.URL)
	require.NoError(t, err)
	state := authorize.Query().Get("state")
	client := &http.Client{Timeout: time.Second}
	for _, query := range []string{"code=valid", "state=wrong&code=valid", "state=" + state + "&state=wrong&code=valid", "state=" + state} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, redirect+"?"+query, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, redirect+"?state="+state+"&code=valid", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	token, err := flow.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "accepted", token.AccessToken)
	require.Equal(t, 1, calls)
	require.Equal(t, state, exchangedState)
	challenge := sha256.Sum256([]byte(verifier))
	require.Equal(t, base64.RawURLEncoding.EncodeToString(challenge[:]), authorize.Query().Get("code_challenge"))
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", address)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}

func TestCancellationReleasesListenerBeforeWait(t *testing.T) {
	t.Parallel()
	address := callbackAddress(t)
	ctx, cancel := context.WithCancel(t.Context())
	flow, err := Start(ctx, Config{AuthorizeURL: "https://example.com/authorize", ClientID: "client", RedirectURI: "http://" + address + "/callback", Exchange: func(context.Context, string, string, string, string) (*oauth.Token, error) {
		t.Error("canceled flow exchanged a grant")
		return nil, nil
	}})
	require.NoError(t, err)
	cancel()
	require.Eventually(t, func() bool {
		listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", address)
		if err != nil {
			return false
		}
		_ = listener.Close()
		return true
	}, time.Second, time.Millisecond)
	_, err = flow.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
}
