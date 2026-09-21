package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func visibleNames(l *list) []string {
	out := []string{}
	for _, idx := range l.filtered() {
		out = append(out, ansi.Strip(l.rows[idx].cells[0]))
	}
	return out
}

func rowByID(l *list, id string) *row {
	for i := range l.rows {
		if l.rows[i].id == id {
			return &l.rows[i]
		}
	}
	return nil
}

func cellText(r *row, i int) string {
	if r == nil || i >= len(r.cells) {
		return ""
	}
	return ansi.Strip(r.cells[i])
}

// routingHomeFixture is a loaded library: one active set inheriting the default
// strategy, one with its own strategy, one empty, and two presets (one whose
// name matches an existing set).
func routingHomeFixture(app *App) *routingModel {
	m := &app.routing
	m.loaded = true
	m.data = routingLoadedMsg{
		routing:  RoutingState{Strategy: "balanced"},
		activeID: 7,
		profiles: []Profile{
			{ID: 7, Name: "Deep work", ModelCount: 3, Strategy: ""},
			{ID: 8, Name: "Fast lane", ModelCount: 2, Strategy: "fastest"},
			{ID: 9, Name: "Scratch", ModelCount: 0, Strategy: ""},
		},
		presets: []ChainPreset{
			{ID: "free", Name: "Free models", Group: "catalogue", Models: 12, Description: "every free model"},
			{ID: "deepwork", Name: "Deep work", Group: "task", Models: 5, Strategy: "smartest", Description: "task preset that matches a set"},
		},
		models: []ModelRow{
			{ID: 1, Platform: "anthropic", DisplayName: "Claude Opus", Access: "subscription", IntelligenceRank: 1, Enabled: true, Available: true},
			{ID: 2, Platform: "groq", DisplayName: "GPT-OSS 120B", Access: "free", IntelligenceRank: 12, Enabled: true, Available: true},
			{ID: 3, Platform: "mistral", DisplayName: "Mistral Large", Access: "free", IntelligenceRank: 8, Enabled: true},
		},
		// Deep work holds a model on a disconnected provider (3): the library
		// must not count it as capacity.
		members: map[int64][]int64{7: {1, 2, 3}, 8: {2, 1}, 9: nil},
	}
	m.setSize(120, 40)
	m.buildHomeRows()
	return m
}

// TestRoutingHomeListsSetsThenPresets: the landing view is the set library -
// the sets group (active first) then the presets group - with each set's own
// strategy or the inherited default shown, and the headline naming the active
// set and the strategy it actually routes with.
func TestRoutingHomeListsSetsThenPresets(t *testing.T) {
	app := New(&Client{}, true, "test")
	m := routingHomeFixture(app)

	names := visibleNames(&m.homeList)
	want := []string{"Your sets", "● Deep work", "○ Fast lane", "○ Scratch", "Presets", "Deep work", "Free models"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("home rows =\n  %v\nwant\n  %v", names, want)
	}

	// The active set inherits the default strategy, so its cell names it as the
	// default; a set with its own strategy shows that strategy verbatim.
	if got := cellText(rowByID(&m.homeList, "set:7"), 2); got != "Balanced · default" {
		t.Fatalf("inheriting set strategy cell = %q; want the default named as default", got)
	}
	if got := cellText(rowByID(&m.homeList, "set:7"), 3); got != "active" {
		t.Fatalf("active set state cell = %q; want active", got)
	}
	if got := cellText(rowByID(&m.homeList, "set:8"), 2); got != "Fastest" {
		t.Fatalf("own-strategy cell = %q; want Fastest", got)
	}
	if got := cellText(rowByID(&m.homeList, "set:9"), 3); got != "empty" {
		t.Fatalf("empty set state cell = %q; want empty", got)
	}
	// A preset whose name matches an existing set is already built.
	if got := cellText(rowByID(&m.homeList, "preset:deepwork"), 1); got != "created" {
		t.Fatalf("matching preset models cell = %q; want created", got)
	}

	if got := cellText(rowByID(&m.homeList, "set:7"), 1); got != "2 of 3" {
		t.Fatalf("models cell = %q; want the usable count when a member's provider is not connected", got)
	}
	if got := cellText(rowByID(&m.homeList, "set:8"), 1); got != "2" {
		t.Fatalf("fully usable set models cell = %q", got)
	}
	if got := m.headline(); got != "active: Deep work · Balanced · 2 of 3 usable models" {
		t.Fatalf("headline = %q", got)
	}
}

// TestRoutingEnterOpensSetCollapsed: enter on a set reads its membership and
// drops into the editor with every provider folded; a directional key expands
// one, space writes membership, and esc returns to the library.
func TestRoutingEnterOpensSetCollapsed(t *testing.T) {
	reordered := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/profiles/7/models" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]profileMember{{ModelDBID: 41, Priority: 1}})
		case r.URL.Path == "/api/profiles/7/reorder" && r.Method == http.MethodPut:
			reordered = true
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data = routingLoadedMsg{
		routing:  RoutingState{Strategy: "balanced"},
		activeID: 7,
		profiles: []Profile{{ID: 7, Name: "Deep work", ModelCount: 1}},
		models: []ModelRow{
			{ID: 41, Platform: "anthropic", DisplayName: "Claude Opus", Access: "subscription", IntelligenceRank: 1, Enabled: true, Available: true},
			{ID: 42, Platform: "openai", DisplayName: "GPT Codex", Access: "paid", IntelligenceRank: 3, Enabled: true, Available: true},
		},
	}
	m.setSize(120, 40)
	m.buildHomeRows()

	if !m.homeList.selectID("set:7") {
		t.Fatal("no Deep work set row")
	}
	_, cmd := m.Update(syntheticKey("enter"))
	if cmd == nil {
		t.Fatal("enter on a set returned no load command")
	}
	msg := cmd()
	if _, ok := msg.(setEditLoadedMsg); !ok {
		t.Fatalf("enter on a set produced %T; want a membership read", msg)
	}
	m.Update(msg)
	if m.mode != routingEditor {
		t.Fatalf("enter did not open the editor (mode=%d)", m.mode)
	}
	// Every visible row is a provider header: the models are folded away.
	for _, idx := range m.editorList.filtered() {
		if !m.editorList.rows[idx].header {
			t.Fatalf("a model row is visible while the editor should be collapsed: %q", cellText(&m.editorList.rows[idx], 0))
		}
	}
	if got := rowByID(&m.editorList, "group:anthropic").summary; got != "1 models · 1 selected" {
		t.Fatalf("anthropic header summary = %q; want its model count and selection", got)
	}
	if got := rowByID(&m.editorList, "group:openai").summary; got != "1 models · 0 selected" {
		t.Fatalf("openai header summary = %q", got)
	}

	// right expands a provider so its models are reachable.
	if !m.editorList.selectID("group:openai") {
		t.Fatal("no openai header")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if !m.editorList.selectID("model:42") {
		t.Fatal("right did not expand openai to reveal its model")
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	if cmd == nil {
		t.Fatal("space on a model returned no command")
	}
	cmd()
	if !reordered {
		t.Fatal("space on a model did not write the set's membership")
	}

	m.Update(syntheticKey("esc"))
	if m.mode != routingHome {
		t.Fatalf("esc did not return to the library (mode=%d)", m.mode)
	}
}

// TestRoutingSelectDisabledModelEnablesIt proves a globally-disabled model
// (rendered ⊘) can be selected into a set: the reorder write (which marks
// members enabled server-side) makes it a member, so it stops reading as
// excluded, and the home library's usable count reflects the new member
// without leaving and re-entering the editor.
func TestRoutingSelectDisabledModelEnablesIt(t *testing.T) {
	var reordered bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/profiles/7/models" && r.Method == http.MethodGet:
			// After the write the set holds both models.
			if reordered {
				_ = json.NewEncoder(w).Encode([]profileMember{{ModelDBID: 41, Priority: 1}, {ModelDBID: 42, Priority: 2}})
				return
			}
			_ = json.NewEncoder(w).Encode([]profileMember{{ModelDBID: 41, Priority: 1}})
		case r.URL.Path == "/api/profiles/7/reorder" && r.Method == http.MethodPut:
			// The client sends one reorder that carries every member as enabled;
			// the server enables them in its transaction (no per-model PATCH).
			var body []reorderReq
			_ = json.NewDecoder(r.Body).Decode(&body)
			has42 := false
			for _, e := range body {
				if e.ModelDBID == 42 {
					has42 = e.Enabled
				}
			}
			if !has42 {
				t.Fatalf("reorder body did not carry model 42 as an enabled member: %+v", body)
			}
			reordered = true
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data = routingLoadedMsg{
		routing:  RoutingState{Strategy: "balanced"},
		activeID: 7,
		profiles: []Profile{{ID: 7, Name: "Deep work", ModelCount: 1}},
		models: []ModelRow{
			{ID: 41, Platform: "anthropic", DisplayName: "Claude Opus", Access: "subscription", IntelligenceRank: 1, Enabled: true, Available: true},
			{ID: 42, Platform: "openai", DisplayName: "GPT Codex", Access: "subscription", IntelligenceRank: 3, Enabled: false, Available: true},
		},
		members: map[int64][]int64{7: {41}},
	}
	m.setSize(120, 40)
	m.buildHomeRows()
	m.Update(m.openSet(7, "Deep work")())

	// The disabled model reads as excluded until selected.
	m.editorList.selectID("group:openai")
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if got := cellText(rowByID(&m.editorList, "model:42"), 0); !strings.Contains(got, "⊘") {
		t.Fatalf("disabled model marker = %q; want ⊘ before selection", got)
	}

	m.editorList.selectID("model:42")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	if cmd == nil {
		t.Fatal("space on a disabled model returned no command")
	}
	m.Update(cmd())
	if !reordered {
		t.Fatal("selecting a disabled model must write the set membership")
	}
	if got := cellText(rowByID(&m.editorList, "model:42"), 0); !strings.Contains(got, "●") {
		t.Fatalf("selected model marker = %q; want ● after selection", got)
	}
	// Home reflects the new member without a reload: Deep work is now 2 usable.
	m.buildHomeRows()
	if got := cellText(rowByID(&m.homeList, "set:7"), 1); got != "2" {
		t.Fatalf("home usable count = %q; want 2 right after selecting in the editor", got)
	}
}

// actionLabelFor returns the label the footer would render for a key, so a test
// can assert an action both appears and reads the way the operator sees it.
func actionLabelFor(actions []action, key string) (string, bool) {
	for _, a := range actions {
		if a.Key == key {
			return a.Label, true
		}
	}
	return "", false
}

func reorderIDs(body []reorderReq) []int64 {
	out := make([]int64, 0, len(body))
	for _, b := range body {
		out = append(out, b.ModelDBID)
	}
	return out
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRoutingSelectAllVisible: A in the editor selects every model the current
// filters and search leave visible - across folded providers - writing only the
// ids that change, and clears them all once every visible model is already in.
func TestRoutingSelectAllVisible(t *testing.T) {
	var (
		mu          sync.Mutex
		members     = []int64{41}
		lastReorder []reorderReq
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/profiles/7/models" && r.Method == http.MethodGet:
			mu.Lock()
			out := make([]profileMember, 0, len(members))
			for i, id := range members {
				out = append(out, profileMember{ModelDBID: id, Priority: int64(i + 1)})
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/api/profiles/7/reorder" && r.Method == http.MethodPut:
			var body []reorderReq
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			lastReorder = body
			members = nil
			for _, b := range body {
				members = append(members, b.ModelDBID)
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data = routingLoadedMsg{
		routing:  RoutingState{Strategy: "balanced"},
		activeID: 7,
		profiles: []Profile{{ID: 7, Name: "Deep work", ModelCount: 1}},
		models: []ModelRow{
			{ID: 41, Platform: "anthropic", DisplayName: "Claude Opus", Access: "subscription", IntelligenceRank: 1, Enabled: true, Available: true},
			{ID: 42, Platform: "openai", DisplayName: "GPT Codex", Access: "paid", IntelligenceRank: 3, Enabled: true, Available: true},
			{ID: 43, Platform: "openai", DisplayName: "GPT Mini", Access: "paid", IntelligenceRank: 5, Enabled: true, Available: true},
		},
	}
	m.setSize(120, 40)
	m.buildHomeRows()
	m.wantSetID = 7
	m.Update(setEditLoadedMsg{id: 7, name: "Deep work", order: []int64{41}})

	// One member so far: the footer offers to select every visible model.
	if label, ok := actionLabelFor(m.editorActions(), "A"); !ok || label != "Select all" {
		t.Fatalf("editor footer A = %q (present=%v); want \"Select all\"", label, ok)
	}

	// A writes every visible id once, existing member first and the two that
	// changed appended, even though every provider is folded away.
	_, cmd := m.Update(syntheticKey("A"))
	if cmd == nil {
		t.Fatal("A returned no write command")
	}
	m.Update(cmd())
	mu.Lock()
	got := reorderIDs(lastReorder)
	mu.Unlock()
	if want := []int64{41, 42, 43}; !equalIDs(got, want) {
		t.Fatalf("select-all PUT ids = %v; want %v (existing member first)", got, want)
	}

	// Server truth now holds all three: the footer flips to clearing them, and
	// A PUTs an empty body.
	if label, ok := actionLabelFor(m.editorActions(), "A"); !ok || label != "Clear all" {
		t.Fatalf("editor footer A = %q (present=%v); want \"Clear all\"", label, ok)
	}
	_, cmd = m.Update(syntheticKey("A"))
	if cmd == nil {
		t.Fatal("A on a full set returned no command")
	}
	m.Update(cmd())
	mu.Lock()
	got = reorderIDs(lastReorder)
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("clear-all PUT ids = %v; want an empty body", got)
	}

	// A search that matches only OpenAI's models limits A to that provider.
	m.Update(syntheticKey("/"))
	for _, r := range "GPT" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.Update(syntheticKey("enter")) // commit the search, keeping the filter set
	_, cmd = m.Update(syntheticKey("A"))
	if cmd == nil {
		t.Fatal("A under a search returned no command")
	}
	m.Update(cmd())
	mu.Lock()
	got = reorderIDs(lastReorder)
	mu.Unlock()
	if want := []int64{42, 43}; !equalIDs(got, want) {
		t.Fatalf("filtered select-all PUT ids = %v; want only OpenAI's ids %v", got, want)
	}
}

// TestRoutingPresetCreatesAndOpens: enter on a fresh preset builds a set (with
// activate:false) and opens it for editing; enter on a preset whose name
// already names a set opens that set without building a second one.
func TestRoutingPresetCreatesAndOpens(t *testing.T) {
	var posts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/profiles/presets/free" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["activate"] != false {
				t.Fatalf("preset build activate flag = %v; want false", body["activate"])
			}
			posts = append(posts, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 15, "name": "Free models", "models": 9, "active": false})
		case r.URL.Path == "/api/profiles/7/models" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]profileMember{{ModelDBID: 41, Priority: 1}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data = routingLoadedMsg{
		routing:  RoutingState{Strategy: "balanced"},
		profiles: []Profile{{ID: 7, Name: "Deep work", ModelCount: 1}},
		presets: []ChainPreset{
			{ID: "free", Name: "Free models", Group: "catalogue", Models: 9},
			{ID: "deepwork", Name: "Deep work", Group: "task", Models: 5},
		},
		models: []ModelRow{{ID: 41, Platform: "anthropic", DisplayName: "Claude Opus", Enabled: true, Available: true}},
	}
	m.setSize(120, 40)
	m.buildHomeRows()

	// A fresh preset: POST then open the created set for editing.
	if !m.homeList.selectID("preset:free") {
		t.Fatal("no Free models preset row")
	}
	_, cmd := m.Update(syntheticKey("enter"))
	if cmd == nil {
		t.Fatal("enter on a preset returned no command")
	}
	created, ok := cmd().(setCreatedMsg)
	if !ok {
		t.Fatalf("building a preset produced %T; want setCreatedMsg", created)
	}
	if len(posts) != 1 || posts[0] != "/api/profiles/presets/free" {
		t.Fatalf("preset build hit %v; want one POST to the preset", posts)
	}
	if created.profile.ID != 15 {
		t.Fatalf("created set id = %d; want the server's 15", created.profile.ID)
	}
	m.Update(created)
	if m.wantSetID != 15 {
		t.Fatalf("after building a preset the editor opens set %d; want 15", m.wantSetID)
	}
	// The set read lands and the editor opens; go back to the library so the
	// next preset can be chosen from the home list.
	m.Update(setEditLoadedMsg{id: 15, name: "Everything free", order: []int64{41}})
	m.Update(syntheticKey("esc"))

	// A preset whose name already names a set opens that set, no POST.
	if !m.homeList.selectID("preset:deepwork") {
		t.Fatal("no matching preset row")
	}
	_, cmd = m.Update(syntheticKey("enter"))
	if cmd == nil {
		t.Fatal("enter on a matching preset returned no command")
	}
	if _, ok := cmd().(setEditLoadedMsg); !ok {
		t.Fatal("a matching preset should open the existing set, not build one")
	}
	if len(posts) != 1 {
		t.Fatalf("a matching preset triggered a build: %v", posts)
	}
}

// TestRoutingStrategyPickerPatchesTheSet: r on a set opens a picker preselected
// on that set's own strategy; a concrete choice patches the set's strategy and
// the default choice clears it back to inheriting.
func TestRoutingStrategyPickerPatchesTheSet(t *testing.T) {
	var patchPath string
	var patchBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&patchBody)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := routingHomeFixture(app)

	if !m.homeList.selectID("set:8") {
		t.Fatal("no Fast lane set row")
	}
	m.Update(syntheticKey("r"))
	pk, ok := app.overlay.(*pickerOverlay)
	if !ok {
		t.Fatalf("r opened %T; want a strategy picker", app.overlay)
	}
	if pk.choices[pk.cursor].id != "fastest" {
		t.Fatalf("picker preselects %q; want the set's own strategy fastest", pk.choices[pk.cursor].id)
	}
	if m.strategyTarget != 8 {
		t.Fatalf("strategy target = %d; want the set the picker was opened for (8)", m.strategyTarget)
	}

	_, cmd := m.Update(pickedMsg{kind: pickStrategy, id: "reliable"})
	if cmd == nil {
		t.Fatal("choosing a strategy returned no command")
	}
	cmd()
	if patchPath != "/api/profiles/8" {
		t.Fatalf("strategy PATCH hit %q; want the set", patchPath)
	}
	if patchBody["strategy"] != "reliable" {
		t.Fatalf("strategy PATCH body = %v; want reliable", patchBody)
	}

	_, cmd = m.Update(pickedMsg{kind: pickStrategy, id: ""})
	cmd()
	if patchBody["strategy"] != "" {
		t.Fatalf("clearing to default PATCHed %v; want an empty strategy", patchBody)
	}
}

// TestRoutingSpaceActivatesSetFromHome: space activates a non-active set from
// the library and is inert on the active one; the Default set never offers
// delete.
func TestRoutingSpaceActivatesSetFromHome(t *testing.T) {
	var activePath string
	var activeBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/profiles/active" && r.Method == http.MethodPost {
			activePath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&activeBody)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data = routingLoadedMsg{
		routing:  RoutingState{Strategy: "balanced"},
		activeID: 7,
		profiles: []Profile{
			{ID: 7, Name: "Deep work", ModelCount: 3},
			{ID: 8, Name: "Fast lane", ModelCount: 2, Strategy: "fastest"},
			{ID: 10, Name: "Default", ModelCount: 5},
		},
		models: []ModelRow{{ID: 1, Platform: "anthropic", DisplayName: "Claude Opus", Enabled: true, Available: true}},
	}
	m.setSize(120, 40)
	m.buildHomeRows()

	if !m.homeList.selectID("set:8") {
		t.Fatal("no Fast lane set row")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	if cmd == nil {
		t.Fatal("space on a non-active set did nothing")
	}
	if _, ok := cmd().(doneMsg); !ok {
		t.Fatal("activating a set did not complete")
	}
	if activePath != "/api/profiles/active" {
		t.Fatalf("activation hit %q; want the active-set endpoint", activePath)
	}
	if activeBody["profileId"] != float64(8) {
		t.Fatalf("activation body = %v; want the selected set id 8", activeBody)
	}

	// Space on the already-active set is inert.
	if !m.homeList.selectID("set:7") {
		t.Fatal("no active set row")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeySpace}); cmd != nil {
		t.Fatal("space on the active set should not re-activate it")
	}

	// The Default set is never offered a delete.
	if !m.homeList.selectID("set:10") {
		t.Fatal("no Default set row")
	}
	for _, a := range m.actions() {
		if a.Key == "d" {
			t.Fatal("the Default set offered delete")
		}
	}
}

// TestRoutingSearchRevealsCollapsedMatches: in the editor with every provider
// folded, a live search surfaces the matching model rows under their headers.
func TestRoutingSearchRevealsCollapsedMatches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/profiles/7/models" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]profileMember{{ModelDBID: 41, Priority: 1}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	m := &app.routing
	m.loaded = true
	m.data = routingLoadedMsg{
		routing:  RoutingState{Strategy: "balanced"},
		activeID: 7,
		profiles: []Profile{{ID: 7, Name: "Deep work", ModelCount: 1}},
		models: []ModelRow{
			{ID: 41, Platform: "anthropic", DisplayName: "Claude Opus", Access: "subscription", IntelligenceRank: 1, Enabled: true, Available: true},
			{ID: 42, Platform: "openai", DisplayName: "GPT Codex", Access: "paid", IntelligenceRank: 3, Enabled: true, Available: true},
		},
	}
	m.setSize(120, 40)
	m.buildHomeRows()
	m.wantSetID = 7
	m.Update(setEditLoadedMsg{id: 7, name: "Deep work", order: []int64{41}})

	// Every group starts folded.
	for _, idx := range m.editorList.filtered() {
		if !m.editorList.rows[idx].header {
			t.Fatal("the editor did not open collapsed")
		}
	}

	m.Update(syntheticKey("/"))
	for _, r := range "codex" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	names := visibleNames(&m.editorList)
	found := false
	for _, n := range names {
		if strings.Contains(n, "GPT Codex") {
			found = true
		}
	}
	if !found {
		t.Fatalf("search did not reveal the collapsed match: %v", names)
	}
}

// TestIntelThresholdIsAQuantileOfConnectedModels: tier cut-offs are quantiles
// of the connected catalogue, so an unconnected provider's ranks cannot skew
// what "top 10%" means.
func TestIntelThresholdIsAQuantileOfConnectedModels(t *testing.T) {
	models := []ModelRow{
		{IntelligenceRank: 1, Available: true},
		{IntelligenceRank: 2, Available: true},
		{IntelligenceRank: 3, Available: true},
		{IntelligenceRank: 4, Available: true},
		{IntelligenceRank: 100},
	}
	if got := intelThreshold(models, 0.5); got != 2 {
		t.Fatalf("median threshold = %d; want 2 (the 100-ranked unconnected model must not count)", got)
	}
	if got := intelThreshold(nil, 0.5); got != 0 {
		t.Fatalf("empty catalogue threshold = %d; want 0", got)
	}
}
