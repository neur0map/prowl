package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestRoutingSetEditEditsExactSetWithoutActivating: Enter on a saved set loads
// that set's membership and edits it in place through /reorder, and never
// activates it - Space is the only activation path. This is the leak-free
// editing contract two distinct sets rely on.
func TestRoutingSetEditEditsExactSetWithoutActivating(t *testing.T) {
	var (
		mu           sync.Mutex
		members      = []map[string]any{}
		reorderBody  []map[string]any
		activeCalled bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/profiles/1/models":
			_ = json.NewEncoder(w).Encode(members)
		case r.Method == http.MethodPut && r.URL.Path == "/api/profiles/1/reorder":
			_ = json.NewDecoder(r.Body).Decode(&reorderBody)
			members = members[:0]
			for _, e := range reorderBody {
				members = append(members, map[string]any{
					"model_db_id": e["modelDbId"], "priority": e["priority"], "enabled": true,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/profiles/active":
			activeCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"activeProfileId": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	app.routing.loaded = true
	app.routing.data.profiles = []Profile{{ID: 1, Name: "coding", ModelCount: 0}}
	app.routing.data.models = []ModelRow{
		{ID: 41, Platform: "anthropic", ModelID: "claude-opus", DisplayName: "Claude Opus", Enabled: true, Available: true},
		{ID: 42, Platform: "openai", ModelID: "gpt-codex", DisplayName: "GPT Codex", Enabled: true, Available: true},
	}
	app.routing.buildRows()

	app.routing.list.selectRow(setChoice{id: 1, name: "coding", active: false})
	if app.routing.list.selected() == nil {
		t.Fatal("set row not selectable in the policy list")
	}

	// Enter opens set editing without activating the set.
	_, openCmd := app.routing.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	loaded := runCmd(t, openCmd)
	editMsg, ok := loaded.(setEditLoadedMsg)
	if !ok {
		t.Fatalf("Enter on a set produced %T; want setEditLoadedMsg", loaded)
	}
	app.routing.Update(editMsg)
	if app.routing.setID != 1 {
		t.Fatalf("setID = %d; want the edited set 1", app.routing.setID)
	}
	if !app.routing.editing() || app.routing.defaultModels {
		t.Fatal("Enter on a set must open that set's editor, not the defaults")
	}

	// An empty set intentionally shows no rows until search opens the catalogue.
	app.routing.list.startSearch()
	app.routing.buildModelRows()
	// Space toggles model 41 into the set and rewrites ordered membership.
	app.routing.list.cursor = 0
	if mo, ok := app.routing.list.selected().key.(ModelRow); !ok || mo.ID != 41 {
		t.Fatalf("first model row = %#v; want model 41", app.routing.list.selected())
	}
	_, toggleCmd := app.routing.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	reloaded := runCmd(t, toggleCmd)
	if editMsg, ok = reloaded.(setEditLoadedMsg); !ok {
		t.Fatalf("membership toggle produced %T; want setEditLoadedMsg", reloaded)
	}
	app.routing.Update(editMsg)

	mu.Lock()
	defer mu.Unlock()
	if activeCalled {
		t.Fatal("editing a set must never activate it")
	}
	if len(reorderBody) != 1 || int64(reorderBody[0]["modelDbId"].(float64)) != 41 {
		t.Fatalf("reorder body = %#v; want exactly model 41", reorderBody)
	}
	if reorderBody[0]["enabled"] != true {
		t.Fatalf("member must be written enabled; got %#v", reorderBody[0])
	}
	if !app.routing.setMembers[41] {
		t.Fatal("model 41 not reflected as a member after the toggle")
	}
}

// TestRoutingSpaceActivatesASet: Space on a set is the activation path, distinct
// from Enter's in-place edit.
func TestRoutingSpaceActivatesASet(t *testing.T) {
	var activated int64 = -1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/profiles/active" {
			var body struct {
				ProfileID int64 `json:"profileId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			activated = body.ProfileID
			_ = json.NewEncoder(w).Encode(map[string]any{"activeProfileId": body.ProfileID})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	app.routing.loaded = true
	app.routing.data.profiles = []Profile{{ID: 7, Name: "writing"}}
	app.routing.buildRows()
	app.routing.list.selectRow(setChoice{id: 7, name: "writing", active: false})

	_, spaceCmd := app.routing.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	msg := runCmd(t, spaceCmd)
	if _, ok := msg.(doneMsg); !ok {
		t.Fatalf("Space on a set produced %T; want doneMsg after activation", msg)
	}
	if activated != 7 {
		t.Fatalf("activated profile = %d; want 7", activated)
	}
	if app.routing.editing() {
		t.Fatal("Space activates a set; it must not open the editor")
	}
}

// TestRoutingEscLeavesModelEditingAndMIsUnbound protects the predictable
// navigation contract: Enter opens the selected active row, Esc returns, and
// the former hidden m shortcut no longer changes modes.
func TestRoutingEscLeavesModelEditingAndMIsUnbound(t *testing.T) {
	app := New(&Client{BaseURL: "http://127.0.0.1:0"}, true, "test")
	app.routing.loaded = true
	app.routing.data.models = []ModelRow{{ID: 1, Platform: "p", ModelID: "x", DisplayName: "X", Available: true}}
	app.routing.buildRows()
	app.routing.list.filter = "Default"

	app.routing.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !app.routing.defaultModels {
		t.Fatal("Enter on active Default models must open its model editor")
	}
	if app.routing.list.filter != "" {
		t.Fatal("the model-set search must not leak into the model catalogue")
	}
	app.routing.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.routing.editing() {
		t.Fatal("Esc must return from model editing to the model-set list")
	}

	app.routing.Update(tea.KeyPressMsg{Text: "m", Code: 'm'})
	if app.routing.editing() {
		t.Fatal("m is intentionally unbound and must not open a hidden editing mode")
	}
	app.routing.list.filter = "saved-set-name"
	app.routing.wantSetID = 7
	app.routing.Update(setEditLoadedMsg{id: 7, name: "saved-set-name"})
	if app.routing.list.filter != "" {
		t.Fatal("a saved-set search must be cleared when its model editor opens")
	}
}

// TestIntelThresholdIsAQuantileNotAConstant: lower intelligence ranks are more
// capable, and the cut is derived from the live catalogue rather than a magic
// rank that would drift as the catalogue changes.
func TestIntelThresholdIsAQuantileNotAConstant(t *testing.T) {
	low := []ModelRow{{IntelligenceRank: 10}, {IntelligenceRank: 20}, {IntelligenceRank: 30}, {IntelligenceRank: 40}}
	high := []ModelRow{{IntelligenceRank: 100}, {IntelligenceRank: 200}, {IntelligenceRank: 300}, {IntelligenceRank: 400}}

	if got := intelThreshold(low, 0.25); got != 10 {
		t.Fatalf("low-distribution top-quartile threshold = %d; want rank 10", got)
	}
	if got := intelThreshold(high, 0.25); got != 100 {
		t.Fatalf("high-distribution top-quartile threshold = %d; want rank 100", got)
	}
	if got := intelThreshold(low, 0.50); got != 20 {
		t.Fatalf("low-distribution top-half threshold = %d; want rank 20", got)
	}
	if intelThreshold(nil, 0.25) != 0 {
		t.Fatal("an empty catalogue must not panic and must threshold at 0")
	}
}

// TestRoutingRenamePrefillsCurrentNameAndPatches: `r` on a set opens a rename
// form pre-filled with the set's current name, and submitting a new name is a
// real PATCH /api/profiles/{id} mutation.
func TestRoutingRenamePrefillsCurrentNameAndPatches(t *testing.T) {
	var (
		patchPath string
		patchBody map[string]string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/profiles/") {
			patchPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&patchBody)
			_ = json.NewEncoder(w).Encode(Profile{ID: 3, Name: patchBody["name"]})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	app.routing.loaded = true
	app.routing.data.profiles = []Profile{{ID: 3, Name: "coding"}}
	app.routing.buildRows()
	app.routing.list.selectRow(setChoice{id: 3, name: "coding", active: false})

	app.routing.Update(tea.KeyPressMsg{Text: "r", Code: 'r'})
	form, ok := app.overlay.(*formOverlay)
	if !ok {
		t.Fatalf("r opened %T; want a rename form", app.overlay)
	}
	if got := form.fields[0].input.Value(); got != "coding" {
		t.Fatalf("rename form prefill = %q; want the set's current name", got)
	}

	form.fields[0].input.SetValue("coding2")
	msg := form.submit(form.values())
	if _, ok := msg.(doneMsg); !ok {
		t.Fatalf("rename submit produced %T; want doneMsg", msg)
	}
	if patchPath != "/api/profiles/3" {
		t.Fatalf("renamed via %q; want PATCH /api/profiles/3", patchPath)
	}
	if patchBody["name"] != "coding2" {
		t.Fatalf("rename body = %#v; want the new name", patchBody)
	}
}

func TestRoutingStartsWithActiveProvidersAndSearchSpansAll(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.routing
	m.loaded = true
	m.defaultModels = true
	m.data.models = []ModelRow{
		{ID: 1, Platform: "alpha", ModelID: "free-best", DisplayName: "Free Best", Access: "free", IntelligenceRank: 1, Available: true},
		{ID: 2, Platform: "beta", ModelID: "paid-hidden", DisplayName: "Paid Hidden", Access: "paid", IntelligenceRank: 2},
		{ID: 3, Platform: "alpha", ModelID: "sub", DisplayName: "Subscription", Access: "subscription", IntelligenceRank: 4, Available: true},
		{ID: 4, Platform: "gamma", ModelID: "free-hidden", DisplayName: "Free Hidden", Access: "free", IntelligenceRank: 3},
	}
	m.buildRows()

	models := func() []ModelRow {
		out := make([]ModelRow, 0, len(m.list.rows))
		for _, row := range m.list.rows {
			out = append(out, row.key.(ModelRow))
		}
		return out
	}
	if got := models(); len(got) != 2 || got[0].Platform != "alpha" || got[1].Platform != "alpha" {
		t.Fatalf("default provider scope = %+v; want only active provider alpha", got)
	}

	m.Update(syntheticKey("/"))
	for _, r := range "Paid" {
		m.Update(tea.KeyPressMsg{Text: string(r), Code: r})
	}
	filtered := m.list.filtered()
	if len(filtered) != 1 {
		t.Fatalf("full-catalogue search found %d rows; want inactive beta model", len(filtered))
	}
	if got := m.list.rows[filtered[0]].key.(ModelRow).Platform; got != "beta" {
		t.Fatalf("search result provider = %q; want beta", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.list.isSearching() || len(m.list.rows) != 4 || len(m.list.filtered()) != 1 {
		t.Fatal("Enter must keep a full-catalogue search active without trapping keyboard actions")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !m.editing() || m.list.filter != "" || len(m.list.rows) != 2 {
		t.Fatal("Esc must clear retained search before leaving the model editor")
	}

	m.providerMode = providerScopeAll
	m.buildRows()
	m.Update(syntheticKey("i"))
	got := models()
	if len(got) != 1 || got[0].ModelID != "free-best" {
		t.Fatalf("elite intelligence filter = %+v; want only the lowest ordinal rank", got)
	}
}

func TestProviderPickerAppliesExplicitMultiSelection(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.routing
	m.loaded = true
	m.defaultModels = true
	m.data.models = []ModelRow{
		{ID: 1, Platform: "alpha", ModelID: "active", DisplayName: "Active", Available: true},
		{ID: 2, Platform: "beta", ModelID: "other", DisplayName: "Other"},
	}
	m.buildRows()

	m.Update(syntheticKey("p"))
	picker, ok := app.overlay.(*providerFilterOverlay)
	if !ok {
		t.Fatalf("p opened %T; want provider multi-select", app.overlay)
	}
	picker.Update(tea.KeyPressMsg{Code: tea.KeySpace}) // alpha off
	picker.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	picker.Update(tea.KeyPressMsg{Code: tea.KeySpace}) // beta on
	_, _ = picker.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	_ = picker.View()
	closed, cmd := picker.Update(tea.MouseClickMsg{X: picker.applyHit.x, Y: picker.applyHit.y})
	if closed != nil || cmd == nil {
		t.Fatalf("clicking Apply left %T open with cmd=%v", closed, cmd != nil)
	}
	raw := cmd()
	msg, ok := raw.(providerFilterMsg)
	if !ok {
		t.Fatalf("provider picker produced %T; want providerFilterMsg", raw)
	}
	m.Update(msg)
	if m.providerMode != providerScopeCustom || !m.selectedProvider["beta"] || m.selectedProvider["alpha"] {
		t.Fatalf("provider selection = mode %v values %#v; want only beta", m.providerMode, m.selectedProvider)
	}
	if len(m.list.rows) != 1 || m.list.rows[0].key.(ModelRow).Platform != "beta" {
		t.Fatalf("custom provider rows = %#v; want only beta", m.list.rows)
	}
}
func TestRoutingHomeKeepsTemplatesInGroupedPicker(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data.profiles = []Profile{{ID: 1, Name: "My set", ModelCount: 2}}
	m.data.presets = []ChainPreset{
		{ID: "access", Name: "Subscription only", Description: "models covered by an account", Group: "catalogue", Models: 3},
		{ID: "coding", Name: "Coding", Description: "strong tool-using models", Group: "task", Models: 4},
	}
	m.buildRows()

	if len(m.list.rows) != 2 {
		t.Fatalf("model-set home has %d rows; want only defaults and saved sets", len(m.list.rows))
	}
	for _, row := range m.list.rows {
		if _, ok := row.key.(setChoice); !ok {
			t.Fatalf("primary list leaked a non-set row: %T", row.key)
		}
	}

	m.Update(syntheticKey("n"))
	picker, ok := app.overlay.(*routingPickerOverlay)
	if !ok {
		t.Fatalf("n opened %T; want grouped template picker", app.overlay)
	}
	if len(picker.choices) != 3 {
		t.Fatalf("template choices = %d; want blank plus two presets", len(picker.choices))
	}
	if picker.choices[0].group != "Start from scratch" ||
		picker.choices[1].group != "For your work" ||
		picker.choices[2].group != "By access" {
		t.Fatalf("template groups = %q, %q, %q", picker.choices[0].group, picker.choices[1].group, picker.choices[2].group)
	}
	_, _ = picker.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	view := picker.View().Content
	for _, group := range []string{"Start from scratch", "For your work", "By access"} {
		if !strings.Contains(view, group) {
			t.Fatalf("template picker did not render group %q", group)
		}
	}
}

func TestBlankTemplateCreatesAnActuallyEmptySet(t *testing.T) {
	var request struct {
		Name  string `json:"name"`
		Empty bool   `json:"empty"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/profiles" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		_ = json.NewEncoder(w).Encode(Profile{ID: 11, Name: request.Name})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	app.routing.loaded = true
	_ = app.routing.openCreateSet()
	form, ok := app.overlay.(*formOverlay)
	if !ok {
		t.Fatalf("blank template opened %T; want naming form", app.overlay)
	}
	form.fields[0].input.SetValue("scratch")
	msg := runCmd(t, form.submitCmd())
	if request.Name != "scratch" {
		t.Fatalf("create request name = %q after %T; want scratch", request.Name, msg)
	}
	if !request.Empty {
		t.Fatal("Start from scratch copied the default models instead of creating an empty set")
	}

	// The POST response must populate the list synchronously. The refresh command
	// returned here is deliberately not run: visibility cannot depend on it.
	_, _ = app.Update(msg)
	selected := app.routing.list.selected()
	choice, selectedCreatedSet := selected.key.(setChoice)
	if !selectedCreatedSet || choice.id != 11 || choice.name != "scratch" {
		t.Fatalf("created set was not immediately selected in the list: %#v", selected)
	}
	if app.overlay != nil {
		t.Fatal("successful create left the naming form open")
	}
}

func TestStaleRoutingRefreshCannotEraseNewlyCreatedSet(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data.routing.Strategy = "balanced"

	_ = m.load() // generation 1 was already in flight when create completed.
	_, _ = m.Update(setCreatedMsg{profile: Profile{ID: 11, Name: "scratch"}})
	if m.loadGen != 2 {
		t.Fatalf("post-create refresh generation = %d; want 2", m.loadGen)
	}
	_, _ = m.Update(routingLoadedMsg{
		gen:     1,
		routing: RoutingState{Strategy: "balanced"},
	})
	if len(m.data.profiles) != 1 || m.data.profiles[0].ID != 11 {
		t.Fatalf("stale refresh erased the new set: %#v", m.data.profiles)
	}
}

func TestSetEditorShowsOnlyMembersUntilSearch(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data.models = []ModelRow{
		{ID: 41, Platform: "anthropic", DisplayName: "Claude Opus", Enabled: true, Available: true},
		{ID: 42, Platform: "openai", DisplayName: "GPT Codex", Enabled: true, Available: true},
	}
	m.wantSetID = 7
	m.Update(setEditLoadedMsg{id: 7, name: "coding", order: []int64{41}})

	if len(m.list.rows) != 1 {
		t.Fatalf("set editor shows %d rows; want its single selected model", len(m.list.rows))
	}
	if model, ok := m.list.rows[0].key.(ModelRow); !ok || model.ID != 41 {
		t.Fatalf("set editor row = %#v; want member model 41", m.list.rows[0].key)
	}

	m.list.startSearch()
	m.buildModelRows()
	if len(m.list.rows) != 2 {
		t.Fatalf("search shows %d rows; want the full two-model catalogue for adding", len(m.list.rows))
	}
}

func TestSubscriptionProviderNamesExposeCodexAndClaude(t *testing.T) {
	if got := providerDisplayName("openai"); got != "OpenAI / Codex" {
		t.Fatalf("openai label = %q; want Codex visible", got)
	}
	if got := providerDisplayName("anthropic"); got != "Anthropic / Claude" {
		t.Fatalf("anthropic label = %q; want Claude visible", got)
	}
}

func TestRoutingDeleteRequiresConfirmationAndCallsDelete(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/profiles/9" {
			deleted = true
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	app.routing.loaded = true
	app.routing.data.profiles = []Profile{{ID: 9, Name: "old-set"}}
	app.routing.buildRows()
	app.routing.list.selectRow(setChoice{id: 9, name: "old-set"})

	app.routing.Update(syntheticKey("d"))
	confirm, ok := app.overlay.(*confirmOverlay)
	if !ok {
		t.Fatalf("d opened %T; want confirmation", app.overlay)
	}
	model, cmd := confirm.Update(syntheticKey("enter"))
	if model != nil || cmd != nil || deleted {
		t.Fatal("Enter on the default safe choice must cancel without deleting")
	}

	app.overlay = nil
	app.routing.Update(syntheticKey("d"))
	confirm = app.overlay.(*confirmOverlay)
	_, _ = confirm.Update(syntheticKey("right"))
	_, cmd = confirm.Update(syntheticKey("enter"))
	if msg := runCmd(t, cmd); deleted {
		if _, ok := msg.(doneMsg); !ok {
			t.Fatalf("confirmed delete produced %T; want doneMsg", msg)
		}
	} else {
		t.Fatal("confirmed delete never called DELETE /api/profiles/9")
	}
}

func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	return cmd()
}

// TestRoutingLoadRejectsRequiredReadFailure: profiles, active, presets and the
// model catalogue are all required for a complete snapshot. If any one read
// fails the whole refresh must carry an error (and report incomplete), so a
// hole never silently overwrites a good loaded snapshot.
func TestRoutingLoadRejectsRequiredReadFailure(t *testing.T) {
	serve := func(fail string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == fail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			switch r.URL.Path {
			case "/api/fallback/routing":
				_ = json.NewEncoder(w).Encode(RoutingState{Strategy: "sequential"})
			case "/api/profiles":
				_ = json.NewEncoder(w).Encode([]Profile{{ID: 1, Name: "coding"}})
			case "/api/profiles/active":
				_ = json.NewEncoder(w).Encode(map[string]any{"activeProfileId": 1})
			case "/api/profiles/presets":
				_ = json.NewEncoder(w).Encode(map[string]any{"presets": []ChainPreset{}})
			case "/api/models":
				_ = json.NewEncoder(w).Encode([]ModelRow{{ID: 7, Platform: "openai", Available: true}})
			default:
				http.NotFound(w, r)
			}
		}))
	}

	for _, fail := range []string{"/api/profiles", "/api/profiles/active", "/api/profiles/presets", "/api/models"} {
		t.Run(fail, func(t *testing.T) {
			server := serve(fail)
			defer server.Close()
			app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
			msg, ok := app.routing.load()().(routingLoadedMsg)
			if !ok {
				t.Fatalf("load produced %T; want routingLoadedMsg", msg)
			}
			if msg.err == nil {
				t.Fatalf("a failed %s read produced no error; a hole would overwrite good state", fail)
			}
			if msg.complete() {
				t.Fatal("an incomplete snapshot must not report complete()")
			}
		})
	}

	// Every read succeeding yields a complete, error-free snapshot.
	server := serve("")
	defer server.Close()
	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	msg := app.routing.load()().(routingLoadedMsg)
	if msg.err != nil || !msg.complete() {
		t.Fatalf("a fully successful load must be complete and error-free; got err=%v complete=%v", msg.err, msg.complete())
	}
}

// TestStaleSetEditResultDoesNotReopenEditor: after the operator leaves a set
// editor with Escape, a set-membership re-read still in flight must not reopen
// the editor - Escape cannot be undone by a stale edit result.
func TestStaleSetEditResultDoesNotReopenEditor(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data.profiles = []Profile{{ID: 1, Name: "coding"}}
	m.data.models = []ModelRow{{ID: 41, Platform: "anthropic", ModelID: "opus", DisplayName: "Opus", Available: true}}
	m.buildRows()

	// Open set 1 for editing (as openSetEdit records intent, then its load lands).
	m.wantSetID = 1
	m.Update(setEditLoadedMsg{id: 1, name: "coding", order: []int64{41}})
	if m.setID != 1 || !m.editing() {
		t.Fatalf("set editor did not open: setID=%d editing=%v", m.setID, m.editing())
	}

	// The operator leaves with Escape.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.editing() || m.setID != 0 {
		t.Fatalf("Escape did not leave the editor: setID=%d editing=%v", m.setID, m.editing())
	}

	// A late membership re-read for the same set arrives after Escape.
	m.Update(setEditLoadedMsg{id: 1, name: "coding", order: []int64{41}})
	if m.editing() || m.setID != 0 {
		t.Fatal("a stale set-edit result reopened an editor the operator had left")
	}

	// A fresh open still works: intent is recorded again, and its result lands.
	m.wantSetID = 1
	m.Update(setEditLoadedMsg{id: 1, name: "coding", order: []int64{41}})
	if m.setID != 1 || !m.editing() {
		t.Fatal("a genuine reopen after leaving must still enter the editor")
	}
}
