package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/neur0map/prowl/internal/daemon"
	"github.com/neur0map/prowl/internal/embed"
	"github.com/neur0map/prowl/internal/store"
)

type tabID int

const (
	tabStats tabID = iota
	tabSearch
	tabDaemon
)

const numTabs = 3

var tabNames = []string{"Stats", "Search", "Daemon"}

// daemonLogMsg carries a log line tick.
type daemonLogMsg struct{}

// DashboardModel is the main 4-tab dashboard.
type DashboardModel struct {
	activeTab tabID
	dir       string
	store     *store.Store
	embedder  *embed.Embedder
	daemon    *daemon.Daemon
	dmnRunning bool
	width     int
	height    int
	quitting  bool

	// Tab models
	search SearchModel

	// Stats cache
	statsFiles      int
	statsSymbols    int
	statsEdges      int
	statsEmbeddings int
	statsCommunities []store.CommunityRow
	statsLangs      map[string]int
	statsLastIndex  time.Time

	// Daemon log
	dmnLogBuf *ringBuffer
	dmnLogW   *ringBufferWriter
}

// ringBuffer is a simple fixed-size ring buffer for log lines.
type ringBuffer struct {
	lines []string
	max   int
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{max: max}
}

func (r *ringBuffer) Add(line string) {
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *ringBuffer) Lines() []string {
	return r.lines
}

// ringBufferWriter implements io.Writer, splitting on newlines.
type ringBufferWriter struct {
	buf    *ringBuffer
	partial string
}

func newRingBufferWriter(buf *ringBuffer) *ringBufferWriter {
	return &ringBufferWriter{buf: buf}
}

func (w *ringBufferWriter) Write(p []byte) (n int, err error) {
	s := w.partial + string(p)
	lines := strings.Split(s, "\n")
	// Last element might be partial (no trailing newline)
	w.partial = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		if line != "" {
			w.buf.Add(line)
		}
	}
	return len(p), nil
}

// NewDashboardModel creates a dashboard for an indexed project.
func NewDashboardModel(dir string, st *store.Store, emb *embed.Embedder) DashboardModel {
	logBuf := newRingBuffer(50)
	logW := newRingBufferWriter(logBuf)

	m := DashboardModel{
		dir:      dir,
		store:    st,
		embedder: emb,
		search:   NewSearchModel(st, emb),
		dmnLogBuf: logBuf,
		dmnLogW:   logW,
	}

	m.refreshStats()

	// Auto-start daemon
	d, err := daemon.New(dir, 1*time.Second)
	if err == nil {
		d.LogWriter = logW
		m.daemon = d
		m.dmnRunning = true
		go d.Run()
	}

	return m
}

func (m *DashboardModel) refreshStats() {
	m.statsFiles, m.statsSymbols, m.statsEdges, _ = m.store.Stats()
	m.statsEmbeddings, _ = m.store.EmbeddingCount()
	m.statsCommunities, _ = m.store.AllCommunities()
	m.statsLangs, _ = m.store.LanguageBreakdown()
	m.statsLastIndex, _ = m.store.LastIndexedTime()
}


func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m DashboardModel) Init() tea.Cmd {
	return tea.Batch(
		m.search.Init(),
		m.tickDaemonLog(),
	)
}

func (m DashboardModel) tickDaemonLog() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return daemonLogMsg{}
	})
}

func (m DashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.search.width = msg.Width

	case tea.KeyMsg:
		k := msg.String()

		// These keys ALWAYS work, even when search input is focused
		switch {
		case k == "ctrl+c":
			m.quitting = true
			if m.daemon != nil && m.dmnRunning {
				m.daemon.Stop()
			}
			return m, tea.Quit
		case k == "tab":
			m.activeTab = (m.activeTab + 1) % numTabs
			if m.activeTab == tabSearch {
				m.search.input.Focus()
			} else {
				m.search.input.Blur()
			}
			return m, nil
		case k == "shift+tab":
			m.activeTab = (m.activeTab + numTabs - 1) % numTabs
			if m.activeTab == tabSearch {
				m.search.input.Focus()
			} else {
				m.search.input.Blur()
			}
			return m, nil
		case k == "esc":
			if m.activeTab == tabSearch {
				m.search.input.Blur()
				m.search.input.SetValue("")
				m.search.results = nil
				m.search.cursor = 0
			}
			return m, nil
		}

		// Keys that only work when search input is NOT focused
		if m.activeTab != tabSearch || !m.search.input.Focused() {
			switch {
			case key.Matches(msg, Keys.Quit):
				m.quitting = true
				if m.daemon != nil && m.dmnRunning {
					m.daemon.Stop()
				}
				return m, tea.Quit
			case key.Matches(msg, Keys.Tab1):
				m.activeTab = tabStats
				m.search.input.Blur()
				return m, nil
			case key.Matches(msg, Keys.Tab2):
				m.activeTab = tabSearch
				m.search.input.Focus()
				return m, nil
			case key.Matches(msg, Keys.Tab3):
				m.activeTab = tabDaemon
				m.search.input.Blur()
				return m, nil
			case key.Matches(msg, Keys.ToggleDmn):
				if m.activeTab == tabDaemon {
					return m, m.toggleDaemon()
				}
			}
		}

	case daemonLogMsg:
		return m, m.tickDaemonLog()

	case searchResultMsg:
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return m, cmd
	}

	// Delegate to active tab
	if m.activeTab == tabSearch {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *DashboardModel) toggleDaemon() tea.Cmd {
	if m.dmnRunning && m.daemon != nil {
		m.daemon.Stop()
		m.daemon = nil
		m.dmnRunning = false
		m.dmnLogBuf.Add("[daemon] stopped")
		return nil
	}

	d, err := daemon.New(m.dir, 1*time.Second)
	if err != nil {
		m.dmnLogBuf.Add("[error] " + err.Error())
		return nil
	}
	d.LogWriter = m.dmnLogW
	m.daemon = d
	m.dmnRunning = true
	m.dmnLogBuf.Add("[daemon] started")

	go d.Run()
	return nil
}

// SetDaemon sets an externally created daemon.
func (m *DashboardModel) SetDaemon(d *daemon.Daemon) {
	d.LogWriter = m.dmnLogW
	m.daemon = d
	m.dmnRunning = true
}

func (m DashboardModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Header
	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(Purple).
		Render("prowl")
	dirLabel := MutedStyle.Render(" " + m.dir)
	b.WriteString("  " + header + dirLabel + "\n\n")

	// Tab bar
	var tabs []string
	for i, name := range tabNames {
		if tabID(i) == m.activeTab {
			tabs = append(tabs, ActiveTabStyle.Render(name))
		} else {
			tabs = append(tabs, InactiveTabStyle.Render(name))
		}
	}
	b.WriteString("  " + strings.Join(tabs, " ") + "\n")
	sepWidth := min(m.width-4, 72)
	if sepWidth < 1 {
		sepWidth = 40
	}
	b.WriteString("  " + lipgloss.NewStyle().Foreground(BorderC).Render(strings.Repeat("─", sepWidth)) + "\n\n")

	// Tab content
	switch m.activeTab {
	case tabStats:
		b.WriteString(m.viewStats())
	case tabSearch:
		b.WriteString(m.search.View())
	case tabDaemon:
		b.WriteString(m.viewDaemon())
	}

	// Help
	b.WriteString("\n" + HelpStyle.Render("  "+HelpText("dashboard")))

	return b.String()
}

func (m DashboardModel) viewStats() string {
	var b strings.Builder

	// Main stats
	stats := []struct{ label, value string }{
		{"Files", fmt.Sprintf("%d", m.statsFiles)},
		{"Symbols", fmt.Sprintf("%d", m.statsSymbols)},
		{"Edges", fmt.Sprintf("%d", m.statsEdges)},
		{"Embeddings", fmt.Sprintf("%d", m.statsEmbeddings)},
		{"Communities", fmt.Sprintf("%d", len(m.statsCommunities))},
	}

	for _, s := range stats {
		b.WriteString("  " + StatLabelStyle.Render(s.label) + StatValueStyle.Render(s.value) + "\n")
	}

	// Last indexed
	if !m.statsLastIndex.IsZero() {
		ago := humanizeDuration(time.Since(m.statsLastIndex))
		b.WriteString("  " + StatLabelStyle.Render("Last indexed") + MutedStyle.Render(ago+" ago") + "\n")
	}

	b.WriteString("\n")

	// Language breakdown (bar chart style)
	if len(m.statsLangs) > 0 {
		b.WriteString("  " + AccentStyle.Render("Languages") + "\n")

		type langCount struct {
			lang  string
			count int
		}
		var sorted []langCount
		for lang, count := range m.statsLangs {
			sorted = append(sorted, langCount{lang, count})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })

		maxCount := sorted[0].count
		barWidth := min(m.width-30, 40)
		if barWidth < 10 {
			barWidth = 10
		}

		for _, lc := range sorted[:min(len(sorted), 8)] {
			filled := (lc.count * barWidth) / maxCount
			if filled < 1 {
				filled = 1
			}
			bar := BarFillStyle.Render(strings.Repeat("█", filled)) +
				BarEmptyStyle.Render(strings.Repeat("░", barWidth-filled))
			label := fmt.Sprintf("%-6s", lc.lang)
			count := fmt.Sprintf("%4d", lc.count)
			b.WriteString("  " + MutedStyle.Render(label) + " " + bar + " " + StatValueStyle.Render(count) + "\n")
		}
	}

	b.WriteString("\n")

	// Top communities
	if len(m.statsCommunities) > 0 {
		b.WriteString("  " + AccentStyle.Render("Communities") + "\n")
		for _, c := range m.statsCommunities[:min(len(m.statsCommunities), 8)] {
			name := fmt.Sprintf("%-20s", c.Name)
			members := fmt.Sprintf("%d files", c.MemberCount)
			b.WriteString("  " + MutedStyle.Render(name) + " " + StatValueStyle.Render(members) + "\n")
		}
	}

	return b.String()
}

func (m DashboardModel) viewDaemon() string {
	var b strings.Builder

	// Status
	status := DotStopped + " " + MutedStyle.Render("Stopped")
	if m.dmnRunning {
		status = DotRunning + " " + SuccessStyle.Render("Running")
	}
	b.WriteString("  " + StatLabelStyle.Render("Status") + status + "\n\n")

	// Log output
	b.WriteString("  " + AccentStyle.Render("Log") + "\n")
	sepW := min(m.width-6, 60)
	if sepW < 1 {
		sepW = 40
	}
	b.WriteString("  " + MutedStyle.Render(strings.Repeat("─", sepW)) + "\n")

	lines := m.dmnLogBuf.Lines()
	if len(lines) == 0 {
		b.WriteString("  " + MutedStyle.Render("Watching for file changes...") + "\n")
	} else {
		maxLines := min(m.height-12, 20)
		if maxLines < 5 {
			maxLines = 5
		}
		if len(lines) > maxLines {
			lines = lines[len(lines)-maxLines:]
		}
		for _, l := range lines {
			b.WriteString("  " + LogLineStyle.Render(l) + "\n")
		}
	}

	return b.String()
}


func humanizeDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
