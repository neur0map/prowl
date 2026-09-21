package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func providersFixture(app *App) *providersModel {
	m := &app.providers
	m.loaded = true
	m.data = providersLoadedMsg{
		directory: []DirectoryProvider{
			{ID: "groq", Name: "Groq", Class: "free", Friction: "none", Platform: "groq", Adapter: true, Routable: true, FreeModels: 12, ModelCount: 14, KeyCount: 1, HealthyKeyCount: 1, Configured: true, Ready: true},
			{ID: "mistral-ai", Name: "Mistral AI", Class: "free", Friction: "registration", Platform: "mistral", Adapter: true, Routable: true, FreeModels: 15, APIKeyURL: "https://console.mistral.ai"},
			{ID: "kilo-code", Name: "Kilo Code", Class: "free", Friction: "none", Platform: "kilo", Adapter: true, Routable: true, FreeModels: 15, Keyless: true},
			{ID: "anthropic", Name: "Anthropic", Class: "oauth", Platform: "anthropic", Adapter: true, Routable: true, ModelCount: 22, KeyCount: 1, Configured: true},
			{ID: "openai", Name: "OpenAI", Class: "oauth", Platform: "openai", Adapter: true, Routable: true},
			{ID: "fireworks", Name: "Fireworks", Class: "paid", Friction: "card", Platform: "fireworks", Adapter: true, Routable: true},
		},
		keys: []KeyView{
			{ID: 5, Platform: "groq", MaskedKey: "gsk_...h9TX", Status: "healthy", Enabled: true},
			{ID: 3, Platform: "anthropic", Label: "Claude Pro / Max", MaskedKey: "(unreadable)", Status: "healthy", Enabled: true},
		},
		platforms: []SignInPlatform{
			{ID: "anthropic", Name: "Claude Pro / Max", RoutesTo: "anthropic", SignedIn: true, Account: "someone@example.com"},
			{ID: "openai", Name: "ChatGPT (Codex)", RoutesTo: "openai"},
			{ID: "copilot", Name: "GitHub Copilot", RoutesTo: ""},
		},
		logins: []LoginRow{{ID: "anthropic", Name: "Claude Pro / Max", Enrolled: true, Models: 22, KeyID: 3}},
	}
	m.setSize(120, 30)
	m.buildRows()
	return m
}

func rowCells(l *list) map[string][]string {
	out := map[string][]string{}
	for _, idx := range l.filtered() {
		r := l.rows[idx]
		if r.header {
			continue
		}
		cells := make([]string, 0, len(r.cells))
		for _, c := range r.cells {
			cells = append(cells, ansi.Strip(c))
		}
		out[cells[0]] = cells
	}
	return out
}

// TestProvidersLeadsWithConnectedAndHidesUnroutableAccounts: the table's first
// section is exactly the providers with something on file (a key or a
// sign-in); every other adapter follows as available to connect; and a
// sign-in platform with no routing adapter (Copilot) is not offered at all.
func TestProvidersLeadsWithConnectedAndHidesUnroutableAccounts(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := providersFixture(app)

	names := visibleNames(&m.list)
	joined := strings.Join(names, "|")
	if strings.Contains(joined, "Copilot") {
		t.Fatalf("an account with no routing adapter is offered: %v", names)
	}
	if names[0] != "Connected" {
		t.Fatalf("first section = %q; want Connected", names[0])
	}
	sep := -1
	for i, n := range names {
		if n == "Available to connect" {
			sep = i
		}
	}
	if sep < 0 {
		t.Fatalf("no Available section: %v", names)
	}
	connected := strings.Join(names[1:sep], "|")
	available := strings.Join(names[sep+1:], "|")
	for _, want := range []string{"Groq", "Anthropic"} {
		if !strings.Contains(connected, want) {
			t.Fatalf("Connected section is missing %q: %v", want, names[1:sep])
		}
	}
	for _, want := range []string{"Mistral AI", "Kilo Code", "OpenAI", "Fireworks"} {
		if strings.Contains(connected, want) || !strings.Contains(available, want) {
			t.Fatalf("%q should be in the Available section only: connected=%v available=%v", want, names[1:sep], names[sep+1:])
		}
	}

	cells := rowCells(&m.list)
	if got := cells["Anthropic"][3]; !strings.Contains(got, "signed in") || !strings.Contains(got, "s•••@example.com") {
		t.Fatalf("Anthropic connection cell = %q; want signed in with a masked account", got)
	}
	if got := cells["Anthropic"][4]; !strings.HasPrefix(got, "routing") {
		t.Fatalf("Anthropic status cell = %q; want routing", got)
	}
	if got := cells["Groq"][3]; got != "1 key · gsk_...h9TX" {
		t.Fatalf("Groq connection cell = %q", got)
	}
	if got := cells["Groq"][4]; got != "healthy" {
		t.Fatalf("Groq status cell = %q", got)
	}
	if got := cells["OpenAI"][3]; got != "browser sign-in" {
		t.Fatalf("OpenAI connection cell = %q; an account should say how it connects", got)
	}
	if got := cells["Kilo Code"][3]; got != "keyless · key optional" {
		t.Fatalf("Kilo connection cell = %q", got)
	}
	if got := cells["Mistral AI"][3]; got != "API key · email signup" {
		t.Fatalf("Mistral connection cell = %q", got)
	}
}

// TestProvidersConnectFollowsTheProviderKind: c does the one right thing per
// provider - a form for an API key, an optional-key form for a keyless endpoint
// (an empty submit keeps the free tier, a typed key posts verbatim), a browser
// flow for a subscription.
func TestProvidersConnectFollowsTheProviderKind(t *testing.T) {
	var posted []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["__path"] = r.URL.Path
		posted = append(posted, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": "unknown"})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := providersFixture(app)

	m.list.selectID("provider:mistral")
	m.Update(syntheticKey("c"))
	form, ok := app.overlay.(*formOverlay)
	if !ok || !strings.Contains(form.title, "Mistral") {
		t.Fatalf("connect on a key provider opened %T; want the Mistral key form", app.overlay)
	}
	app.overlay = nil

	// Kilo is keyless: c opens the credential form with the key optional, not
	// an immediate enable, so a paid-tier key can be pasted when there is one.
	m.list.selectID("provider:kilo")
	_, cmd := m.Update(syntheticKey("c"))
	form, ok = app.overlay.(*formOverlay)
	if !ok || !strings.Contains(form.title, "Kilo") {
		t.Fatalf("keyless connect opened %T; want the Kilo key form", app.overlay)
	}
	// An empty submit keeps the free tier: the sentinel POST carries no key.
	if sub := form.submitCmd(); sub != nil {
		sub()
	}
	if last := posted[len(posted)-1]; last["__path"] != "/api/keys" || last["platform"] != "kilo" || last["key"] != "" {
		t.Fatalf("empty keyless submit posted %v; want an empty-key sentinel for kilo", last)
	}
	app.overlay = nil

	// A typed key is posted verbatim.
	m.Update(syntheticKey("c"))
	form, _ = app.overlay.(*formOverlay)
	form.fields[0].input.SetValue("kilo-abc")
	if sub := form.submitCmd(); sub != nil {
		sub()
	}
	if last := posted[len(posted)-1]; last["platform"] != "kilo" || last["key"] != "kilo-abc" {
		t.Fatalf("typed keyless submit posted %v; want kilo-abc verbatim", last)
	}
	app.overlay = nil

	m.list.selectID("provider:openai")
	_, cmd = m.Update(syntheticKey("c"))
	if cmd == nil || app.overlay != nil {
		t.Fatalf("connect on a sign-in provider should start the browser flow, got cmd=%v overlay=%T", cmd != nil, app.overlay)
	}
	if !m.signin.starting {
		t.Fatal("sign-in start gate not set")
	}
}

// TestKeylessConnectedStillAcceptsKey: a keyless provider whose sentinel is on
// file still takes c - the same optional-key form opens, not an "already
// enabled" toast - and its row reads as the free tier, not a masked secret.
func TestKeylessConnectedStillAcceptsKey(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := providersFixture(app)
	m.data.keys = append(m.data.keys, KeyView{ID: 12, Platform: "kilo", MaskedKey: keylessSentinelMask, Status: "unknown", Enabled: true, Keyless: true})
	m.buildRows()

	r := m.rowFor("kilo")
	if r == nil {
		t.Fatal("kilo row missing after adding its sentinel key")
	}
	if conn, _ := m.connectionCells(*r); conn != "free tier · no key" {
		t.Fatalf("connected keyless connection cell = %q; want free tier · no key", conn)
	}

	m.list.selectID("provider:kilo")
	m.Update(syntheticKey("c"))
	if _, ok := app.overlay.(*formOverlay); !ok {
		t.Fatalf("c on a connected keyless provider opened %T; want the key form, not a toast", app.overlay)
	}
}

// TestProvidersPauseResumeBySelection: space withholds capacity without
// forgetting anything - a login is unenrolled, keys are disabled - and the
// footer names the verb that applies.
func TestProvidersPauseResumeBySelection(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := providersFixture(app)

	m.list.selectID("provider:anthropic")
	if !hasAction(m.actions(), "space", "Pause") {
		t.Fatalf("footer for a routing account should offer Pause: %v", m.actions())
	}
	_, cmd := m.Update(syntheticKey("space"))
	if _, ok := cmd().(doneMsg); !ok {
		t.Fatal("pausing an account did not complete")
	}
	m.busy = ""

	m.list.selectID("provider:groq")
	_, cmd = m.Update(syntheticKey("space"))
	if _, ok := cmd().(doneMsg); !ok {
		t.Fatal("pausing a key provider did not complete")
	}
	m.busy = ""
	want := []string{"DELETE /api/logins/anthropic/enroll", "PATCH /api/keys/5"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("pause calls = %v; want %v", calls, want)
	}

	m.list.selectID("provider:mistral")
	if _, cmd := m.Update(syntheticKey("space")); cmd == nil {
		t.Fatal("space on an unconnected provider should explain itself")
	} else if toast, ok := cmd().(toastMsg); !ok || !strings.Contains(toast.Text, "Connect") {
		t.Fatalf("space on an unconnected provider produced %v; want a connect hint", cmd())
	}
}

// TestProvidersFilterSections: the filter panel offers connection, access and
// signup effort with honest counts, and applying it narrows the table.
func TestProvidersFilterSections(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := providersFixture(app)

	counts := map[string]int{}
	for _, section := range m.filterSections() {
		for _, option := range section.options {
			counts[section.id+"/"+option.id] = option.count
		}
	}
	if counts[filterConnection+"/connected"] != 2 || counts[filterConnection+"/available"] != 4 {
		t.Fatalf("connection counts = %v", counts)
	}
	if counts[filterAccess+"/subscription"] != 2 || counts[filterFriction+"/card required"] != 1 {
		t.Fatalf("facet counts = %v", counts)
	}

	state := filterState{}
	state.set(filterConnection, "available", true)
	state.set(filterAccess, "free", true)
	m.Update(filterAppliedMsg{state: state})
	names := visibleNames(&m.list)
	if strings.Join(names, "|") != "Available to connect|Kilo Code|Mistral AI" {
		t.Fatalf("filters did not narrow to free, unconnected providers: %v", names)
	}
}

// TestManagePanelActsOnSelectedCredential: the manage panel lists the login
// and every pasted key of a provider, and its actions address the selected
// line - a key check goes to that key, a forget to the login.
func TestManagePanelActsOnSelectedCredential(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"keyId": 8, "status": "healthy"})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := providersFixture(app)
	m.data.keys = append(m.data.keys, KeyView{ID: 8, Platform: "anthropic", Label: "spare", MaskedKey: "sk-a...zzz", Status: "unknown", Enabled: true})
	m.buildRows()
	m.list.selectID("provider:anthropic")

	m.Update(syntheticKey("enter"))
	panel, ok := app.overlay.(*manageOverlay)
	if !ok {
		t.Fatalf("enter opened %T; want the manage panel", app.overlay)
	}
	if creds := panel.credentials(); len(creds) != 2 || !creds[0].login || creds[1].key.ID != 8 {
		t.Fatalf("panel credentials = %+v; want the login then the spare key", creds)
	}
	view := ansi.Strip(panel.View().Content)
	for _, want := range []string{"Anthropic", "s•••@example.com", "spare", "Forget login"} {
		if !strings.Contains(view, want) {
			t.Fatalf("manage panel is missing %q:\n%s", want, view)
		}
	}

	panel.Update(syntheticKey("down"))
	_, cmd := panel.Update(syntheticKey("enter"))
	if cmd == nil {
		t.Fatal("check on the selected key returned no command")
	}
	cmd()
	m.busy = "" // the check's doneMsg would clear this through the root model
	if len(calls) != 1 || calls[0] != "POST /api/health/check/8" {
		t.Fatalf("check called %v; want the spare key's health probe", calls)
	}

	panel.Update(syntheticKey("up"))
	// The root model installs whatever the overlay returns, so the panel must
	// hand back the confirm it opened - returning itself would keep the panel
	// on top, returning nil would drop the confirm.
	next, _ := panel.Update(syntheticKey("d"))
	confirm, ok := next.(*confirmOverlay)
	if !ok {
		t.Fatalf("forget returned %T; want the confirm as the new overlay", next)
	}
	if !strings.Contains(confirm.question, "Forget the Anthropic login") {
		t.Fatalf("confirm asks %q; want the login forget", confirm.question)
	}
}

func hasAction(actions []action, key, label string) bool {
	for _, a := range actions {
		if a.Key == key && a.Label == label {
			return true
		}
	}
	return false
}
