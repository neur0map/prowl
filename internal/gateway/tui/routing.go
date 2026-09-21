package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Routing is the operator's model workspace, built around the two jobs they
// actually do: keep a small library of named sets, and give each set the models
// and the routing strategy it should use. It opens on the sets, never on the
// 140-model catalogue, so the landing view is a decision surface rather than a
// wall. Choosing a set drills into its models with providers collapsed, so
// selection is deliberate instead of a scroll through everything at once.
//
// Only connected providers appear in the editor. A model whose provider has no
// usable credential is not something the router can try, so offering it would
// only invite selections that never route. Connecting happens on Providers.

// routingMode is which of the two views the one model is showing.
type routingMode int

const (
	routingHome   routingMode = iota // the set library and presets
	routingEditor                    // one set's model selection
)

type routingLoadedMsg struct {
	gen      uint64
	routing  RoutingState
	profiles []Profile
	// activeID is the active profile id, or 0 when no set is active (the
	// server reports activeProfileId == null). Profile ids are positive, so 0
	// can never collide with a real set.
	activeID int64
	presets  []ChainPreset
	models   []ModelRow
	// members is every set's membership (profile id -> model ids), read so
	// the library can say how many of a set's models routing can actually
	// use: a raw count that includes providers the operator disconnected
	// would promise capacity that is not there.
	members map[int64][]int64
	err     error
}

// complete reports whether a snapshot carries the routing policy and model
// catalogue it needs to replace the current view. A partial refresh - a
// degraded policy read or a failed catalogue read - must not clobber a good
// snapshot with holes.
func (msg routingLoadedMsg) complete() bool {
	return msg.err == nil && msg.routing.Strategy != ""
}

// setEditLoadedMsg carries a set's ordered membership so it can be shown and
// edited in place. It is emitted both when a set is opened and after every
// membership write, so the editor always reflects the server's truth.
type setEditLoadedMsg struct {
	id    int64
	name  string
	order []int64
	err   error
}

// setCreatedMsg carries a server-created set (from the new-set form or a
// preset) back to the screen so it can be opened immediately, before the
// authoritative refresh completes.
type setCreatedMsg struct {
	profile Profile
	op      uint64
}

// profileMember is the slice of /api/profiles/{id}/models the editor reads:
// the ordered member ids, including globally excluded members so a reorder
// never silently drops one.
type profileMember struct {
	ModelDBID int64 `json:"model_db_id"`
	Priority  int64 `json:"priority"`
}

// reorderReq is one entry in the PUT /api/profiles/{id}/reorder body. Every
// member is written enabled; membership IS the enabled state in this schema.
type reorderReq struct {
	ModelDBID int64 `json:"modelDbId"`
	Priority  int64 `json:"priority"`
	Enabled   bool  `json:"enabled"`
}

const (
	filterProvider = "provider"
	filterAccess   = "access"
	filterShow     = "show"
	filterTier     = "tier"
)

// setRef and presetRef are the row payloads the home list carries, so a keypress
// knows whether the selected row is a saved set or a ready-made preset without
// re-parsing its id string.
type setRef struct {
	id   int64
	name string
}

type presetRef struct {
	id   string
	name string
	// existing is the id of a saved set with the same name (0 when none), so
	// enter opens that set instead of building a second one.
	existing int64
}

type routingModel struct {
	app    *App
	width  int
	height int
	mode   routingMode

	data    routingLoadedMsg
	loaded  bool
	loadGen uint64
	busy    string

	// The home library and the model editor are separate tables: they have
	// different columns, and collapsing a provider in the editor must not
	// disturb the library's cursor.
	homeList   list
	editorList list

	// The set the editor is on, its cached membership, and the guard that
	// keeps a late load for a set the operator moved away from out of view.
	setID     int64
	setName   string
	members   map[int64]bool
	order     []int64
	wantSetID int64
	// collapsedFor is the set the editor last collapsed its groups for.
	// Re-reading the same set keeps the operator's expansions; opening a
	// different set resets them so a new drill-in starts folded.
	collapsedFor int64

	// strategyTarget is the set a strategy picker was opened for, applied when
	// the pick returns.
	strategyTarget int64

	filters filterState
}

func newRoutingModel(app *App) routingModel {
	m := routingModel{app: app, filters: filterState{}}
	m.homeList = newList("Search sets")
	m.homeList.setHeaders("Set", "Usable", "Strategy", "State")
	m.homeList.setWeights(5, 2, 3, 5)
	m.homeList.empty = "No model sets yet."
	m.homeList.emptyHint = "Press n to name your first set, or enter on a preset to build one."
	m.editorList = newList("Search models")
	m.editorList.setHeaders("Model", "Access", "Ctx", "Intel")
	m.editorList.setWeights(6, 2, 1, 2)
	m.editorList.empty = "No models from connected providers."
	m.editorList.emptyHint = "Connect a provider on the Providers page (3) to see its models here."
	return m
}

func (m *routingModel) Init() tea.Cmd { return nil }

func (m *routingModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.homeList.width = w
	m.editorList.width = w
}

// activeList is whichever table the current mode shows, for the app's shared
// pointer and scroll handling.
func (m *routingModel) activeList() *list {
	if m.mode == routingEditor {
		return &m.editorList
	}
	return &m.homeList
}

func (m *routingModel) searching() bool { return m.activeList().isSearching() }

// crumb is the editor's set name, rendered by the page heading as a breadcrumb
// after the section title. Empty on the home view, which is the section itself.
func (m *routingModel) crumb() string {
	if m.mode == routingEditor && m.loaded {
		return m.setName
	}
	return ""
}

func (m *routingModel) load() tea.Cmd {
	m.loadGen++
	gen := m.loadGen
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out := routingLoadedMsg{gen: gen}
		var routing RoutingState
		if err := c.Get(ctx, "/api/fallback/routing", &routing); err != nil {
			out.err = err
			return out
		}
		out.routing = routing
		// Every read below is required for a complete snapshot. A silent skip on
		// failure would let an empty profiles/presets list overwrite good state,
		// so a failed read fails the whole refresh instead (complete() rejects it).
		var profiles []Profile
		if err := c.Get(ctx, "/api/profiles", &profiles); err != nil {
			out.err = fmt.Errorf("model sets unavailable: %w", err)
			return out
		}
		out.profiles = profiles
		var active struct {
			ActiveProfileID *int64 `json:"activeProfileId"`
		}
		if err := c.Get(ctx, "/api/profiles/active", &active); err != nil {
			out.err = fmt.Errorf("active set unavailable: %w", err)
			return out
		}
		if active.ActiveProfileID != nil {
			out.activeID = *active.ActiveProfileID
		}
		var env struct {
			Presets []ChainPreset `json:"presets"`
		}
		if err := c.Get(ctx, "/api/profiles/presets", &env); err != nil {
			out.err = fmt.Errorf("preset sets unavailable: %w", err)
			return out
		}
		out.presets = env.Presets
		var models []ModelRow
		if err := c.Get(ctx, "/api/models", &models); err != nil {
			out.err = fmt.Errorf("model catalogue unavailable: %w", err)
			return out
		}
		out.models = models
		out.members = make(map[int64][]int64, len(profiles))
		for _, p := range profiles {
			var members []profileMember
			if err := c.Get(ctx, fmt.Sprintf("/api/profiles/%d/models", p.ID), &members); err != nil {
				out.err = fmt.Errorf("set %s unavailable: %w", p.Name, err)
				return out
			}
			out.members[p.ID] = memberIDs(members)
		}
		return out
	}
}

// usableCount is how many of a set's members belong to a connected provider
// right now - the number the router can actually try.
func (m *routingModel) usableCount(setID int64) (usable, total int) {
	available := make(map[int64]bool, len(m.data.models))
	for _, model := range m.data.models {
		if model.Available {
			available[model.ID] = true
		}
	}
	ids := m.data.members[setID]
	for _, id := range ids {
		if available[id] {
			usable++
		}
	}
	return usable, len(ids)
}

// modelsCell words a set's size so a set whose members are mostly on
// disconnected providers is not read as bigger than it routes.
func (m *routingModel) modelsCell(setID int64) string {
	usable, total := m.usableCount(setID)
	switch {
	case total == 0:
		return "0"
	case usable == total:
		return fmt.Sprintf("%d", total)
	default:
		return fmt.Sprintf("%d of %d", usable, total)
	}
}

func (m *routingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case routingLoadedMsg:
		if msg.gen != m.loadGen {
			return m, nil
		}
		if !msg.complete() && m.loaded {
			// Reject a partial or failed refresh: keep the good snapshot rather
			// than blanking the view, and surface a transient error as a toast.
			m.busy = ""
			if msg.err != nil {
				return m, m.app.showToast("bad", msg.err.Error())
			}
			return m, nil
		}
		first := !m.loaded
		m.data = msg
		m.loaded = true
		m.busy = ""
		m.buildHomeRows()
		if first {
			// Land on the first set, not the "Your sets" header, so enter and
			// space act on a real row the moment the section opens.
			m.homeList.cursor, m.homeList.offset = 0, 0
			for i, idx := range m.homeList.filtered() {
				if !m.homeList.rows[idx].header {
					m.homeList.cursor = i
					break
				}
			}
			m.homeList.clamp()
		}
		// If the editor is open on a set that still exists, re-read its
		// membership so its counts follow the fresh snapshot; a deleted set
		// drops the operator back to the library.
		if m.mode == routingEditor {
			if m.profileExists(m.setID) {
				return m, m.openSet(m.setID, m.profileName(m.setID))
			}
			m.mode = routingHome
		}
		return m, nil

	case setCreatedMsg:
		found := false
		for i := range m.data.profiles {
			if m.data.profiles[i].ID == msg.profile.ID {
				m.data.profiles[i] = msg.profile
				found = true
				break
			}
		}
		if !found {
			m.data.profiles = append(m.data.profiles, msg.profile)
		}
		m.loaded = true
		return m, tea.Batch(m.openSet(msg.profile.ID, msg.profile.Name), m.load())

	case setEditLoadedMsg:
		// A late set result must not replace the set the operator moved to.
		// Only the set they currently intend to view may consume it.
		if msg.id != m.wantSetID {
			return m, nil
		}
		m.busy = ""
		if msg.err != nil {
			return m, m.app.showToast("bad", msg.err.Error())
		}
		m.mode = routingEditor
		m.setID = msg.id
		m.setName = msg.name
		m.order = msg.order
		m.members = make(map[int64]bool, len(msg.order))
		for _, id := range msg.order {
			m.members[id] = true
		}
		for i := range m.data.profiles {
			if m.data.profiles[i].ID == msg.id {
				m.data.profiles[i].ModelCount = len(msg.order)
				break
			}
		}
		// Keep the home library's usable-count source in step with this write,
		// so returning to the set list shows the new count without a reload.
		if m.data.members == nil {
			m.data.members = map[int64][]int64{}
		}
		m.data.members[msg.id] = msg.order
		if m.collapsedFor != msg.id {
			// A different set: fold every provider away and start its filters
			// and cursor fresh, so each drill-in is a deliberate expansion.
			m.collapsedFor = msg.id
			m.editorList.clearSearch()
			m.editorList.cursor, m.editorList.offset = 0, 0
			m.filters = filterState{}
			m.collapseAllGroups()
		}
		m.buildEditorRows()
		return m, nil

	case filterAppliedMsg:
		m.filters = msg.state
		m.editorList.cursor, m.editorList.offset = 0, 0
		m.buildEditorRows()
		return m, nil

	case pickedMsg:
		if msg.kind == pickStrategy {
			return m, m.applyStrategy(m.strategyTarget, msg.id)
		}
		return m, nil

	case tea.KeyPressMsg:
		if m.mode == routingEditor {
			return m.updateEditor(msg)
		}
		return m.updateHome(msg)
	}
	return m, nil
}

// ── home view ───────────────────────────────────────────────────────────────

func (m *routingModel) updateHome(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.homeList.typeFilter(msg) {
		m.buildHomeRows()
		return m, nil
	}
	switch msg.String() {
	case "up", "k":
		m.homeList.move(-1)
		return m, nil
	case "down", "j":
		m.homeList.move(1)
		return m, nil
	case "pgup":
		m.homeList.page(-1)
		return m, nil
	case "pgdn":
		m.homeList.page(1)
		return m, nil
	case "home", "g":
		m.homeList.top()
		return m, nil
	case "end", "G":
		m.homeList.bot()
		return m, nil
	case "/":
		m.homeList.startSearch()
		m.buildHomeRows()
		return m, nil
	}
	if m.busy != "" {
		return m, nil
	}
	if msg.String() == "n" {
		return m, m.openCreateSet()
	}
	r := m.homeList.selected()
	if r == nil {
		return m, nil
	}
	switch ref := r.key.(type) {
	case setRef:
		switch msg.String() {
		case "enter":
			return m, m.openSet(ref.id, ref.name)
		case "space":
			if ref.id != m.data.activeID {
				return m, m.activateSet(ref.id)
			}
		case "r":
			return m, m.openStrategyPicker(ref.id)
		case "x":
			return m, m.openRename(ref.id, ref.name)
		case "d":
			// The Default set is the fallback the router lands on; deleting it
			// would leave no floor, so it is never offered.
			if !strings.EqualFold(ref.name, "Default") {
				return m, m.confirmDeleteSet(ref.id, ref.name)
			}
		}
	case presetRef:
		if msg.String() == "enter" {
			return m, m.openPreset(ref)
		}
	}
	return m, nil
}

func (m *routingModel) buildHomeRows() {
	rows := make([]row, 0, len(m.data.profiles)+len(m.data.presets)+2)

	activeSummary := "no active set"
	if m.data.activeID != 0 {
		activeSummary = "active: " + m.profileName(m.data.activeID)
	}
	rows = append(rows, row{
		id:      "group:sets",
		cells:   []string{"Your sets"},
		summary: fmt.Sprintf("%d sets · %s", len(m.data.profiles), activeSummary),
		header:  true,
		group:   "sets",
		key:     "sets",
	})
	profiles := append([]Profile(nil), m.data.profiles...)
	sort.SliceStable(profiles, func(i, j int) bool {
		ai, aj := profiles[i].ID == m.data.activeID, profiles[j].ID == m.data.activeID
		if ai != aj {
			return ai
		}
		di := strings.EqualFold(profiles[i].Name, "Default")
		dj := strings.EqualFold(profiles[j].Name, "Default")
		if di != dj {
			return di
		}
		return strings.ToLower(profiles[i].Name) < strings.ToLower(profiles[j].Name)
	})
	for _, p := range profiles {
		active := p.ID == m.data.activeID
		inheriting := p.Strategy == ""
		marker := stFaint.Render("○") + " "
		if active {
			marker = stGood.Render("●") + " "
		}
		stratLabel := m.defaultStrategyName() + " · default"
		if !inheriting {
			stratLabel = strategyDisplayName(p.Strategy)
		}
		usable, total := m.usableCount(p.ID)
		state := ""
		switch {
		case active:
			state = "active"
		case total == 0:
			state = "empty"
		case usable == 0:
			state = "nothing usable"
		}
		rows = append(rows, row{
			id: fmt.Sprintf("set:%d", p.ID),
			cells: []string{
				marker + p.Name,
				m.modelsCell(p.ID),
				stratLabel,
				state,
			},
			styles: []func(string) string{
				func(v string) string {
					if active {
						return stHead.Render(v)
					}
					return stSubtle.Render(v)
				},
				func(v string) string { return stSubtle.Render(v) },
				func(v string) string {
					if inheriting {
						return stFaint.Render(v)
					}
					return stSubtle.Render(v)
				},
				stateCell,
			},
			group: "sets",
			key:   setRef{id: p.ID, name: p.Name},
		})
	}

	rows = append(rows, row{
		id:      "group:presets",
		cells:   []string{"Presets"},
		summary: "ready-made sets · enter creates one",
		header:  true,
		group:   "presets",
		key:     "presets",
	})
	presets := append([]ChainPreset(nil), m.data.presets...)
	sort.SliceStable(presets, func(i, j int) bool {
		order := map[string]int{"task": 0, "catalogue": 1}
		gi, iok := order[presets[i].Group]
		if !iok {
			gi = 2
		}
		gj, jok := order[presets[j].Group]
		if !jok {
			gj = 2
		}
		if gi != gj {
			return gi < gj
		}
		return strings.ToLower(presets[i].Name) < strings.ToLower(presets[j].Name)
	})
	for _, p := range presets {
		existing := m.profileIDByName(p.Name)
		modelsCell := fmt.Sprintf("%d matching", p.Models)
		if existing != 0 {
			// The preset already produced a set; enter opens it rather than
			// building a duplicate, so the cell says so.
			modelsCell = "created"
		}
		stratLabel := m.defaultStrategyName() + " · default"
		if p.Strategy != "" {
			stratLabel = strategyDisplayName(p.Strategy)
		}
		rows = append(rows, row{
			id:    "preset:" + p.ID,
			cells: []string{p.Name, modelsCell, stratLabel, p.Description},
			dim:   p.Models == 0,
			group: "presets",
			key:   presetRef{id: p.ID, name: p.Name, existing: existing},
		})
	}

	m.homeList.setRows(rows)
}

// stateCell colours a set's state so an empty or active set reads at a glance.
func stateCell(state string) string {
	switch state {
	case "active":
		return stGood.Render(state)
	case "empty", "nothing usable":
		return stWarn.Render(state)
	default:
		return stFaint.Render(state)
	}
}

func (m *routingModel) openPreset(ref presetRef) tea.Cmd {
	if ref.existing != 0 {
		return m.openSet(ref.existing, ref.name)
	}
	c := m.app.Client
	id, name := ref.id, ref.name
	m.busy = "building " + name + "…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var out struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			Models   int    `json:"models"`
			Strategy string `json:"strategy"`
		}
		// The preset builds a set but does not activate it: enter is "open to
		// edit", not "switch routing". Activation is the deliberate space/a.
		if err := c.Post(ctx, fmt.Sprintf("/api/profiles/presets/%s", id), map[string]any{"activate": false}, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		if out.Name == "" {
			out.Name = name
		}
		return setCreatedMsg{profile: Profile{ID: out.ID, Name: out.Name, ModelCount: out.Models, Strategy: out.Strategy}}
	}
}

// homeHeadline is the page heading's right-hand summary on the library view.
func (m *routingModel) homeHeadline() string {
	if m.data.activeID == 0 {
		return "no active set · routing uses every enabled model"
	}
	return fmt.Sprintf("active: %s · %s · %s usable models",
		m.profileName(m.data.activeID), m.effectiveStrategyName(m.data.activeID), m.modelsCell(m.data.activeID))
}

func (m *routingModel) homeStatus() string {
	if m.busy != "" {
		return stWarn.Render("● " + m.busy)
	}
	providers, models := m.connectedStats()
	return stSubtle.Render(fmt.Sprintf("%d connected providers · %d models available to sets", providers, models)) +
		stFaint.Render("  ·  "+m.benchmarkSummary())
}

// ── editor view ─────────────────────────────────────────────────────────────

func (m *routingModel) updateEditor(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.editorList.typeFilter(msg) {
		m.buildEditorRows()
		return m, nil
	}
	switch msg.String() {
	case "up", "k":
		m.editorList.move(-1)
		return m, nil
	case "down", "j":
		m.editorList.move(1)
		return m, nil
	case "pgup":
		m.editorList.page(-1)
		return m, nil
	case "pgdn":
		m.editorList.page(1)
		return m, nil
	case "home", "g":
		m.editorList.top()
		return m, nil
	case "end", "G":
		m.editorList.bot()
		return m, nil
	case "/":
		m.editorList.startSearch()
		m.buildEditorRows()
		return m, nil
	case "esc":
		// typeFilter above already handled esc while searching, so a plain esc
		// here always means "back to the sets". Rebuild the library first so a
		// membership change made in the editor shows in the set's count.
		m.mode = routingHome
		m.buildHomeRows()
		return m, nil
	}
	// Directional expand/collapse on a provider header, before the busy gate so
	// browsing a folded set stays responsive during a write.
	if r := m.editorList.selected(); r != nil && r.header {
		switch msg.String() {
		case "right", "l":
			if m.editorList.collapsed[r.group] {
				m.editorList.toggleGroup()
			}
			return m, nil
		case "left", "h":
			if !m.editorList.collapsed[r.group] {
				m.editorList.toggleGroup()
			}
			return m, nil
		}
	}
	if m.busy != "" {
		return m, nil
	}
	switch msg.String() {
	case "r":
		return m, m.openStrategyPicker(m.setID)
	case "a":
		if m.setID != m.data.activeID {
			return m, m.activateSet(m.setID)
		}
		return m, nil
	case "A":
		return m, m.toggleAllVisible()
	case "f":
		return m, m.openFilters()
	case "x":
		return m, m.openRename(m.setID, m.setName)
	case "d":
		if !strings.EqualFold(m.setName, "Default") {
			return m, m.confirmDeleteSet(m.setID, m.setName)
		}
		return m, nil
	}
	r := m.editorList.selected()
	if r == nil {
		return m, nil
	}
	if r.header {
		switch msg.String() {
		case "enter":
			m.editorList.toggleGroup()
		case "space":
			return m, m.toggleGroupMembership(r.group)
		}
		return m, nil
	}
	model, ok := r.key.(ModelRow)
	if !ok {
		return m, nil
	}
	switch msg.String() {
	case "space":
		return m, m.toggleMembership(model)
	case "enter":
		m.app.overlay = m.modelDetail(model)
		return m, m.app.overlay.Init()
	case "e":
		next := !model.Enabled
		return m, m.patchModelCmd(model.ID, map[string]any{"enabled": next},
			fmt.Sprintf("%s %s", model.DisplayName, excludedWord(next)))
	}
	return m, nil
}

func (m *routingModel) buildEditorRows() {
	elite, frontier, strong := m.tierThresholds()
	groups := m.editorGroups()
	rows := make([]row, 0, len(m.data.models)+len(groups))
	for _, g := range groups {
		rows = append(rows, row{
			id:      "group:" + g.platform,
			cells:   []string{providerDisplayName(g.platform)},
			summary: fmt.Sprintf("%d models · %d selected", len(g.models), g.members),
			header:  true,
			group:   g.platform,
			key:     g.platform,
		})
		for _, model := range g.models {
			member := m.isMember(model)
			var marker string
			switch {
			case member:
				// Membership is what the operator controls here, so a selected
				// model always reads as selected even if it was globally
				// disabled - selecting it re-enables it (see toggleMembership).
				marker = stGood.Render("●") + " "
			case !model.Enabled:
				// Not selected and globally excluded from routing.
				marker = stBad.Render("⊘") + " "
			default:
				marker = stFaint.Render("○") + " "
			}
			rows = append(rows, row{
				id: fmt.Sprintf("model:%d", model.ID),
				cells: []string{
					marker + model.DisplayName,
					model.Access,
					contextLabel(int(deref(model.ContextWindow))),
					tierRankCell(model.IntelligenceRank, elite, frontier, strong),
				},
				styles: []func(string) string{
					func(value string) string {
						if member {
							return stHead.Render(value)
						}
						if !model.Enabled {
							return stFaint.Render(value)
						}
						return stSubtle.Render(value)
					},
					accessCell,
					func(value string) string { return stSubtle.Render(value) },
					tierCell,
				},
				key:   model,
				group: g.platform,
			})
		}
	}
	if len(rows) == 0 {
		if _, models := m.connectedStats(); models == 0 {
			m.editorList.empty = "No connected providers yet."
			m.editorList.emptyHint = "Connect one on the Providers page (3) to choose its models here."
		} else if m.filters.active(m.filterSections()) > 0 {
			m.editorList.empty = "No models match these filters."
			m.editorList.emptyHint = "Press f to widen them, or x inside the filter panel to reset every lens."
		} else {
			m.editorList.empty = "No models from connected providers."
			m.editorList.emptyHint = "Connect a provider on the Providers page (3) to see its models here."
		}
	}
	m.editorList.setRows(rows)
}

// collapseAllGroups folds every provider away, the editor's default so a new
// drill-in reads as a short list of providers rather than every model at once.
func (m *routingModel) collapseAllGroups() {
	m.editorList.collapsed = map[string]bool{}
	for _, g := range m.editorGroups() {
		m.editorList.collapsed[g.platform] = true
	}
}

// editorHeadline is the page heading's right-hand summary while editing a set.
func (m *routingModel) editorHeadline() string {
	state := "not active"
	if m.setID == m.data.activeID {
		state = "active"
	}
	usable := 0
	for _, model := range m.data.models {
		if model.Available && m.members[model.ID] {
			usable++
		}
	}
	selected := fmt.Sprintf("%d selected", usable)
	if extra := len(m.order) - usable; extra > 0 {
		selected += fmt.Sprintf(" · %d on disconnected providers", extra)
	}
	return fmt.Sprintf("%s · %s · %s", selected, m.effectiveStrategyName(m.setID), state)
}

func (m *routingModel) editorStatus() string {
	if m.busy != "" {
		return stWarn.Render("● " + m.busy)
	}
	if m.setID != m.data.activeID {
		active := "every enabled model"
		if m.data.activeID != 0 {
			active = m.profileName(m.data.activeID)
		}
		return stWarn.Render("● "+m.setName+" is not the active set; ") +
			stSubtle.Render("routing keeps using "+active)
	}
	return stFaint.Render("Space selects a model or a provider, A selects everything shown; esc returns to your sets")
}

// modelDetail is the read-only drill-down for one model.
func (m *routingModel) modelDetail(model ModelRow) tea.Model {
	elite, frontier, strong := m.tierThresholds()
	credential := "none"
	switch {
	case model.Access == "subscription" && model.KeyCount > 0:
		credential = "subscription account"
	case model.Keyless:
		credential = "keyless endpoint"
	case model.KeyCount == 1:
		credential = "1 API key"
	case model.KeyCount > 1:
		credential = fmt.Sprintf("%d API keys", model.KeyCount)
	}
	membershipText := "not in " + m.setName
	if m.isMember(model) {
		membershipText = "in " + m.setName
	}
	if !model.Enabled {
		membershipText += " · excluded from all routing"
	}
	size := model.SizeLabel
	if size == "" {
		size = "-"
	}
	body := strings.Join([]string{
		keyRow("Model id", stKey.Render(model.ModelID), 14),
		keyRow("Provider", providerDisplayName(model.Platform), 14),
		keyRow("Access", accessCell(model.Access), 14),
		keyRow("Credential", credential, 14),
		keyRow("Context", contextLabel(int(deref(model.ContextWindow))), 14),
		keyRow("Size", size, 14),
		keyRow("Tier", tierCell(tierWord(model.IntelligenceRank, elite, frontier, strong))+
			stFaint.Render(fmt.Sprintf("  rank %d · speed rank %d", model.IntelligenceRank, model.SpeedRank)), 14),
		keyRow("Membership", membershipText, 14),
	}, "\n")
	membership := action{Key: "space", Label: "Add to " + m.setName, Primary: true}
	if m.isMember(model) {
		membership.Label = "Remove from " + m.setName
	}
	exclude := action{Key: "e", Label: "Exclude from routing", Dangerous: true}
	if !model.Enabled {
		exclude = action{Key: "e", Label: "Include in routing"}
	}
	return newDetailOverlay(model.DisplayName, providerDisplayName(model.Platform), body, []action{membership, exclude})
}

// ── set operations ──────────────────────────────────────────────────────────

func (m *routingModel) openSet(id int64, name string) tea.Cmd {
	c := m.app.Client
	m.busy = "reading " + name + "…"
	m.wantSetID = id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var members []profileMember
		if err := c.Get(ctx, fmt.Sprintf("/api/profiles/%d/models", id), &members); err != nil {
			return setEditLoadedMsg{id: id, name: name, err: err}
		}
		return setEditLoadedMsg{id: id, name: name, order: memberIDs(members)}
	}
}

func (m *routingModel) activateSet(id int64) tea.Cmd {
	c := m.app.Client
	name := m.profileName(id)
	m.busy = "activating " + name + "…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out map[string]any
		if err := c.Post(ctx, "/api/profiles/active", map[string]any{"profileId": id}, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		return doneMsg{Tab: TabRouting, Text: name + " is now the active set"}
	}
}

func (m *routingModel) applyStrategy(setID int64, strategyID string) tea.Cmd {
	setNm := m.profileName(setID)
	m.busy = "setting " + setNm + " strategy…"
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out map[string]any
		if err := c.Patch(ctx, fmt.Sprintf("/api/profiles/%d", setID), map[string]any{"strategy": strategyID}, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		text := setNm + " follows the default strategy"
		if strategyID != "" {
			text = setNm + " strategy: " + strategyDisplayName(strategyID)
		}
		return doneMsg{Tab: TabRouting, Text: text}
	}
}

func (m *routingModel) openCreateSet() tea.Cmd {
	f := newForm("Name your set", "Name")
	c := m.app.Client
	f.submit = func(values []string) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out Profile
		if err := c.Post(ctx, "/api/profiles", map[string]any{"name": values[0], "empty": true}, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		return setCreatedMsg{profile: out}
	}
	m.app.overlay = f
	return f.Init()
}

func (m *routingModel) openRename(id int64, current string) tea.Cmd {
	f := newForm("Rename set", "Name")
	f.fields[0].input.SetValue(current)
	c := m.app.Client
	f.submit = func(values []string) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out Profile
		if err := c.Patch(ctx, fmt.Sprintf("/api/profiles/%d", id), map[string]any{"name": values[0]}, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		return doneMsg{Tab: TabRouting, Text: "set renamed to " + values[0]}
	}
	m.app.overlay = f
	return f.Init()
}

func (m *routingModel) confirmDeleteSet(id int64, name string) tea.Cmd {
	c := m.app.Client
	m.app.overlay = &confirmOverlay{
		question: fmt.Sprintf("Delete the model set %q? Its saved model selection is removed. This cannot be undone.", name),
		yes: func() tea.Msg {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var out map[string]any
				if err := c.Delete(ctx, fmt.Sprintf("/api/profiles/%d", id), &out); err != nil {
					return errMsg{Screen: "routing", Err: err}
				}
				return doneMsg{Tab: TabRouting, Text: "set " + name + " deleted"}
			}
		},
	}
	return nil
}

// ── membership writes ───────────────────────────────────────────────────────

// toggleMembership flips one model in the set, rewriting its ordered membership
// atomically and re-reading server truth.
func (m *routingModel) toggleMembership(model ModelRow) tea.Cmd {
	return m.writeSetOrder(toggledOrder(m.order, model.ID))
}

// toggleGroupMembership selects every model of a provider, or clears them all
// when every one is already in the set. Only the ids that change are written.
func (m *routingModel) toggleGroupMembership(platform string) tea.Cmd {
	type member struct {
		id int64
		in bool
	}
	group := make([]member, 0, 16)
	allIn := true
	for _, model := range m.data.models {
		if !model.Available || model.Platform != platform || !m.passesFilters(model) {
			continue
		}
		in := m.isMember(model)
		group = append(group, member{id: model.ID, in: in})
		if !in {
			allIn = false
		}
	}
	if len(group) == 0 {
		return nil
	}
	on := !allIn
	ids := make([]int64, 0, len(group))
	for _, mem := range group {
		if mem.in != on {
			ids = append(ids, mem.id)
		}
	}
	order := append([]int64(nil), m.order...)
	if on {
		order = append(order, ids...)
	} else {
		drop := map[int64]bool{}
		for _, id := range ids {
			drop[id] = true
		}
		kept := order[:0]
		for _, id := range order {
			if !drop[id] {
				kept = append(kept, id)
			}
		}
		order = kept
	}
	return m.writeSetOrder(order)
}

// visibleEditorModels are the models the editor currently shows: available,
// passing the active filters and the search filter, regardless of whether their
// provider is folded away. It mirrors buildEditorRows' predicate so a
// select-all acts on exactly the rows the user can see, not the hidden ones a
// collapsed group would drop.
func (m *routingModel) visibleEditorModels() []ModelRow {
	q := strings.ToLower(strings.TrimSpace(m.editorList.filter))
	out := make([]ModelRow, 0, len(m.editorList.rows))
	for i := range m.editorList.rows {
		r := &m.editorList.rows[i]
		if r.header {
			continue
		}
		if q != "" && !rowMatches(r, q) {
			continue
		}
		if model, ok := r.key.(ModelRow); ok {
			out = append(out, model)
		}
	}
	return out
}

// toggleAllVisible selects every model the editor currently shows, or clears
// them all when every visible model is already in the set. Only the ids whose
// membership changes are written, so a mixed selection fills in rather than
// churning the whole order.
func (m *routingModel) toggleAllVisible() tea.Cmd {
	visible := m.visibleEditorModels()
	if len(visible) == 0 {
		return nil
	}
	allIn := true
	for _, model := range visible {
		if !m.isMember(model) {
			allIn = false
			break
		}
	}
	on := !allIn
	ids := make([]int64, 0, len(visible))
	for _, model := range visible {
		if m.isMember(model) != on {
			ids = append(ids, model.ID)
		}
	}
	order := append([]int64(nil), m.order...)
	if on {
		order = append(order, ids...)
	} else {
		drop := map[int64]bool{}
		for _, id := range ids {
			drop[id] = true
		}
		kept := order[:0]
		for _, id := range order {
			if !drop[id] {
				kept = append(kept, id)
			}
		}
		order = kept
	}
	return m.writeSetOrder(order)
}

// writeSetOrder replaces the set's membership and re-reads server truth. The
// reorder marks every member enabled, so a model selected into a set (even a
// globally-disabled one) becomes usable in the same write - the server does
// the enable in its transaction, so a bulk select is one round trip, not one
// per model.
func (m *routingModel) writeSetOrder(order []int64) tea.Cmd {
	c := m.app.Client
	setID, setName := m.setID, m.setName
	m.busy = "updating " + setName + "…"
	m.wantSetID = setID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var out map[string]any
		if err := c.Put(ctx, fmt.Sprintf("/api/profiles/%d/reorder", setID), reorderPayload(order), &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		var members []profileMember
		if err := c.Get(ctx, fmt.Sprintf("/api/profiles/%d/models", setID), &members); err != nil {
			return setEditLoadedMsg{id: setID, name: setName, err: err}
		}
		return setEditLoadedMsg{id: setID, name: setName, order: memberIDs(members)}
	}
}

func (m *routingModel) patchModelCmd(id int64, patch map[string]any, text string) tea.Cmd {
	c := m.app.Client
	m.busy = "updating model…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out map[string]any
		if err := c.Patch(ctx, fmt.Sprintf("/api/models/%d", id), patch, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		return doneMsg{Tab: TabRouting, Text: text}
	}
}

// toggledOrder removes id if present, otherwise appends it, preserving the rest
// of the order.
func toggledOrder(order []int64, id int64) []int64 {
	out := make([]int64, 0, len(order)+1)
	found := false
	for _, x := range order {
		if x == id {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		out = append(out, id)
	}
	return out
}

func reorderPayload(order []int64) []reorderReq {
	out := make([]reorderReq, 0, len(order))
	for i, id := range order {
		out = append(out, reorderReq{ModelDBID: id, Priority: int64(i + 1), Enabled: true})
	}
	return out
}

func memberIDs(members []profileMember) []int64 {
	out := make([]int64, 0, len(members))
	for _, mem := range members {
		out = append(out, mem.ModelDBID)
	}
	return out
}

func excludedWord(enabled bool) string {
	if enabled {
		return "included in routing again"
	}
	return "excluded from all routing"
}

// ── pickers ─────────────────────────────────────────────────────────────────

// openStrategyPicker opens the per-set strategy chooser. The set the picker is
// for is stashed so the returned pick applies to it even if the selection moved.
func (m *routingModel) openStrategyPicker(id int64) tea.Cmd {
	m.strategyTarget = id
	current := ""
	if p, ok := m.profileByID(id); ok {
		current = p.Strategy
	}
	choices := make([]pickerChoice, 0, len(strategyChoices)+1)
	choices = append(choices, pickerChoice{
		id: "", title: "Default (" + m.defaultStrategyName() + ")",
		detail:  "follow the console-wide strategy",
		current: current == "", tag: setTag(current == ""),
	})
	for _, strategy := range strategyChoices {
		choices = append(choices, pickerChoice{
			id: strategy.id, title: strategy.name, detail: strategy.blurb,
			current: strategy.id == current, tag: setTag(strategy.id == current),
		})
	}
	name := m.profileName(id)
	overlay := newPicker(
		"Strategy for "+name,
		"How "+name+" orders its models for each request. Default follows the console-wide strategy.",
		pickStrategy,
		choices,
	)
	m.app.overlay = overlay
	return overlay.Init()
}

func setTag(active bool) string {
	if active {
		return "in use"
	}
	return ""
}

func (m *routingModel) filterSections() []filterSection {
	providers := map[string]int{}
	access := map[string]int{}
	inSet, outSet := 0, 0
	elite, frontier, strong := m.tierThresholds()
	tiers := map[string]int{}
	for _, model := range m.data.models {
		if !model.Available {
			continue
		}
		providers[model.Platform]++
		access[model.Access]++
		if m.isMember(model) {
			inSet++
		} else {
			outSet++
		}
		switch {
		case model.IntelligenceRank <= elite:
			tiers["elite"]++
			fallthrough
		case model.IntelligenceRank <= frontier:
			tiers["frontier"]++
			fallthrough
		case model.IntelligenceRank <= strong:
			tiers["strong"]++
		}
	}
	providerOptions := make([]filterOption, 0, len(providers))
	for id, n := range providers {
		providerOptions = append(providerOptions, filterOption{id: id, label: providerDisplayName(id), count: n})
	}
	sort.Slice(providerOptions, func(i, j int) bool {
		return strings.ToLower(providerOptions[i].label) < strings.ToLower(providerOptions[j].label)
	})
	total := inSet + outSet
	return []filterSection{
		{id: filterShow, title: "Show", radio: true, options: []filterOption{
			{id: "all", label: "All models", count: total},
			{id: "in", label: "In this set", count: inSet},
			{id: "out", label: "Not in this set", count: outSet},
		}},
		{id: filterProvider, title: "Providers (none checked = every connected provider)", options: providerOptions},
		{id: filterAccess, title: "Access", radio: true, options: []filterOption{
			{id: "all", label: "Any", count: total},
			{id: "free", label: "Free", count: access["free"]},
			{id: "subscription", label: "Subscription", count: access["subscription"]},
			{id: "paid", label: "Paid", count: access["paid"]},
		}},
		{id: filterTier, title: "Intelligence tier", radio: true, options: []filterOption{
			{id: "all", label: "Any", count: total},
			{id: "elite", label: "Elite · top 10%", count: tiers["elite"]},
			{id: "frontier", label: "Frontier · top 25%", count: tiers["frontier"]},
			{id: "strong", label: "Strong · top 50%", count: tiers["strong"]},
		}},
	}
}

func (m *routingModel) openFilters() tea.Cmd {
	overlay := newFilterOverlay("Filter models", m.filterSections(), m.filters)
	m.app.overlay = overlay
	return overlay.Init()
}

// ── lookups ─────────────────────────────────────────────────────────────────

func (m *routingModel) profileExists(id int64) bool {
	_, ok := m.profileByID(id)
	return ok
}

func (m *routingModel) profileByID(id int64) (Profile, bool) {
	for _, p := range m.data.profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

func (m *routingModel) profileName(id int64) string {
	if p, ok := m.profileByID(id); ok {
		return p.Name
	}
	return fmt.Sprintf("set %d", id)
}

func (m *routingModel) profileIDByName(name string) int64 {
	for _, p := range m.data.profiles {
		if strings.EqualFold(p.Name, name) {
			return p.ID
		}
	}
	return 0
}

// isMember reports whether a model is in the set the editor is on.
func (m *routingModel) isMember(model ModelRow) bool {
	return m.members[model.ID]
}

func (m *routingModel) defaultStrategyName() string {
	return strategyDisplayName(m.data.routing.Strategy)
}

// effectiveStrategyName is the strategy a set actually routes with: its own
// when set, otherwise the console-wide default.
func (m *routingModel) effectiveStrategyName(id int64) string {
	if p, ok := m.profileByID(id); ok && p.Strategy != "" {
		return strategyDisplayName(p.Strategy)
	}
	return m.defaultStrategyName()
}

// ── rows and tiers ──────────────────────────────────────────────────────────

// intelThreshold returns the highest (worst) intelligence_rank admitted by a
// top fraction of the connected catalogue. intelligence_rank is an ordinal
// where a smaller value is more capable. A quantile, rather than a magic rank,
// keeps "top 10%" and friends truthful as the catalogue changes.
func intelThreshold(models []ModelRow, fraction float64) int {
	ranks := make([]int, 0, len(models))
	for _, mo := range models {
		if mo.Available {
			ranks = append(ranks, mo.IntelligenceRank)
		}
	}
	if len(ranks) == 0 {
		return 0
	}
	sort.Ints(ranks)
	idx := int(fraction * float64(len(ranks)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(ranks) {
		idx = len(ranks) - 1
	}
	return ranks[idx]
}

func (m *routingModel) tierThresholds() (elite, frontier, strong int) {
	return intelThreshold(m.data.models, 0.10),
		intelThreshold(m.data.models, 0.25),
		intelThreshold(m.data.models, 0.50)
}

func tierWord(rank, elite, frontier, strong int) string {
	switch {
	case rank <= elite:
		return "elite"
	case rank <= frontier:
		return "frontier"
	case rank <= strong:
		return "strong"
	default:
		return "-"
	}
}

func (m *routingModel) passesFilters(model ModelRow) bool {
	if providers := m.filters[filterProvider]; len(providers) > 0 && !providers[model.Platform] {
		return false
	}
	if access := m.filters.radio(filterAccess); access != "" && access != "all" && model.Access != access {
		return false
	}
	switch m.filters.radio(filterShow) {
	case "in":
		if !m.isMember(model) {
			return false
		}
	case "out":
		if m.isMember(model) {
			return false
		}
	}
	elite, frontier, strong := m.tierThresholds()
	switch m.filters.radio(filterTier) {
	case "elite":
		return model.IntelligenceRank <= elite
	case "frontier":
		return model.IntelligenceRank <= frontier
	case "strong":
		return model.IntelligenceRank <= strong
	}
	return true
}

type providerGroup struct {
	platform string
	models   []ModelRow
	members  int
}

// editorGroups is the editor table's content: every available model, grouped by
// provider and ranked by intelligence within each group.
func (m *routingModel) editorGroups() []providerGroup {
	byPlatform := map[string]*providerGroup{}
	for _, model := range m.data.models {
		if !model.Available || !m.passesFilters(model) {
			continue
		}
		g := byPlatform[model.Platform]
		if g == nil {
			g = &providerGroup{platform: model.Platform}
			byPlatform[model.Platform] = g
		}
		g.models = append(g.models, model)
		if m.isMember(model) {
			g.members++
		}
	}
	groups := make([]providerGroup, 0, len(byPlatform))
	for _, g := range byPlatform {
		sort.SliceStable(g.models, func(i, j int) bool {
			if g.models[i].IntelligenceRank != g.models[j].IntelligenceRank {
				return g.models[i].IntelligenceRank < g.models[j].IntelligenceRank
			}
			return strings.ToLower(g.models[i].DisplayName) < strings.ToLower(g.models[j].DisplayName)
		})
		groups = append(groups, *g)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return strings.ToLower(providerDisplayName(groups[i].platform)) < strings.ToLower(providerDisplayName(groups[j].platform))
	})
	return groups
}

func (m *routingModel) groupFor(platform string) *providerGroup {
	for _, g := range m.editorGroups() {
		if g.platform == platform {
			return &g
		}
	}
	return nil
}

func deref(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

// accessCell renders the free/paid/subscription tier with colour as decoration.
func accessCell(access string) string {
	switch access {
	case "free":
		return stGood.Render("free")
	case "paid":
		return stWarn.Render("paid")
	case "subscription":
		return stKey.Render("subscription")
	default:
		return stFaint.Render("-")
	}
}

// tierRankCell is the model-list capability cell: the tier word plus the raw
// intelligence rank (rank 1 = best), so an operator sees both the band and the
// exact ordinal the router sorts by. Kept as raw text so the list measures its
// width correctly; tierCell colours it by the leading word.
func tierRankCell(rank, elite, frontier, strong int) string {
	word := tierWord(rank, elite, frontier, strong)
	if rank <= 0 {
		return word
	}
	return fmt.Sprintf("%s #%d", word, rank)
}

func tierCell(tier string) string {
	word := tier
	if i := strings.IndexByte(tier, ' '); i >= 0 {
		word = tier[:i]
	}
	switch word {
	case "elite":
		return stKey.Render(tier)
	case "frontier":
		return stHead.Render(tier)
	case "strong":
		return stSubtle.Render(tier)
	default:
		return stFaint.Render(tier)
	}
}

type strategyChoice struct{ id, name, blurb string }

var strategyChoices = []strategyChoice{
	{"efficient", "Efficient", "right-sizes the model to each prompt: simple work goes to cheaper models, hard tasks to the strongest"},
	{"smartest", "Smartest", "always the most capable healthy model, whatever the task"},
	{"balanced", "Balanced", "blends capability, reliability and speed for every prompt"},
	{"fastest", "Fastest", "lowest observed latency first"},
	{"reliable", "Most reliable", "highest observed success rate first"},
	{"priority", "Priority", "your manual model order, no automatic re-ranking"},
}

func strategyDisplayName(id string) string {
	for _, strategy := range strategyChoices {
		if strategy.id == id {
			return strategy.name
		}
	}
	return id
}

// ── view ────────────────────────────────────────────────────────────────────

// headline is the page heading's right-hand summary, adapting to the view.
func (m *routingModel) headline() string {
	if !m.loaded {
		return ""
	}
	if m.mode == routingEditor {
		return m.editorHeadline()
	}
	return m.homeHeadline()
}

func (m *routingModel) connectedStats() (providers, models int) {
	seen := map[string]bool{}
	for _, model := range m.data.models {
		if !model.Available {
			continue
		}
		models++
		if !seen[model.Platform] {
			seen[model.Platform] = true
			providers++
		}
	}
	return providers, models
}

func (m *routingModel) View() tea.View {
	if !m.loaded {
		return tea.NewView(" " + m.app.spinner.View() + " " + stSubtle.Render("Reading model sets and routing policy"))
	}
	if m.data.err != nil {
		return tea.NewView(" " + stBad.Render("● "+m.data.err.Error()))
	}
	if m.mode == routingEditor {
		return tea.NewView(m.viewBody(m.editorStatus(), &m.editorList))
	}
	return tea.NewView(m.viewBody(m.homeStatus(), &m.homeList))
}

// viewBody lays out one status line above a table, the shared shape of both
// views. There is no toolbar: keys live in the footer only.
func (m *routingModel) viewBody(status string, l *list) string {
	var body strings.Builder
	body.WriteString(" " + truncate(status, max(m.width-1, 1)) + "\n")
	localY := strings.Count(body.String(), "\n")
	l.height = max(m.height-localY, 5)
	l.setOrigin(m.app.bodyX, m.app.bodyY+localY)
	body.WriteString(l.render())
	return body.String()
}

func (m *routingModel) benchmarkSummary() string {
	status := m.data.routing.Benchmark
	switch {
	case status.Matched > 0 && status.Status == "error":
		return fmt.Sprintf("benchmark refresh failed; %d cached scores in use", status.Matched)
	case status.Matched > 0 && status.LastSuccess != nil:
		return fmt.Sprintf("smart routing scores %d/%d models, refreshed %s ago", status.Matched, status.Available, relTime(status.LastSuccess.Format(time.RFC3339)))
	case !status.Configured:
		return "smart routing uses catalogue priors (set ARTIFICIAL_ANALYSIS_API_KEY for live scores)"
	default:
		return "benchmark refresh scheduled"
	}
}

// buildRows rebuilds whichever table the current mode shows, so callers that do
// not care which view is up (tests, refreshes) need one entry point.
func (m *routingModel) buildRows() {
	if m.mode == routingEditor {
		m.buildEditorRows()
		return
	}
	m.buildHomeRows()
}

func (m *routingModel) actions() []action {
	if m.mode == routingEditor {
		return m.editorActions()
	}
	return m.homeActions()
}

func (m *routingModel) homeActions() []action {
	acts := make([]action, 0, 8)
	r := m.homeList.selected()
	switch ref := rowPayload(r).(type) {
	case setRef:
		acts = append(acts, action{Key: "enter", Label: "Open set", Primary: true})
		if ref.id != m.data.activeID {
			acts = append(acts, action{Key: "space", Label: "Activate"})
		}
		acts = append(acts,
			action{Key: "r", Label: "Strategy"},
			action{Key: "n", Label: "New set"},
			action{Key: "x", Label: "Rename"},
		)
		if !strings.EqualFold(ref.name, "Default") {
			acts = append(acts, action{Key: "d", Label: "Delete", Dangerous: true})
		}
	case presetRef:
		label := "Create set"
		if ref.existing != 0 {
			label = "Open set"
		}
		acts = append(acts,
			action{Key: "enter", Label: label, Primary: true},
			action{Key: "n", Label: "New set"},
		)
	default:
		acts = append(acts, action{Key: "n", Label: "New set"})
	}
	return append(acts, action{Key: "/", Label: "Search"})
}

func (m *routingModel) editorActions() []action {
	acts := make([]action, 0, 12)
	r := m.editorList.selected()
	if r != nil && r.header {
		label := "Select provider"
		if g := m.groupFor(r.group); g != nil && len(g.models) > 0 && g.members == len(g.models) {
			label = "Clear provider"
		}
		expand := "Expand"
		if !m.editorList.collapsed[r.group] {
			expand = "Collapse"
		}
		acts = append(acts,
			action{Key: "space", Label: label, Primary: true},
			action{Key: "enter", Label: expand},
		)
	} else if model, ok := rowPayload(r).(ModelRow); ok {
		label := "Select"
		if m.isMember(model) {
			label = "Deselect"
		}
		acts = append(acts,
			action{Key: "space", Label: label, Primary: true},
			action{Key: "enter", Label: "Details"},
		)
	}
	if vis := m.visibleEditorModels(); len(vis) > 0 {
		label := "Select all"
		allIn := true
		for _, model := range vis {
			if !m.isMember(model) {
				allIn = false
				break
			}
		}
		if allIn {
			label = "Clear all"
		}
		acts = append(acts, action{Key: "A", Label: label})
	}
	acts = append(acts, action{Key: "r", Label: "Strategy"})
	if m.setID != m.data.activeID {
		acts = append(acts, action{Key: "a", Label: "Activate"})
	}
	acts = append(acts,
		action{Key: "f", Label: "Filter"},
		action{Key: "/", Label: "Search"},
		action{Key: "esc", Label: "Back to sets"},
		action{Key: "x", Label: "Rename"},
	)
	if !strings.EqualFold(m.setName, "Default") {
		acts = append(acts, action{Key: "d", Label: "Delete", Dangerous: true})
	}
	if model, ok := rowPayload(r).(ModelRow); ok {
		label := "Exclude"
		if !model.Enabled {
			label = "Include"
		}
		acts = append(acts, action{Key: "e", Label: label})
	}
	return acts
}

// rowPayload is the selected row's key, or nil when nothing is selected, so the
// action builders can type-switch without a nil guard at every call.
func rowPayload(r *row) any {
	if r == nil {
		return nil
	}
	return r.key
}
