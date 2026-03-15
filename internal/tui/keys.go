package tui

import "github.com/charmbracelet/bubbles/key"

// KeyMap defines all key bindings for the TUI.
type KeyMap struct {
	Quit       key.Binding
	Tab        key.Binding
	ShiftTab   key.Binding
	Tab1       key.Binding
	Tab2       key.Binding
	Tab3       key.Binding
	Enter      key.Binding
	Up         key.Binding
	Down       key.Binding
	ToggleDmn  key.Binding
	Back       key.Binding
}

// Keys is the default key map.
var Keys = KeyMap{
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
	Tab: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "next tab"),
	),
	ShiftTab: key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift+tab", "prev tab"),
	),
	Tab1: key.NewBinding(
		key.WithKeys("1"),
		key.WithHelp("1", "stats"),
	),
	Tab2: key.NewBinding(
		key.WithKeys("2"),
		key.WithHelp("2", "search"),
	),
	Tab3: key.NewBinding(
		key.WithKeys("3"),
		key.WithHelp("3", "daemon"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "confirm"),
	),
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("k/↑", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("j/↓", "down"),
	),
	ToggleDmn: key.NewBinding(
		key.WithKeys("s"),
		key.WithHelp("s", "start/stop daemon"),
	),
	Back: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "back"),
	),
}

// HelpText returns a one-line help string for the given context.
func HelpText(context string) string {
	switch context {
	case "dashboard":
		return "tab/1-3: switch tabs · s: toggle daemon · q: quit"
	case "search":
		return "enter: search · j/k: navigate · esc: clear · q: quit"
	case "wizard":
		return "enter: confirm · q: quit"
	default:
		return "q: quit"
	}
}
