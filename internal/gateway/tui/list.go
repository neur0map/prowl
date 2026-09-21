package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// row is one line in a data view. Cells render left to right; the first cell
// carries the row's name. key is the opaque payload the owning screen reads.
type row struct {
	cells  []string
	styles []func(string) string
	key    any
	dim    bool
}

// list is the shared cursor-first data view. It owns filtering, scrolling and
// pointer geometry; screens retain their domain-specific actions.
type list struct {
	rows      []row
	headers   []string
	filter    string
	searching bool
	place     string
	cursor    int
	offset    int
	width     int
	height    int
	originX   int
	originY   int
	empty     string
	noMatch   string
}

func newList(placeholder string) list {
	return list{place: placeholder, noMatch: "No matches. Refine the search or press esc to clear it."}
}

func (l *list) setHeaders(headers ...string) { l.headers = headers }
func (l *list) setOrigin(x, y int)           { l.originX, l.originY = x, y }

func (l *list) filtered() []int {
	q := strings.ToLower(strings.TrimSpace(l.filter))
	out := make([]int, 0, len(l.rows))
	for i := range l.rows {
		if q == "" || rowMatches(&l.rows[i], q) {
			out = append(out, i)
		}
	}
	return out
}

func rowMatches(r *row, q string) bool {
	for _, c := range r.cells {
		if strings.Contains(strings.ToLower(ansi.Strip(c)), q) {
			return true
		}
	}
	return false
}

func (l *list) selected() *row {
	vis := l.filtered()
	if l.cursor < 0 || l.cursor >= len(vis) {
		return nil
	}
	return &l.rows[vis[l.cursor]]
}

func (l *list) selectRow(k any) {
	for i, idx := range l.filtered() {
		if l.rows[idx].key == k {
			l.cursor = i
			l.clamp()
			return
		}
	}
}

func (l *list) setRows(rows []row) {
	l.rows = rows
	vis := l.filtered()
	if l.cursor >= len(vis) {
		l.cursor = max(0, len(vis)-1)
	}
	l.clamp()
}

func (l *list) move(delta int) {
	vis := l.filtered()
	if len(vis) == 0 {
		return
	}
	l.cursor = min(max(l.cursor+delta, 0), len(vis)-1)
	l.clamp()
}

func (l *list) page(delta int) {
	vis := l.filtered()
	if len(vis) == 0 {
		return
	}
	l.cursor = min(max(l.cursor+delta*max(l.viewportRows(), 1), 0), len(vis)-1)
	l.clamp()
}

func (l *list) top() { l.cursor = 0; l.clamp() }
func (l *list) bot() {
	l.cursor = max(len(l.filtered())-1, 0)
	l.clamp()
}

func (l *list) chromeRows() int {
	rows := 3 // rounded top, search row, rounded bottom
	if len(l.headers) > 0 {
		rows++
	}
	return rows
}

func (l *list) viewportRows() int {
	return max(l.height-l.chromeRows(), 1)
}

func (l *list) clamp() {
	per := l.viewportRows()
	if l.cursor < l.offset {
		l.offset = l.cursor
	} else if l.cursor >= l.offset+per {
		l.offset = l.cursor - per + 1
	}
	l.offset = max(l.offset, 0)
}

func (l *list) typeFilter(k tea.KeyPressMsg) bool {
	if !l.searching {
		if k.String() == "esc" && l.filter != "" {
			l.filter = ""
			l.cursor, l.offset = 0, 0
			return true
		}
		return false
	}
	switch s := k.String(); s {
	case "esc":
		l.searching = false
		l.filter = ""
		l.cursor = 0
		l.clamp()
		return true
	case "enter":
		l.searching = false
		return true
	case "backspace":
		r := []rune(l.filter)
		if len(r) > 0 {
			l.filter = string(r[:len(r)-1])
		} else {
			l.searching = false
		}
		l.cursor = 0
		l.clamp()
		return true
	default:
		if len(s) == 1 && s[0] >= 32 && s[0] <= 126 {
			l.filter += s
			l.cursor = 0
			l.clamp()
			return true
		}
	}
	return false
}

func (l *list) startSearch()      { l.searching = true }
func (l *list) isSearching() bool { return l.searching }
func (l *list) rowStartY() int    { return l.originY + 2 + boolInt(len(l.headers) > 0) }
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func (l *list) containsX(x int) bool { return x >= l.originX && x < l.originX+l.width }
func (l *list) containsY(y int) bool { return y >= l.originY && y < l.originY+l.height }

// mouse applies pointer navigation using the geometry from the last render.
// First click selects; clicking the selected row again activates its primary
// action. This prevents destructive one-click surprises.
func (l *list) mouse(msg tea.Msg) (handled, activate bool) {
	switch m := msg.(type) {
	case tea.MouseWheelMsg:
		if !l.containsX(m.X) || !l.containsY(m.Y) {
			return false, false
		}
		switch m.Button {
		case tea.MouseWheelUp:
			l.move(-3)
		case tea.MouseWheelDown:
			l.move(3)
		}
		return true, false
	case tea.MouseMotionMsg:
		if !l.containsX(m.X) || !l.containsY(m.Y) {
			return false, false
		}
		rowAt := m.Y - l.rowStartY()
		next := l.offset + rowAt
		if rowAt >= 0 && rowAt < l.viewportRows() && next < len(l.filtered()) {
			l.cursor = next
			l.clamp()
			return true, false
		}
	case tea.MouseClickMsg:
		if !l.containsX(m.X) || !l.containsY(m.Y) {
			return false, false
		}
		if m.Y == l.originY+1 {
			l.startSearch()
			return true, false
		}
		rowAt := m.Y - l.rowStartY()
		if rowAt < 0 || rowAt >= l.viewportRows() {
			return true, false
		}
		next := l.offset + rowAt
		if next >= len(l.filtered()) {
			return true, false
		}
		l.searching = false
		if next == l.cursor {
			return true, true
		}
		l.cursor = next
		l.clamp()
		return true, false
	}
	return false, false
}

func (l *list) render() string {
	vis := l.filtered()
	width := max(l.width, 8)
	inner := width - 2
	title := strings.TrimSpace(strings.TrimPrefix(l.place, "Search "))
	if title == "" {
		title = "Items"
	}
	title = strings.ToUpper(title[:1]) + title[1:]
	topLabel := " " + title + " "
	top := stFaint.Render("╭─") + brandText(topLabel, l.cursor) +
		stFaint.Render(strings.Repeat("─", max(width-3-lipgloss.Width(topLabel), 0))+"╮")

	var search, hint string
	if l.searching {
		search = stCursor.Render("⌕") + " " +
			stHead.Render(truncate(l.filter, max(inner-22, 1))) +
			stCursor.Render("│")
		hint = stFaint.Render("esc clear · enter keep")
	} else if l.filter != "" {
		search = stFaint.Render("⌕") + " " +
			stHead.Render(truncate(l.filter, max(inner-22, 1)))
		hint = stFaint.Render("/ edit · esc clear")
	} else {
		subject := strings.ToLower(title[:1]) + title[1:]
		search = stFaint.Render("⌕") + " " +
			stSubtle.Render(truncate("Type / to filter "+subject, max(inner-18, 1)))
		hint = stFaint.Render("↑↓ move")
	}
	search = joinEdges(" "+search, hint+" ", inner)

	lines := []string{top, frameRow(search, inner)}
	if len(l.headers) > 0 {
		lines = append(lines, frameRow(" "+l.renderHeader()+" ", inner))
	}

	per := l.viewportRows()
	end := min(l.offset+per, len(vis))
	if len(vis) == 0 {
		msg := l.empty
		if msg == "" {
			if len(l.rows) > 0 {
				msg = l.noMatch
			} else {
				msg = "Nothing here yet."
			}
		}
		lines = append(lines, frameRow("  "+stFaint.Render(truncate(msg, max(inner-4, 1))), inner))
		for i := 1; i < per; i++ {
			lines = append(lines, frameRow("", inner))
		}
	} else {
		for i := l.offset; i < end; i++ {
			lines = append(lines, frameRow(l.renderRow(&l.rows[vis[i]], i == l.cursor), inner))
		}
		for i := end - l.offset; i < per; i++ {
			lines = append(lines, frameRow("", inner))
		}
	}

	rangeText := " 0 items "
	if len(vis) > 0 {
		rangeText = fmt.Sprintf(" %d-%d of %d ", l.offset+1, end, len(vis))
	}
	bottom := stFaint.Render("╰─") + stSubtle.Render(rangeText) +
		stFaint.Render(strings.Repeat("─", max(width-3-lipgloss.Width(rangeText), 0))+"╯")
	lines = append(lines, bottom)
	return strings.Join(lines, "\n")
}

func (l *list) colWidths(n int) []int {
	inner := max(l.width-6, 8)
	if n <= 1 {
		return []int{inner}
	}
	first := inner * 32 / 100
	rest := (inner - first) / (n - 1)
	if rest < 7 {
		first = max(inner-rest*(n-1), 8)
		rest = max((inner-first)/(n-1), 4)
	}
	widths := make([]int, n)
	widths[0] = first
	for i := 1; i < n; i++ {
		widths[i] = rest
	}
	return widths
}

func (l *list) renderHeader() string {
	widths := l.colWidths(len(l.headers))
	cells := make([]string, 0, len(l.headers))
	for i, h := range l.headers {
		cells = append(cells, stFaint.Render(padRight(truncate(h, widths[i]), widths[i])))
	}
	return padRight("  "+strings.Join(cells, ""), max(l.width-4, 1))
}

func (l *list) renderRow(r *row, selected bool) string {
	widths := l.colWidths(len(r.cells))
	cells := make([]string, 0, len(r.cells))
	for i, c := range r.cells {
		t := truncate(c, widths[i])
		if i < len(r.styles) && r.styles[i] != nil {
			cells = append(cells, r.styles[i](t)+strings.Repeat(" ", max(widths[i]-lipgloss.Width(t), 0)))
			continue
		}
		style := lipgloss.NewStyle().Foreground(colorMoon)
		if r.dim {
			style = style.Foreground(colorShadow)
		} else if i > 0 {
			style = style.Foreground(colorMist)
		}
		cells = append(cells, style.Render(padRight(t, widths[i])))
	}
	contentW := max(l.width-6, 1)
	if selected {
		body := stSelected.Render(padRight(strings.Join(stripStyles(cells), ""), contentW))
		return " " + brandText("◆", l.cursor) + " " + body + " "
	}
	return "   " + strings.Join(cells, "") + " "
}

func stripStyles(cells []string) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		out = append(out, ansi.Strip(c))
	}
	return out
}
