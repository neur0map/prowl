package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Routing separates two decisions that users make at different speeds:
// a saved model set controls which models may run, while the routing strategy
// decides how those candidates are ordered for each request. The home view
// keeps only saved sets in the table. Focused pickers own strategies, templates
// and provider visibility so the primary list remains short and predictable.

type routingLoadedMsg struct {
	routing  RoutingState
	profiles []Profile
	activeID int64
	presets  []ChainPreset
	models   []ModelRow
	err      error
}

// complete reports whether a routing snapshot carries the routing policy and
// model catalogue it needs to replace the current view. A partial refresh -
// a degraded policy read or a failed model-catalogue read - must not clobber
// a good snapshot with holes.
func (msg routingLoadedMsg) complete() bool {
	return msg.err == nil && msg.routing.Strategy != ""
}

// setEditLoadedMsg carries a specific set's ordered membership so it can be
// edited in place. It is emitted both when a set is opened and after every
// membership write, so the view always reflects the server's truth.
type setEditLoadedMsg struct {
	id    int64
	name  string
	order []int64
	err   error
}

// profileMember is the slice of /api/profiles/{id}/models the editor reads:
// the ordered member ids, including globally disabled members so a reorder
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

type routingModel struct {
	app    *App
	width  int
	height int
	list   list
	data   routingLoadedMsg
	loaded bool

	// defaultModels edits the global fallback selection used when no named set
	// is active. A specific set uses setID/setMembers instead.
	defaultModels bool
	setID         int64
	setName       string
	setMembers    map[int64]bool
	setOrder      []int64
	// wantSetID is the set the operator currently intends to edit: set when a
	// set editor is opened, cleared on leaveEditing. A late setEditLoadedMsg for
	// any other id is stale and must not reopen an editor the user has left.
	wantSetID int64

	// Model browsing starts with providers that can route now. The provider
	// picker can expand that to all or an explicit multi-selection. Search
	// deliberately spans the full catalogue regardless of this scope.
	providerMode     providerScope
	selectedProvider map[string]bool
	accessFilter     string
	intelTier        int
	busy             string
}

func (m *routingModel) Init() tea.Cmd { return nil }

func (m *routingModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.list.width = w
	if m.list.place == "" {
		m.list.place = "Search models"
	}
}

// editing reports whether the screen is editing the default model selection or
// one specific saved set.
func (m *routingModel) editing() bool { return m.defaultModels || m.setID != 0 }

func (m *routingModel) load() tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var out routingLoadedMsg
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
			out.err = fmt.Errorf("set templates unavailable: %w", err)
			return out
		}
		out.presets = env.Presets
		var models []ModelRow
		if err := c.Get(ctx, "/api/models", &models); err != nil {
			out.err = fmt.Errorf("model catalogue unavailable: %w", err)
			return out
		}
		out.models = models
		return out
	}
}

func (m *routingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case routingLoadedMsg:
		if !msg.complete() && m.loaded {
			// Reject a partial or failed refresh: keep the good snapshot rather
			// than blanking the editor, and surface a transient error as a toast.
			m.busy = ""
			if msg.err != nil {
				return m, m.app.showToast("bad", msg.err.Error())
			}
			return m, nil
		}
		m.data = msg
		m.loaded = true
		m.busy = ""
		m.buildRows()
		return m, nil

	case setEditLoadedMsg:
		// A late set-editor result must not reopen an editor the operator left
		// (Escape) or replace the one they moved to. Only the set they currently
		// intend to edit may consume it; a stale result is dropped untouched so
		// it cannot clobber busy or reopen editing.
		if msg.id != m.wantSetID {
			return m, nil
		}
		m.busy = ""
		if msg.err != nil {
			return m, m.app.showToast("bad", msg.err.Error())
		}
		m.list.filter = ""
		m.list.searching = false
		fresh := m.setID != msg.id
		m.setID = msg.id
		m.setName = msg.name
		m.defaultModels = false
		m.setOrder = msg.order
		m.setMembers = make(map[int64]bool, len(msg.order))
		for _, id := range msg.order {
			m.setMembers[id] = true
		}
		for i := range m.data.profiles {
			if m.data.profiles[i].ID == msg.id {
				m.data.profiles[i].ModelCount = len(msg.order)
				break
			}
		}
		if fresh {
			m.list.cursor, m.list.offset = 0, 0
		}
		m.buildRows()
		return m, nil

	case providerFilterMsg:
		m.providerMode = msg.mode
		m.selectedProvider = msg.selected
		m.list.cursor, m.list.offset = 0, 0
		m.buildRows()
		return m, nil

	case routingPickedMsg:
		switch msg.kind {
		case routingPickerStrategy:
			return m, m.applyStrategy(msg.id)
		case routingPickerTemplate:
			if msg.id == "__blank__" {
				return m, m.openCreateSet()
			}
			return m, m.applyPreset(msg.id)
		}

	case tea.KeyPressMsg:
		if m.list.typeFilter(msg) {
			m.buildRows()
			return m, nil
		}
		switch msg.String() {
		case "esc":
			// Leave the editor, and also abandon a set-open still loading so its
			// in-flight result cannot reopen an editor the operator dismissed.
			if m.editing() || m.wantSetID != 0 {
				m.leaveEditing()
			}
			return m, nil
		case "up", "k":
			m.list.move(-1)
			return m, nil
		case "down", "j":
			m.list.move(1)
			return m, nil
		case "pgup":
			m.list.page(-1)
			return m, nil
		case "pgdn":
			m.list.page(1)
			return m, nil
		case "/":
			m.list.startSearch()
			m.buildRows()
			return m, nil
		}
		if m.busy != "" {
			return m, nil
		}
		if m.editing() {
			switch msg.String() {
			case "p":
				return m, m.openProviderPicker()
			case "t":
				m.cycleAccess()
				return m, nil
			case "i":
				m.cycleIntel()
				return m, nil
			}
		} else {
			switch msg.String() {
			case "n":
				return m, m.openTemplatePicker()
			case "s":
				return m, m.openStrategyPicker()
			}
		}

		r := m.list.selected()
		if r == nil {
			return m, nil
		}
		c := m.app.Client
		if !m.editing() {
			return m.handlePolicyKey(msg.String(), r, c)
		}
		model, ok := r.key.(ModelRow)
		if !ok {
			return m, nil
		}
		if m.setID != 0 {
			return m.handleSetEditKey(msg.String(), model, c)
		}
		return m.handleDefaultModelKey(msg.String(), model, c)
	}
	return m, nil
}

// enterDefaultModels opens the fallback model selection used when no named set
// is active.
func (m *routingModel) enterDefaultModels() {
	m.defaultModels = true
	m.setID = 0
	m.list.filter = ""
	m.list.searching = false
	m.list.cursor, m.list.offset = 0, 0
	m.buildRows()
}

// leaveEditing returns to the policy list from either editing mode.
func (m *routingModel) leaveEditing() {
	m.defaultModels = false
	m.setID = 0
	m.setName = ""
	m.setOrder = nil
	m.setMembers = nil
	// Drop the edit intent and any in-flight set spinner so a pending set load
	// or membership write cannot revive the editor after the operator leaves.
	m.wantSetID = 0
	m.busy = ""
	m.list.filter = ""
	m.list.searching = false
	m.list.cursor, m.list.offset = 0, 0
	m.buildRows()
}

func (m *routingModel) handlePolicyKey(key string, r *row, c *Client) (tea.Model, tea.Cmd) {
	set, ok := r.key.(setChoice)
	if !ok {
		return m, nil
	}
	switch key {
	case "enter":
		if set.defaultModels {
			if !set.active {
				return m, m.app.showToast("tip", "Activate Default models with Space before editing them.")
			}
			m.enterDefaultModels()
			return m, nil
		}
		return m, m.openSetEdit(set.id, set.name)
	case "space":
		return m, m.activateSet(c, set)
	case "r":
		if !set.defaultModels {
			return m, m.openRename(set.id, set.name)
		}
	case "d":
		if !set.defaultModels {
			return m, m.confirmDeleteSet(set.id, set.name)
		}
	}
	return m, nil
}

func (m *routingModel) activateSet(c *Client, set setChoice) tea.Cmd {
	m.busy = "activating model set…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var profileID any = set.id
		text := "set " + set.name + " is now active"
		if set.defaultModels {
			profileID = nil
			text = "default models are now active"
		}
		var out map[string]any
		if err := c.Post(ctx, "/api/profiles/active", map[string]any{"profileId": profileID}, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		return doneMsg{Tab: TabRouting, Text: text}
	}
}

func (m *routingModel) applyStrategy(id string) tea.Cmd {
	name := id
	for _, strategy := range strategyChoices {
		if strategy.id == id {
			name = strategy.name
			break
		}
	}
	m.busy = "applying routing strategy…"
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out map[string]any
		if err := c.Put(ctx, "/api/fallback/routing", map[string]any{"strategy": id}, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		return doneMsg{Tab: TabRouting, Text: "routing strategy: " + name}
	}
}

func (m *routingModel) applyPreset(id string) tea.Cmd {
	name := id
	for _, preset := range m.data.presets {
		if preset.ID == id {
			name = preset.Name
			break
		}
	}
	m.busy = "building model set…"
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var out struct {
			ProfileID int64 `json:"profileId"`
		}
		if err := c.Post(ctx, fmt.Sprintf("/api/profiles/presets/%s", id), map[string]any{"activate": true}, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		return doneMsg{Tab: TabRouting, Text: name + " model set built and active"}
	}
}

func (m *routingModel) openStrategyPicker() tea.Cmd {
	choices := make([]routingPickerChoice, 0, len(strategyChoices))
	for _, strategy := range strategyChoices {
		choices = append(choices, routingPickerChoice{
			id: strategy.id, title: strategy.name, detail: strategy.blurb,
			selected: strategy.id == m.data.routing.Strategy,
		})
	}
	overlay := newRoutingPicker(
		"Routing strategy",
		"Choose how Prowl orders the models in the active set.",
		routingPickerStrategy,
		choices,
	)
	m.app.overlay = overlay
	return overlay.Init()
}

func (m *routingModel) openTemplatePicker() tea.Cmd {
	choices := []routingPickerChoice{{
		id: "__blank__", title: "Blank set", detail: "start with no selected models", group: "Start from scratch",
	}}
	presets := append([]ChainPreset(nil), m.data.presets...)
	sort.SliceStable(presets, func(i, j int) bool {
		order := map[string]int{"task": 0, "catalogue": 1}
		gi, iok := order[presets[i].Group]
		gj, jok := order[presets[j].Group]
		if !iok {
			gi = 2
		}
		if !jok {
			gj = 2
		}
		if gi != gj {
			return gi < gj
		}
		return strings.ToLower(presets[i].Name) < strings.ToLower(presets[j].Name)
	})
	for _, preset := range presets {
		group := "More templates"
		switch preset.Group {
		case "task":
			group = "For your work"
		case "catalogue":
			group = "By access"
		}
		detail := fmt.Sprintf("%d matching models · %s", preset.Models, preset.Description)
		choices = append(choices, routingPickerChoice{
			id: preset.ID, title: preset.Name, detail: detail, group: group,
			disabled: preset.Models == 0,
		})
	}
	overlay := newRoutingPicker(
		"New model set",
		"Start blank or choose a compact, task-shaped template.",
		routingPickerTemplate,
		choices,
	)
	m.app.overlay = overlay
	return overlay.Init()
}

func (m *routingModel) openProviderPicker() tea.Cmd {
	overlay := newProviderFilterOverlay(m.data.models, m.providerMode, m.selectedProvider)
	m.app.overlay = overlay
	return overlay.Init()
}

// handleSetEditKey toggles membership of the set being edited or the model's
// catalogue-wide enable state. Space is the one consistent selection gesture:
// it rewrites ordered membership atomically, then re-reads server truth.
func (m *routingModel) handleSetEditKey(action string, mo ModelRow, c *Client) (tea.Model, tea.Cmd) {
	switch action {
	case "space":
		order := toggledOrder(m.setOrder, mo.ID)
		setID, setName := m.setID, m.setName
		m.busy = "updating set…"
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	case "e":
		next := !mo.Enabled
		return m, m.patchModelCmd(c, mo.ID, map[string]any{"enabled": next},
			fmt.Sprintf("%s %s", mo.ModelID, enabledWord(next)))
	}
	return m, nil
}

// handleDefaultModelKey edits the fallback selection used when no named set is
// active. Space selects a model; e controls its catalogue-wide enabled state.
func (m *routingModel) handleDefaultModelKey(action string, mo ModelRow, c *Client) (tea.Model, tea.Cmd) {
	if action != "space" && action != "e" {
		return m, nil
	}
	patch := map[string]any{}
	text := ""
	switch action {
	case "space":
		next := !mo.FallbackEnabled
		patch["fallbackEnabled"] = next
		if next && !mo.Enabled {
			patch["enabled"] = true
		}
		if next {
			text = mo.ModelID + " selected for default routing"
		} else {
			text = mo.ModelID + " removed from default routing"
		}
	case "e":
		next := !mo.Enabled
		patch["enabled"] = next
		text = fmt.Sprintf("%s %s", mo.ModelID, enabledWord(next))
	}
	return m, m.patchModelCmd(c, mo.ID, patch, text)
}

func (m *routingModel) patchModelCmd(c *Client, id int64, patch map[string]any, text string) tea.Cmd {
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

func (m *routingModel) openSetEdit(id int64, name string) tea.Cmd {
	c := m.app.Client
	m.busy = "loading set…"
	// Record the set the operator is opening so its load result is recognised as
	// current - and any earlier set's late result is rejected as stale.
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

func (m *routingModel) openCreateSet() tea.Cmd {
	f := newForm("Create a named set", "Name")
	c := m.app.Client
	f.submit = func(values []string) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out Profile
		if err := c.Post(ctx, "/api/profiles", map[string]any{"name": values[0], "empty": true}, &out); err != nil {
			return errMsg{Screen: "routing", Err: err}
		}
		return doneMsg{Tab: TabRouting, Text: "model set created - Enter to choose models, Space to activate"}
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

// ── filter cycling ──────────────────────────────────────────────────────────

func (m *routingModel) cycleAccess() {
	seq := []string{"", "free", "paid", "subscription"}
	m.accessFilter = seq[(indexOfString(seq, m.accessFilter)+1)%len(seq)]
	m.list.cursor, m.list.offset = 0, 0
	m.buildRows()
}

func (m *routingModel) cycleIntel() {
	m.intelTier = (m.intelTier + 1) % 4
	m.list.cursor, m.list.offset = 0, 0
	m.buildRows()
}

func indexOfString(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return 0
}

// intelThreshold returns the highest (worst) intelligence_rank admitted by a
// top fraction of the live catalogue. intelligence_rank is an ordinal where a
// smaller value is more capable. A quantile, rather than a magic rank, keeps
// "top 10%" and friends truthful as the catalogue changes.
func intelThreshold(models []ModelRow, fraction float64) int {
	if len(models) == 0 {
		return 0
	}
	ranks := make([]int, 0, len(models))
	for _, mo := range models {
		ranks = append(ranks, mo.IntelligenceRank)
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

type strategyChoice struct{ id, name, blurb string }
type setChoice struct {
	id            int64
	name          string
	active        bool
	defaultModels bool
}

func (m *routingModel) buildRows() {
	if m.editing() {
		m.buildModelRows()
		return
	}
	m.buildPolicyRows()
}

func (m *routingModel) buildPolicyRows() {
	m.list.empty = ""
	defaultActive := m.data.activeID == 0
	defaultModels := "default"
	if defaultActive {
		count := 0
		for _, model := range m.data.models {
			if model.FallbackEnabled && model.Enabled {
				count++
			}
		}
		defaultModels = fmt.Sprintf("%d models", count)
	}
	rows := []row{{
		cells: []string{"Default models", defaultModels, activeState(defaultActive)},
		styles: []func(string) string{
			func(value string) string {
				if defaultActive {
					return stGood.Render(value)
				}
				return stHead.Render(value)
			},
			func(value string) string { return stSubtle.Render(value) },
			func(value string) string {
				if defaultActive {
					return stGood.Render(value)
				}
				return stFaint.Render(value)
			},
		},
		key: setChoice{name: "Default models", active: defaultActive, defaultModels: true},
	}}

	profiles := append([]Profile(nil), m.data.profiles...)
	sort.SliceStable(profiles, func(i, j int) bool {
		ai, aj := profiles[i].ID == m.data.activeID, profiles[j].ID == m.data.activeID
		if ai != aj {
			return ai
		}
		return strings.ToLower(profiles[i].Name) < strings.ToLower(profiles[j].Name)
	})
	for _, profile := range profiles {
		active := profile.ID == m.data.activeID
		state := activeState(active)
		if profile.ModelCount == 0 && !active {
			state = "empty"
		}
		rows = append(rows, row{
			cells: []string{profile.Name, fmt.Sprintf("%d models", profile.ModelCount), state},
			styles: []func(string) string{
				func(value string) string {
					if active {
						return stGood.Render(value)
					}
					return stHead.Render(value)
				},
				func(value string) string { return stSubtle.Render(value) },
				func(value string) string {
					switch value {
					case "active":
						return stGood.Render(value)
					case "empty":
						return stWarn.Render(value)
					default:
						return stFaint.Render(value)
					}
				},
			},
			key: setChoice{id: profile.ID, name: profile.Name, active: active},
		})
	}
	m.list.setRows(rows)
}

func activeState(active bool) string {
	if active {
		return "active"
	}
	return "saved"
}

func (m *routingModel) buildModelRows() {
	elite := intelThreshold(m.data.models, 0.10)
	frontier := intelThreshold(m.data.models, 0.25)
	strong := intelThreshold(m.data.models, 0.50)
	activeProviders := map[string]bool{}
	for _, model := range m.data.models {
		if model.Available {
			activeProviders[model.Platform] = true
		}
	}

	searching := m.list.isSearching() || strings.TrimSpace(m.list.filter) != ""
	rows := make([]row, 0, len(m.data.models))
	for _, model := range m.data.models {
		if !searching {
			switch m.providerMode {
			case providerScopeActive:
				if !activeProviders[model.Platform] {
					continue
				}
			case providerScopeCustom:
				if !m.selectedProvider[model.Platform] {
					continue
				}
			}
			if m.accessFilter != "" && model.Access != m.accessFilter {
				continue
			}
			switch m.intelTier {
			case 1:
				if model.IntelligenceRank > elite {
					continue
				}
			case 2:
				if model.IntelligenceRank > frontier {
					continue
				}
			case 3:
				if model.IntelligenceRank > strong {
					continue
				}
			}
		}

		member := model.FallbackEnabled
		if m.setID != 0 {
			member = m.setMembers[model.ID]
		}
		marker := "○ "
		membership := "not selected"
		if member {
			marker = "● "
			membership = "selected"
		}
		state := "enabled"
		if !model.Enabled {
			state = "disabled"
		}
		credential := "missing"
		switch {
		case model.Access == "subscription" && model.KeyCount > 0:
			credential = "account"
		case model.Keyless:
			credential = "keyless"
		case model.KeyCount == 1:
			credential = "1 key"
		case model.KeyCount > 1:
			credential = fmt.Sprintf("%d keys", model.KeyCount)
		}
		rows = append(rows, row{
			cells: []string{
				marker + model.DisplayName,
				providerDisplayName(model.Platform),
				model.Access,
				state,
				membership,
				credential,
			},
			styles: []func(string) string{
				func(value string) string {
					if member {
						return stGood.Render(value)
					}
					return stHead.Render(value)
				},
				func(value string) string { return stSubtle.Render(value) },
				accessCell,
				func(value string) string { return pill(value) },
				func(value string) string {
					if member {
						return stGood.Render(value)
					}
					return stFaint.Render(value)
				},
				func(value string) string {
					if value == "missing" {
						return stBad.Render(value)
					}
					return stSubtle.Render(value)
				},
			},
			key: model,
			dim: !model.Available,
		})
	}
	m.list.empty = ""
	if len(rows) == 0 && !searching {
		switch m.providerMode {
		case providerScopeActive:
			m.list.empty = "No active providers yet. Press p to browse all, or connect an account."
		case providerScopeCustom:
			m.list.empty = "The selected providers have no models. Press p to change the selection."
		default:
			m.list.empty = "No models are available yet. Connect a provider or account first."
		}
	}
	m.list.setRows(rows)
}

// accessCell renders the free/paid/subscription tier with colour as decoration.
func accessCell(access string) string {
	switch access {
	case "free":
		return stGood.Render("free")
	case "paid":
		return stWarn.Render("paid")
	case "subscription":
		return stHead.Render("subscription")
	default:
		return stFaint.Render("-")
	}
}

var strategyChoices = []strategyChoice{
	{"balanced", "Balanced", "live reliability, latency and catalogue capability"},
	{"smartest", "Smart routing", "classifies each prompt, then combines capability and live health"},
	{"fastest", "Fastest", "observed latency first"},
	{"reliable", "Most reliable", "observed success rate first"},
	{"priority", "Priority", "preserves your selected model order without automatic re-ranking"},
}

func (m *routingModel) View() tea.View {
	if !m.loaded {
		return tea.NewView(roundedPanel(
			"Models",
			m.app.spinner.View()+" "+stSubtle.Render("Reading model sets and routing policy"),
			m.width,
		))
	}
	if m.data.err != nil {
		return tea.NewView(roundedPanel(
			"Model state unavailable",
			stBad.Render("● "+m.data.err.Error()),
			m.width,
		))
	}

	var body strings.Builder
	if m.editing() {
		m.list.setHeaders("Model", "Provider", "Access", "Catalogue", "Selected", "Credential")
		body.WriteString(metricStrip(m.editMetrics(), m.width) + "\n")
		body.WriteString(roundedPanel("Choose models", m.setEditBanner(), m.width) + "\n")
		body.WriteString(roundedPanel("View", m.filterChips(), m.width) + "\n")
	} else {
		m.list.setHeaders("Model set", "Models", "State")
		body.WriteString(metricStrip(m.policyMetrics(), m.width) + "\n")
		body.WriteString(roundedPanel("Routing setup", m.policyOverview(), m.width) + "\n")
	}
	if m.busy != "" {
		body.WriteString(roundedPanel("Applying", stWarn.Render("● "+m.busy), m.width) + "\n")
	}
	localY := strings.Count(body.String(), "\n")
	m.list.height = max(m.height-localY, 5)
	m.list.setOrigin(m.app.bodyX, m.app.bodyY+localY)
	body.WriteString(m.list.render())
	return tea.NewView(body.String())
}

func (m *routingModel) policyMetrics() []metric {
	activeName, activeModels := m.activeSetSummary()
	readyProviders, readyModels := m.readyCatalogueStats()
	return []metric{
		{"Active set", activeName, fmt.Sprintf("%d selected models", activeModels)},
		{"Strategy", m.strategyName(), "s to change"},
		{"Ready now", fmt.Sprintf("%d models", readyModels), fmt.Sprintf("%d connected providers", readyProviders)},
		{"Saved sets", fmt.Sprintf("%d", len(m.data.profiles)), "n to create"},
	}
}

func (m *routingModel) activeSetSummary() (string, int) {
	if m.data.activeID != 0 {
		for _, profile := range m.data.profiles {
			if profile.ID == m.data.activeID {
				return profile.Name, profile.ModelCount
			}
		}
	}
	count := 0
	for _, model := range m.data.models {
		if model.Enabled && model.FallbackEnabled {
			count++
		}
	}
	return "Default models", count
}

func (m *routingModel) readyCatalogueStats() (providers, models int) {
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

func (m *routingModel) strategyName() string {
	for _, strategy := range strategyChoices {
		if strategy.id == m.data.routing.Strategy {
			return strategy.name
		}
	}
	return m.data.routing.Strategy
}

func (m *routingModel) strategyBlurb() string {
	for _, strategy := range strategyChoices {
		if strategy.id == m.data.routing.Strategy {
			return strategy.blurb
		}
	}
	return "orders the selected models for each request"
}

func (m *routingModel) policyOverview() string {
	task, catalogue := 0, 0
	for _, preset := range m.data.presets {
		switch preset.Group {
		case "task":
			task++
		case "catalogue":
			catalogue++
		}
	}
	line1 := stHead.Render(m.strategyName()) + stFaint.Render("  ·  ") +
		stSubtle.Render(m.strategyBlurb()) + "  " + stKey.Render("s change")
	line2 := stHead.Render("Templates") + stFaint.Render("  ·  ") +
		stSubtle.Render(fmt.Sprintf("%d for your work · %d by access", task, catalogue)) +
		"  " + stKey.Render("n browse")
	return line1 + "\n" + line2 + "\n" + m.benchmarkSummary()
}

func (m *routingModel) editMetrics() []metric {
	visible := len(m.list.filtered())
	selected := 0
	name := "Default models"
	activity := "active fallback"
	if m.setID != 0 {
		name = m.setName
		activity = m.setActivityLabel()
		selected = len(m.setMembers)
	} else {
		for _, model := range m.data.models {
			if model.FallbackEnabled && model.Enabled {
				selected++
			}
		}
	}
	return []metric{
		{"Model set", name, activity},
		{"Selected", fmt.Sprintf("%d", selected), "Space toggles"},
		{"Visible", fmt.Sprintf("%d", visible), m.providerScopeLabel()},
		{"Catalogue", fmt.Sprintf("%d", len(m.data.models)), "search spans all"},
	}
}

func (m *routingModel) setActivityLabel() string {
	if m.setID == m.data.activeID {
		return "active set"
	}
	return "not active"
}

func (m *routingModel) setEditBanner() string {
	name := m.setName
	state := stFaint.Render("Saved but inactive; edits do not change current routing.")
	if m.setID == 0 {
		name = "Default models"
		state = stGood.Render("Active whenever no named model set is selected.")
	} else if m.setID == m.data.activeID {
		state = stGood.Render("Active now; model changes take effect immediately.")
	}
	return fmt.Sprintf(
		"Editing %s. %s selects a model; %s chooses visible providers; %s searches every model. %s returns.\n%s",
		stHead.Render(name),
		stKey.Render("Space"),
		stKey.Render("p"),
		stKey.Render("/"),
		stKey.Render("Esc"),
		state,
	)
}

func (m *routingModel) filterChips() string {
	accessLabel := m.accessFilter
	if accessLabel == "" {
		accessLabel = "all"
	}
	intelLabels := []string{"all", "elite · top 10%", "frontier · top 25%", "strong · top 50%"}
	provider := actionChip("p", "Providers: "+m.providerScopeLabel(), m.providerMode != providerScopeActive, false, false)
	if m.list.isSearching() || strings.TrimSpace(m.list.filter) != "" {
		provider = actionChip("/", "Search: all providers", true, false, false)
	}
	return provider + " " +
		actionChip("t", "Access: "+accessLabel, m.accessFilter != "", false, false) + " " +
		actionChip("i", "Intelligence: "+intelLabels[m.intelTier], m.intelTier != 0, false, false)
}

func (m *routingModel) providerScopeLabel() string {
	switch m.providerMode {
	case providerScopeAll:
		return "all"
	case providerScopeCustom:
		return fmt.Sprintf("%d selected", len(m.selectedProvider))
	default:
		providers, _ := m.readyCatalogueStats()
		return fmt.Sprintf("%d active", providers)
	}
}

func (m *routingModel) benchmarkSummary() string {
	status := m.data.routing.Benchmark
	switch {
	case status.Matched > 0 && status.Status == "error":
		return stWarn.Render(fmt.Sprintf("Benchmark refresh failed; using %d cached Artificial Analysis matches. %s", status.Matched, status.Error))
	case status.Matched > 0 && status.LastSuccess != nil:
		return stFaint.Render(fmt.Sprintf(
			"Smart routing: %d/%d current-model scores, refreshed %s ago. Source: Artificial Analysis · artificialanalysis.ai",
			status.Matched, status.Available, relTime(status.LastSuccess.Format(time.RFC3339))))
	case !status.Configured:
		return stFaint.Render("Smart routing currently uses catalogue priors. Set ARTIFICIAL_ANALYSIS_API_KEY before launch for a daily independent benchmark refresh.")
	default:
		return stFaint.Render("Artificial Analysis refresh is scheduled; catalogue priors remain active until matching scores arrive.")
	}
}

func (m *routingModel) actions() []action {
	if !m.editing() {
		actions := make([]action, 0, 8)
		if selected := m.list.selected(); selected != nil {
			if set, ok := selected.key.(setChoice); ok {
				switch {
				case set.defaultModels && !set.active:
					actions = append(actions, action{Key: "space", Label: "Activate defaults", Primary: true})
				default:
					actions = append(actions, action{Key: "enter", Label: "Edit models", Primary: true})
					if !set.active {
						actions = append(actions, action{Key: "space", Label: "Activate"})
					}
				}
				if !set.defaultModels {
					actions = append(actions,
						action{Key: "r", Label: "Rename"},
						action{Key: "d", Label: "Delete", Dangerous: true},
					)
				}
			}
		}
		actions = append(actions,
			action{Key: "n", Label: "New from template"},
			action{Key: "s", Label: "Routing strategy"},
			action{Key: "/", Label: "Search sets"},
		)
		return actions
	}

	membershipLabel := "Select model"
	stateLabel := "Disable model"
	hasModel := false
	if selected := m.list.selected(); selected != nil {
		if model, ok := selected.key.(ModelRow); ok {
			hasModel = true
			member := model.FallbackEnabled
			if m.setID != 0 {
				member = m.setMembers[model.ID]
			}
			if member {
				membershipLabel = "Deselect model"
			}
			if !model.Enabled {
				stateLabel = "Enable model"
			}
		}
	}
	actions := make([]action, 0, 8)
	if hasModel {
		actions = append(actions,
			action{Key: "space", Label: membershipLabel, Primary: true},
			action{Key: "e", Label: stateLabel},
		)
	}
	actions = append(actions,
		action{Key: "p", Label: "Providers"},
		action{Key: "/", Label: "Search all"},
		action{Key: "esc", Label: "Back"},
		action{Key: "t", Label: "Access"},
		action{Key: "i", Label: "Intelligence"},
	)
	return actions
}

func (m *routingModel) searching() bool { return m.list.isSearching() }
