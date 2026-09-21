package tui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The gateway is drawn from Prowl's warm Tokyo palette: sumi, charcoal,
// weathered shoji paper, torii ember and aged gold. Gradients stay inside the
// ember-to-gold range so motion reads as reflected city light, not neon.
var (
	colorSumi   = lipgloss.Color("#11100F")
	colorDeep   = lipgloss.Color("#0C0B0A")
	colorShoji  = lipgloss.Color("#191714")
	colorRaised = lipgloss.Color("#24201C")
	colorLine   = lipgloss.Color("#40382F")
	colorShadow = lipgloss.Color("#6E6255")
	colorMist   = lipgloss.Color("#C7B79C")
	colorMoon   = lipgloss.Color("#EDE6D6")

	colorEmber  = lipgloss.Color("#E0863C")
	colorGold   = lipgloss.Color("#D9A05B")
	colorMatcha = lipgloss.Color("#8FA879")
	colorBeni   = lipgloss.Color("#B96E78")
)

type rgb struct{ r, g, b int }

var (
	rgbEmber    = rgb{224, 134, 60}
	rgbGold     = rgb{217, 160, 91}
	rgbEmberDim = rgb{168, 106, 58}
	rgbLine     = rgb{64, 56, 47}
)

var (
	stApp      = lipgloss.NewStyle().Background(colorSumi).Foreground(colorMoon)
	stTitle    = lipgloss.NewStyle().Foreground(colorMoon).Bold(true)
	stSubtle   = lipgloss.NewStyle().Foreground(colorMist)
	stFaint    = lipgloss.NewStyle().Foreground(colorShadow)
	stGood     = lipgloss.NewStyle().Foreground(colorMatcha)
	stWarn     = lipgloss.NewStyle().Foreground(colorGold)
	stBad      = lipgloss.NewStyle().Foreground(colorBeni)
	stKey      = lipgloss.NewStyle().Foreground(colorGold)
	stHead     = lipgloss.NewStyle().Foreground(colorMoon).Bold(true)
	stCursor   = lipgloss.NewStyle().Foreground(colorEmber).Bold(true)
	stSelected = lipgloss.NewStyle().
			Background(colorRaised).
			Foreground(colorMoon).
			Bold(true)
	stPanel = lipgloss.NewStyle().
		Background(colorShoji).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorLine).
		Padding(0, 1)
	stToast = lipgloss.NewStyle().
		Background(colorRaised).
		Foreground(colorMoon).
		Padding(0, 1)
	stModal = lipgloss.NewStyle().
		Background(colorShoji).
		Foreground(colorMoon).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorEmber).
		Padding(1, 2)
)

func mixRGB(a, b rgb, t float64) color.Color {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	mix := func(x, y int) int { return int(float64(x) + (float64(y-x) * t)) }
	return lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", mix(a.r, b.r), mix(a.g, b.g), mix(a.b, b.b)))
}

// gradientText is deliberately small and local. It performs one warm sweep
// over a label; phase advances only during a user-triggered transition.
func gradientText(text string, from, to rgb, phase int, bold bool) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	period := max(len(runes)*2-2, 1)
	var out strings.Builder
	for i, r := range runes {
		at := (i + phase) % period
		if at >= len(runes) {
			at = period - at
		}
		t := 0.0
		if len(runes) > 1 {
			t = float64(at) / float64(len(runes)-1)
		}
		style := lipgloss.NewStyle().Foreground(mixRGB(from, to, t))
		if bold {
			style = style.Bold(true)
		}
		out.WriteString(style.Render(string(r)))
	}
	return out.String()
}

func brandText(text string, phase int) string {
	return gradientText(text, rgbEmber, rgbGold, phase, true)
}

func gradientRule(width, phase int) string {
	if width <= 0 {
		return ""
	}
	return gradientText(strings.Repeat("─", width), rgbEmberDim, rgbGold, phase, false)
}

// section is a quiet visual stop used inside an already composed surface.
func section(label string) string {
	return gradientText("──", rgbEmberDim, rgbGold, 0, false) + " " + stSubtle.Render(label)
}

// pill renders a state badge with its word - colour is decoration, never the
// only carrier.
func pill(word string) string {
	lower := strings.ToLower(word)
	style := stSubtle
	switch {
	case strings.HasPrefix(lower, "healthy"), lower == "enabled", lower == "ok",
		lower == "configured", lower == "signed in", lower == "success",
		lower == "free", lower == "current", lower == "installed":
		style = stGood
	case strings.HasPrefix(lower, "error"), lower == "disabled", lower == "failed",
		lower == "revoked", lower == "broken", lower == "expired":
		style = stBad
	case strings.HasPrefix(lower, "check"), lower == "unknown", lower == "pending",
		lower == "cooling", lower == "credits", lower == "degraded":
		style = stWarn
	}
	return style.Render("● " + word)
}

func keyCap(key string) string {
	return lipgloss.NewStyle().
		Background(colorRaised).
		Foreground(colorGold).
		Bold(true).
		Padding(0, 1).
		Render(key)
}

func actionLabel(key, label string, primary, dangerous bool) string {
	style := lipgloss.NewStyle().Foreground(colorMist)
	if primary {
		style = style.Foreground(colorMoon).Bold(true)
	}
	if dangerous {
		style = style.Foreground(colorBeni)
	}
	return keyCap(key) + " " + style.Render(label)
}

func actionChip(key, label string, primary, dangerous, hovered bool) string {
	fg, bg := colorMist, colorShoji
	if primary {
		fg, bg = colorMoon, colorRaised
	}
	if dangerous {
		fg = colorBeni
	}
	if hovered {
		fg, bg = colorMoon, colorRaised
	}
	content := stKey.Render(key) + " " + lipgloss.NewStyle().Foreground(fg).Bold(primary || hovered).Render(label)
	return lipgloss.NewStyle().Background(bg).Padding(0, 1).Render(content)
}

func bar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	return gradientText(strings.Repeat("━", filled), rgbEmber, rgbGold, 0, false) +
		stFaint.Render(strings.Repeat("─", width-filled))
}

func keyRow(k, v string, kw int) string {
	return stFaint.Render(padRight(k, kw)) + v
}

func padRight(s string, w int) string {
	if lipgloss.Width(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-lipgloss.Width(s))
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return ansi.Truncate(s, w, "…")
}

func clipLines(s string, width, height int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = truncate(lines[i], width)
	}
	return strings.Join(lines, "\n")
}

func fillLines(s string, width, height int) string {
	s = clipLines(s, width, height)
	lines := strings.Split(s, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = padRight(lines[i], width)
	}
	return strings.Join(lines, "\n")
}

func roundedPanel(title, body string, width int) string {
	width = max(width, 8)
	inner := width - 2
	label := " " + title + " "
	labelW := lipgloss.Width(label)
	if labelW > inner-2 {
		label = " " + truncate(title, inner-4) + " "
		labelW = lipgloss.Width(label)
	}
	top := stFaint.Render("╭─") + brandText(label, 0) +
		stFaint.Render(strings.Repeat("─", max(inner-labelW-1, 0))+"╮")
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines)+2)
	out = append(out, top)
	for _, line := range lines {
		line = truncate(line, inner-2)
		out = append(out, stFaint.Render("│")+" "+padRight(line, inner-2)+" "+stFaint.Render("│"))
	}
	out = append(out, stFaint.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(out, "\n")
}

type metric struct {
	label  string
	value  string
	detail string
}

// metricStrip is the flat stat row every page leads with: a small-caps label,
// a bold value and one faint detail line per column, with no box around it.
// Three rows instead of four, and the eye reads left to right like a
// dashboard stat bar rather than hopping between framed cards.
func metricStrip(items []metric, width int) string {
	if len(items) == 0 {
		return ""
	}
	colW := max((width-1)/len(items), 10)
	var labels, values, details strings.Builder
	for _, item := range items {
		cell := func(s string) string { return padRight(truncate(s, colW-2), colW) }
		labels.WriteString(cell(stFaint.Render(item.label)))
		values.WriteString(cell(stTitle.Render(item.value)))
		details.WriteString(cell(stSubtle.Render(item.detail)))
	}
	return truncate(labels.String(), width) + "\n" +
		truncate(values.String(), width) + "\n" +
		truncate(details.String(), width)
}

// toolbar lays chips out on one line, the way a web page keeps its primary
// controls above the table. Chips that do not fit are dropped from the right.
func toolbar(chips []string, width int) string {
	var out strings.Builder
	out.WriteString(" ")
	x := 1
	for i, chip := range chips {
		w := lipgloss.Width(chip)
		gap := 0
		if i > 0 {
			gap = 1
		}
		if x+gap+w > width {
			break
		}
		if gap > 0 {
			out.WriteString(" ")
		}
		out.WriteString(chip)
		x += gap + w
	}
	return padRight(out.String(), width)
}

// toolbarWrap is toolbar for a modal: chips that do not fit flow onto the
// next line instead of being dropped, so every action stays reachable.
func toolbarWrap(chips []string, width int) string {
	var lines []string
	var line strings.Builder
	x := 0
	for _, chip := range chips {
		w := lipgloss.Width(chip)
		if x > 0 && x+1+w > width {
			lines = append(lines, line.String())
			line.Reset()
			x = 0
		}
		if x > 0 {
			line.WriteString(" ")
			x++
		}
		line.WriteString(chip)
		x += w
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	return strings.Join(lines, "\n")
}

// filterChip is a toolbar control whose label carries its current value, so
// the active lens is visible without opening anything. A non-neutral value is
// rendered as primary so a narrowed view is never a mystery.
func filterChip(key, label, value string, active bool) string {
	return actionChip(key, label+": "+value, active, false, false)
}

// groupRule is a section divider inside a table: a marker, the group title,
// then a faint rule carrying the trailing summary at its right end.
func groupRule(marker, title, summary string, width int) string {
	head := marker + " " + stHead.Render(title) + " "
	tail := ""
	if summary != "" {
		tail = " " + stSubtle.Render(summary) + " "
	}
	rule := max(width-lipgloss.Width(head)-lipgloss.Width(tail), 0)
	return head + stFaint.Render(strings.Repeat("─", rule)) + tail
}

// emptyState is the centred placeholder a table shows when it has nothing to
// list: what is missing, then the one key that fixes it.
func emptyState(title, hint string, width int) string {
	return "\n" + centre(stHead.Render(title), width) + "\n" + centre(stSubtle.Render(hint), width)
}

func centre(s string, width int) string {
	pad := max((width-lipgloss.Width(s))/2, 0)
	return strings.Repeat(" ", pad) + s
}

func humanInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	out = append([]string{s}, out...)
	return strings.Join(out, ",")
}

func themeBackground() color.Color { return colorSumi }
func themeForeground() color.Color { return colorMoon }
