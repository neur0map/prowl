package cli

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/selfupdate"
)

// Card geometry. Content is laid out to exactly cardW columns so nothing wraps
// and every column aligns; the border and padding sit outside that.
const (
	cardW    = 60
	langBarW = 32
)

const (
	hexAccent = "#89b4fa"
	hexGood   = "#a6e3a1"
)

var (
	cAccent = lipgloss.Color(hexAccent) // blue: structure, counts
	cGood   = lipgloss.Color(hexGood)   // green: savings
	cWarn   = lipgloss.Color("#f9e2af") // yellow: attention
	cText   = lipgloss.Color("#cdd6f4") // primary text
	cSub    = lipgloss.Color("#a6adc8") // section headers
	cMuted  = lipgloss.Color("#9399b2") // labels
	cFaint  = lipgloss.Color("#585b70") // rules, hints

	stTitle   = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stName    = lipgloss.NewStyle().Bold(true).Foreground(cText)
	stSection = lipgloss.NewStyle().Bold(true).Foreground(cSub)
	stGutter  = lipgloss.NewStyle().Foreground(cAccent)
	stLabel   = lipgloss.NewStyle().Foreground(cMuted)
	stNum     = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stVal     = lipgloss.NewStyle().Foreground(cText)
	stBig     = lipgloss.NewStyle().Bold(true).Foreground(cGood)
	stGood    = lipgloss.NewStyle().Foreground(cGood)
	stWarn    = lipgloss.NewStyle().Foreground(cWarn)
	stFaint   = lipgloss.NewStyle().Foreground(cFaint)
)

// isTTY reports whether f is an interactive terminal (so we render the card; a
// pipe or file gets plain text instead).
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func bar(frac float64, width int, fill string) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(frac*float64(width) + 0.5)
	if n > width {
		n = width
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(fill)).Render(strings.Repeat("█", n)) +
		stFaint.Render(strings.Repeat("░", width-n))
}

// humanTokens renders a token count compactly (1.2M, 9.4k, 530).
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// comma inserts thousands separators into a non-negative count.
func comma(n int) string {
	s := strconv.Itoa(n)
	if n < 1000 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func relTime(meta string) string {
	n, err := strconv.ParseInt(meta, 10, 64)
	if err != nil || n == 0 {
		return "never"
	}
	d := time.Since(time.Unix(n, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// truncate limits s to w display columns, ending in an ellipsis when cut.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// truncateLeft keeps the tail of s (the informative end of a path) within w.
func truncateLeft(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return "…" + string(r[len(r)-(w-1):])
}

// padRight fits s to exactly w display columns (truncating or right-padding),
// operating on plain text so a later style wrap preserves alignment.
func padRight(s string, w int) string {
	s = truncate(s, w)
	if d := lipgloss.Width(s); d < w {
		s += strings.Repeat(" ", w-d)
	}
	return s
}

// padLeft right-aligns s within w display columns.
func padLeft(s string, w int) string {
	s = truncate(s, w)
	if d := lipgloss.Width(s); d < w {
		s = strings.Repeat(" ", w-d) + s
	}
	return s
}

// section renders an accent-gutter header with a faint rule filling the width.
func section(title string) string {
	head := stGutter.Render("▌") + " " + stSection.Render(title)
	dashes := cardW - 3 - lipgloss.Width(title)
	if dashes < 0 {
		dashes = 0
	}
	return head + " " + stFaint.Render(strings.Repeat("─", dashes))
}

// cleanVersion collapses a nightly "SHA-SHA" build id to a single SHA.
func cleanVersion(v string) string {
	if v == "" {
		return "dev"
	}
	if i := strings.IndexByte(v, '-'); i > 0 && v[:i] == v[i+1:] {
		return v[:i]
	}
	return v
}

// collapseHome shortens a path under the home directory to ~/….
func collapseHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && strings.HasPrefix(p, h) {
		return "~" + p[len(h):]
	}
	return p
}

// kv renders a "label value" pair with fixed label and value widths so columns
// line up across rows. valW <= 0 leaves the value unpadded (a trailing column).
func kv(label, val string, labelW, valW int) string {
	if valW <= 0 {
		return stLabel.Render(padRight(label, labelW)) + stNum.Render(val)
	}
	return stLabel.Render(padRight(label, labelW)) + stNum.Render(padRight(val, valW))
}

// langBars renders the top languages as aligned proportional bars.
func langBars(langs map[string]int) []string {
	type lc struct {
		l string
		n int
	}
	arr := make([]lc, 0, len(langs))
	maxN := 1
	for l, n := range langs {
		arr = append(arr, lc{l, n})
		if n > maxN {
			maxN = n
		}
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].n != arr[j].n {
			return arr[i].n > arr[j].n
		}
		return arr[i].l < arr[j].l
	})
	if len(arr) > 6 {
		arr = arr[:6]
	}
	countW := 1
	for _, e := range arr {
		if w := len(comma(e.n)); w > countW {
			countW = w
		}
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		row := "  " + stVal.Render(padRight(e.l, 11)) + " " +
			bar(float64(e.n)/float64(maxN), langBarW, hexAccent) + " " +
			stFaint.Render(padLeft(comma(e.n), countW))
		out = append(out, row)
	}
	return out
}

// renderStatusCard builds the bordered, colored status card for a terminal.
func renderStatusCard(version, root, name string, st query.Status, upd selfupdate.Result, perProject []projSaving, combined query.Savings) string {
	c := st.Counts
	var L []string

	// Header: name, version, and home-relative path.
	L = append(L, stTitle.Render("prowl")+"  "+stFaint.Render(cleanVersion(version)))
	L = append(L, stName.Render(truncate(name, cardW)))
	L = append(L, stFaint.Render(truncateLeft(collapseHome(root), cardW)))
	L = append(L, "")

	// INDEX: a two-column key/value grid.
	resolvedFrac := 0.0
	if c.Edges > 0 {
		resolvedFrac = float64(c.Resolved) / float64(c.Edges)
	}
	ai := stFaint.Render("off")
	if st.AIEnabled {
		ai = stGood.Render("on")
	}
	L = append(L, section("INDEX"))
	L = append(L, "  "+kv("files", comma(c.Files), 9, 12)+kv("symbols", comma(c.Symbols), 9, 0))
	L = append(L, "  "+kv("edges", comma(c.Edges), 9, 12)+stLabel.Render(padRight("resolved", 9))+
		bar(resolvedFrac, 12, hexAccent)+stNum.Render(fmt.Sprintf(" %3.0f%%", resolvedFrac*100)))
	L = append(L, "  "+stLabel.Render(padRight("updated", 9))+stVal.Render(padRight(relTime(st.LastIndex), 12))+
		stLabel.Render(padRight("ai", 9))+ai)
	L = append(L, "")

	// LANGUAGES: aligned proportional bars.
	L = append(L, section("LANGUAGES"))
	L = append(L, langBars(c.Langs)...)
	L = append(L, "")

	// TOKENS SAVED: the hero stat.
	sv := st.Savings
	L = append(L, section("TOKENS SAVED"))
	switch {
	case sv.SavedTokens > 0:
		denom := sv.SavedTokens + sv.AnswerTokens
		frac := 0.0
		if denom > 0 {
			frac = float64(sv.SavedTokens) / float64(denom)
		}
		L = append(L, "  "+stBig.Render("~"+humanTokens(sv.SavedTokens))+stFaint.Render("  saved by cited retrieval"))
		L = append(L, "  "+bar(frac, cardW-2, hexGood))
		if sv.Queries > 0 {
			avg := sv.AnswerTokens / sv.Queries
			L = append(L, "  "+stFaint.Render(truncate(fmt.Sprintf("across %s answers · ~%s tokens each vs reading files",
				comma(int(sv.Queries)), humanTokens(avg)), cardW-2)))
		}
	case combined.SavedTokens > 0:
		L = append(L, "  "+stFaint.Render("none in this project yet; ~"+humanTokens(combined.SavedTokens)+" across your projects"))
	default:
		L = append(L, "  "+stFaint.Render("no queries yet; savings grow as your agent uses prowl"))
	}

	// ACROSS YOUR PROJECTS: per-project savings with a combined total.
	if len(perProject) >= 1 && combined.Queries > sv.Queries {
		L = append(L, "")
		L = append(L, section("ACROSS YOUR PROJECTS"))
		shown := perProject
		if len(shown) > 4 {
			shown = shown[:4]
		}
		for _, p := range shown {
			L = append(L, "  "+stLabel.Render(padRight(p.Name, 22))+stNum.Render("~"+humanTokens(p.Saved)))
		}
		if len(perProject) >= 2 {
			L = append(L, "  "+stFaint.Render(strings.Repeat("─", 30)))
			L = append(L, "  "+stVal.Render(padRight("combined", 22))+stBig.Render("~"+humanTokens(combined.SavedTokens)))
		}
	}

	// Footer: status dot and where to verify.
	L = append(L, "")
	switch {
	case upd.Available:
		L = append(L, stWarn.Render("● update available")+stFaint.Render("  ·  run ")+stNum.Render("prowl update"))
	case upd.Checked:
		L = append(L, stGood.Render("●")+stFaint.Render(" up to date"))
	}
	L = append(L, stFaint.Render("measure it yourself"))
	L = append(L, stFaint.Render(truncate(tokensDocURL, cardW)))

	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).Padding(0, 2)
	return box.Render(lipgloss.JoinVertical(lipgloss.Left, L...))
}
