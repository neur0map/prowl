package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// formOverlay is the shared mutation surface: labels stay above their inputs,
// pointer clicks move focus, and the action row is identical everywhere.
type formOverlay struct {
	title      string
	fields     []formField
	cursor     int
	width      int
	height     int
	submit     func(values []string) tea.Msg
	after      func(values []string) string
	errLine    string
	submitting bool
	// op is the id of the operation this form launched, stamped on the result
	// so only this form is dismissed when its own submit completes.
	op        uint64
	fieldHits []hitRegion
	cancelHit hitRegion
	submitHit hitRegion
}

type formField struct {
	label       string
	input       textinput.Model
	placeholder string
	secret      bool
	optional    bool
}

func newForm(title string, labels ...string) *formOverlay {
	f := &formOverlay{title: title}
	for _, label := range labels {
		input := textinput.New()
		input.Prompt = ""
		switch strings.ToLower(label) {
		case "api key":
			input.Placeholder = "Paste the API key"
		case "base url":
			input.Placeholder = "https://api.example.com/v1"
		case "label":
			input.Placeholder = "Optional name"
		default:
			input.Placeholder = "Enter " + strings.ToLower(label)
		}
		f.fields = append(f.fields, formField{label: label, input: input})
	}
	return f
}

func (f *formOverlay) secret(i int) *formOverlay {
	f.fields[i].secret = true
	f.fields[i].input.EchoMode = textinput.EchoPassword
	f.fields[i].input.EchoCharacter = '•'
	return f
}

func (f *formOverlay) optional(i int) *formOverlay {
	f.fields[i].optional = true
	return f
}

func (f *formOverlay) Init() tea.Cmd {
	if len(f.fields) > 0 {
		return f.fields[0].input.Focus()
	}
	return nil
}

func (f *formOverlay) values() []string {
	out := make([]string, len(f.fields))
	for i := range f.fields {
		out[i] = strings.TrimSpace(f.fields[i].input.Value())
	}
	return out
}

func (f *formOverlay) validate() string {
	values := f.values()
	for i := range f.fields {
		if !f.fields[i].optional && values[i] == "" {
			return f.fields[i].label + " is required"
		}
	}
	return ""
}

func (f *formOverlay) submitCmd() tea.Cmd {
	if problem := f.validate(); problem != "" {
		f.errLine = problem
		return nil
	}
	f.errLine = ""
	f.submitting = true
	f.op = nextOverlayOp()
	op := f.op
	values := f.values()
	submit, after := f.submit, f.after
	return func() tea.Msg {
		result := submit(values)
		if done, ok := result.(doneMsg); ok {
			if after != nil {
				done.Text = after(values)
			}
			done.op = op
			return done
		}
		return tagOverlayOp(result, op)
	}
}

func (f *formOverlay) focus(i int) tea.Cmd {
	if i < 0 || i >= len(f.fields) {
		return nil
	}
	f.blur()
	f.cursor = i
	return f.fields[i].input.Focus()
}

func (f *formOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		f.width, f.height = msg.Width, msg.Height
		for i := range f.fields {
			f.fields[i].input.SetWidth(max(f.boxWidth()-16, 10))
		}
		return f, nil
	case tea.MouseClickMsg:
		if f.submitting {
			return f, nil
		}
		if f.cancelHit.contains(msg.X, msg.Y) {
			return nil, func() tea.Msg { return closeOverlayMsg{Reload: -1} }
		}
		if f.submitHit.contains(msg.X, msg.Y) {
			return f, f.submitCmd()
		}
		for i, hit := range f.fieldHits {
			if hit.contains(msg.X, msg.Y) {
				return f, f.focus(i)
			}
		}
	case tea.KeyPressMsg:
		if f.submitting {
			return f, nil
		}
		switch msg.String() {
		case "esc":
			return nil, func() tea.Msg { return closeOverlayMsg{Reload: -1} }
		case "tab", "down":
			return f, f.focus((f.cursor + 1) % len(f.fields))
		case "shift+tab", "up":
			return f, f.focus((f.cursor - 1 + len(f.fields)) % len(f.fields))
		case "enter":
			if f.cursor == len(f.fields)-1 {
				return f, f.submitCmd()
			}
			return f, f.focus((f.cursor + 1) % len(f.fields))
		}
	}

	for i := range f.fields {
		if i != f.cursor {
			continue
		}
		var cmd tea.Cmd
		f.fields[i].input, cmd = f.fields[i].input.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return f, tea.Batch(cmds...)
}

func (f *formOverlay) blur() {
	if len(f.fields) > 0 {
		f.fields[f.cursor].input.Blur()
	}
}

func (f *formOverlay) boxWidth() int {
	return min(max(f.width-12, 38), 74)
}

func (f *formOverlay) View() tea.View {
	var body strings.Builder
	body.WriteString(brandText(f.title, 0) + "\n")
	body.WriteString(stSubtle.Render("Complete the fields, then apply the change.") + "\n\n")

	fieldW := max(f.boxWidth()-8, 24)
	for i := range f.fields {
		field := &f.fields[i]
		label := field.label
		if field.optional {
			label += " · optional"
		}
		if i == f.cursor {
			label = "◆ " + label
		}
		inputStyle := lipgloss.NewStyle().Foreground(colorMist)
		if i == f.cursor {
			inputStyle = inputStyle.Foreground(colorMoon).Bold(true)
		}
		body.WriteString(roundedPanel(label, inputStyle.Render(field.input.View()), fieldW))
		body.WriteString("\n\n")
	}

	switch {
	case f.errLine != "":
		body.WriteString(stBad.Render("● "+f.errLine) + "\n")
	case f.submitting:
		body.WriteString(stWarn.Render("● Applying change…") + "\n")
	default:
		body.WriteString(stFaint.Render("Tab moves focus. Enter advances or applies.") + "\n")
	}
	body.WriteString("\n")
	cancel := actionChip("esc", "Cancel", false, false, false)
	submit := actionChip("enter", "Apply", true, false, false)
	body.WriteString(cancel + " " + submit)

	box := stModal.Width(f.boxWidth()).Render(strings.TrimRight(body.String(), "\n"))
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(f.width, f.height, boxW, boxH)

	f.fieldHits = f.fieldHits[:0]
	for i := range f.fields {
		f.fieldHits = append(f.fieldHits, hitRegion{
			x: boxX + 3, y: boxY + 5 + i*4, w: fieldW, h: 3,
		})
	}
	buttonY := boxY + boxH - 3
	f.cancelHit = hitRegion{x: boxX + 3, y: buttonY, w: lipgloss.Width(cancel), h: 1}
	f.submitHit = hitRegion{x: boxX + 4 + lipgloss.Width(cancel), y: buttonY, w: lipgloss.Width(submit), h: 1}
	return tea.NewView(box)
}
