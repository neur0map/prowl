package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func mergeOverlay(base, modal string, width, height int) tea.View {
	canvasW := max(width-1, 1)
	lines := strings.Split(modal, "\n")
	boxW := 0
	for _, line := range lines {
		boxW = max(boxW, lipgloss.Width(line))
	}
	boxW = min(boxW, max(canvasW-4, 1))
	boxH := min(len(lines), max(height-2, 1))
	startX, startY := overlayOrigin(width, height, boxW, boxH)

	baseLines := strings.Split(fillLines(base, canvasW, height), "\n")
	for i := range baseLines {
		baseLines[i] = stFaint.Render(ansi.Strip(baseLines[i]))
	}
	for y := 0; y < boxH && startY+y < len(baseLines); y++ {
		row := truncate(lines[y], boxW)
		baseLines[startY+y] = strings.Repeat(" ", startX) +
			padRight(row, boxW) +
			strings.Repeat(" ", max(canvasW-startX-boxW, 0))
	}
	return tea.NewView(strings.Join(baseLines, "\n"))
}

// overlayOrigin is shared by compositing and pointer hit-testing. The app
// deliberately renders on a width-1 canvas so terminal-edge wrapping remains
// impossible; calculating hits from the raw terminal width shifts every modal
// target one cell right for half of all terminal widths.
func overlayOrigin(width, height, boxW, boxH int) (int, int) {
	canvasW := max(width-1, 1)
	return max((canvasW-boxW)/2, 1), max((height-boxH)/2, 1)
}

type actionOverlay struct {
	width, height int
	actions       []action
	cursor        int
	hits          []hitRegion
}

func newActionOverlay(actions []action) *actionOverlay {
	return &actionOverlay{actions: append([]action(nil), actions...)}
}

func (o *actionOverlay) Init() tea.Cmd { return nil }

func (o *actionOverlay) choose(i int) (tea.Model, tea.Cmd) {
	if i < 0 || i >= len(o.actions) {
		return o, nil
	}
	key := o.actions[i].Key
	return nil, func() tea.Msg { return actionChosenMsg{Key: key} }
}

func (o *actionOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		o.width, o.height = msg.Width, msg.Height
	case tea.MouseClickMsg:
		for i, hit := range o.hits {
			if hit.contains(msg.X, msg.Y) {
				return o.choose(i)
			}
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			o.cursor = max(o.cursor-1, 0)
		case tea.MouseWheelDown:
			o.cursor = min(o.cursor+1, max(len(o.actions)-1, 0))
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "q", ".":
			return nil, nil
		case "up", "k":
			o.cursor = max(o.cursor-1, 0)
		case "down", "j":
			o.cursor = min(o.cursor+1, max(len(o.actions)-1, 0))
		case "enter", "space":
			return o.choose(o.cursor)
		}
	}
	return o, nil
}

func (o *actionOverlay) View() tea.View {
	var body strings.Builder
	body.WriteString(brandText("Screen actions", 0) + "\n")
	body.WriteString(stSubtle.Render("Move with ↑↓ and press enter.") + "\n\n")
	for i, item := range o.actions {
		label := actionChip(item.Key, item.Label, i == o.cursor, item.Dangerous, false)
		if i == o.cursor {
			body.WriteString(brandText("◆", i) + " " + label)
		} else {
			body.WriteString("  " + label)
		}
		body.WriteString("\n")
	}
	body.WriteString("\n" + actionChip("esc", "Close", false, false, false))

	box := stModal.Width(min(max(o.width-16, 40), 62)).Render(strings.TrimRight(body.String(), "\n"))
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(o.width, o.height, boxW, boxH)
	o.hits = o.hits[:0]
	for i := range o.actions {
		o.hits = append(o.hits, hitRegion{x: boxX + 3, y: boxY + 5 + i, w: boxW - 6, h: 1})
	}
	return tea.NewView(box)
}

// detailOverlay is the shared read-only drill-down used by list screens. It
// keeps long reports navigable on a short terminal and routes any domain
// actions back through the owning screen, just like the action palette.
type detailOverlay struct {
	width, height int
	title         string
	subtitle      string
	body          string
	actions       []action
	offset        int
	actionHits    []hitRegion
	closeHit      hitRegion
}

func newDetailOverlay(title, subtitle, body string, actions []action) *detailOverlay {
	return &detailOverlay{
		title: title, subtitle: subtitle, body: body,
		actions: append([]action(nil), actions...),
	}
}

func (o *detailOverlay) Init() tea.Cmd { return nil }

func (o *detailOverlay) bodyLines() []string {
	body := strings.TrimRight(o.body, "\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}

func (o *detailOverlay) viewportRows() int {
	if o.height > 0 && o.height < 17 {
		// Compact rendering drops subtitle/blank/indicator rows and vertical
		// padding. Border + heading + buttons consume six terminal rows once
		// mergeOverlay's one-row top/bottom margin is included.
		return max(o.height-6, 1)
	}
	rows := o.height - 13
	if o.subtitle == "" {
		rows++
	}
	return max(rows, 1)
}

func (o *detailOverlay) clamp() {
	o.offset = min(max(o.offset, 0), max(len(o.bodyLines())-o.viewportRows(), 0))
}

func (o *detailOverlay) choose(i int) (tea.Model, tea.Cmd) {
	if i < 0 || i >= len(o.actions) {
		return o, nil
	}
	key := o.actions[i].Key
	return nil, func() tea.Msg { return actionChosenMsg{Key: key} }
}

func (o *detailOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		o.width, o.height = msg.Width, msg.Height
		o.clamp()
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			o.offset--
		case tea.MouseWheelDown:
			o.offset++
		}
		o.clamp()
	case tea.MouseClickMsg:
		if o.closeHit.contains(msg.X, msg.Y) {
			return nil, nil
		}
		for i, hit := range o.actionHits {
			if hit.contains(msg.X, msg.Y) {
				return o.choose(i)
			}
		}
	case tea.KeyPressMsg:
		key := msg.String()
		switch key {
		case "esc", "q", ".":
			return nil, nil
		case "up", "k":
			o.offset--
		case "down", "j":
			o.offset++
		case "pgup":
			o.offset -= o.viewportRows()
		case "pgdn":
			o.offset += o.viewportRows()
		case "home", "g":
			o.offset = 0
		case "end", "G":
			o.offset = len(o.bodyLines())
		case "enter":
			for i, item := range o.actions {
				if item.Primary {
					return o.choose(i)
				}
			}
			// A detail view may expose destructive secondary actions (withdraw,
			// forget, delete). Enter invokes only an explicitly primary action;
			// when there are secondary actions it keeps the report open.
			if len(o.actions) == 0 {
				return nil, nil
			}
		default:
			for i, item := range o.actions {
				if key == item.Key {
					return o.choose(i)
				}
			}
		}
		o.clamp()
	}
	return o, nil
}

func (o *detailOverlay) View() tea.View {
	availableWidth := max(o.width-6, 12)
	boxWidth := min(min(max(o.width-12, 42), 84), availableWidth)
	compact := o.height > 0 && o.height < 17
	contentWidth := max(boxWidth-6, 8)
	modalStyle := stModal
	contentXInset, buttonYInset := 3, 3
	if compact {
		modalStyle = modalStyle.Padding(0, 1)
		contentWidth = max(boxWidth-4, 8)
		contentXInset, buttonYInset = 2, 2
	}

	lines := o.bodyLines()
	o.clamp()
	end := min(o.offset+o.viewportRows(), len(lines))

	var body strings.Builder
	if compact {
		heading := brandText(o.title, 0)
		if o.subtitle != "" {
			heading += stSubtle.Render(" · " + o.subtitle)
		}
		scrollState := ""
		if o.offset > 0 {
			scrollState += "↑"
		}
		if end < len(lines) {
			scrollState += "↓"
		}
		if scrollState != "" {
			scrollState = stFaint.Render(" " + scrollState)
		}
		body.WriteString(truncate(heading, max(contentWidth-lipgloss.Width(scrollState), 1)) + scrollState + "\n")
		if len(lines) == 0 {
			body.WriteString(stFaint.Render("No details.") + "\n")
		} else {
			for _, line := range lines[o.offset:end] {
				body.WriteString(truncate(line, contentWidth) + "\n")
			}
		}
	} else {
		body.WriteString(brandText(o.title, 0) + "\n")
		if o.subtitle != "" {
			body.WriteString(stSubtle.Render(truncate(o.subtitle, contentWidth)) + "\n")
		}
		body.WriteString("\n")
		if o.offset > 0 {
			body.WriteString(stFaint.Render("↑ more") + "\n")
		}
		for _, line := range lines[o.offset:end] {
			body.WriteString(truncate(line, contentWidth) + "\n")
		}
		if end < len(lines) {
			body.WriteString(stFaint.Render("↓ more") + "\n")
		}
		body.WriteString("\n")
	}

	o.actionHits = o.actionHits[:0]
	actionWidths := make([]int, 0, len(o.actions))
	for i, item := range o.actions {
		chip := actionChip(item.Key, item.Label, item.Primary, item.Dangerous, false)
		if i > 0 {
			body.WriteString(" ")
		}
		body.WriteString(chip)
		actionWidths = append(actionWidths, lipgloss.Width(chip))
	}
	if len(o.actions) > 0 {
		body.WriteString("  ")
	}
	closeLabel := actionChip("esc", "Close", len(o.actions) == 0, false, false)
	body.WriteString(closeLabel)

	box := modalStyle.Width(boxWidth).Render(strings.TrimRight(body.String(), "\n"))
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(o.width, o.height, boxW, boxH)
	buttonX, buttonY := boxX+contentXInset, boxY+boxH-buttonYInset
	for _, width := range actionWidths {
		o.actionHits = append(o.actionHits, hitRegion{x: buttonX, y: buttonY, w: width, h: 1})
		buttonX += width + 1
	}
	if len(actionWidths) > 0 {
		buttonX++
	}
	o.closeHit = hitRegion{x: buttonX, y: buttonY, w: lipgloss.Width(closeLabel), h: 1}
	return tea.NewView(box)
}

type helpOverlay struct {
	width, height int
	close         hitRegion
}

func newHelpOverlay() *helpOverlay   { return &helpOverlay{} }
func (h *helpOverlay) Init() tea.Cmd { return nil }

func (h *helpOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		h.width, h.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "q", "?":
			return nil, nil
		}
	case tea.MouseClickMsg:
		if h.close.contains(msg.X, msg.Y) {
			return nil, nil
		}
	}
	return h, nil
}

func (h *helpOverlay) View() tea.View {
	groups := []struct {
		title string
		rows  [][2]string
	}{
		{"Move", [][2]string{{"1-7", "jump to a section"}, {"tab / shift+tab", "next / previous section"}, {"↑↓ / j k", "select a row"}, {"pgup / pgdn / g G", "scroll"}, {"← → / h l", "collapse or expand a provider"}}},
		{"Routing", [][2]string{{"enter", "open a set, or create one from a preset"}, {"space", "activate a set · select a model or a whole provider"}, {"r", "strategy for the selected set"}, {"n / x / d", "new, rename, delete a set"}}},
		{"Providers", [][2]string{{"enter", "manage a provider"}, {"c", "connect: key, keyless enable or sign-in"}, {"space", "pause / resume"}, {"d", "disconnect"}}},
		{"Everywhere", [][2]string{{"/", "search the table"}, {"f", "filter the table"}, {".", "every action for this section"}, {"esc", "close, clear or go back"}, {"ctrl+r", "reload this section"}, {"q / ctrl+c", "quit"}}},
	}
	var body strings.Builder
	body.WriteString(brandText("How to move", 0) + "\n")
	body.WriteString(stSubtle.Render("Pointer and keyboard actions follow the same safe path.") + "\n\n")
	for _, group := range groups {
		body.WriteString(section(group.title) + "\n")
		for _, row := range group.rows {
			body.WriteString("  " + stKey.Render(padRight(row[0], 22)) + stSubtle.Render(row[1]) + "\n")
		}
		body.WriteString("\n")
	}
	closeLabel := actionChip("esc", "Close", true, false, false)
	body.WriteString(closeLabel)
	box := stModal.Render(strings.TrimRight(body.String(), "\n"))
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	x, y := overlayOrigin(h.width, h.height, boxW, boxH)
	h.close = hitRegion{x: x + 3, y: y + boxH - 3, w: lipgloss.Width(closeLabel), h: 1}
	return tea.NewView(box)
}

// confirmOverlay is the pointer-safe gate every destructive action passes
// through. The operator already chose the verb, so the confirm button owns
// focus: enter applies it, esc keeps.
type confirmOverlay struct {
	question      string
	yes           func() tea.Msg
	verb          string
	width, height int
	running       bool
	// op is the id of the operation this confirm launched, stamped on the
	// result so only this confirm - not an unrelated background completion -
	// is dismissed when it finishes.
	op                uint64
	cancelHit, yesHit hitRegion
}

func (c *confirmOverlay) Init() tea.Cmd { return nil }

func (c *confirmOverlay) runYes() (tea.Model, tea.Cmd) {
	c.running = true
	c.op = nextOverlayOp()
	op := c.op
	inner := c.yes()
	return c, func() tea.Msg {
		msg := inner
		if fn, ok := inner.(func() tea.Msg); ok {
			msg = fn()
		}
		return tagOverlayOp(msg, op)
	}
}

func (c *confirmOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
	case tea.MouseClickMsg:
		if c.running {
			return c, nil
		}
		switch {
		case c.cancelHit.contains(msg.X, msg.Y):
			return nil, nil
		case c.yesHit.contains(msg.X, msg.Y):
			return c.runYes()
		}
	case tea.KeyPressMsg:
		if c.running {
			return c, nil
		}
		switch msg.String() {
		case "esc", "n", "left", "h":
			return nil, nil
		case "enter", "space", "y", "right", "l":
			return c.runYes()
		default:
			return c, nil
		}
	}
	return c, nil
}

func (c *confirmOverlay) View() tea.View {
	verb := c.verb
	if verb == "" {
		verb = "Remove"
	}
	cancel := actionChip("esc", "Keep", false, false, false)
	confirm := actionChip("enter", verb, true, true, false)
	status := stFaint.Render("Nothing changes until you confirm.")
	if c.running {
		status = stWarn.Render("● Applying change…")
	}
	body := brandText("Confirm "+strings.ToLower(verb), 0) + "\n" +
		stSubtle.Render("This action changes gateway state immediately.") + "\n\n" +
		c.question + "\n\n" + status + "\n\n" + cancel + " " + confirm
	box := stModal.Width(min(max(c.width-16, 44), 72)).Render(body)
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(c.width, c.height, boxW, boxH)
	buttonY := boxY + boxH - 3
	c.cancelHit = hitRegion{x: boxX + 3, y: buttonY, w: lipgloss.Width(cancel), h: 1}
	c.yesHit = hitRegion{x: boxX + 4 + lipgloss.Width(cancel), y: buttonY, w: lipgloss.Width(confirm), h: 1}
	return tea.NewView(box)
}
