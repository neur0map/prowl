package tui

import "github.com/charmbracelet/lipgloss"

// Color palette
var (
	Purple  = lipgloss.Color("#9B59B6")
	Cyan    = lipgloss.Color("#1ABC9C")
	Amber   = lipgloss.Color("#F39C12")
	Red     = lipgloss.Color("#E74C3C")
	Green   = lipgloss.Color("#2ECC71")
	Muted   = lipgloss.Color("#7F8C8D")
	White   = lipgloss.Color("#ECF0F1")
	DarkBg  = lipgloss.Color("#1a1a2e")
	CardBg  = lipgloss.Color("#16213e")
	BorderC = lipgloss.Color("#0f3460")
)

// Component styles
var (
	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Purple).
			MarginBottom(1)

	SubtitleStyle = lipgloss.NewStyle().
			Foreground(Muted).
			Italic(true)

	ActiveTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Purple).
			Background(lipgloss.Color("#2d2d4e")).
			Padding(0, 2)

	InactiveTabStyle = lipgloss.NewStyle().
				Foreground(Muted).
				Padding(0, 2)

	TabBarStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(BorderC).
			MarginBottom(1)

	StatLabelStyle = lipgloss.NewStyle().
			Foreground(Muted).
			Width(14)

	StatValueStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Cyan)

	BoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(BorderC).
			Padding(1, 2)

	HeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(Purple).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(BorderC).
			MarginBottom(1).
			Width(60)

	SpinnerStyle = lipgloss.NewStyle().
			Foreground(Cyan)

	ErrorStyle = lipgloss.NewStyle().
			Foreground(Red).
			Bold(true)

	SuccessStyle = lipgloss.NewStyle().
			Foreground(Green).
			Bold(true)

	MutedStyle = lipgloss.NewStyle().
			Foreground(Muted)

	AccentStyle = lipgloss.NewStyle().
			Foreground(Amber).
			Bold(true)

	InputStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(Purple).
			Padding(0, 1)

	BarFillStyle = lipgloss.NewStyle().
			Foreground(Cyan)

	BarEmptyStyle = lipgloss.NewStyle().
			Foreground(Muted)

	LogLineStyle = lipgloss.NewStyle().
			Foreground(Muted)

	SelectedStyle = lipgloss.NewStyle().
			Foreground(Cyan).
			Bold(true)

	HelpStyle = lipgloss.NewStyle().
			Foreground(Muted).
			MarginTop(1)

	WizardStepStyle = lipgloss.NewStyle().
			Foreground(Purple).
			Bold(true).
			MarginBottom(1)

	DotRunning = lipgloss.NewStyle().Foreground(Green).Render("●")
	DotStopped = lipgloss.NewStyle().Foreground(Red).Render("●")
)

// Logo returns the prowl ASCII art header.
func Logo() string {
	return lipgloss.NewStyle().Foreground(Purple).Bold(true).Render(`
  ┌─┐┬─┐┌─┐┬ ┬┬
  ├─┘├┬┘│ │││││
  ┴  ┴└─└─┘└┴┘┴─┘`)
}
