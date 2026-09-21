package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/logins/anthropic"
)

// fakeCredsource is a stand-in for the login vault: it reports which accounts
// are signed in and hands out a bearer (or a per-account failure) without any
// network or refresh flow.
type fakeCredsource struct {
	linkable []gateway.LinkableProvider
	tokens   map[string]string
	errs     map[string]error
}

func (f *fakeCredsource) Credential(_ context.Context, provider string) (string, bool, error) {
	if err, ok := f.errs[provider]; ok {
		return "", true, err
	}
	tok, ok := f.tokens[provider]
	if !ok {
		return "", false, nil
	}
	return tok, true, nil
}

func (f *fakeCredsource) Linkable(context.Context) []gateway.LinkableProvider  { return f.linkable }
func (f *fakeCredsource) Models(context.Context, string) []gateway.LinkedModel { return nil }

type usageEnvelope struct {
	Accounts []struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		Windows  []struct {
			Key         string  `json:"key"`
			Label       string  `json:"label"`
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resetsAt"`
		} `json:"windows"`
		Balance     *float64 `json:"balance"`
		Unit        string   `json:"unit"`
		Cadence     string   `json:"cadence"`
		Note        string   `json:"note"`
		NeedsSignIn bool     `json:"needsSignIn"`
		Error       string   `json:"error"`
	} `json:"accounts"`
}

func swapUsageSeams(t *testing.T,
	anth func(context.Context, string) (*anthropic.Usage, error),
	hyp func(context.Context, string) (hyperCredits, error),
) {
	t.Helper()
	prevA, prevH := fetchAnthropicUsage, fetchHyperCredits
	fetchAnthropicUsage = anth
	fetchHyperCredits = hyp
	t.Cleanup(func() { fetchAnthropicUsage, fetchHyperCredits = prevA, prevH })
}

func window(util float64, reset string) *anthropic.UsageWindow {
	return &anthropic.UsageWindow{Utilization: util, ResetsAt: reset}
}

func bothSignedIn() *fakeCredsource {
	return &fakeCredsource{
		linkable: []gateway.LinkableProvider{
			{ID: "anthropic", Name: "Claude Pro / Max"},
			{ID: "hyper", Name: "Charm Hyper"},
		},
		tokens: map[string]string{"anthropic": "tok-anthropic-secret", "hyper": "tok-hyper-secret"},
	}
}

func TestLoginsUsageReportsClaudeWindowsAndHyperBalance(t *testing.T) {
	s, h := keyedServer(t)
	s.engine.SetCredentialSource(bothSignedIn())
	swapUsageSeams(t,
		func(context.Context, string) (*anthropic.Usage, error) {
			return &anthropic.Usage{
				FiveHour: window(12, "2026-05-03T06:50Z"),
				SevenDay: window(70, "2026-05-04T00:00Z"),
			}, nil
		},
		func(context.Context, string) (hyperCredits, error) {
			return hyperCredits{Balance: 42}, nil
		},
	)

	resp, body := do(t, s, http.MethodGet, "/api/logins/usage", "", h)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var env usageEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	require.Len(t, env.Accounts, 2)

	claude := env.Accounts[0]
	require.Equal(t, "anthropic", claude.Provider)
	require.Empty(t, claude.Error)
	require.Len(t, claude.Windows, 2)
	require.Equal(t, "five_hour", claude.Windows[0].Key)
	require.Equal(t, float64(12), claude.Windows[0].Utilization)
	require.Equal(t, "2026-05-03T06:50Z", claude.Windows[0].ResetsAt)
	require.Equal(t, "seven_day", claude.Windows[1].Key)
	require.Equal(t, float64(70), claude.Windows[1].Utilization)

	hyper := env.Accounts[1]
	require.Equal(t, "hyper", hyper.Provider)
	require.NotNil(t, hyper.Balance)
	require.Equal(t, float64(42), *hyper.Balance)
	require.Equal(t, "Hypercredits", hyper.Unit)
	require.Empty(t, hyper.Cadence, "mixed grants and purchased bundles have no single cadence")
	require.Contains(t, hyper.Note, "100 Hypercredits/month")
	require.Contains(t, hyper.Note, "no 24h")
	require.Empty(t, hyper.Windows, "Hyper reports a balance, never invented windows")

	// A secret must never appear in the response.
	require.NotContains(t, body, "tok-anthropic-secret")
	require.NotContains(t, body, "tok-hyper-secret")
}

func TestLoginsUsageDegradesPerAccount(t *testing.T) {
	s, h := keyedServer(t)
	s.engine.SetCredentialSource(bothSignedIn())
	swapUsageSeams(t,
		func(context.Context, string) (*anthropic.Usage, error) {
			return nil, fmt.Errorf("claude usage endpoint returned 503")
		},
		func(context.Context, string) (hyperCredits, error) {
			return hyperCredits{Balance: 7}, nil
		},
	)

	resp, body := do(t, s, http.MethodGet, "/api/logins/usage", "", h)
	// One provider failing must not fail the screen.
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var env usageEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	require.Len(t, env.Accounts, 2)

	require.Equal(t, "anthropic", env.Accounts[0].Provider)
	require.Contains(t, env.Accounts[0].Error, "503")
	require.Empty(t, env.Accounts[0].Windows)

	require.Equal(t, "hyper", env.Accounts[1].Provider)
	require.Empty(t, env.Accounts[1].Error)
	require.NotNil(t, env.Accounts[1].Balance)
	require.Equal(t, float64(7), *env.Accounts[1].Balance)
}

func TestLoginsUsageMarksExpiredLoginAsReconnectable(t *testing.T) {
	s, h := keyedServer(t)
	s.engine.SetCredentialSource(&fakeCredsource{
		linkable: []gateway.LinkableProvider{{ID: "anthropic", Name: "Claude Pro / Max"}},
		errs:     map[string]error{"anthropic": fmt.Errorf("OAuth session expired or was revoked; sign in again")},
	})

	resp, body := do(t, s, http.MethodGet, "/api/logins/usage", "", h)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var env usageEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	require.Len(t, env.Accounts, 1)
	require.True(t, env.Accounts[0].NeedsSignIn)
	require.Contains(t, env.Accounts[0].Error, "sign in again")
}

func TestLoginsUsageOnlyReportsSignedInAccounts(t *testing.T) {
	s, h := keyedServer(t)
	// Only Hyper is signed in; Claude must not appear at all.
	s.engine.SetCredentialSource(&fakeCredsource{
		linkable: []gateway.LinkableProvider{{ID: "hyper", Name: "Charm Hyper"}},
		tokens:   map[string]string{"hyper": "tok-hyper"},
	})
	swapUsageSeams(t,
		func(context.Context, string) (*anthropic.Usage, error) {
			t.Fatal("Claude usage must not be fetched when Claude is not signed in")
			return nil, nil
		},
		func(context.Context, string) (hyperCredits, error) {
			return hyperCredits{Balance: 1}, nil
		},
	)

	resp, body := do(t, s, http.MethodGet, "/api/logins/usage", "", h)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var env usageEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	require.Len(t, env.Accounts, 1)
	require.Equal(t, "hyper", env.Accounts[0].Provider)
}

func TestLoginsUsageRequiresACredential(t *testing.T) {
	s := bareServer(t)
	resp, _ := do(t, s, http.MethodGet, "/api/logins/usage", "", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestFetchHyperCreditsLiveParsesBalance(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		require.Equal(t, "/v1/credits", r.URL.Path)
		_, _ = w.Write([]byte(`{"balance": 100}`))
	}))
	t.Cleanup(server.Close)

	prev := hyperCreditsBaseURL
	hyperCreditsBaseURL = server.URL
	t.Cleanup(func() { hyperCreditsBaseURL = prev })

	credits, err := fetchHyperCreditsLive(context.Background(), "sk-hyper-live")
	require.NoError(t, err)
	require.Equal(t, float64(100), credits.Balance)
	require.Equal(t, "Bearer sk-hyper-live", got.Get("Authorization"))
}

func TestFetchHyperCreditsLiveSurfacesErrorEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Missing or invalid API key","type":"authentication_error"}}`))
	}))
	t.Cleanup(server.Close)

	prev := hyperCreditsBaseURL
	hyperCreditsBaseURL = server.URL
	t.Cleanup(func() { hyperCreditsBaseURL = prev })

	_, err := fetchHyperCreditsLive(context.Background(), "sk-hyper-bad")
	require.Error(t, err)
	require.Contains(t, err.Error(), "Missing or invalid API key")
	require.NotContains(t, err.Error(), "sk-hyper-bad")
}

func TestLoginsUsageEmitsScopedWeeklyWindows(t *testing.T) {
	s, h := keyedServer(t)
	s.engine.SetCredentialSource(&fakeCredsource{
		linkable: []gateway.LinkableProvider{{ID: "anthropic", Name: "Claude Pro / Max"}},
		tokens:   map[string]string{"anthropic": "tok-a"},
	})
	swapUsageSeams(t,
		func(context.Context, string) (*anthropic.Usage, error) {
			return &anthropic.Usage{
				FiveHour:       window(3, ""),
				SevenDay:       window(20, ""),
				SevenDayOpus:   window(100, "2026-05-04T00:00Z"),
				SevenDaySonnet: window(77, "2026-05-04T00:00Z"),
			}, nil
		},
		func(context.Context, string) (hyperCredits, error) { return hyperCredits{}, nil },
	)

	resp, body := do(t, s, http.MethodGet, "/api/logins/usage", "", h)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var env usageEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	require.Len(t, env.Accounts, 1)
	labels := map[string]float64{}
	for _, w := range env.Accounts[0].Windows {
		labels[w.Label] = w.Utilization
	}
	require.Equal(t, float64(3), labels["5h session"])
	require.Equal(t, float64(20), labels["7d all models"])
	require.Equal(t, float64(100), labels["7d Opus"], "scoped rows keep their model display name")
	require.Equal(t, float64(77), labels["7d Sonnet"])
}

func TestLoginsUsageRedactsBearerFromErrors(t *testing.T) {
	s, h := keyedServer(t)
	s.engine.SetCredentialSource(bothSignedIn())
	swapUsageSeams(t,
		func(_ context.Context, token string) (*anthropic.Usage, error) {
			return nil, fmt.Errorf("upstream echoed the token %s in its error", token)
		},
		func(_ context.Context, token string) (hyperCredits, error) {
			return hyperCredits{}, fmt.Errorf("hyper leaked %s", token)
		},
	)

	resp, body := do(t, s, http.MethodGet, "/api/logins/usage", "", h)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotContains(t, body, "tok-anthropic-secret")
	require.NotContains(t, body, "tok-hyper-secret")
	require.Contains(t, body, "«redacted»")
}

func TestFetchHyperCreditsLiveDoesNotFollowRedirects(t *testing.T) {
	leaked := false
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked = true
		}
		_, _ = w.Write([]byte(`{"balance": 999}`))
	}))
	t.Cleanup(leak.Close)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", leak.URL)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	prev := hyperCreditsBaseURL
	hyperCreditsBaseURL = server.URL
	t.Cleanup(func() { hyperCreditsBaseURL = prev })

	_, err := fetchHyperCreditsLive(context.Background(), "sk-hyper-live")
	require.Error(t, err, "a 302 must surface as an error, not a followed redirect")
	require.False(t, leaked, "the bearer must never reach a redirect target")
}
