package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

func allowanceOf(t *testing.T, m *loginsModel, id string) string {
	t.Helper()
	for _, r := range m.list.rows {
		if rd, ok := r.key.(loginRowData); ok && strings.EqualFold(rd.platform.ID, id) {
			require.GreaterOrEqual(t, len(r.cells), 5, "allowance column must exist")
			return r.cells[3]
		}
	}
	t.Fatalf("no row for %s", id)
	return ""
}

func TestLoginsAllowanceColumnShowsWindowsAndBalance(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.logins
	m.data.platforms = []SignInPlatform{
		{ID: "anthropic", Name: "Claude Pro / Max", RoutesTo: "anthropic", SignedIn: true},
		{ID: "hyper", Name: "Charm Hyper", RoutesTo: "hyper", SignedIn: true},
	}
	m.loaded = true
	m.buildRows()

	// Before the first allowance read, supported accounts show a pending marker.
	require.Equal(t, "…", allowanceOf(t, m, "anthropic"))
	require.Equal(t, "…", allowanceOf(t, m, "hyper"))

	bal := 42.0
	m.Update(loginsUsageLoadedMsg{accounts: []LoginUsage{
		{Provider: "anthropic", Windows: []LoginUsageWindow{
			{Key: "five_hour", Utilization: 12},
			{Key: "seven_day", Utilization: 70},
		}},
		{Provider: "HYPER", Balance: &bal, Unit: "Hypercredits"},
	}})

	require.True(t, m.usageLoaded)
	require.Equal(t, "5h 12% used · 7d 70% used", allowanceOf(t, m, "anthropic"))
	// The provider key is matched case-insensitively.
	require.Equal(t, "42 Hypercredits", allowanceOf(t, m, "hyper"))
}

func TestLoginsDistinguishesAllowanceOutageFromExpiredLogin(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.logins
	m.data.platforms = []SignInPlatform{
		{ID: "anthropic", Name: "Claude Pro / Max", RoutesTo: "anthropic", SignedIn: true},
	}
	m.data.logins = []LoginRow{{ID: "anthropic", Enrolled: true, Models: 4}}
	m.loaded = true
	m.Update(loginsUsageLoadedMsg{accounts: []LoginUsage{
		{Provider: "anthropic", Error: "claude usage endpoint returned 503"},
	}})
	require.Equal(t, "report offline", allowanceOf(t, m, "anthropic"))
	require.False(t, m.accountNeedsSignIn(m.data.platforms[0]))

	m.Update(loginsUsageLoadedMsg{accounts: []LoginUsage{
		{Provider: "anthropic", NeedsSignIn: true, Error: "token expired"},
	}})
	require.Equal(t, "sign in again", allowanceOf(t, m, "anthropic"))
	require.True(t, m.accountNeedsSignIn(m.data.platforms[0]))
	require.Equal(t, "reconnect", m.list.rows[0].cells[1])
	require.Equal(t, "paused · sign in again", m.list.rows[0].cells[2])
}

func TestLoginsConnectionPanelShowsAutomaticRoutingProgress(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.logins
	m.width = 80
	m.data.platforms = []SignInPlatform{
		{ID: "anthropic", Name: "Claude Pro / Max", RoutesTo: "anthropic", SignedIn: true},
	}
	m.loaded = true
	m.active = &SignInSession{ID: "f1", Provider: "anthropic", State: "pending"}
	m.activeSince = time.Now()

	// Completing the flow retires the active panel and raises the durable one.
	m.Update(signInPolledMsg{session: SignInSession{ID: "f1", Provider: "anthropic", State: "complete"}})
	require.Nil(t, m.active)
	require.NotNil(t, m.connected)
	require.Equal(t, "anthropic", m.connected.provider)

	panel := m.connectedPanel()
	require.Contains(t, panel, "Connected")
	require.Contains(t, panel, "model discovery")

	// x dismisses the panel without disturbing anything else.
	m.Update(tea.KeyPressMsg{Text: "x", Code: 'x'})
	require.Nil(t, m.connected)
}

func TestLoginsConnectionPanelClearsWhenEnrolled(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.logins
	m.connected = &connectedLogin{provider: "anthropic"}
	m.data.platforms = []SignInPlatform{
		{ID: "anthropic", Name: "Claude Pro / Max", RoutesTo: "anthropic", SignedIn: true},
	}
	m.data.logins = []LoginRow{{ID: "anthropic", Name: "Claude Pro / Max", Enrolled: true, Models: 4}}
	m.loaded = true
	m.buildRows()
	// Once the account is active in routing, the completion panel is done.
	require.Nil(t, m.connected)
}

func TestLoginsUsageRebuildsOpenAccountDetail(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.tab = TabLogins
	m := &app.logins
	m.data.platforms = []SignInPlatform{
		{ID: "anthropic", Name: "Claude Pro / Max", RoutesTo: "anthropic", SignedIn: true},
	}
	m.loaded = true
	m.buildRows()

	sel := m.list.selected()
	require.NotNil(t, sel)
	row := sel.key.(loginRowData)
	app.overlay = m.accountDetail(row)

	// Before the allowance lands the drill-down is still reading.
	pre, ok := app.overlay.(*detailOverlay)
	require.True(t, ok)
	require.Contains(t, pre.body, "Reading allowance")

	// Usage arriving while the detail is open must rebuild it in place, not be
	// swallowed by the overlay.
	app.Update(loginsUsageLoadedMsg{accounts: []LoginUsage{
		{Provider: "anthropic", Windows: []LoginUsageWindow{
			{Key: "five_hour", Label: "5h session", Utilization: 12},
		}},
	}})

	post, ok := app.overlay.(*detailOverlay)
	require.True(t, ok, "account detail must remain open after usage lands")
	require.Contains(t, post.body, "12% used")
	require.NotContains(t, post.body, "Reading allowance")
}

func TestMaskAccountHidesEmailLocalPart(t *testing.T) {
	require.Equal(t, "n•••@example.com", maskAccount("nero.person@example.com"))
	require.Equal(t, "acct…7890", maskAccount("acct_1234567890"))
	require.Empty(t, maskAccount(" "))
}

// TestSignInStartGatedWhileStarting: a second sign-in start must not fire while
// the first start's POST is in flight. The gate is the dedicated signInStarting
// flag, not the shared busy string - an unrelated loginsLoadedMsg clears busy
// but must not reopen the gate, and only the start's own result clears it.
func TestSignInStartGatedWhileStarting(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.logins
	p := SignInPlatform{ID: "anthropic", Name: "Claude"}

	require.NotNil(t, m.beginSignIn(p), "first sign-in start returned no command")
	require.True(t, m.signInStarting, "starting a sign-in must arm the start gate")
	require.Nil(t, m.active, "m.active must still be nil before signInStartedMsg lands")

	// A background account refresh clears the shared busy string, but the
	// dedicated start gate must hold so a second press cannot open a second flow.
	m.Update(loginsLoadedMsg{})
	require.Empty(t, m.busy, "loginsLoadedMsg is expected to clear busy")
	require.True(t, m.signInStarting, "an unrelated load reopened the sign-in start gate")
	require.Nil(t, m.beginSignIn(p), "a second sign-in start fired after busy was cleared mid-start")

	// Only the start's own failure result clears the gate and allows a retry.
	m.Update(signInStartFailedMsg{err: fmt.Errorf("boom")})
	require.False(t, m.signInStarting, "the start's own failure must clear the gate")
	require.NotNil(t, m.beginSignIn(p), "after a failed start the gate must allow a retry")
}

// TestUsageStaleLoadIgnoredIncludingABA: only the newest load's response may
// update the screen. Generation defeats the ABA case - a slow initial 24h load
// whose window matches again after the operator cycled 24h→7d→24h must still be
// rejected - and the window is checked too so an accepted response can never
// disagree with the chosen range.
func TestUsageStaleLoadIgnoredIncludingABA(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.usage

	// Each load() bumps loadGen; the returned command is discarded (no network).
	m.window = "24h"
	m.load() // gen1, window 24h (in-flight, slow)
	gen1 := m.loadGen
	m.window = "7d"
	m.load() // gen2, window 7d
	m.window = "24h"
	m.load() // gen3, window 24h (the chosen, latest load)
	gen3 := m.loadGen

	// The slow first 24h response lands last. Its window matches 24h again, but
	// its generation is stale - window equality alone would wrongly accept it.
	m.Update(usageLoadedMsg{gen: gen1, window: "24h", summary: UsageSummary{Requests: 1}})
	require.False(t, m.loaded, "a stale ABA response (matching window, older gen) was accepted")
	require.EqualValues(t, 0, m.data.summary.Requests)

	// The newest 24h load's response is accepted.
	m.Update(usageLoadedMsg{gen: gen3, window: "24h", summary: UsageSummary{Requests: 24}})
	require.True(t, m.loaded)
	require.EqualValues(t, 24, m.data.summary.Requests)

	// A current-generation response for the wrong window is still rejected.
	m.Update(usageLoadedMsg{gen: m.loadGen, window: "7d", summary: UsageSummary{Requests: 7}})
	require.EqualValues(t, 24, m.data.summary.Requests, "a wrong-window response clobbered current data")
}
