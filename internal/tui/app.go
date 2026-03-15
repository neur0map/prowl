package tui

import (
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/neur0map/prowl/internal/embed"
	"github.com/neur0map/prowl/internal/store"
)

type appMode int

const (
	modeWizard appMode = iota
	modeDashboard
)

// programReadyMsg is sent once the tea.Program is available.
type programReadyMsg struct{ p *tea.Program }

// AppModel is the top-level tea.Model that routes between wizard and dashboard.
type AppModel struct {
	mode      appMode
	wizard    WizardModel
	dashboard DashboardModel
	dir       string
	version   string
	width     int
	height    int
	program   *tea.Program
}

// RunWithWizard launches the TUI starting with the setup wizard.
func RunWithWizard(dir string) error {
	m := AppModel{
		mode:   modeWizard,
		wizard: NewWizardModel(dir),
		dir:    dir,
	}
	p := tea.NewProgram(m, tea.WithAltScreen())

	// Send the program reference as a message so the running model receives it
	go func() { p.Send(programReadyMsg{p: p}) }()

	_, err := p.Run()
	return err
}

// RunDashboard launches the TUI directly into the dashboard.
func RunDashboard(dir string, version string) error {
	absDir, _ := filepath.Abs(dir)
	dbPath := filepath.Join(absDir, ".prowl", "prowl.db")

	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	homeDir, _ := os.UserHomeDir()
	modelDir := filepath.Join(homeDir, ".prowl", "models")
	embedder, _ := embed.New(modelDir) // nil is OK, search will be disabled

	dash := NewDashboardModel(absDir, st, embedder, version)

	m := AppModel{
		mode:      modeDashboard,
		dashboard: dash,
		dir:       absDir,
		version:   version,
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()

	st.Close()
	if embedder != nil {
		embedder.Close()
	}
	return err
}

func (m AppModel) Init() tea.Cmd {
	switch m.mode {
	case modeWizard:
		return m.wizard.Init()
	case modeDashboard:
		return m.dashboard.Init()
	}
	return nil
}

func (m AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case programReadyMsg:
		m.program = msg.p
		return m, nil
	}

	switch m.mode {
	case modeWizard:
		return m.updateWizard(msg)
	case modeDashboard:
		return m.updateDashboard(msg)
	}
	return m, nil
}

func (m AppModel) updateWizard(msg tea.Msg) (tea.Model, tea.Cmd) {
	wasStep := m.wizard.step

	model, cmd := m.wizard.Update(msg)
	wizard, ok := model.(WizardModel)
	if !ok {
		return m, cmd
	}
	m.wizard = wizard

	// If we just transitioned to indexing step, start the pipeline
	if wasStep != stepIndexing && m.wizard.step == stepIndexing && m.program != nil {
		m.wizard.SetProgram(m.program)
		m.wizard.StartIndexing()
	}

	// If wizard is done and user presses enter, transition to dashboard
	if m.wizard.Done() {
		if keyMsg, ok := msg.(tea.KeyMsg); ok && keyMsg.String() == "enter" {
			return m.transitionToDashboard()
		}
	}

	return m, cmd
}

func (m AppModel) updateDashboard(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.dashboard.Update(msg)
	dash, ok := model.(DashboardModel)
	if ok {
		m.dashboard = dash
	}
	return m, cmd
}

func (m AppModel) transitionToDashboard() (tea.Model, tea.Cmd) {
	absDir, _ := filepath.Abs(m.dir)
	dbPath := filepath.Join(absDir, ".prowl", "prowl.db")

	st, err := store.Open(dbPath)
	if err != nil {
		return m, nil
	}

	homeDir, _ := os.UserHomeDir()
	modelDir := filepath.Join(homeDir, ".prowl", "models")
	embedder, _ := embed.New(modelDir)

	m.dashboard = NewDashboardModel(absDir, st, embedder, m.version)
	m.mode = modeDashboard
	return m, m.dashboard.Init()
}

func (m AppModel) View() string {
	switch m.mode {
	case modeWizard:
		return m.wizard.View()
	case modeDashboard:
		return m.dashboard.View()
	}
	return ""
}
