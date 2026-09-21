package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

type toolkitItem struct {
	name    string
	command string
	detail  string
}

type toolkitModel struct {
	app    *App
	width  int
	height int
	list   list
	items  []toolkitItem
}

func newToolkitModel(app *App) toolkitModel {
	model := toolkitModel{app: app, list: newList("Search features")}
	model.list.setHeaders("Function", "Start here", "What it gives you")
	model.items = []toolkitItem{
		{"Project map", "prowl overview", "languages, subsystems, entrypoints, hotspots"},
		{"Find and read", "prowl search / find / def / peek", "cited code without whole-file reads"},
		{"Trace dependencies", "prowl references / callers / impact", "uses, relationships, and change blast radius"},
		{"Project health", "prowl status / doctor", "index freshness, coverage, and structural risks"},
		{"Current work", "prowl wip / changed / history", "unfinished work and affected code"},
		{"Bounded context", "prowl context search / brief", "small question-shaped context packets"},
		{"Documentation", "prowl docs add / list / refresh", "indexed external docs beside project code"},
		{"Durable knowledge", "prowl knowledge", "reviewed decisions, concepts, and gotchas"},
		{"Large-change review", "prowl review plan", "bounded review units with a coverage gate"},
		{"Agent setup", "prowl init / skills", "project index, rules, MCP, and client skills"},
		{"Model gateway", "prowl gateway", "accounts, credentials, models, routing, and usage"},
		{"Editor services", "prowl serve / lsp", "MCP and language-server integrations"},
		{"Capability finder", "prowl capabilities search", "the right workflow for an intent"},
	}
	model.buildRows()
	return model
}

func (m *toolkitModel) Init() tea.Cmd { return nil }

func (m *toolkitModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.list.width = w
	m.list.height = max(h-5, 4)
}

func (m *toolkitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		if m.list.typeFilter(key) {
			m.buildRows()
			return m, nil
		}
		switch key.String() {
		case "up", "k":
			m.list.move(-1)
		case "down", "j":
			m.list.move(1)
		case "pgup":
			m.list.page(-1)
		case "pgdn":
			m.list.page(1)
		case "home", "g":
			m.list.top()
		case "end", "G":
			m.list.bot()
		case "/":
			m.list.startSearch()
		case "c", "enter":
			if selected := m.list.selected(); selected != nil {
				item := selected.key.(toolkitItem)
				return m, tea.Batch(
					tea.SetClipboard(item.command),
					func() tea.Msg { return newToast("ok", "Command copied") },
				)
			}
		}
	}
	return m, nil
}

func (m *toolkitModel) buildRows() {
	rows := make([]row, 0, len(m.items))
	for _, item := range m.items {
		rows = append(rows, row{
			cells: []string{item.name, item.command, item.detail},
			styles: []func(string) string{
				nil,
				func(value string) string { return stKey.Render(value) },
				func(value string) string { return stSubtle.Render(value) },
			},
			key: item,
		})
	}
	m.list.setRows(rows)
}

func (m *toolkitModel) View() tea.View {
	explainer := roundedPanel(
		"How the pieces fit",
		stHead.Render("Models")+stSubtle.Render(" are the enabled candidates that can answer a request.  ")+
			stHead.Render("Smart routing")+stSubtle.Render(" scores those candidates for each prompt, then tries the strongest healthy option first.")+"\n"+
			stFaint.Render("Code intelligence stays local; the gateway is optional and serves model traffic."),
		m.width,
	)
	m.list.setOrigin(m.app.bodyX, m.app.bodyY+strings.Count(explainer, "\n")+1)
	return tea.NewView(explainer + "\n" + m.list.render())
}

func (m *toolkitModel) actions() []action {
	return []action{{Key: "c", Label: "Copy command", Primary: true}, {Key: "/", Label: "Search"}}
}

func (m *toolkitModel) searching() bool { return m.list.isSearching() }
