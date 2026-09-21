package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestOverviewRegenerateKeyRotatesAndReveals proves the Home page can rotate the
// unified inference key: pressing g asks first (the old key stops working the
// instant the new one is stored, so it must not fire on a stray keypress), and
// confirming calls the regenerate endpoint and reveals the fresh key so the
// operator can copy it into their harnesses.
func TestOverviewRegenerateKeyRotatesAndReveals(t *testing.T) {
	var posted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/settings/api-key/regenerate" && r.Method == http.MethodPost {
			posted = true
			_ = json.NewEncoder(w).Encode(map[string]any{"apiKey": "prowlag-newnewnewnewnew"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.overview
	m.data.unifiedKey = "prowlag-oldoldoldoldold"

	// g must confirm before rotating, since the rotation is irreversible.
	m.Update(syntheticKey("g"))
	overlay, ok := app.overlay.(*confirmOverlay)
	if !ok {
		t.Fatalf("g opened %T; want a confirm before rotating the key", app.overlay)
	}
	if overlay.verb != "Regenerate" {
		t.Fatalf("confirm verb = %q; want Regenerate", overlay.verb)
	}

	// Confirming calls the regenerate endpoint and produces the new key.
	raw := overlay.yes()
	cmd, ok := raw.(func() tea.Msg)
	if !ok {
		t.Fatalf("confirm yes returned %T; want a command", raw)
	}
	result := cmd()
	if !posted {
		t.Fatal("confirming did not POST to /api/settings/api-key/regenerate")
	}
	out, ok := result.(keyRegeneratedMsg)
	if !ok {
		t.Fatalf("regenerate produced %T; want keyRegeneratedMsg", result)
	}
	if out.key != "prowlag-newnewnewnewnew" {
		t.Fatalf("regenerated key = %q; want the server's new key", out.key)
	}

	// Delivering the result shows the new key so it can be copied at once.
	m.Update(out)
	if m.data.unifiedKey != "prowlag-newnewnewnewnew" {
		t.Fatalf("Home key = %q; want the rotated key shown", m.data.unifiedKey)
	}
	if !m.revealed {
		t.Fatal("the rotated key must be revealed so the operator can copy it")
	}
}
