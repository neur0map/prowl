package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
	"github.com/neur0map/prowl/internal/workspace"
)

type projectRow struct {
	root    string
	current bool
	status  query.Status
	state   string
	err     error
}

type projectsLoadedMsg struct {
	projects []projectRow
	err      error
}

type projectsModel struct {
	app    *App
	width  int
	height int
	list   list
	data   projectsLoadedMsg
	loaded bool
}

func (m *projectsModel) Init() tea.Cmd { return nil }

func (m *projectsModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.list.width = w
	m.list.height = max(h-4, 4)
}

func (m *projectsModel) load() tea.Cmd {
	return func() tea.Msg {
		entries, err := workspace.List()
		if err != nil {
			return projectsLoadedMsg{err: err}
		}
		current := ""
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			if ws, resolveErr := workspace.Resolve(cwd); resolveErr == nil {
				current = ws.Root
			}
		}
		rows := make([]projectRow, 0, len(entries))
		for _, entry := range entries {
			row := projectRow{root: entry.Root, current: entry.Root == current, state: "not indexed"}
			ws, resolveErr := workspace.Resolve(entry.Root)
			if resolveErr != nil {
				row.err = resolveErr
				rows = append(rows, row)
				continue
			}
			if _, statErr := os.Stat(ws.DB); statErr != nil {
				row.err = statErr
				rows = append(rows, row)
				continue
			}
			db, openErr := store.Open(ws.DB)
			if openErr != nil {
				row.err = openErr
				rows = append(rows, row)
				continue
			}
			row.status, row.err = query.New(db).Status()
			_ = db.Close()
			if row.err == nil {
				row.state = "ready"
				if row.status.Semantic.Remaining > 0 {
					row.state = "semantic building"
				}
			}
			rows = append(rows, row)
		}
		return projectsLoadedMsg{projects: rows}
	}
}

func (m *projectsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case projectsLoadedMsg:
		m.data = msg
		m.loaded = true
		m.buildRows()
		return m, nil
	case tea.KeyPressMsg:
		if m.list.typeFilter(msg) {
			m.buildRows()
			return m, nil
		}
		switch msg.String() {
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
		case "c":
			if selected := m.list.selected(); selected != nil {
				project := selected.key.(projectRow)
				return m, tea.Batch(
					tea.SetClipboard(project.root),
					func() tea.Msg { return newToast("ok", "Project path copied") },
				)
			}
		case "enter":
			if selected := m.list.selected(); selected != nil {
				project := selected.key.(projectRow)
				m.app.overlay = newDetailOverlay(
					"Prowl status · "+filepath.Base(project.root),
					project.root,
					projectStatusReport(project),
					nil,
				)
				return m, m.app.overlay.Init()
			}
		}
	}
	return m, nil
}

func (m *projectsModel) buildRows() {
	rows := make([]row, 0, len(m.data.projects))
	for _, project := range m.data.projects {
		name := filepath.Base(project.root)
		if project.current {
			name += "  current"
		}
		last := "-"
		if project.status.LastIndex != "" {
			last = relTime(project.status.LastIndex)
		}
		saved := "-"
		if project.status.Savings.Queries > 0 {
			saved = "~" + compactTokens(project.status.Savings.SavedTokens)
		}
		rows = append(rows, row{
			cells: []string{
				name,
				project.state,
				fmt.Sprintf("%d", project.status.Counts.Files),
				fmt.Sprintf("%d", project.status.Counts.Symbols),
				fmt.Sprintf("%d", project.status.Counts.Edges),
				saved,
				last,
			},
			styles: []func(string) string{
				nil,
				func(value string) string {
					if value == "ready" {
						return stGood.Render(value)
					}
					return stWarn.Render(value)
				},
				nil, nil, nil,
				func(value string) string {
					if value == "-" {
						return stFaint.Render(value)
					}
					return stGood.Render(value)
				},
				func(value string) string { return stFaint.Render(value) },
			},
			key: project,
		})
	}
	m.list.setRows(rows)
}

func compactTokens(tokens int64) string {
	switch {
	case tokens >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(tokens)/1_000_000_000)
	case tokens >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		return fmt.Sprintf("%.1fK", float64(tokens)/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

func projectStatusReport(project projectRow) string {
	if project.err != nil {
		return section("INDEX") + "\n" +
			stWarn.Render("  "+project.state) + "\n" +
			stBad.Render("  "+project.err.Error())
	}

	status := project.status
	counts := status.Counts
	updated := "never"
	if status.LastIndex != "" {
		updated = status.LastIndex + " · " + relTime(status.LastIndex)
	}
	ai := "off"
	if status.AIEnabled {
		ai = "on"
	}
	semantic := "not enabled"
	if status.AIEnabled {
		switch {
		case status.Semantic.Complete:
			semantic = fmt.Sprintf("complete · %d chunks embedded", status.Semantic.Embedded)
		case status.Semantic.Chunks > 0:
			semantic = fmt.Sprintf("%d/%d chunks embedded · %d remaining",
				status.Semantic.Embedded, status.Semantic.Chunks, status.Semantic.Remaining)
		default:
			semantic = "no embeddable chunks"
		}
	}

	languages := make([]string, 0, len(counts.Langs))
	for language := range counts.Langs {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	for i, language := range languages {
		languages[i] = fmt.Sprintf("%s %d", language, counts.Langs[language])
	}
	languageLine := "none"
	if len(languages) > 0 {
		languageLine = strings.Join(languages, " · ")
	}

	saved := "none yet"
	if status.Savings.Queries > 0 {
		saved = "~" + compactTokens(status.Savings.SavedTokens) + " tokens"
	}
	return section("INDEX") + "\n" +
		fmt.Sprintf("  state       %s\n", project.state) +
		fmt.Sprintf("  files       %d\n", counts.Files) +
		fmt.Sprintf("  symbols     %d\n", counts.Symbols) +
		fmt.Sprintf("  resources   %d\n", counts.Resources) +
		fmt.Sprintf("  chunks      %d\n", counts.Chunks) +
		fmt.Sprintf("  edges       %d · %d resolved · %d external · %d unresolved\n",
			counts.Edges, counts.Resolved, counts.External, counts.Unresolved) +
		fmt.Sprintf("  updated     %s\n", updated) +
		fmt.Sprintf("  AI assist   %s\n", ai) +
		fmt.Sprintf("  semantic    %s\n\n", semantic) +
		section("LANGUAGES") + "\n" +
		"  " + languageLine + "\n\n" +
		section("TOTAL ESTIMATED TOKENS SAVED") + "\n" +
		stGood.Render("  "+saved) + "\n" +
		fmt.Sprintf("  queries     %d\n", status.Savings.Queries) +
		fmt.Sprintf("  returned    ~%s answer tokens\n", compactTokens(status.Savings.AnswerTokens)) +
		stFaint.Render("  Conservative estimate versus reading the cited files.")
}

func (m *projectsModel) View() tea.View {
	if !m.loaded {
		return tea.NewView(roundedPanel("Projects", m.app.spinner.View()+" "+stSubtle.Render("Reading local project indexes"), m.width))
	}
	if m.data.err != nil {
		return tea.NewView(roundedPanel("Projects unavailable", stBad.Render("● "+m.data.err.Error()), m.width))
	}
	ready := 0
	for _, project := range m.data.projects {
		if project.state == "ready" {
			ready++
		}
	}
	summary := roundedPanel(
		"Project indexes",
		fmt.Sprintf("%s registered  %s ready\n%s",
			stHead.Render(fmt.Sprintf("%d", len(m.data.projects))),
			stGood.Render(fmt.Sprintf("%d", ready)),
			stSubtle.Render("Run `prowl init` inside any folder to create or refresh its index.")),
		m.width,
	)
	m.list.empty = "No projects yet. Exit, cd into a project, and run `prowl init`."
	localY := strings.Count(summary, "\n") + 1
	m.list.height = max(m.height-localY, 4)
	m.list.setOrigin(m.app.bodyX, m.app.bodyY+localY)
	return tea.NewView(summary + "\n" + m.list.render())
}

func (m *projectsModel) actions() []action {
	return []action{
		{Key: "enter", Label: "Full status", Primary: true},
		{Key: "c", Label: "Copy path"},
		{Key: "/", Label: "Search"},
	}
}

func (m *projectsModel) searching() bool { return m.list.isSearching() }
