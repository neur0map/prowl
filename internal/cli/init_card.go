package cli

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// renderInitCard is the bordered, colored "ready" panel shown after a terminal
// `init`: what got indexed, the language mix, the integrations wired, and the
// first commands to run. It reuses the status-card palette and helpers so the
// two cards read as one product, and replaces the old wall of per-file plan
// lines with a compact, scannable summary.
func renderInitCard(name string, indexed, symbols, edges, resolved int, langs map[string]int, integrations []string, aiOn bool) string {
	var L []string
	L = append(L, stTitle.Render("prowl")+"  "+stGood.Render("ready"))
	if name != "" {
		L = append(L, stName.Render(truncate(name, cardW)))
	}
	L = append(L, "")

	L = append(L, section("INDEXED"))
	L = append(L, "  "+kv("files", comma(indexed), 9, 12)+kv("symbols", comma(symbols), 9, 0))
	if edges > 0 && resolved > 0 {
		frac := float64(resolved) / float64(edges)
		L = append(L, "  "+kv("edges", comma(edges), 9, 12)+stLabel.Render(padRight("resolved", 9))+
			bar(frac, 12, hexAccent)+stNum.Render(fmt.Sprintf(" %3.0f%%", frac*100)))
	} else if edges > 0 {
		L = append(L, "  "+kv("edges", comma(edges), 9, 0))
	}
	ai := stFaint.Render("off")
	if aiOn {
		ai = stGood.Render("on")
	}
	L = append(L, "  "+stLabel.Render(padRight("ai", 9))+ai)
	L = append(L, "")

	if len(langs) > 0 {
		L = append(L, section("LANGUAGES"))
		L = append(L, langBars(langs)...)
		L = append(L, "")
	}

	if len(integrations) > 0 {
		L = append(L, section("INTEGRATIONS"))
		L = append(L, "  "+stVal.Render(truncate(strings.Join(integrations, " "), cardW-2)))
		L = append(L, "  "+stFaint.Render("+ prowl skills installed for each agent"))
		L = append(L, "")
	}

	L = append(L, section("NEXT"))
	for _, row := range [][2]string{
		{"overview", "a map of this project"},
		{"find <name>", "locate any symbol"},
		{"graph", "interactive dependency graph"},
		{"docs add <url>", "index external docs"},
	} {
		L = append(L, "  "+stNum.Render(padRight(row[0], 16))+stFaint.Render(row[1]))
	}
	L = append(L, "")
	L = append(L, stGood.Render("●")+stFaint.Render(" no server to run · .prowl/ is gitignored"))

	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).Padding(0, 2)
	return box.Render(lipgloss.JoinVertical(lipgloss.Left, L...))
}
