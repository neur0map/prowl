package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	tea "charm.land/bubbletea/v2"
)

// TestClearedToastDoesNotRearmTimer: a live toast schedules exactly one expiry
// tick; the zero toast that clears it must not schedule another, or the console
// spins forever rescheduling a toast that already equals itself.
func TestClearedToastDoesNotRearmTimer(t *testing.T) {
	app := New(&Client{}, true, "test")

	_, cmd := app.Update(toastMsg{Text: "saved", Kind: "ok", Expires: time.Now().Add(2 * time.Second)})
	if cmd == nil {
		t.Fatal("a live toast must schedule its expiry tick")
	}

	_, cmd = app.Update(toastMsg{})
	if cmd != nil {
		t.Fatal("a cleared toast rearmed the timer instead of stopping")
	}
	if app.toast.Text != "" {
		t.Fatalf("cleared toast left text %q", app.toast.Text)
	}
}

// TestConsoleKeepsKeybindFooterWhileToastUsesHeading: a toast takes over the
// page heading's description line and never the footer, so the keybinds stay
// visible while a message shows. The chrome costs six rows in total.
func TestConsoleKeepsKeybindFooterWhileToastUsesHeading(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.ready = true
	app.width = 100
	app.height = 30
	app.applyLayout()

	view := ansi.Strip(app.View().Content)
	for _, keybind := range []string{"? Help", "q Quit"} {
		if !strings.Contains(view, keybind) {
			t.Fatalf("console is missing persistent footer action %q", keybind)
		}
	}
	if app.bodyH != 24 {
		t.Fatalf("body height = %d; want 24 rows inside a six-row chrome", app.bodyH)
	}

	app.toast = toastMsg{Text: "model set created", Kind: "ok"}
	lines := strings.Split(ansi.Strip(app.pageHeading()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "model set created") {
		t.Fatalf("toast did not replace the heading description: %q", lines)
	}
	footer := ansi.Strip(app.footer())
	if strings.Contains(footer, "model set created") {
		t.Fatalf("toast replaced the footer keybinds: %q", footer)
	}
	for _, keybind := range []string{"? Help", "q Quit"} {
		if !strings.Contains(footer, keybind) {
			t.Fatalf("toast-visible footer is missing action %q", keybind)
		}
	}
}

// TestSidebarListsEverySectionWithItsNumber: the sidebar is the navigation, so
// every section must be there with the digit that jumps to it, and the digit
// must actually navigate.
func TestSidebarListsEverySectionWithItsNumber(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.ready = true
	app.width = 120
	app.height = 30
	app.applyLayout()

	side := ansi.Strip(app.sidebar(app.bodyH + headingH))
	for i, tab := range app.tabs {
		want := tabsMeta[tab].name
		if !strings.Contains(side, want) {
			t.Fatalf("sidebar is missing section %q:\n%s", want, side)
		}
		if !strings.Contains(side, fmt.Sprintf("%d", i+1)) {
			t.Fatalf("sidebar is missing shortcut %d for %q", i+1, want)
		}
	}
	for _, name := range []string{"Keys", "Credentials", "Accounts"} {
		if strings.Contains(side, name) {
			t.Fatalf("sidebar still offers the retired section %q", name)
		}
	}

	_, _ = app.Update(tea.KeyPressMsg{Text: "3", Code: '3'})
	if app.tab != TabProviders {
		t.Fatalf("pressing 3 landed on %v; want Providers", app.tab)
	}

	// Below the collapse width the sidebar keeps the glyphs and drops labels.
	app.width = 80
	app.applyLayout()
	narrow := ansi.Strip(app.sidebar(app.bodyH + headingH))
	if strings.Contains(narrow, "Providers") || !strings.Contains(narrow, tabsMeta[TabProviders].glyph) {
		t.Fatalf("narrow sidebar did not collapse to glyphs:\n%s", narrow)
	}
}

// TestExpiredRevealDoesNotRearmTimer: a reveal that arrives already expired (a
// zero deadline) must hide the secret and schedule nothing - a tick on a past
// deadline would fire immediately and rearm forever.
func TestExpiredRevealDoesNotRearmTimer(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.providers

	_, cmd := m.Update(revealMsg{platform: "anthropic", value: "sk-secret", until: time.Now().Add(30 * time.Second)})
	if cmd == nil {
		t.Fatal("a live reveal must schedule its hide tick")
	}
	if m.reveal.value != "sk-secret" {
		t.Fatalf("live reveal stored %q; want the secret", m.reveal.value)
	}

	_, cmd = m.Update(revealMsg{})
	if cmd != nil {
		t.Fatal("expired reveal rearmed the hide timer forever")
	}
	if m.reveal.value != "" {
		t.Fatalf("expired reveal left the secret %q visible", m.reveal.value)
	}
}

// TestAsyncResultReachesOwningModelOffTabAndUnderOverlay: an async load that
// completes while the operator is on another section (or behind an overlay)
// must still update its owning model, not get swallowed by whatever screen is
// in front.
func TestAsyncResultReachesOwningModelOffTabAndUnderOverlay(t *testing.T) {
	app := New(&Client{}, true, "test")

	// A background provider refresh completes while the operator is on Home.
	app.tab = TabOverview
	app.Update(providersLoadedMsg{keys: []KeyView{{ID: 1, Platform: "anthropic", Enabled: true}}})
	if !app.providers.loaded || len(app.providers.data.keys) != 1 {
		t.Fatal("providers result was swallowed by the current section instead of updating providers")
	}

	// A models refresh lands even with a full-screen overlay open in front.
	app.tab = TabRouting
	app.overlay = newHelpOverlay()
	app.Update(routingLoadedMsg{
		gen:     app.routing.loadGen,
		routing: RoutingState{Strategy: "sequential"},
		models:  []ModelRow{{ID: 7, Platform: "openai", Available: true}},
	})
	if !app.routing.loaded || len(app.routing.data.models) != 1 {
		t.Fatal("models result was swallowed by the overlay instead of updating models")
	}
}

// TestErrorClearsOwningScreenBusy: an action that fails leaves its screen busy
// unless the error clears it. The error must clear only the owning screen so a
// concurrent screen's spinner is untouched.
func TestErrorClearsOwningScreenBusy(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.providers.busy = "checking anthropic…"
	app.setup.busy = "injecting claude…"

	app.Update(errMsg{Screen: "providers", Err: fmt.Errorf("boom")})
	if app.providers.busy != "" {
		t.Fatalf("providers stayed busy (%q) after its action errored", app.providers.busy)
	}
	if app.setup.busy != "injecting claude…" {
		t.Fatal("an error on providers must not clear an unrelated screen's busy state")
	}

	app.Update(errMsg{Screen: "setup", Err: fmt.Errorf("nope")})
	if app.setup.busy != "" {
		t.Fatalf("setup stayed busy (%q) after its action errored", app.setup.busy)
	}
}

// TestCooldownStatusReadsUnixSeconds: coolingUntil is Unix seconds. Reading it
// as milliseconds compares ~1.7e9 against ~1.7e12 and always hides an active
// cooldown, so the status must interpret the value as seconds.
func TestCooldownStatusReadsUnixSeconds(t *testing.T) {
	future := time.Now().Add(90 * time.Second).Unix()
	keys := []KeyView{{ID: 5, Platform: "anthropic", Enabled: true, Status: "healthy"}}
	activity := map[int64]keyActivityRow{5: {KeyID: 5, CoolingUntil: &future}}
	if got := keyStatus(keys, activity); !strings.Contains(got, "cooling") {
		t.Fatalf("key status = %q; want a live cooldown read from Unix seconds", got)
	}
}

// TestKeyRemovalRunsAsCommandNotSynchronously: confirming a key removal must
// hand the HTTP DELETE back as a Bubble Tea command, not run it inline inside
// Update where it blocks the event loop.
func TestKeyRemovalRunsAsCommandNotSynchronously(t *testing.T) {
	var called int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.providers
	m.confirmRemoveKey("Anthropic", KeyView{ID: 9, Platform: "anthropic", Label: "prod"})
	overlay, ok := app.overlay.(*confirmOverlay)
	if !ok {
		t.Fatalf("confirmRemoveKey opened %T; want confirmOverlay", app.overlay)
	}

	_, cmd := overlay.runYes()
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("delete hit the server synchronously inside Update (calls=%d)", got)
	}
	if cmd == nil {
		t.Fatal("confirming delete returned no command")
	}

	msg := cmd()
	if got := atomic.LoadInt32(&called); got != 1 {
		t.Fatalf("running the delete command made %d server calls; want 1", got)
	}
	if done, ok := msg.(doneMsg); !ok || done.Tab != TabProviders {
		t.Fatalf("delete command produced %T; want a providers doneMsg", msg)
	}
}

// TestSignInPollRetriesTransientErrorThenTimesOut: a transient poll failure
// keeps the flow alive and retries; only crossing the overall deadline aborts
// it.
func TestSignInPollRetriesTransientErrorThenTimesOut(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.providers

	m.signin.active = &SignInSession{ID: "flow-1", Provider: "anthropic"}
	m.signin.since = time.Now()
	_, cmd := m.Update(signInPollErrMsg{id: "flow-1", err: fmt.Errorf("connection reset")})
	if m.signin.active == nil {
		t.Fatal("a transient poll error aborted the sign-in flow")
	}
	if cmd == nil {
		t.Fatal("a transient poll error did not schedule a retry")
	}

	m.signin.active = &SignInSession{ID: "flow-1", Provider: "anthropic"}
	m.signin.since = time.Now().Add(-16 * time.Minute)
	m.Update(signInPollErrMsg{id: "flow-1", err: fmt.Errorf("still down")})
	if m.signin.active != nil {
		t.Fatal("poll errors past the deadline must abort the flow")
	}
	if !strings.Contains(app.toast.Text, "timed out") {
		t.Fatalf("deadline abort toast = %q; want a timeout", app.toast.Text)
	}
}

// TestActiveSignInBlocksUnderlyingActions: while a sign-in is in progress the
// underlying list actions (pause, disconnect, connect another) must be inert;
// only the sign-in's own controls stay live.
func TestActiveSignInBlocksUnderlyingActions(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.providers
	m.data.directory = []DirectoryProvider{{ID: "anthropic", Name: "Anthropic", Class: "oauth", Platform: "anthropic", Adapter: true}}
	m.data.platforms = []SignInPlatform{{ID: "anthropic", Name: "Claude", SignedIn: true, RoutesTo: "anthropic"}}
	m.loaded = true
	m.buildRows()
	if !m.list.selectID("provider:anthropic") {
		t.Fatal("expected a selectable provider row for the test")
	}
	m.signin.active = &SignInSession{ID: "flow-1", Provider: "anthropic", URL: "https://example.test/oauth"}

	for _, key := range []string{"space", "d", "enter"} {
		_, cmd := m.Update(syntheticKey(key))
		if cmd != nil {
			t.Fatalf("%s triggered an underlying action during an active sign-in", key)
		}
		if m.busy != "" {
			t.Fatalf("an underlying action set busy=%q during an active sign-in", m.busy)
		}
		if app.overlay != nil {
			t.Fatalf("%s opened %T during an active sign-in", key, app.overlay)
		}
	}

	// The sign-in's own controls remain live.
	_, cmd := m.Update(tea.KeyPressMsg{Text: "o", Code: 'o'})
	if cmd == nil {
		t.Fatal("the open-URL control was blocked during an active sign-in")
	}
}

// TestAllowanceLoadFailureIsSurfaced: when the whole allowance report fails to
// load, an account that DOES publish an allowance must show the failure, not a
// blank - that is reserved for accounts with no allowance surface at all.
func TestAllowanceLoadFailureIsSurfaced(t *testing.T) {
	err := fmt.Errorf("usage endpoint returned 503")
	if got := allowanceSummary("anthropic", nil, true, err); got != "allowance report offline" {
		t.Fatalf("anthropic allowance after a load failure = %q; want the outage", got)
	}
	if got := allowanceSummary("openai", nil, true, err); got != "" {
		t.Fatalf("openai allowance after a load failure = %q; want blank (no allowance surface)", got)
	}
	if got := allowanceSummary("anthropic", nil, false, nil); got != "…" {
		t.Fatalf("anthropic allowance before the first read = %q; want a pending marker", got)
	}
}

// TestPartialRoutingSnapshotIsRejected: a refresh that failed to fetch the
// policy or the model catalogue must not overwrite a good loaded snapshot with
// holes; a complete snapshot still replaces state.
func TestPartialRoutingSnapshotIsRejected(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.routing
	m.data = routingLoadedMsg{
		routing: RoutingState{Strategy: "sequential"},
		models:  []ModelRow{{ID: 1, Platform: "openai", Available: true}},
	}
	m.loaded = true
	m.buildRows()

	// A failed refresh (model catalogue unavailable) must keep the good data
	// and surface the failure through the toast rather than blanking the view.
	m.Update(routingLoadedMsg{gen: m.loadGen, err: fmt.Errorf("model catalogue unavailable")})
	if len(m.data.models) != 1 {
		t.Fatal("a failed refresh clobbered the good model catalogue")
	}
	if !strings.Contains(app.toast.Text, "model catalogue unavailable") {
		t.Fatalf("a failed refresh should surface a toast; got %q", app.toast.Text)
	}

	// A structurally incomplete snapshot (no routing strategy) is also rejected.
	m.Update(routingLoadedMsg{gen: m.loadGen, routing: RoutingState{}, models: []ModelRow{}})
	if len(m.data.models) != 1 {
		t.Fatal("an incomplete snapshot without a strategy clobbered good state")
	}

	// A complete snapshot replaces state as usual.
	m.Update(routingLoadedMsg{
		gen:     m.loadGen,
		routing: RoutingState{Strategy: "race"},
		models:  []ModelRow{{ID: 2, Platform: "anthropic"}, {ID: 3, Platform: "openai"}},
	})
	if len(m.data.models) != 2 || m.data.routing.Strategy != "race" {
		t.Fatal("a complete snapshot failed to replace prior state")
	}
}

// TestStaleToastExpiryIgnored: a toast's expiry timer carries the exact deadline
// it was armed for. A superseded toast's late deadline must be ignored so it
// cannot clear the toast now on screen; only the matching deadline clears it.
func TestStaleToastExpiryIgnored(t *testing.T) {
	app := New(&Client{}, true, "test")

	exp1 := time.Now().Add(2 * time.Second)
	app.Update(toastMsg{Text: "first", Kind: "ok", Expires: exp1})
	exp2 := time.Now().Add(5 * time.Second)
	app.Update(toastMsg{Text: "second", Kind: "ok", Expires: exp2})

	app.Update(toastExpiredMsg{expires: exp1})
	if app.toast.Text != "second" {
		t.Fatalf("a stale toast expiry cleared the current toast (now %q)", app.toast.Text)
	}
	app.Update(toastExpiredMsg{expires: exp2})
	if app.toast.Text != "" {
		t.Fatalf("the matching toast expiry failed to clear the toast (now %q)", app.toast.Text)
	}
}

// TestStaleRevealExpiryIgnored: each shown secret is tagged with a generation.
// An earlier reveal's hide timer must not clear a newer secret; only the timer
// carrying the current generation hides.
func TestStaleRevealExpiryIgnored(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.providers

	m.Update(revealMsg{platform: "anthropic", value: "sk-first", until: time.Now().Add(30 * time.Second)})
	staleGen := m.revealGen
	m.Update(revealMsg{platform: "openai", value: "sk-second", until: time.Now().Add(30 * time.Second)})
	currentGen := m.revealGen
	if currentGen == staleGen {
		t.Fatal("a new reveal must advance the generation so the old timer is distinguishable")
	}

	m.Update(revealExpiredMsg{gen: staleGen})
	if m.reveal.value != "sk-second" {
		t.Fatalf("a stale reveal expiry hid the current secret (now %q)", m.reveal.value)
	}
	m.Update(revealExpiredMsg{gen: currentGen})
	if m.reveal.value != "" {
		t.Fatalf("the current reveal expiry failed to hide the secret (now %q)", m.reveal.value)
	}
}

// TestStaleSignInResultsIgnoredAcrossAllKinds: every asynchronous sign-in result
// carries the session it belongs to. A late poll, error, tick or cancel from a
// superseded session A must not clobber, retry or clear the newer session B; and
// none may revive a flow the operator already finished.
func TestStaleSignInResultsIgnoredAcrossAllKinds(t *testing.T) {
	newActive := func() *providersModel {
		app := New(&Client{}, true, "test")
		m := &app.providers
		m.signin.active = &SignInSession{ID: "flow-2", Provider: "openai"}
		m.signin.since = time.Now()
		return m
	}

	// A stale poll for A must not clobber the active session B.
	m := newActive()
	if _, cmd := m.Update(signInPolledMsg{session: SignInSession{ID: "flow-1", Provider: "anthropic", State: "pending"}}); cmd != nil {
		t.Fatal("a stale sign-in poll scheduled work against the newer session")
	}
	if m.signin.active == nil || m.signin.active.ID != "flow-2" {
		t.Fatalf("a stale sign-in poll clobbered the active session: %#v", m.signin.active)
	}

	// A stale error for A must not retry against B.
	m = newActive()
	if _, cmd := m.Update(signInPollErrMsg{id: "flow-1", err: fmt.Errorf("A blip")}); cmd != nil {
		t.Fatal("a stale poll error scheduled a retry against the newer session")
	}
	if m.signin.active == nil || m.signin.active.ID != "flow-2" {
		t.Fatalf("a stale poll error disturbed the active session: %#v", m.signin.active)
	}

	// A stale tick for A must not poll B.
	m = newActive()
	if _, cmd := m.Update(signInTickMsg{id: "flow-1"}); cmd != nil {
		t.Fatal("a stale sign-in tick polled the newer session")
	}
	if m.signin.active == nil || m.signin.active.ID != "flow-2" {
		t.Fatalf("a stale sign-in tick disturbed the active session: %#v", m.signin.active)
	}

	// A stale cancel for A must not wipe B.
	m = newActive()
	if _, cmd := m.Update(signInCancelledMsg{id: "flow-1"}); cmd != nil {
		t.Fatal("a stale sign-in cancel scheduled work against the newer session")
	}
	if m.signin.active == nil || m.signin.active.ID != "flow-2" {
		t.Fatal("a stale sign-in cancel wiped the newer active session")
	}

	// After the flow is finished (active nil), no late result revives it.
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{"poll", signInPolledMsg{session: SignInSession{ID: "flow-1", State: "complete"}}},
		{"error", signInPollErrMsg{id: "flow-1", err: fmt.Errorf("late")}},
		{"tick", signInTickMsg{id: "flow-1"}},
		{"cancel", signInCancelledMsg{id: "flow-1"}},
	} {
		app := New(&Client{}, true, "test")
		pm := &app.providers
		pm.signin.active = nil
		if _, cmd := pm.Update(tc.msg); cmd != nil {
			t.Fatalf("a late sign-in %s for a finished flow scheduled follow-up work", tc.name)
		}
		if pm.signin.active != nil {
			t.Fatalf("a late sign-in %s revived a finished flow", tc.name)
		}
	}
}

// TestSignInStartGatedWhileStarting: a second connect while the POST that
// opens the flow is still in flight must not start a second flow; the gate
// is cleared only by that start's own result.
func TestSignInStartGatedWhileStarting(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.providers
	m.data.directory = []DirectoryProvider{{ID: "openai", Name: "OpenAI", Class: "oauth", Platform: "openai", Adapter: true}}
	m.data.platforms = []SignInPlatform{{ID: "openai", Name: "ChatGPT", RoutesTo: "openai"}}
	m.loaded = true
	m.buildRows()
	m.list.selectID("provider:openai")

	if _, cmd := m.Update(syntheticKey("c")); cmd == nil {
		t.Fatal("first connect returned no start command")
	}
	m.busy = "" // an unrelated load clears busy; the gate must survive that
	if _, cmd := m.Update(syntheticKey("c")); cmd != nil {
		t.Fatal("a second connect started another flow while the first start was in flight")
	}

	m.Update(signInStartFailedMsg{err: fmt.Errorf("upstream refused")})
	if _, cmd := m.Update(syntheticKey("c")); cmd == nil {
		t.Fatal("the start gate stayed closed after the failed start reported")
	}
}

// TestUnrelatedOverlaySurvivesBackgroundResult: a background success or error
// must dismiss only the overlay that launched the operation (a running confirm
// or a submitting form). A passive overlay the operator opened meanwhile - help
// here - must stay on screen.
func TestUnrelatedOverlaySurvivesBackgroundResult(t *testing.T) {
	app := New(&Client{}, true, "test")

	app.overlay = newHelpOverlay()
	app.Update(doneMsg{Tab: TabProviders, Text: "cooldowns cleared"})
	if app.overlay == nil {
		t.Fatal("a background success closed an unrelated help overlay")
	}

	app.overlay = newHelpOverlay()
	app.Update(errMsg{Screen: "providers", Err: fmt.Errorf("boom")})
	if app.overlay == nil {
		t.Fatal("a background error closed an unrelated help overlay")
	}

	// A running confirm carries the op id of the operation it launched. A
	// completion from a DIFFERENT in-flight operation (op mismatch) must not
	// close it; only its own completion may.
	app.overlay = &confirmOverlay{running: true, op: 5}
	app.Update(doneMsg{Tab: TabProviders, op: 7})
	if app.overlay == nil {
		t.Fatal("an unrelated background completion closed a running confirm")
	}
	app.Update(doneMsg{Tab: TabProviders, op: 5})
	if app.overlay != nil {
		t.Fatalf("a confirm stayed open after its own operation completed: %T", app.overlay)
	}

	// A submitting form behaves the same for an error result: an unrelated op is
	// ignored, its own op dismisses it.
	app.overlay = &formOverlay{submitting: true, op: 9}
	app.Update(errMsg{Screen: "providers", Err: fmt.Errorf("boom"), op: 3})
	if app.overlay == nil {
		t.Fatal("an unrelated background error closed a submitting form")
	}
	app.Update(errMsg{Screen: "providers", Err: fmt.Errorf("boom"), op: 9})
	if app.overlay != nil {
		t.Fatalf("a form stayed open after its own submit errored: %T", app.overlay)
	}

	app.overlay = &formOverlay{}
	app.Update(doneMsg{Tab: TabProviders})
	if app.overlay == nil {
		t.Fatal("a background success closed a form the operator was still filling in")
	}
}

// TestDisconnectRunsAsCommandNotSynchronously: confirming a disconnect must
// hand the HTTP DELETEs back as a command, not run them inline inside Update,
// and a signed-in provider's disconnect must forget the login.
func TestDisconnectRunsAsCommandNotSynchronously(t *testing.T) {
	var called int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/logins/anthropic/forget" {
			atomic.AddInt32(&called, 1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.providers
	m.data.directory = []DirectoryProvider{{ID: "anthropic", Name: "Anthropic", Class: "oauth", Platform: "anthropic", Adapter: true}}
	m.data.platforms = []SignInPlatform{{ID: "anthropic", Name: "Claude", SignedIn: true, RoutesTo: "anthropic"}}
	m.loaded = true
	m.buildRows()
	m.list.selectID("provider:anthropic")

	m.Update(tea.KeyPressMsg{Text: "d", Code: 'd'})
	overlay, ok := app.overlay.(*confirmOverlay)
	if !ok {
		t.Fatalf("disconnect opened %T; want confirmOverlay", app.overlay)
	}
	_, cmd := overlay.runYes()
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("disconnect hit the server synchronously inside Update (calls=%d)", got)
	}
	if cmd == nil {
		t.Fatal("confirming disconnect returned no command")
	}
	msg := cmd()
	if got := atomic.LoadInt32(&called); got != 1 {
		t.Fatalf("running the disconnect command made %d forget calls; want 1", got)
	}
	if done, ok := msg.(doneMsg); !ok || done.Tab != TabProviders {
		t.Fatalf("disconnect command produced %T; want a providers doneMsg", msg)
	}
}

// TestConfirmDefaultsToTheActionEnterRuns: the operator already chose the
// destructive verb, so the confirm dialog focuses the action - enter applies
// it, esc keeps - and the buttons name the verb.
func TestConfirmDefaultsToTheActionEnterRuns(t *testing.T) {
	var ran int
	c := &confirmOverlay{verb: "Disconnect", yes: func() tea.Msg {
		return func() tea.Msg {
			ran++
			return doneMsg{Tab: TabProviders}
		}
	}}
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(c.View().Content)
	if !strings.Contains(view, "enter Disconnect") || !strings.Contains(view, "esc Keep") {
		t.Fatalf("confirm view = %q; want enter <verb> and esc Keep", view)
	}

	kept, _ := c.Update(syntheticKey("esc"))
	if kept != nil {
		t.Fatalf("esc kept the dialog open as %T; want it dismissed", kept)
	}
	if ran != 0 {
		t.Fatalf("esc ran the action %d times; want none", ran)
	}

	_, cmd := c.Update(syntheticKey("enter"))
	if cmd == nil {
		t.Fatal("enter did not run the action; the confirm must be focused by default")
	}
	if msg := cmd(); ran != 1 {
		t.Fatalf("enter's command ran the action %d times (msg %T); want 1", ran, msg)
	}
}

// TestLogClearRunsAsCommandNotSynchronously: confirming a log clear must hand
// the HTTP DELETE back as a command, not run it inline inside Update.
func TestLogClearRunsAsCommandNotSynchronously(t *testing.T) {
	var called int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/logs" {
			atomic.AddInt32(&called, 1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.usage
	m.tab = 1 // the log tab, where clear is offered
	m.Update(tea.KeyPressMsg{Text: "d", Code: 'd'})
	overlay, ok := app.overlay.(*confirmOverlay)
	if !ok {
		t.Fatalf("log clear opened %T; want confirmOverlay", app.overlay)
	}
	_, cmd := overlay.runYes()
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("log clear hit the server synchronously inside Update (calls=%d)", got)
	}
	if cmd == nil {
		t.Fatal("confirming log clear returned no command")
	}
	msg := cmd()
	if got := atomic.LoadInt32(&called); got != 1 {
		t.Fatalf("running the log-clear command made %d server calls; want 1", got)
	}
	if done, ok := msg.(doneMsg); !ok || done.Tab != TabUsage {
		t.Fatalf("log-clear command produced %T; want a usage doneMsg", msg)
	}
}

// TestHarnessRemovalDefersToCommand: confirming a harness removal must return
// the filesystem revert as a deferred command, not run it inside Update.
func TestHarnessRemovalDefersToCommand(t *testing.T) {
	app := New(&Client{Home: t.TempDir()}, true, "test")
	m := &app.setup
	m.data.supported = []string{"claude"}
	m.loaded = true
	m.buildRows()

	m.Update(tea.KeyPressMsg{Text: "d", Code: 'd'})
	overlay, ok := app.overlay.(*confirmOverlay)
	if !ok {
		t.Fatalf("harness removal opened %T; want confirmOverlay", app.overlay)
	}
	// yes() must yield the revert as a command (a func), not an already-run
	// result - proving the filesystem work is deferred off the Update loop.
	inner := overlay.yes()
	if _, isCmd := inner.(func() tea.Msg); !isCmd {
		t.Fatalf("harness removal yes returned %T; want a deferred command", inner)
	}
}
