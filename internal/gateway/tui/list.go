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
//
// A header row opens a group: cells[0] is its title, summary its trailing
// note, and every following row with the same group belongs to it until the
// next header. Groups collapse and expand in place, so a long table reads as
// sections rather than one wall of rows.
type row struct {
	id      string
	cells   []string
	styles  []func(string) string
	key     any
	dim     bool
	header  bool
	group   string
	summary string
}

// list is the shared cursor-first data view. It owns filtering, grouping,
// scrolling and pointer geometry; screens retain their domain-specific actions.
//
// It draws as a flat table - a search line, a small-caps header, the rows and
// a range line - with no frame, so the page's own rules carry the structure.
type list struct {
	rows      []row
	headers   []string
	weights   []int
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
	emptyHint string
	noMatch   string
	collapsed map[string]bool
}

func newList(placeholder string) list {
	return list{
		place:     placeholder,
		noMatch:   "No matches. Refine the search or press esc to clear it.",
		collapsed: map[string]bool{},
	}
}

// setHeaders names the columns; weights (optional) are the relative widths,
// one per column, so a name column can be wide and a state column narrow.
func (l *list) setHeaders(headers ...string) { l.headers = headers }
func (l *list) setWeights(weights ...int)    { l.weights = weights }
func (l *list) setOrigin(x, y int)           { l.originX, l.originY = x, y }

// filtered returns the indices of the rows on screen: text-filtered, with the
// children of a collapsed group hidden and a header shown only while at least
// one of its children matches the search.
func (l *list) filtered() []int {
	q := strings.ToLower(strings.TrimSpace(l.filter))
	out := make([]int, 0, len(l.rows))
	for i := 0; i < len(l.rows); i++ {
		r := &l.rows[i]
		if !r.header {
			if q == "" || rowMatches(r, q) {
				out = append(out, i)
			}
			continue
		}
		// A header: gather its children, decide visibility, then either emit
		// the children (expanded) or skip them (collapsed or unmatched).
		end := i + 1
		for end < len(l.rows) && !l.rows[end].header {
			end++
		}
		children := make([]int, 0, end-i-1)
		for j := i + 1; j < end; j++ {
			if q == "" || rowMatches(&l.rows[j], q) {
				children = append(children, j)
			}
		}
		if q == "" || len(children) > 0 {
			out = append(out, i)
			// A live search reveals matches wherever they are: a collapsed
			// group is folded for browsing, not for hiding a hit from a query.
			if q != "" || !l.collapsed[r.group] {
				out = append(out, children...)
			}
		}
		i = end - 1
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

// groupSize reports how many rows a group holds and how many the search keeps
// visible, for the summary a header shows while collapsed or filtered.
func (l *list) groupSize(group string) (total, matching int) {
	q := strings.ToLower(strings.TrimSpace(l.filter))
	for i := range l.rows {
		r := &l.rows[i]
		if r.header || r.group != group {
			continue
		}
		total++
		if q == "" || rowMatches(r, q) {
			matching++
		}
	}
	return total, matching
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

// selectID moves the cursor to the row with this id when it is visible.
func (l *list) selectID(id string) bool {
	if id == "" {
		return false
	}
	for i, idx := range l.filtered() {
		if l.rows[idx].id == id {
			l.cursor = i
			l.clamp()
			return true
		}
	}
	return false
}

// setRows replaces the rows and keeps the cursor on the same row when it is
// still there (by id), so a rebuild after a toggle does not jump the
// selection to wherever the old index now lands.
func (l *list) setRows(rows []row) {
	keep := ""
	if cur := l.selected(); cur != nil {
		keep = cur.id
	}
	l.rows = rows
	if l.selectID(keep) {
		return
	}
	vis := l.filtered()
	if l.cursor >= len(vis) {
		l.cursor = max(0, len(vis)-1)
	}
	l.clamp()
}

// toggleGroup collapses or expands the group under the cursor (or the group
// the selected row belongs to) and keeps the cursor on that header.
func (l *list) toggleGroup() bool {
	cur := l.selected()
	if cur == nil || cur.group == "" {
		return false
	}
	group := cur.group
	l.collapsed[group] = !l.collapsed[group]
	l.selectID("group:" + group)
	return true
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
	rows := 2 // search line, range line
	if len(l.headers) > 0 {
		rows++
	}
	return rows
}

func (l *list) viewportRows() int {
	return max(l.height-l.chromeRows(), 1)
}

func (l *list) clamp() {
	if l.height == 0 {
		// Not laid out yet: a one-row viewport would scroll the top rows
		// away before the first render sizes the list. Leave the offset alone.
		l.offset = max(min(l.offset, l.cursor), 0)
		return
	}
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
		l.firstMatch()
		return true
	case "up", "down":
		// Moving while typing keeps the search open: the cursor walks the
		// matches without leaving the box.
		if s == "up" {
			l.move(-1)
		} else {
			l.move(1)
		}
		return true
	default:
		if len(s) == 1 && s[0] >= 32 && s[0] <= 126 {
			l.filter += s
			l.firstMatch()
			return true
		}
	}
	return false
}

// firstMatch puts the cursor on the first row a search kept, skipping the
// group header above it so enter or space acts on the match, not the group.
func (l *list) firstMatch() {
	l.cursor, l.offset = 0, 0
	vis := l.filtered()
	if len(vis) > 1 && l.rows[vis[0]].header && strings.TrimSpace(l.filter) != "" {
		l.cursor = 1
	}
	l.clamp()
}

func (l *list) startSearch()      { l.searching = true }
func (l *list) isSearching() bool { return l.searching }
func (l *list) filtering() bool   { return l.searching || strings.TrimSpace(l.filter) != "" }
func (l *list) clearSearch() {
	l.searching = false
	l.filter = ""
	l.cursor, l.offset = 0, 0
}
func (l *list) rowStartY() int { return l.originY + 1 + boolInt(len(l.headers) > 0) }
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
		if m.Y == l.originY {
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
	subject := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(l.place, "Search ")))
	if subject == "" {
		subject = "rows"
	}

	var search, hint string
	switch {
	case l.searching:
		search = stCursor.Render("⌕") + " " +
			stHead.Render(truncate(l.filter, max(width-26, 1))) +
			stCursor.Render("▏")
		hint = stFaint.Render("esc clear · enter keep")
	case l.filter != "":
		search = stFaint.Render("⌕") + " " +
			stHead.Render(truncate(l.filter, max(width-26, 1)))
		hint = stFaint.Render("/ edit · esc clear")
	default:
		search = stFaint.Render("⌕") + " " +
			stFaint.Render(truncate("Type / to search "+subject, max(width-14, 1)))
		hint = stFaint.Render("↑↓ move")
	}
	lines := []string{joinEdges(" "+search, hint+" ", width)}
	if len(l.headers) > 0 {
		lines = append(lines, l.renderHeader())
	}

	per := l.viewportRows()
	end := min(l.offset+per, len(vis))
	if len(vis) == 0 {
		msg, hint := l.empty, l.emptyHint
		if msg == "" {
			if len(l.rows) > 0 {
				msg = l.noMatch
			} else {
				msg = "Nothing here yet."
			}
		}
		placeholder := strings.Split(emptyState(msg, hint, width), "\n")
		for i := range per {
			if i < len(placeholder) {
				lines = append(lines, truncate(placeholder[i], width))
				continue
			}
			lines = append(lines, "")
		}
	} else {
		for i := l.offset; i < end; i++ {
			lines = append(lines, l.renderRow(&l.rows[vis[i]], i == l.cursor))
		}
		for i := end - l.offset; i < per; i++ {
			lines = append(lines, "")
		}
	}

	rangeText := "no rows"
	if len(vis) > 0 {
		rangeText = fmt.Sprintf("%d-%d of %d", l.offset+1, end, len(vis))
	}
	lines = append(lines, " "+stFaint.Render(rangeText))
	return strings.Join(lines, "\n")
}

// colWidths splits the content width across the columns. With weights the
// split is proportional; otherwise the name column takes a third and the rest
// share what remains.
func (l *list) colWidths(n int) []int {
	inner := max(l.width-4, 8)
	if n <= 1 {
		return []int{inner}
	}
	widths := make([]int, n)
	if len(l.weights) == n {
		total := 0
		for _, w := range l.weights {
			total += max(w, 1)
		}
		used := 0
		for i := 1; i < n; i++ {
			widths[i] = max(inner*max(l.weights[i], 1)/total, 5)
			used += widths[i]
		}
		widths[0] = max(inner-used, 8)
		return widths
	}
	first := inner * 34 / 100
	rest := (inner - first) / (n - 1)
	if rest < 7 {
		first = max(inner-rest*(n-1), 8)
		rest = max((inner-first)/(n-1), 4)
	}
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
		cells = append(cells, stFaint.Render(padRight(truncate(strings.ToUpper(h), widths[i]-1), widths[i])))
	}
	return padRight("   "+strings.Join(cells, ""), max(l.width, 1))
}

func (l *list) renderRow(r *row, selected bool) string {
	contentW := max(l.width-4, 1)
	if r.header {
		marker := stSubtle.Render("▾")
		if l.collapsed[r.group] {
			marker = stSubtle.Render("▸")
		}
		title := r.cells[0]
		summary := r.summary
		if total, matching := l.groupSize(r.group); l.filter != "" && matching != total {
			summary = fmt.Sprintf("%d of %d match", matching, total)
		}
		line := groupRule(marker, title, summary, contentW)
		if selected {
			return " " + brandText("◆", l.cursor) + " " + line + " "
		}
		return "   " + line + " "
	}

	widths := l.colWidths(len(r.cells))
	cells := make([]string, 0, len(r.cells))
	for i, c := range r.cells {
		// Style the whole value first: value-keyed styles (a pill that colours
		// "healthy") must see the full word, not a truncated stub.
		var styled string
		if i < len(r.styles) && r.styles[i] != nil {
			styled = r.styles[i](c)
		} else {
			style := lipgloss.NewStyle().Foreground(colorMoon)
			if r.dim {
				style = style.Foreground(colorShadow)
			} else if i > 0 {
				style = style.Foreground(colorMist)
			}
			styled = style.Render(c)
		}
		t := truncate(styled, widths[i]-1)
		cells = append(cells, t+strings.Repeat(" ", max(widths[i]-lipgloss.Width(t), 0)))
	}
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
