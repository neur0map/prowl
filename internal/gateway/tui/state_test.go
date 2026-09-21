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

func TestConsoleOmitsKeybindFooterAndRendersToastInHeading(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.ready = true
	app.width = 100
	app.height = 30

	view := ansi.Strip(app.View().Content)
	for _, unwanted := range []string{"ctrl+r Reload", "? Help", "q Quit"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("console still renders persistent footer action %q", unwanted)
		}
	}
	if app.bodyH != 22 {
		t.Fatalf("body height = %d; want all 22 rows below header and heading", app.bodyH)
	}

	app.toast = toastMsg{Text: "model set created", Kind: "ok"}
	lines := strings.Split(ansi.Strip(app.pageHeading()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], "model set created") {
		t.Fatalf("toast did not replace the heading rule: %q", lines)
	}
}

// TestExpiredRevealDoesNotRearmTimer: a reveal that arrives already expired (a
// zero deadline) must hide the secret and schedule nothing - a tick on a past
// deadline would fire immediately and rearm forever.
func TestExpiredRevealDoesNotRearmTimer(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.keys

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
// completes while the operator is on another tab (or behind an overlay) must
// still update its owning model, not get swallowed by whatever screen is in
// front.
func TestAsyncResultReachesOwningModelOffTabAndUnderOverlay(t *testing.T) {
	app := New(&Client{}, true, "test")

	// A background credential refresh completes while the operator is on Home.
	app.tab = TabOverview
	app.Update(keysLoadedMsg{keys: []KeyView{{ID: 1, Platform: "anthropic", Enabled: true}}})
	if !app.keys.loaded || len(app.keys.data.keys) != 1 {
		t.Fatal("keys result was swallowed by the current tab instead of updating keys")
	}

	// A routing refresh lands even with a full-screen overlay open in front.
	app.tab = TabRouting
	app.overlay = newHelpOverlay()
	app.Update(routingLoadedMsg{
		routing: RoutingState{Strategy: "sequential"},
		models:  []ModelRow{{ID: 7, Platform: "openai", Available: true}},
	})
	if !app.routing.loaded || len(app.routing.data.models) != 1 {
		t.Fatal("routing result was swallowed by the overlay instead of updating routing")
	}
}

// TestErrorClearsOwningScreenBusy: an action that fails leaves its screen busy
// unless the error clears it. The error must clear only the owning screen so a
// concurrent screen's spinner is untouched.
func TestErrorClearsOwningScreenBusy(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.keys.busy = "checking anthropic…"
	app.setup.busy = "injecting claude…"

	app.Update(errMsg{Screen: "keys", Err: fmt.Errorf("boom")})
	if app.keys.busy != "" {
		t.Fatalf("keys stayed busy (%q) after its action errored", app.keys.busy)
	}
	if app.setup.busy != "injecting claude…" {
		t.Fatal("an error on keys must not clear an unrelated screen's busy state")
	}

	app.Update(errMsg{Screen: "setup", Err: fmt.Errorf("nope")})
	if app.setup.busy != "" {
		t.Fatalf("setup stayed busy (%q) after its action errored", app.setup.busy)
	}
}

// TestCooldownColumnReadsUnixSeconds: coolingUntil is Unix seconds. Reading it
// as milliseconds compares ~1.7e9 against ~1.7e12 and always hides an active
// cooldown, so the column must interpret the value as seconds.
func TestCooldownColumnReadsUnixSeconds(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.keys
	future := time.Now().Add(90 * time.Second).Unix()
	m.data.keys = []KeyView{{ID: 5, Platform: "anthropic", Enabled: true}}
	m.data.activity = map[int64]keyActivityRow{5: {KeyID: 5, CoolingUntil: &future}}
	m.buildRows()

	cell := ""
	for _, r := range m.list.rows {
		if kv, ok := r.key.(KeyView); ok && kv.ID == 5 {
			cell = ansi.Strip(r.cells[5])
		}
	}
	if !strings.Contains(cell, "cooling") {
		t.Fatalf("cooldown cell = %q; want a live cooldown read from Unix seconds", cell)
	}
}

// TestKeyDeleteRunsAsCommandNotSynchronously: confirming a key deletion must
// hand the HTTP DELETE back as a Bubble Tea command, not run it inline inside
// Update where it blocks the event loop.
func TestKeyDeleteRunsAsCommandNotSynchronously(t *testing.T) {
	var called int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.keys
	m.confirmDelete(KeyView{ID: 9, Platform: "anthropic", Label: "prod"})
	overlay, ok := app.overlay.(*confirmOverlay)
	if !ok {
		t.Fatalf("confirmDelete opened %T; want confirmOverlay", app.overlay)
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
	if done, ok := msg.(doneMsg); !ok || done.Tab != TabKeys {
		t.Fatalf("delete command produced %T; want a keys doneMsg", msg)
	}
}

// TestSignInPollRetriesTransientErrorThenTimesOut: a transient poll failure
// keeps the flow alive and retries; only crossing the overall deadline aborts
// it.
func TestSignInPollRetriesTransientErrorThenTimesOut(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.logins

	m.active = &SignInSession{ID: "flow-1", Provider: "anthropic"}
	m.activeSince = time.Now()
	_, cmd := m.Update(signInPollErrMsg{id: "flow-1", err: fmt.Errorf("connection reset")})
	if m.active == nil {
		t.Fatal("a transient poll error aborted the sign-in flow")
	}
	if cmd == nil {
		t.Fatal("a transient poll error did not schedule a retry")
	}

	m.active = &SignInSession{ID: "flow-1", Provider: "anthropic"}
	m.activeSince = time.Now().Add(-16 * time.Minute)
	_, cmd = m.Update(signInPollErrMsg{id: "flow-1", err: fmt.Errorf("still down")})
	if m.active != nil {
		t.Fatal("poll errors past the deadline must abort the flow")
	}
	if e, ok := cmd().(errMsg); !ok || e.Screen != "logins" {
		t.Fatalf("deadline abort produced %v; want a logins errMsg", cmd())
	}
}

// TestActiveSignInBlocksUnderlyingActions: while a sign-in is in progress the
// underlying list actions (enrol, forget, start another) must be inert; only
// the sign-in's own controls stay live.
func TestActiveSignInBlocksUnderlyingActions(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.logins
	m.data.platforms = []SignInPlatform{{ID: "anthropic", Name: "Claude", SignedIn: true, RoutesTo: "anthropic"}}
	m.loaded = true
	m.buildRows()
	if m.list.selected() == nil {
		t.Fatal("expected a selectable account row for the test")
	}
	m.active = &SignInSession{ID: "flow-1", Provider: "anthropic", URL: "https://example.test/oauth"}

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	if cmd != nil {
		t.Fatal("space triggered an underlying action during an active sign-in")
	}
	if m.busy != "" {
		t.Fatalf("an underlying action set busy=%q during an active sign-in", m.busy)
	}

	// The sign-in's own controls remain live.
	_, cmd = m.Update(tea.KeyPressMsg{Text: "o", Code: 'o'})
	if cmd == nil {
		t.Fatal("the open-URL control was blocked during an active sign-in")
	}
}

// TestAllowanceLoadFailureIsSurfaced: when the whole allowance report fails to
// load, an account that DOES publish an allowance must show the failure, not
// "not published" - that copy is reserved for accounts with no allowance
// surface at all.
func TestAllowanceLoadFailureIsSurfaced(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.logins

	m.Update(loginsUsageLoadedMsg{err: fmt.Errorf("usage endpoint returned 503")})

	if got := m.allowanceCell("anthropic", true); got != "report offline" {
		t.Fatalf("anthropic allowance after a load failure = %q; want report offline", got)
	}
	if got := m.allowanceCell("openai", true); got != "not published" {
		t.Fatalf("openai allowance after a load failure = %q; want not published", got)
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
	m.Update(routingLoadedMsg{err: fmt.Errorf("model catalogue unavailable")})
	if len(m.data.models) != 1 {
		t.Fatal("a failed refresh clobbered the good model catalogue")
	}
	if !strings.Contains(app.toast.Text, "model catalogue unavailable") {
		t.Fatalf("a failed refresh should surface a toast; got %q", app.toast.Text)
	}

	// A structurally incomplete snapshot (no routing strategy) is also rejected.
	m.Update(routingLoadedMsg{routing: RoutingState{}, models: []ModelRow{}})
	if len(m.data.models) != 1 {
		t.Fatal("an incomplete snapshot without a strategy clobbered good state")
	}

	// A complete snapshot replaces state as usual.
	m.Update(routingLoadedMsg{
		routing: RoutingState{Strategy: "race"},
		models:  []ModelRow{{ID: 2, Platform: "anthropic"}, {ID: 3, Platform: "openai"}},
	})
	if len(m.data.models) != 2 || m.data.routing.Strategy != "race" {
		t.Fatal("a complete snapshot failed to replace prior state")
	}
}

// TestProviderPickerPreservesExplicitAllScope: when every provider is active,
// "All" and "Active" select the same set. An explicit All choice must be kept
// as All so a provider that later goes inactive is not silently dropped.
func TestProviderPickerPreservesExplicitAllScope(t *testing.T) {
	models := []ModelRow{
		{ID: 1, Platform: "alpha", Available: true},
		{ID: 2, Platform: "beta", Available: true},
	}

	apply := func(o *providerFilterOverlay) providerScope {
		_, cmd := o.apply()
		return cmd().(providerFilterMsg).mode
	}

	if got := apply(newProviderFilterOverlay(models, providerScopeAll, nil)); got != providerScopeAll {
		t.Fatalf("explicit All collapsed to %v when All and Active coincide", got)
	}
	if got := apply(newProviderFilterOverlay(models, providerScopeActive, nil)); got != providerScopeActive {
		t.Fatalf("Active scope became %v when All and Active coincide", got)
	}

	o := newProviderFilterOverlay(models, providerScopeActive, nil)
	o.selectAll()
	if got := apply(o); got != providerScopeAll {
		t.Fatalf("explicit selectAll recorded %v; want All", got)
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
	m := &app.keys

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
	newActive := func() *loginsModel {
		app := New(&Client{}, true, "test")
		m := &app.logins
		m.active = &SignInSession{ID: "flow-2", Provider: "openai"}
		m.activeSince = time.Now()
		return m
	}

	// A stale poll for A must not clobber the active session B.
	m := newActive()
	if _, cmd := m.Update(signInPolledMsg{session: SignInSession{ID: "flow-1", Provider: "anthropic", State: "pending"}}); cmd != nil {
		t.Fatal("a stale sign-in poll scheduled work against the newer session")
	}
	if m.active == nil || m.active.ID != "flow-2" {
		t.Fatalf("a stale sign-in poll clobbered the active session: %#v", m.active)
	}

	// A stale error for A must not retry against B.
	m = newActive()
	if _, cmd := m.Update(signInPollErrMsg{id: "flow-1", err: fmt.Errorf("A blip")}); cmd != nil {
		t.Fatal("a stale poll error scheduled a retry against the newer session")
	}
	if m.active == nil || m.active.ID != "flow-2" {
		t.Fatalf("a stale poll error disturbed the active session: %#v", m.active)
	}

	// A stale tick for A must not poll B.
	m = newActive()
	if _, cmd := m.Update(signInTickMsg{id: "flow-1"}); cmd != nil {
		t.Fatal("a stale sign-in tick polled the newer session")
	}
	if m.active == nil || m.active.ID != "flow-2" {
		t.Fatalf("a stale sign-in tick disturbed the active session: %#v", m.active)
	}

	// A stale cancel for A must not wipe B.
	m = newActive()
	if _, cmd := m.Update(signInCancelledMsg{id: "flow-1"}); cmd != nil {
		t.Fatal("a stale sign-in cancel scheduled work against the newer session")
	}
	if m.active == nil || m.active.ID != "flow-2" {
		t.Fatal("a stale sign-in cancel wiped the newer active session")
	}

	// After the flow is finished (m.active nil), no late result revives it.
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
		lm := &app.logins
		lm.active = nil
		if _, cmd := lm.Update(tc.msg); cmd != nil {
			t.Fatalf("a late sign-in %s for a finished flow scheduled follow-up work", tc.name)
		}
		if lm.active != nil {
			t.Fatalf("a late sign-in %s revived a finished flow", tc.name)
		}
	}
}

// TestUnrelatedOverlaySurvivesBackgroundResult: a background success or error
// must dismiss only the overlay that launched the operation (a running confirm
// or a submitting form). A passive overlay the operator opened meanwhile - help
// here - must stay on screen.
func TestUnrelatedOverlaySurvivesBackgroundResult(t *testing.T) {
	app := New(&Client{}, true, "test")

	app.overlay = newHelpOverlay()
	app.Update(doneMsg{Tab: TabKeys, Text: "cooldowns cleared"})
	if app.overlay == nil {
		t.Fatal("a background success closed an unrelated help overlay")
	}

	app.overlay = newHelpOverlay()
	app.Update(errMsg{Screen: "keys", Err: fmt.Errorf("boom")})
	if app.overlay == nil {
		t.Fatal("a background error closed an unrelated help overlay")
	}

	// A running confirm carries the op id of the operation it launched. A
	// completion from a DIFFERENT in-flight operation (op mismatch) must not
	// close it; only its own completion may.
	app.overlay = &confirmOverlay{running: true, op: 5}
	app.Update(doneMsg{Tab: TabKeys, op: 7})
	if app.overlay == nil {
		t.Fatal("an unrelated background completion closed a running confirm")
	}
	app.Update(doneMsg{Tab: TabKeys, op: 5})
	if app.overlay != nil {
		t.Fatalf("a confirm stayed open after its own operation completed: %T", app.overlay)
	}

	// A submitting form behaves the same for an error result: an unrelated op is
	// ignored, its own op dismisses it.
	app.overlay = &formOverlay{submitting: true, op: 9}
	app.Update(errMsg{Screen: "keys", Err: fmt.Errorf("boom"), op: 3})
	if app.overlay == nil {
		t.Fatal("an unrelated background error closed a submitting form")
	}
	app.Update(errMsg{Screen: "keys", Err: fmt.Errorf("boom"), op: 9})
	if app.overlay != nil {
		t.Fatalf("a form stayed open after its own submit errored: %T", app.overlay)
	}

	app.overlay = &formOverlay{}
	app.Update(doneMsg{Tab: TabKeys})
	if app.overlay == nil {
		t.Fatal("a background success closed a form the operator was still filling in")
	}
}

// TestLoginForgetRunsAsCommandNotSynchronously: confirming a login forget must
// hand the HTTP DELETE back as a command, not run it inline inside Update.
func TestLoginForgetRunsAsCommandNotSynchronously(t *testing.T) {
	var called int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/logins/anthropic/forget" {
			atomic.AddInt32(&called, 1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.logins
	m.data.platforms = []SignInPlatform{{ID: "anthropic", Name: "Claude", SignedIn: true, RoutesTo: "anthropic"}}
	m.loaded = true
	m.buildRows()

	m.Update(tea.KeyPressMsg{Text: "d", Code: 'd'})
	overlay, ok := app.overlay.(*confirmOverlay)
	if !ok {
		t.Fatalf("forget opened %T; want confirmOverlay", app.overlay)
	}
	_, cmd := overlay.runYes()
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("forget hit the server synchronously inside Update (calls=%d)", got)
	}
	if cmd == nil {
		t.Fatal("confirming forget returned no command")
	}
	msg := cmd()
	if got := atomic.LoadInt32(&called); got != 1 {
		t.Fatalf("running the forget command made %d server calls; want 1", got)
	}
	if done, ok := msg.(doneMsg); !ok || done.Tab != TabLogins {
		t.Fatalf("forget command produced %T; want a logins doneMsg", msg)
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
