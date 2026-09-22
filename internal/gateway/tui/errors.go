package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Errors is the debugging log: every failed request the gateway recorded, with
// its provider, model, error kind and the upstream's own message. The gateway
// routes and forwards faithfully, but when a provider refuses, that refusal
// belongs where an operator can read it after the fact instead of tailing the
// daemon log.

type errorsLoadedMsg struct {
	rows []UsageRequestRow
	err  error
}

type errorsModel struct {
	app      *App
	width    int
	height   int
	list     list
	data     errorsLoadedMsg
	loaded   bool
	loadedAt time.Time
}

func (m *errorsModel) Init() tea.Cmd { return nil }

func (m *errorsModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.list.width = w
	m.list.height = max(h-4, 4)
}

func (m *errorsModel) load() tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out struct {
			Requests []UsageRequestRow `json:"requests"`
		}
		if err := c.Get(ctx, "/api/usage/requests?failures=1&limit=100", &out); err != nil {
			return errorsLoadedMsg{err: err}
		}
		return errorsLoadedMsg{rows: out.Requests}
	}
}

func (m *errorsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errorsLoadedMsg:
		m.data = msg
		m.loaded = true
		m.loadedAt = time.Now()
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
			if sel := m.list.selected(); sel != nil {
				r := sel.key.(UsageRequestRow)
				return m, tea.Batch(
					tea.SetClipboard(errorClipboard(r)),
					func() tea.Msg { return newToast("ok", "Error detail copied") },
				)
			}
		case "enter":
			if sel := m.list.selected(); sel != nil {
				r := sel.key.(UsageRequestRow)
				m.app.overlay = newDetailOverlay(
					"Failure · "+r.Platform+"/"+r.Model,
					relTime(r.CreatedAt),
					errorDetail(r),
					nil,
				)
				return m, m.app.overlay.Init()
			}
		}
	}
	return m, nil
}

func (m *errorsModel) buildRows() {
	rows := make([]row, 0, len(m.data.rows))
	for _, r := range m.data.rows {
		kind := r.ErrorKind
		if kind == "" {
			kind = r.Outcome
		}
		message := errorFirstLine(r.Error)
		if message == "" {
			message = "(no message)"
		}
		rows = append(rows, row{
			cells: []string{
				relTime(r.CreatedAt),
				r.Platform + "/" + r.Model,
				kind,
				message,
			},
			styles: []func(string) string{
				func(v string) string { return stFaint.Render(v) },
				func(v string) string { return stSubtle.Render(v) },
				func(v string) string { return stBad.Render(v) },
				func(v string) string { return stSubtle.Render(v) },
			},
			key: r,
		})
	}
	m.list.setRows(rows)
}

func (m *errorsModel) View() tea.View {
	if !m.loaded {
		return tea.NewView(roundedPanel("Errors", m.app.spinner.View()+" "+stSubtle.Render("Reading the failure log"), m.width))
	}
	if m.data.err != nil {
		return tea.NewView(roundedPanel("Errors unavailable", stBad.Render("● "+m.data.err.Error()), m.width))
	}
	summary := roundedPanel(
		"Failures",
		fmt.Sprintf("%s recent failed request(s)\n%s",
			stHead.Render(fmt.Sprintf("%d", len(m.data.rows))),
			stSubtle.Render("enter reads the upstream's own message; c copies it. An empty list is good.")),
		m.width,
	)
	m.list.empty = "No failures recorded. Nothing to debug."
	localY := strings.Count(summary, "\n") + 1
	m.list.height = max(m.height-localY, 4)
	m.list.setOrigin(m.app.bodyX, m.app.bodyY+localY)
	return tea.NewView(summary + "\n" + m.list.render())
}

func (m *errorsModel) actions() []action {
	return []action{
		{Key: "enter", Label: "Read detail", Primary: true},
		{Key: "c", Label: "Copy error"},
		{Key: "/", Label: "Search"},
	}
}

func (m *errorsModel) searching() bool { return m.list.isSearching() }

// errorFirstLine is the one-line preview a row shows; the full message is on the
// detail overlay.
func errorFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// errorDetail is the full failure record for the overlay: the routing context
// that produced it, then the upstream's verbatim message.
func errorDetail(r UsageRequestRow) string {
	var b strings.Builder
	b.WriteString(stKey.Render("model") + "  " + r.Platform + "/" + r.Model + "\n")
	if r.ErrorKind != "" {
		b.WriteString(stKey.Render("kind") + "   " + r.ErrorKind + "\n")
	}
	if r.Status != 0 {
		b.WriteString(stKey.Render("status") + " " + fmt.Sprintf("%d", r.Status) + "\n")
	}
	b.WriteString(stKey.Render("route") + "  " + orDashValue(r.Class) + " · " + orDashValue(r.Effort))
	if r.RoutedFrom != "" {
		b.WriteString(" · from " + r.RoutedFrom)
	}
	b.WriteString("\n" + stKey.Render("tries") + "  " + fmt.Sprintf("%d attempt(s) · %dms", r.Attempts, r.LatencyMs) + "\n\n")
	if r.Error != "" {
		b.WriteString(stBad.Render(r.Error))
	} else {
		b.WriteString(stFaint.Render("The upstream reported no message; the outcome was " + r.Outcome + "."))
	}
	return b.String()
}

func errorClipboard(r UsageRequestRow) string {
	return r.Platform + "/" + r.Model + " [" + r.ErrorKind + "]\n" + r.Error
}

func orDashValue(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
