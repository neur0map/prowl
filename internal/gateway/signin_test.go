package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/gateway/logins/browserflow"
	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
	"github.com/stretchr/testify/require"
)

func TestSignInOutlivesStartingRequest(t *testing.T) {
	vault := &LoginVault{}
	registry := NewSignInRegistry(vault)
	started := make(chan struct{})
	finished := make(chan struct{})
	registry.startLogin = func(_ context.Context, provider string) (*PendingLogin, error) {
		return &PendingLogin{
			Provider: provider,
			URL:      "https://example.com/authorize",
			vault:    vault,
			complete: func(ctx context.Context) (*oauth.Token, error) {
				close(started)
				<-ctx.Done()
				close(finished)
				return nil, ctx.Err()
			},
		}, nil
	}

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	session, err := registry.Start(requestCtx, "anthropic")
	require.NoError(t, err)
	<-started
	cancelRequest()

	select {
	case <-finished:
		t.Fatal("browser flow was cancelled with the completed HTTP request")
	case <-time.After(50 * time.Millisecond):
	}
	current, err := registry.Status(session.ID)
	require.NoError(t, err)
	require.Equal(t, SignInPending, current.State)

	require.True(t, registry.Cancel(session.ID))
	require.Eventually(t, func() bool {
		select {
		case <-finished:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}

func TestSignableKeepsReadableStoredLoginHealthy(t *testing.T) {
	vault := &LoginVault{logins: map[string]*StoredLogin{
		"openai": {
			Provider: "openai",
			Token:    &oauth.Token{AccessToken: "live-token"},
		},
	}}

	var openAI SignInPlatformRow
	for _, row := range NewSignInRegistry(vault).Signable() {
		if row.ID == "openai" {
			openAI = row
			break
		}
	}
	require.True(t, openAI.SignedIn)
	require.False(t, openAI.Broken, "safe metadata listing must not make a readable token look broken")
}

func TestSignInCompletesOnlyAfterRoutingReconciliation(t *testing.T) {
	vault, err := OpenLoginVault(t.TempDir())
	require.NoError(t, err)
	registry := NewSignInRegistry(vault)
	registry.startLogin = func(_ context.Context, provider string) (*PendingLogin, error) {
		return &PendingLogin{
			Provider: provider,
			URL:      "https://example.com/authorize",
			vault:    vault,
			complete: func(context.Context) (*oauth.Token, error) {
				return &oauth.Token{AccessToken: "live-token"}, nil
			},
		}, nil
	}
	reconciling := make(chan string, 1)
	release := make(chan struct{})
	registry.SetOnComplete(func(provider string) {
		reconciling <- provider
		<-release
	})

	session, err := registry.Start(t.Context(), "anthropic")
	require.NoError(t, err)
	require.Equal(t, "anthropic", <-reconciling)

	current, err := registry.Status(session.ID)
	require.NoError(t, err)
	require.Equal(t, SignInPending, current.State, "connected must not be reported before its models are reconciled")
	close(release)

	require.Eventually(t, func() bool {
		current, err = registry.Status(session.ID)
		return err == nil && current.State == SignInComplete
	}, time.Second, time.Millisecond)
	require.Equal(t, "anthropic", current.Provider)
	linkable := vault.Linkable(t.Context())
	require.Len(t, linkable, 1)
	require.True(t, linkable[0].PoolEnabled)
}

// TestSignInExpiryReleasesCallbackListener proves an abandoned browser flow
// releases its loopback callback listener at its deadline, with no client ever
// polling it and no successful flow ever completing.
func TestSignInExpiryReleasesCallbackListener(t *testing.T) {
	vault := &LoginVault{logins: map[string]*StoredLogin{}}
	registry := NewSignInRegistry(vault)
	registry.grace = 0 // force the flow to expire essentially immediately

	// Reserve a loopback port, then hand the registry a real browser flow bound
	// to it so we can observe the listener being released.
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := reserve.Addr().(*net.TCPAddr).Port
	require.NoError(t, reserve.Close())
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	registry.startLogin = func(ctx context.Context, provider string) (*PendingLogin, error) {
		flow, err := browserflow.Start(ctx, browserflow.Config{
			AuthorizeURL: "https://example.com/authorize",
			ClientID:     "test-client",
			RedirectURI:  redirect,
			Scope:        "openid",
			Exchange: func(context.Context, string, string, string, string) (*oauth.Token, error) {
				return nil, errors.New("exchange must not run in this test")
			},
		})
		if err != nil {
			return nil, err
		}
		return &PendingLogin{
			Provider: provider, URL: flow.URL, vault: vault,
			complete: func(ctx context.Context) (*oauth.Token, error) { return flow.Wait(ctx) },
			flow:     flow,
		}, nil
	}

	session, err := registry.Start(t.Context(), "anthropic")
	require.NoError(t, err)

	// The scheduled expiry must release the loopback listener even though no
	// flow ever completed - re-binding the same port proves it.
	require.Eventually(t, func() bool {
		probe, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return false
		}
		_ = probe.Close()
		return true
	}, 2*time.Second, 5*time.Millisecond)

	// And the flow's verdict is observable as cancelled, not successful.
	require.Eventually(t, func() bool {
		current, err := registry.Status(session.ID)
		return err == nil && current.State == SignInCancelled
	}, 2*time.Second, 5*time.Millisecond)
}
