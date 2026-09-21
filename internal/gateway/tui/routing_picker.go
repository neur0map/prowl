package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type providerScope int

const (
	providerScopeActive providerScope = iota
	providerScopeAll
	providerScopeCustom
)

type routingPickerKind int

const (
	routingPickerStrategy routingPickerKind = iota
	routingPickerTemplate
)

type routingPickerChoice struct {
	id       string
	title    string
	detail   string
	group    string
	selected bool
	disabled bool
}

type routingPickedMsg struct {
	kind routingPickerKind
	id   string
}

type routingPickerOverlay struct {
	width, height        int
	title                string
	subtitle             string
	kind                 routingPickerKind
	choices              []routingPickerChoice
	cursor               int
	offset               int
	hits                 []hitRegion
	chooseHit, cancelHit hitRegion
}

func newRoutingPicker(title, subtitle string, kind routingPickerKind, choices []routingPickerChoice) *routingPickerOverlay {
	overlay := &routingPickerOverlay{
		title: title, subtitle: subtitle, kind: kind,
		choices: append([]routingPickerChoice(nil), choices...),
	}
	for i, choice := range overlay.choices {
		if choice.selected {
			overlay.cursor = i
			break
		}
	}
	return overlay
}

func (o *routingPickerOverlay) Init() tea.Cmd { return nil }

func (o *routingPickerOverlay) viewportRows() int {
	return max(o.height-14, 4)
}

func (o *routingPickerOverlay) ensureVisible() {
	if len(o.choices) == 0 {
		o.cursor, o.offset = 0, 0
		return
	}
	o.cursor = min(max(o.cursor, 0), len(o.choices)-1)
	if o.cursor < o.offset {
		o.offset = o.cursor
	}
	if o.cursor >= o.offset+o.viewportRows() {
		o.offset = o.cursor - o.viewportRows() + 1
	}
	o.offset = min(max(o.offset, 0), max(len(o.choices)-o.viewportRows(), 0))
}

func (o *routingPickerOverlay) move(delta int) {
	if len(o.choices) == 0 {
		return
	}
	o.cursor = min(max(o.cursor+delta, 0), len(o.choices)-1)
	o.ensureVisible()
}

func (o *routingPickerOverlay) choose() (tea.Model, tea.Cmd) {
	if o.cursor < 0 || o.cursor >= len(o.choices) {
		return o, nil
	}
	choice := o.choices[o.cursor]
	if choice.disabled {
		return o, func() tea.Msg {
			return newToast("tip", "No matching models are connected for "+choice.title+" yet.")
		}
	}
	return nil, func() tea.Msg { return routingPickedMsg{kind: o.kind, id: choice.id} }
}

func (o *routingPickerOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		o.width, o.height = msg.Width, msg.Height
		o.ensureVisible()
	case tea.MouseClickMsg:
		switch {
		case o.cancelHit.contains(msg.X, msg.Y):
			return nil, nil
		case o.chooseHit.contains(msg.X, msg.Y):
			return o.choose()
		}
		for i, hit := range o.hits {
			if hit.contains(msg.X, msg.Y) {
				o.cursor = o.offset + i
				return o.choose()
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
		case "pgup":
			o.move(-o.viewportRows())
		case "pgdn":
			o.move(o.viewportRows())
		case "enter", "space":
			return o.choose()
		}
	}
	return o, nil
}

func (o *routingPickerOverlay) View() tea.View {
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
		if choice.selected {
			marker = stGood.Render(" active")
		}
		nameWidth := min(28, max(contentWidth/3, 16))
		name := padRight(truncate(choice.title, nameWidth), nameWidth)
		detail := truncate(choice.detail, max(contentWidth-nameWidth-5, 8))
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
	cancel := actionChip("esc", "Cancel", false, false, false)
	body.WriteString("\n" + choose + " " + cancel)

	box := stModal.Width(boxWidth).Render(strings.TrimRight(body.String(), "\n"))
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(o.width, o.height, boxW, boxH)
	o.hits = o.hits[:0]
	for _, rowLine := range rowLines {
		o.hits = append(o.hits, hitRegion{x: boxX + 3, y: boxY + 2 + rowLine, w: boxW - 6, h: 1})
	}
	buttonY := boxY + boxH - 3
	o.chooseHit = hitRegion{x: boxX + 3, y: buttonY, w: lipgloss.Width(choose), h: 1}
	o.cancelHit = hitRegion{x: boxX + 4 + lipgloss.Width(choose), y: buttonY, w: lipgloss.Width(cancel), h: 1}
	return tea.NewView(box)
}

type providerFilterOption struct {
	id     string
	name   string
	models int
	active bool
}

type providerFilterMsg struct {
	mode     providerScope
	selected map[string]bool
}

type providerFilterOverlay struct {
	width, height       int
	options             []providerFilterOption
	selected            map[string]bool
	mode                providerScope
	cursor              int
	offset              int
	errLine             string
	hits                []hitRegion
	activeHit, allHit   hitRegion
	applyHit, cancelHit hitRegion
}

func newProviderFilterOverlay(models []ModelRow, mode providerScope, current map[string]bool) *providerFilterOverlay {
	byID := map[string]*providerFilterOption{}
	for _, model := range models {
		if strings.TrimSpace(model.Platform) == "" {
			continue
		}
		option := byID[model.Platform]
		if option == nil {
			option = &providerFilterOption{id: model.Platform, name: providerDisplayName(model.Platform)}
			byID[model.Platform] = option
		}
		option.models++
		option.active = option.active || model.Available
	}
	options := make([]providerFilterOption, 0, len(byID))
	for _, option := range byID {
		options = append(options, *option)
	}
	sort.SliceStable(options, func(i, j int) bool {
		if options[i].active != options[j].active {
			return options[i].active
		}
		return strings.ToLower(options[i].name) < strings.ToLower(options[j].name)
	})

	selected := make(map[string]bool, len(options))
	switch mode {
	case providerScopeAll:
		for _, option := range options {
			selected[option.id] = true
		}
	case providerScopeCustom:
		for id, on := range current {
			if _, exists := byID[id]; on && exists {
				selected[id] = true
			}
		}
	default:
		for _, option := range options {
			if option.active {
				selected[option.id] = true
			}
		}
	}
	return &providerFilterOverlay{options: options, selected: selected, mode: mode}
}

func (o *providerFilterOverlay) Init() tea.Cmd { return nil }

func (o *providerFilterOverlay) viewportRows() int { return max(o.height-15, 4) }

func (o *providerFilterOverlay) ensureVisible() {
	if len(o.options) == 0 {
		o.cursor, o.offset = 0, 0
		return
	}
	o.cursor = min(max(o.cursor, 0), len(o.options)-1)
	if o.cursor < o.offset {
		o.offset = o.cursor
	}
	if o.cursor >= o.offset+o.viewportRows() {
		o.offset = o.cursor - o.viewportRows() + 1
	}
	o.offset = min(max(o.offset, 0), max(len(o.options)-o.viewportRows(), 0))
}

func (o *providerFilterOverlay) move(delta int) {
	o.cursor = min(max(o.cursor+delta, 0), max(len(o.options)-1, 0))
	o.ensureVisible()
}

func (o *providerFilterOverlay) toggle(i int) {
	if i < 0 || i >= len(o.options) {
		return
	}
	id := o.options[i].id
	if o.selected[id] {
		delete(o.selected, id)
	} else {
		o.selected[id] = true
	}
	o.mode = providerScopeCustom
	o.errLine = ""
}

func (o *providerFilterOverlay) selectActive() {
	clear(o.selected)
	for _, option := range o.options {
		if option.active {
			o.selected[option.id] = true
		}
	}
	o.mode = providerScopeActive
	o.errLine = ""
}

func (o *providerFilterOverlay) selectAll() {
	clear(o.selected)
	for _, option := range o.options {
		o.selected[option.id] = true
	}
	o.mode = providerScopeAll
	o.errLine = ""
}

func sameProviderSelection(options []providerFilterOption, selected map[string]bool, predicate func(providerFilterOption) bool) bool {
	for _, option := range options {
		if selected[option.id] != predicate(option) {
			return false
		}
	}
	return true
}

func (o *providerFilterOverlay) apply() (tea.Model, tea.Cmd) {
	if len(o.selected) == 0 && len(o.options) > 0 {
		o.errLine = "Choose at least one provider, or press a for active providers."
		return o, nil
	}
	matchesActive := sameProviderSelection(o.options, o.selected, func(option providerFilterOption) bool { return option.active })
	matchesAll := sameProviderSelection(o.options, o.selected, func(providerFilterOption) bool { return true })
	mode := providerScopeCustom
	switch {
	case matchesActive && matchesAll:
		// Active and All coincide (every provider is currently active). Keep the
		// operator's explicit All rather than collapsing to Active, so a later
		// inactive provider is not silently dropped from the scope.
		if o.mode == providerScopeAll {
			mode = providerScopeAll
		} else {
			mode = providerScopeActive
		}
	case matchesActive:
		mode = providerScopeActive
	case matchesAll:
		mode = providerScopeAll
	}
	selected := make(map[string]bool, len(o.selected))
	for id, on := range o.selected {
		selected[id] = on
	}
	return nil, func() tea.Msg { return providerFilterMsg{mode: mode, selected: selected} }
}

func (o *providerFilterOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		o.width, o.height = msg.Width, msg.Height
		o.ensureVisible()
	case tea.MouseClickMsg:
		switch {
		case o.activeHit.contains(msg.X, msg.Y):
			o.selectActive()
			return o, nil
		case o.allHit.contains(msg.X, msg.Y):
			o.selectAll()
			return o, nil
		case o.applyHit.contains(msg.X, msg.Y):
			return o.apply()
		case o.cancelHit.contains(msg.X, msg.Y):
			return nil, nil
		}
		for i, hit := range o.hits {
			if hit.contains(msg.X, msg.Y) {
				o.cursor = o.offset + i
				o.toggle(o.cursor)
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
		case "pgup":
			o.move(-o.viewportRows())
		case "pgdn":
			o.move(o.viewportRows())
		case "space":
			o.toggle(o.cursor)
		case "a":
			o.selectActive()
		case "x":
			o.selectAll()
		case "enter":
			return o.apply()
		}
	}
	return o, nil
}

func (o *providerFilterOverlay) View() tea.View {
	boxWidth := min(max(o.width-14, 52), 82)
	contentWidth := max(boxWidth-8, 26)
	end := min(o.offset+o.viewportRows(), len(o.options))

	var body strings.Builder
	body.WriteString(brandText("Visible providers", 0) + "\n")
	body.WriteString(stSubtle.Render("Space selects providers. Search still spans the full catalogue.") + "\n\n")
	activeOnly := actionChip("a", "Active only", false, false, false)
	allProviders := actionChip("x", "All providers", false, false, false)
	body.WriteString(activeOnly + " " + allProviders + "\n\n")
	if o.offset > 0 {
		body.WriteString(stFaint.Render("↑ more") + "\n")
	}

	line := 5
	if o.offset > 0 {
		line++
	}
	rowLines := make([]int, 0, max(end-o.offset, 0))
	for i := o.offset; i < end; i++ {
		option := o.options[i]
		rowLines = append(rowLines, line)
		cursor := "  "
		if i == o.cursor {
			cursor = brandText("◆", i) + " "
		}
		check := stFaint.Render("○")
		if o.selected[option.id] {
			check = stGood.Render("●")
		}
		ready := stFaint.Render("not connected")
		if option.active {
			ready = stGood.Render("active")
		}
		models := fmt.Sprintf("%d models", option.models)
		row := fmt.Sprintf("%s%s  %s  %s · %s", cursor, check, padRight(option.name, 24), models, ready)
		body.WriteString(truncate(row, contentWidth) + "\n")
		line++
	}
	if end < len(o.options) {
		body.WriteString(stFaint.Render("↓ more") + "\n")
	}
	if len(o.options) == 0 {
		body.WriteString(stFaint.Render("No model providers are in the catalogue yet.") + "\n")
	}
	if o.errLine != "" {
		body.WriteString("\n" + stBad.Render("● "+o.errLine) + "\n")
	}
	apply := actionChip("enter", "Apply", true, false, false)
	cancel := actionChip("esc", "Cancel", false, false, false)
	body.WriteString("\n" + apply + " " + cancel)

	box := stModal.Width(boxWidth).Render(strings.TrimRight(body.String(), "\n"))
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(o.width, o.height, boxW, boxH)
	o.hits = o.hits[:0]
	for _, rowLine := range rowLines {
		o.hits = append(o.hits, hitRegion{x: boxX + 3, y: boxY + 2 + rowLine, w: boxW - 6, h: 1})
	}
	quickY := boxY + 5
	o.activeHit = hitRegion{x: boxX + 3, y: quickY, w: lipgloss.Width(activeOnly), h: 1}
	o.allHit = hitRegion{x: boxX + 4 + lipgloss.Width(activeOnly), y: quickY, w: lipgloss.Width(allProviders), h: 1}
	buttonY := boxY + boxH - 3
	o.applyHit = hitRegion{x: boxX + 3, y: buttonY, w: lipgloss.Width(apply), h: 1}
	o.cancelHit = hitRegion{x: boxX + 4 + lipgloss.Width(apply), y: buttonY, w: lipgloss.Width(cancel), h: 1}
	return tea.NewView(box)
}

func providerDisplayName(id string) string {
	switch strings.ToLower(id) {
	case "openai":
		return "OpenAI / Codex"
	case "anthropic":
		return "Anthropic / Claude"
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
