package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func visibleProviders(m *providersModel) []DirectoryProvider {
	var out []DirectoryProvider
	for _, idx := range m.list.filtered() {
		out = append(out, m.list.rows[idx].key.(DirectoryProvider))
	}
	return out
}

// TestProviderEnterOpensDetailWithoutMutating: Enter on a row raises the
// read-only drill-down, filled with the provider's facts and its quota probe,
// and it never posts anything - browsing is not a state change.
func TestProviderEnterOpensDetailWithoutMutating(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/probe") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"kind": "none", "published": false, "message": "no published quota",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app := New(&Client{BaseURL: srv.URL, HTTP: srv.Client()}, true, "test")
	app.providers.loaded = true
	app.providers.data.providers = []DirectoryProvider{{
		ID: "groq", Name: "Groq", Class: "free", Platform: "groq",
		Adapter: true, Ready: true, ModelCount: 3, EnabledKeyCount: 1, HealthyKeyCount: 1,
		APIKeyURL: "https://console.groq.com/keys", Modalities: []string{"text"},
	}}
	app.providers.buildRows()

	_, cmd := app.providers.Update(syntheticKey("enter"))
	if cmd == nil {
		t.Fatal("Enter returned no command")
	}
	msg := cmd()
	detail, ok := msg.(providerDetailMsg)
	if !ok {
		t.Fatalf("Enter produced %T; want providerDetailMsg", msg)
	}
	if posts != 0 {
		t.Fatalf("Enter caused %d POSTs; details must not mutate", posts)
	}

	app.providers.Update(detail)
	ov, ok := app.overlay.(*detailOverlay)
	if !ok {
		t.Fatalf("detail message opened %T; want *detailOverlay", app.overlay)
	}
	_, _ = ov.Update(tea.WindowSizeMsg{Width: 100, Height: 44})
	body := ansi.Strip(ov.View().Content)
	for _, want := range []string{
		"Groq", "Access", "Adapter", "groq", "Models", "Keys",
		"Quota", "no published quota", "Signup",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail overlay missing %q in:\n%s", want, body)
		}
	}
}

// TestProviderAccessFilterScopesAndCycles proves the access lens covers the
// exact vocabulary the storefront promises and that cycling it scopes the rows.
func TestProviderAccessFilterScopesAndCycles(t *testing.T) {
	if got := strings.Join(accessFilters, ","); got != "all,free,credits,subscription,local,paid" {
		t.Fatalf("access filter vocabulary drifted: %q", got)
	}

	app := New(&Client{BaseURL: "http://127.0.0.1:0"}, true, "test")
	app.providers.loaded = true
	app.providers.data.providers = []DirectoryProvider{
		{ID: "f", Name: "FreeOne", Class: "free"},
		{ID: "c", Name: "CreditOne", Class: "credits"},
		{ID: "s", Name: "SubOne", Class: "oauth"},
		{ID: "l", Name: "LocalOne", Class: "local"},
		{ID: "p", Name: "PaidOne", Class: "paid"},
	}
	app.providers.buildRows()

	for _, want := range []string{"free", "credits", "subscription", "local", "paid"} {
		app.providers.Update(syntheticKey("f"))
		if got := accessFilters[app.providers.accessIdx]; got != want {
			t.Fatalf("cycled to %q; want %q", got, want)
		}
		vis := visibleProviders(&app.providers)
		if len(vis) != 1 || accessOf(vis[0]) != want {
			t.Fatalf("access filter %q showed %d rows", want, len(vis))
		}
	}
	// One more press wraps back to the neutral lens.
	app.providers.Update(syntheticKey("f"))
	if got := accessFilters[app.providers.accessIdx]; got != "all" {
		t.Fatalf("cycle did not wrap to all, got %q", got)
	}
	if n := len(visibleProviders(&app.providers)); n != 5 {
		t.Fatalf("all lens showed %d rows; want 5", n)
	}
}

// TestProviderReadinessFilterScopesAndCycles proves the readiness lens covers
// the four buckets and that each bucket is honest about what it contains.
func TestProviderReadinessFilterScopesAndCycles(t *testing.T) {
	if got := strings.Join(readyFilters, ","); got != "all,ready,connected,setup,unavailable" {
		t.Fatalf("readiness filter vocabulary drifted: %q", got)
	}

	app := New(&Client{BaseURL: "http://127.0.0.1:0"}, true, "test")
	app.providers.loaded = true
	app.providers.data.providers = []DirectoryProvider{
		{ID: "rd", Name: "ReadyOne", Class: "free", Adapter: true, Ready: true, Configured: true},
		{ID: "cn", Name: "ConnOne", Class: "free", Adapter: true, Configured: true},
		{ID: "st", Name: "SetupOne", Class: "free", Adapter: true},
		{ID: "un", Name: "UnavailOne", Class: "free"},
	}
	app.providers.buildRows()

	for _, want := range []string{"ready", "connected", "setup", "unavailable"} {
		app.providers.Update(syntheticKey("r"))
		if got := readyFilters[app.providers.readyIdx]; got != want {
			t.Fatalf("cycled to %q; want %q", got, want)
		}
		vis := visibleProviders(&app.providers)
		if len(vis) != 1 || readinessOf(vis[0]) != want {
			t.Fatalf("readiness filter %q showed %d rows", want, len(vis))
		}
	}
	app.providers.Update(syntheticKey("r"))
	if n := len(visibleProviders(&app.providers)); n != 4 {
		t.Fatalf("all lens showed %d rows; want 4", n)
	}
}

// TestProviderAddCredentialByKind: a keyless provider enables in one action
// with an empty key, an unsupported provider is refused without a form or a
// POST, and a keyed provider opens the credential form.
func TestProviderAddCredentialByKind(t *testing.T) {
	var posted map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/keys" {
			_ = json.NewDecoder(r.Body).Decode(&posted)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": "unknown"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app := New(&Client{BaseURL: srv.URL, HTTP: srv.Client()}, true, "test")
	app.providers.loaded = true
	app.providers.data.providers = []DirectoryProvider{
		{ID: "kilo", Name: "Kilo", Class: "free", Platform: "kilo", Adapter: true, Keyless: true},
		{ID: "nox", Name: "NoAdapter", Class: "free"},
		{ID: "groq", Name: "Groq", Class: "free", Platform: "groq", Adapter: true},
	}
	app.providers.buildRows()
	// Sorted free-first by name: Groq(0), Kilo(1), NoAdapter(2).

	app.providers.list.cursor = 1
	_, cmd := app.providers.Update(syntheticKey("a"))
	if cmd == nil {
		t.Fatal("keyless add returned no command")
	}
	if res := cmd(); !isDone(res) {
		t.Fatalf("keyless add produced %T; want doneMsg (one action)", res)
	}
	if posted == nil || posted["platform"] != "kilo" || posted["key"] != "" {
		t.Fatalf("keyless add posted %v; want empty key for kilo", posted)
	}

	posted = nil
	app.providers.list.cursor = 2
	_, cmd = app.providers.Update(syntheticKey("a"))
	if cmd == nil {
		t.Fatal("no-adapter add returned no command")
	}
	toast, ok := cmd().(toastMsg)
	if !ok {
		t.Fatalf("no-adapter add did not refuse with a toast")
	}
	if !strings.Contains(toast.Text, "no adapter") {
		t.Fatalf("refusal text = %q", toast.Text)
	}
	if posted != nil {
		t.Fatalf("a refused add still posted %v", posted)
	}

	posted = nil
	app.overlay = nil
	app.providers.list.cursor = 0
	_, _ = app.providers.Update(syntheticKey("a"))
	if _, ok := app.overlay.(*formOverlay); !ok {
		t.Fatalf("keyed add opened %T; want *formOverlay", app.overlay)
	}
	if posted != nil {
		t.Fatalf("opening the form must not post yet: %v", posted)
	}
}

// TestProviderOpenSignupFallsBackToDocs: o opens the signup page, falls back to
// docs when there is no signup URL, and refuses cleanly when neither exists.
func TestProviderOpenSignupFallsBackToDocs(t *testing.T) {
	opened := ""
	orig := openURL
	openURL = func(u string) error { opened = u; return nil }
	t.Cleanup(func() { openURL = orig })

	app := New(&Client{BaseURL: "http://127.0.0.1:0"}, true, "test")
	app.providers.loaded = true
	app.providers.data.providers = []DirectoryProvider{
		{ID: "sg", Name: "HasSignup", Class: "free", APIKeyURL: "https://signup.test", DocsURL: "https://docs.test"},
		{ID: "do", Name: "DocsOnly", Class: "free", DocsURL: "https://docs.only"},
		{ID: "nl", Name: "NoLinks", Class: "free"},
	}
	app.providers.buildRows()
	// Sorted by name: DocsOnly(0), HasSignup(1), NoLinks(2).

	app.providers.list.cursor = 1
	_, cmd := app.providers.Update(syntheticKey("o"))
	if cmd == nil {
		t.Fatal("open returned no command")
	}
	cmd()
	if opened != "https://signup.test" {
		t.Fatalf("opened %q; want the signup URL", opened)
	}

	opened = ""
	app.providers.list.cursor = 0
	_, cmd = app.providers.Update(syntheticKey("o"))
	cmd()
	if opened != "https://docs.only" {
		t.Fatalf("opened %q; want the docs fallback", opened)
	}

	opened = ""
	app.providers.list.cursor = 2
	_, cmd = app.providers.Update(syntheticKey("o"))
	if cmd == nil {
		t.Fatal("open returned no command")
	}
	if _, ok := cmd().(toastMsg); !ok {
		t.Fatal("a provider with no links must refuse with a toast")
	}
	if opened != "" {
		t.Fatalf("no-links open still opened %q", opened)
	}
}

func isDone(msg tea.Msg) bool {
	_, ok := msg.(doneMsg)
	return ok
}
