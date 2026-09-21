package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// pickerKind names what a picker chooses, so the owning screen can route the
// result. One picker may emit two kinds: its primary choice (enter) and an
// alternative the same row offers on another key (a set picker views on enter
// and activates on space).
type pickerKind int

const (
	pickStrategy pickerKind = iota
)

type pickerChoice struct {
	id       string
	title    string
	detail   string
	group    string
	current  bool   // the value in effect now
	tag      string // trailing badge, e.g. "active"
	disabled bool
}

type pickedMsg struct {
	kind pickerKind
	id   string
}

// pickerOverlay is the single-choice modal every "choose one of these" control
// shares: the routing strategy chooser is the one that uses it now. Rows may be
// grouped; the current value is pre-selected so enter with no movement is a
// no-op choice.
type pickerOverlay struct {
	width, height        int
	title                string
	subtitle             string
	kind                 pickerKind
	choices              []pickerChoice
	cursor               int
	offset               int
	hits                 []hitRegion
	chooseHit, cancelHit hitRegion
}

func newPicker(title, subtitle string, kind pickerKind, choices []pickerChoice) *pickerOverlay {
	overlay := &pickerOverlay{
		title: title, subtitle: subtitle, kind: kind,
		choices: append([]pickerChoice(nil), choices...),
	}
	for i, choice := range overlay.choices {
		if choice.current {
			overlay.cursor = i
			break
		}
	}
	return overlay
}

func (o *pickerOverlay) Init() tea.Cmd { return nil }

func (o *pickerOverlay) viewportRows() int {
	return max(o.height-14, 4)
}

func (o *pickerOverlay) ensureVisible() {
	per := o.viewportRows()
	if o.cursor < o.offset {
		o.offset = o.cursor
	} else if o.cursor >= o.offset+per {
		o.offset = o.cursor - per + 1
	}
	o.offset = max(o.offset, 0)
}

func (o *pickerOverlay) move(delta int) {
	if len(o.choices) == 0 {
		return
	}
	o.cursor = min(max(o.cursor+delta, 0), len(o.choices)-1)
	o.ensureVisible()
}

func (o *pickerOverlay) choose(kind pickerKind) (tea.Model, tea.Cmd) {
	if o.cursor < 0 || o.cursor >= len(o.choices) {
		return o, nil
	}
	choice := o.choices[o.cursor]
	if choice.disabled {
		return o, func() tea.Msg {
			return newToast("tip", "No matching models are connected for "+choice.title+" yet.")
		}
	}
	return nil, func() tea.Msg { return pickedMsg{kind: kind, id: choice.id} }
}

func (o *pickerOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		o.width, o.height = msg.Width, msg.Height
		o.ensureVisible()
	case tea.MouseClickMsg:
		switch {
		case o.cancelHit.contains(msg.X, msg.Y):
			return nil, nil
		case o.chooseHit.contains(msg.X, msg.Y):
			return o.choose(o.kind)
		}
		for i, hit := range o.hits {
			if hit.contains(msg.X, msg.Y) {
				o.cursor = o.offset + i
				return o.choose(o.kind)
			}
		}
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelUp {
			o.move(-1)
		} else if msg.Button == tea.MouseWheelDown {
			o.move(1)
		}
	case tea.KeyPressMsg:
		key := msg.String()
		switch key {
		case "esc", "q":
			return nil, nil
		case "up", "k":
			o.move(-1)
		case "down", "j":
			o.move(1)
		case "pgup":
			o.move(-o.viewportRows())
		case "pgdn":
			o.move(o.viewportRows())
		case "enter", "space":
			return o.choose(o.kind)
		}
	}
	return o, nil
}

func (o *pickerOverlay) View() tea.View {
	boxWidth := min(max(o.width-14, 50), 92)
	contentWidth := max(boxWidth-8, 24)
	end := min(o.offset+o.viewportRows(), len(o.choices))

	var body strings.Builder
	body.WriteString(brandText(o.title, 0) + "\n")
	body.WriteString(stSubtle.Render(truncate(o.subtitle, contentWidth)) + "\n\n")
	if o.offset > 0 {
		body.WriteString(stFaint.Render("↑ more") + "\n")
	}

	rowLines := make([]int, 0, max(end-o.offset, 0))
	line := 3
	if o.offset > 0 {
		line++
	}
	lastGroup := ""
	for i := o.offset; i < end; i++ {
		choice := o.choices[i]
		if choice.group != "" && choice.group != lastGroup {
			body.WriteString(section(choice.group) + "\n")
			line++
			lastGroup = choice.group
		}
		rowLines = append(rowLines, line)
		prefix := "  "
		if i == o.cursor {
			prefix = brandText("◆", i) + " "
		}
		marker := ""
		if choice.tag != "" {
			marker = "  " + stGood.Render(choice.tag)
		}
		nameWidth := min(28, max(contentWidth/3, 16))
		name := padRight(truncate(choice.title, nameWidth), nameWidth)
		detail := truncate(choice.detail, max(contentWidth-nameWidth-5-lipgloss.Width(marker), 8))
		rowText := prefix + stHead.Render(name) + "  " + stFaint.Render(detail) + marker
		if choice.disabled {
			rowText = prefix + stFaint.Render(name+"  "+detail)
		}
		body.WriteString(truncate(rowText, contentWidth) + "\n")
		line++
	}
	if end < len(o.choices) {
		body.WriteString(stFaint.Render("↓ more") + "\n")
	}
	if len(o.choices) == 0 {
		body.WriteString(stFaint.Render("Nothing is available here yet.") + "\n")
	}
	choose := actionChip("enter", "Choose", true, false, false)
	buttons := choose
	cancel := actionChip("esc", "Cancel", false, false, false)
	body.WriteString("\n" + buttons + " " + cancel)

	box := stModal.Width(boxWidth).Render(strings.TrimRight(body.String(), "\n"))
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(o.width, o.height, boxW, boxH)
	o.hits = o.hits[:0]
	for _, rowLine := range rowLines {
		o.hits = append(o.hits, hitRegion{x: boxX + 3, y: boxY + 2 + rowLine, w: boxW - 6, h: 1})
	}
	buttonY := boxY + boxH - 3
	o.chooseHit = hitRegion{x: boxX + 3, y: buttonY, w: lipgloss.Width(choose), h: 1}
	o.cancelHit = hitRegion{x: boxX + 4 + lipgloss.Width(buttons), y: buttonY, w: lipgloss.Width(cancel), h: 1}
	return tea.NewView(box)
}

// ── filter overlay ──────────────────────────────────────────────────────────

// filterOption is one row in a filter section. count is the number of rows
// the option would keep, shown so an empty lens is never a surprise.
type filterOption struct {
	id    string
	label string
	count int
}

// filterSection is one facet of a filter panel. A radio section keeps exactly
// one option on; a check section keeps any number, and an empty selection
// means "everything" so the neutral state never hides rows.
type filterSection struct {
	id      string
	title   string
	radio   bool
	options []filterOption
}

// filterState is the selection per section: for a radio section the single
// chosen id; for a check section every id that is on.
type filterState map[string]map[string]bool

func (s filterState) on(section, id string) bool { return s[section][id] }

// radio returns the chosen id of a radio section ("" when neutral).
func (s filterState) radio(section string) string {
	for id, on := range s[section] {
		if on {
			return id
		}
	}
	return ""
}

func (s filterState) set(section, id string, on bool) {
	if s[section] == nil {
		s[section] = map[string]bool{}
	}
	if on {
		s[section][id] = true
	} else {
		delete(s[section], id)
	}
}

func (s filterState) clone() filterState {
	out := make(filterState, len(s))
	for section, ids := range s {
		out[section] = make(map[string]bool, len(ids))
		for id, on := range ids {
			out[section][id] = on
		}
	}
	return out
}

// active counts sections that are narrowed away from neutral.
func (s filterState) active(sections []filterSection) int {
	n := 0
	for _, section := range sections {
		if section.radio {
			if id := s.radio(section.id); id != "" && id != "all" {
				n++
			}
			continue
		}
		if len(s[section.id]) > 0 {
			n++
		}
	}
	return n
}

type filterAppliedMsg struct {
	state filterState
}

// filterOverlay is the facet panel every table's f key opens: every lens on
// one screen with its current value and row counts, applied together. It
// replaces the blind "press f to cycle" filters that hid what the other
// values were.
type filterOverlay struct {
	width, height       int
	title               string
	sections            []filterSection
	state               filterState
	cursor              int
	offset              int
	hits                []hitRegion
	applyHit, cancelHit hitRegion
}

// filterRowRef addresses one option by section and option index.
type filterRowRef struct{ section, option int }

func newFilterOverlay(title string, sections []filterSection, current filterState) *filterOverlay {
	return &filterOverlay{title: title, sections: sections, state: current.clone()}
}

func (o *filterOverlay) Init() tea.Cmd { return nil }

func (o *filterOverlay) rows() []filterRowRef {
	out := make([]filterRowRef, 0, 16)
	for si, section := range o.sections {
		for oi := range section.options {
			out = append(out, filterRowRef{si, oi})
		}
	}
	return out
}

// lineCount is how many rendered lines the option list takes: one per option
// plus a heading and a spacer per section.
func (o *filterOverlay) lineCount() int {
	n := 0
	for _, section := range o.sections {
		n += len(section.options) + 2
	}
	return n
}

func (o *filterOverlay) viewportLines() int { return max(o.height-12, 6) }

func (o *filterOverlay) lineOf(ref filterRowRef) int {
	line := 0
	for si, section := range o.sections {
		line++ // heading
		if si == ref.section {
			return line + ref.option
		}
		line += len(section.options) + 1
	}
	return line
}

func (o *filterOverlay) ensureVisible() {
	rows := o.rows()
	if len(rows) == 0 {
		return
	}
	o.cursor = min(max(o.cursor, 0), len(rows)-1)
	line := o.lineOf(rows[o.cursor])
	per := o.viewportLines()
	if line < o.offset+1 {
		o.offset = max(line-1, 0)
	} else if line >= o.offset+per {
		o.offset = line - per + 1
	}
	o.offset = min(max(o.offset, 0), max(o.lineCount()-per, 0))
}

func (o *filterOverlay) move(delta int) {
	rows := o.rows()
	if len(rows) == 0 {
		return
	}
	o.cursor = min(max(o.cursor+delta, 0), len(rows)-1)
	o.ensureVisible()
}

func (o *filterOverlay) toggle() {
	rows := o.rows()
	if o.cursor < 0 || o.cursor >= len(rows) {
		return
	}
	ref := rows[o.cursor]
	section := o.sections[ref.section]
	id := section.options[ref.option].id
	if section.radio {
		o.state[section.id] = map[string]bool{id: true}
		return
	}
	o.state.set(section.id, id, !o.state.on(section.id, id))
}

func (o *filterOverlay) reset() {
	for _, section := range o.sections {
		delete(o.state, section.id)
		if section.radio && len(section.options) > 0 {
			o.state.set(section.id, section.options[0].id, true)
		}
	}
}

func (o *filterOverlay) apply() (tea.Model, tea.Cmd) {
	state := o.state.clone()
	return nil, func() tea.Msg { return filterAppliedMsg{state: state} }
}

func (o *filterOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		o.width, o.height = msg.Width, msg.Height
		o.ensureVisible()
	case tea.MouseClickMsg:
		switch {
		case o.cancelHit.contains(msg.X, msg.Y):
			return nil, nil
		case o.applyHit.contains(msg.X, msg.Y):
			return o.apply()
		}
		for i, hit := range o.hits {
			if hit.contains(msg.X, msg.Y) {
				o.cursor = i
				o.toggle()
				return o, nil
			}
		}
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelUp {
			o.move(-1)
		} else if msg.Button == tea.MouseWheelDown {
			o.move(1)
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "q":
			return nil, nil
		case "up", "k":
			o.move(-1)
		case "down", "j":
			o.move(1)
		case "space":
			o.toggle()
		case "x":
			o.reset()
		case "enter":
			return o.apply()
		}
	}
	return o, nil
}

func (o *filterOverlay) View() tea.View {
	boxWidth := min(max(o.width-14, 52), 82)
	contentWidth := max(boxWidth-8, 26)

	var lines []string
	rows := o.rows()
	cursorRef := filterRowRef{-1, -1}
	if o.cursor >= 0 && o.cursor < len(rows) {
		cursorRef = rows[o.cursor]
	}
	rowLine := map[filterRowRef]int{}
	for si, sec := range o.sections {
		lines = append(lines, section(sec.title))
		for oi, option := range sec.options {
			ref := filterRowRef{si, oi}
			rowLine[ref] = len(lines)
			cursor := "  "
			if ref == cursorRef {
				cursor = brandText("◆", oi) + " "
			}
			var mark string
			on := o.state.on(sec.id, option.id)
			switch {
			case sec.radio && on:
				mark = stGood.Render("◉")
			case sec.radio:
				mark = stFaint.Render("○")
			case on:
				mark = stGood.Render("■")
			default:
				mark = stFaint.Render("□")
			}
			count := ""
			if option.count >= 0 {
				count = stFaint.Render(fmt.Sprintf("%d", option.count))
			}
			label := padRight(truncate(option.label, contentWidth-10), contentWidth-10)
			lines = append(lines, truncate(cursor+mark+" "+label+" "+count, contentWidth))
		}
		lines = append(lines, "")
	}
	end := min(o.offset+o.viewportLines(), len(lines))

	var body strings.Builder
	body.WriteString(brandText(o.title, 0) + "\n")
	body.WriteString(stSubtle.Render(truncate("Space toggles a lens · x resets every lens · enter applies", contentWidth)) + "\n\n")
	if o.offset > 0 {
		body.WriteString(stFaint.Render("↑ more") + "\n")
	}
	for _, line := range lines[o.offset:end] {
		body.WriteString(line + "\n")
	}
	if end < len(lines) {
		body.WriteString(stFaint.Render("↓ more") + "\n")
	}
	apply := actionChip("enter", "Apply", true, false, false)
	resetChip := actionChip("x", "Reset", false, false, false)
	cancel := actionChip("esc", "Cancel", false, false, false)
	body.WriteString("\n" + apply + " " + resetChip + " " + cancel)

	box := stModal.Width(boxWidth).Render(strings.TrimRight(body.String(), "\n"))
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(o.width, o.height, boxW, boxH)
	o.hits = o.hits[:0]
	firstLineY := boxY + 4
	if o.offset > 0 {
		firstLineY++
	}
	for _, ref := range rows {
		line := rowLine[ref]
		if line < o.offset || line >= end {
			o.hits = append(o.hits, hitRegion{})
			continue
		}
		o.hits = append(o.hits, hitRegion{x: boxX + 3, y: firstLineY + line - o.offset, w: boxW - 6, h: 1})
	}
	buttonY := boxY + boxH - 3
	o.applyHit = hitRegion{x: boxX + 3, y: buttonY, w: lipgloss.Width(apply), h: 1}
	o.cancelHit = hitRegion{x: boxX + 5 + lipgloss.Width(apply) + lipgloss.Width(resetChip), y: buttonY, w: lipgloss.Width(cancel), h: 1}
	return tea.NewView(box)
}

// providerDisplayName turns a platform id into the label the tables show.
func providerDisplayName(id string) string {
	switch strings.ToLower(id) {
	case "openai":
		return "OpenAI / Codex"
	case "anthropic":
		return "Anthropic / Claude"
	case "hyper":
		return "Charm Hyper"
	case "nvidia":
		return "NVIDIA NIM"
	case "ovh":
		return "OVHcloud"
	case "github-copilot", "github_copilot":
		return "GitHub Copilot"
	}
	parts := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	if len(parts) == 0 {
		return id
	}
	return strings.Join(parts, " ")
}
