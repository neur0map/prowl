package tui

import (
	"bufio"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/neur0map/prowl/internal/pipeline"
)

const maxLogLines = 14

// Ordered phases — progress advances as each is seen.
var phaseOrder = []struct {
	prefix string
	pct    float64
	label  string
}{
	{"Phase 1", 0.05, "Scanning files"},
	{"Found", 0.10, "Scanning files"},
	{"Phase 2", 0.20, "Extracting symbols"},
	{"Phase 3", 0.35, "Resolving imports"},
	{"Phase 4", 0.45, "Resolving calls"},
	{"Phase 6", 0.55, "Detecting communities"},
	{"Phase 7", 0.65, "Detecting processes"},
	{"Phase 8", 0.72, "Embedding signatures"},
	{"Skipping", 0.85, "Skipping embeddings"},
	{"Embedded", 0.85, "Embedded signatures"},
	{"Persisting", 0.90, "Writing to database"},
	{"Writing", 0.95, "Writing context files"},
	{"Done!", 1.00, "Complete"},
}

// indexLogMsg carries a log line from the pipeline.
type indexLogMsg string

// indexDoneMsg signals indexing is complete with an optional error.
type indexDoneMsg struct{ Err error }

// IndexModel displays pipeline progress with a spinner, progress bar, and log.
type IndexModel struct {
	spinner  spinner.Model
	progress progress.Model
	logLines []string
	phase    string
	pct      float64
	done     bool
	err      error
	dir      string
	width    int
}

// NewIndexModel creates a new indexing progress model.
func NewIndexModel(dir string) IndexModel {
	s := spinner.New()
	s.Spinner = spinner.MiniDot
	s.Style = SpinnerStyle

	p := progress.New(
		progress.WithDefaultGradient(),
		progress.WithWidth(50),
		progress.WithoutPercentage(),
	)

	return IndexModel{spinner: s, progress: p, dir: dir, phase: "Preparing..."}
}

// startIndexBatchCmd pipes progress lines as individual messages via tea.Program.Send.
func startIndexBatchCmd(dir string, p *tea.Program, extraOpts ...pipeline.Option) {
	pr, pw := io.Pipe()

	go func() {
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			p.Send(indexLogMsg(scanner.Text()))
		}
	}()

	go func() {
		opts := append([]pipeline.Option{pipeline.WithProgressWriter(pw)}, extraOpts...)
		err := pipeline.Index(dir, opts...)
		pw.Close()
		p.Send(indexDoneMsg{Err: err})
	}()
}

func (m IndexModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m IndexModel) Update(msg tea.Msg) (IndexModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		barW := min(msg.Width-10, 50)
		if barW < 20 {
			barW = 20
		}
		m.progress.Width = barW
	case indexLogMsg:
		line := string(msg)
		m.logLines = append(m.logLines, line)
		if len(m.logLines) > maxLogLines {
			m.logLines = m.logLines[len(m.logLines)-maxLogLines:]
		}
		// Update phase — walk ordered list, pick the latest match
		for _, ph := range phaseOrder {
			if strings.Contains(line, ph.prefix) && ph.pct >= m.pct {
				m.pct = ph.pct
				m.phase = ph.label
			}
		}
		return m, nil
	case indexDoneMsg:
		m.done = true
		m.err = msg.Err
		m.pct = 1.0
		m.phase = "Complete"
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case progress.FrameMsg:
		pm, cmd := m.progress.Update(msg)
		m.progress = pm.(progress.Model)
		return m, cmd
	}
	return m, nil
}

func (m IndexModel) View() string {
	var b strings.Builder

	if m.done {
		if m.err != nil {
			b.WriteString(ErrorStyle.Render("✗ Indexing failed") + "\n")
			b.WriteString(MutedStyle.Render(m.err.Error()) + "\n")
		} else {
			b.WriteString(SuccessStyle.Render("✓ Indexing complete!") + "\n")
			// Show final stats from log
			for _, l := range m.logLines {
				if strings.HasPrefix(l, "Done!") {
					b.WriteString(MutedStyle.Render(l) + "\n")
				}
			}
		}
		b.WriteString("\n")
		b.WriteString(m.progress.ViewAs(1.0) + "\n")
	} else {
		b.WriteString(m.spinner.View() + " " + AccentStyle.Render(m.phase) + "\n\n")
		b.WriteString(m.progress.ViewAs(m.pct) + "\n")
	}

	b.WriteString("\n")

	// Log lines
	for _, l := range m.logLines {
		b.WriteString(MutedStyle.Render(l) + "\n")
	}
	if len(m.logLines) == 0 {
		b.WriteString(MutedStyle.Render("Starting pipeline...") + "\n")
	}

	return b.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
