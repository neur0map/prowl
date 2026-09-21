package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

func TestProjectEnterShowsFullStatusAndSavings(t *testing.T) {
	app := New(&Client{BaseURL: "http://127.0.0.1:8788"}, true, "test")
	app.projects.data = projectsLoadedMsg{projects: []projectRow{{
		root:  "/work/prowl-agent",
		state: "ready",
		status: query.Status{
			Counts: store.Counts{
				Files: 12, Symbols: 340, Resources: 9, Chunks: 44,
				Edges: 80, Resolved: 60, External: 15, Unresolved: 5,
				Langs: map[string]int{"go": 12},
			},
			LastIndex: "2026-09-14T22:41:30Z",
			AIEnabled: true,
			Semantic:  query.SemanticCoverage{Chunks: 44, Embedded: 40, Remaining: 4},
			Savings:   query.Savings{Queries: 100, AnswerTokens: 632_000, SavedTokens: 3_100_000},
		},
	}}}
	app.projects.loaded = true
	app.projects.buildRows()

	if got := app.projects.list.rows[0].cells[5]; got != "~3.1M" {
		t.Fatalf("saved-token cell = %q; want ~3.1M", got)
	}
	_, _ = app.projects.Update(syntheticKey("enter"))
	overlay, ok := app.overlay.(*detailOverlay)
	if !ok {
		t.Fatalf("Enter opened %T; want project detail overlay", app.overlay)
	}
	_, _ = overlay.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	got := ansi.Strip(overlay.View().Content)
	for _, want := range []string{
		"Prowl status · prowl-agent",
		"TOKENS SAVED",
		"~3.1M tokens",
		"60 resolved · 15 external · 5 unresolved",
		"40/44 chunks embedded · 4 remaining",
		"go 12",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("project report missing %q:\n%s", want, got)
		}
	}
}

func TestOverviewShowsCombinedSavingsAcrossProjects(t *testing.T) {
	app := New(&Client{BaseURL: "http://127.0.0.1:8788"}, true, "test")
	app.overview.data = overviewMsg{
		project:         projectOverview{Name: "prowl-agent", State: "ready"},
		savings:         query.Savings{Queries: 151, SavedTokens: 4_200_000},
		savingsProjects: 3,
		strategy:        "smartest",
	}
	app.overview.loadedAt = time.Now()
	app.overview.setSize(120, 30)

	view := ansi.Strip(app.overview.View().Content)
	for _, want := range []string{"saved", "~4.2M", "151 answers · 3 projects"} {
		if !strings.Contains(view, want) {
			t.Fatalf("Home overview missing %q:\n%s", want, view)
		}
	}
}

// TestHomeCapacityLeadsWithTightestWindow proves the bar's remaining percent is
// 100 minus the busiest window across accounts, the block is led by whichever
// window is closest to its limit, each window shows its own used-percent, an
// account with measured routed tokens shows them as an observation, and a
// credit account shows its balance.
func TestHomeCapacityLeadsWithTightestWindow(t *testing.T) {
	app := New(&Client{BaseURL: "http://127.0.0.1:8788"}, true, "test")
	bal := 97.5
	app.overview.data = overviewMsg{
		accounts: []LoginUsage{
			{Provider: "anthropic", Name: "Claude", Windows: []LoginUsageWindow{
				{Key: "five_hour", Label: "5h session", Utilization: 20, TokensUsed: 50000},
				{Key: "seven_day", Label: "7d all models", Utilization: 53},
			}},
			{Provider: "openai", Name: "ChatGPT (Codex)", Windows: []LoginUsageWindow{
				{Key: "seven_day", Label: "7d all models", Utilization: 95, ResetsAt: "2999-01-01T00:00:00Z"},
			}},
			{Provider: "hyper", Name: "Hyper", Balance: &bal, Unit: "Hypercredits"},
		},
	}
	app.overview.loadedAt = time.Now()
	app.overview.setSize(120, 40)

	view := ansi.Strip(app.overview.View().Content)
	for _, want := range []string{
		"Subscription capacity",
		"5% left", // 100 - max(53, 95)
		"ChatGPT (Codex) 7d all models at 95% used", // the tightest window leads
		" 20% used",
		"50k routed via Prowl", // measured tokens shown as an observation
		" 95% used",
		"97.5 Hypercredits left",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("Home capacity view missing %q:\n%s", want, view)
		}
	}
	// The old fabricated token-total apology must be gone.
	if strings.Contains(view, "token totals not measured") || strings.Contains(view, " of ~") {
		t.Fatalf("Home capacity still shows a fabricated token total:\n%s", view)
	}
}

// TestHomeCapacityShowsPercentWithoutRoutedTokens proves a window with no
// measured routed tokens still shows its honest remaining percent and simply
// omits the routed-token observation, never an apology.
func TestHomeCapacityShowsPercentWithoutRoutedTokens(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.overview.data = overviewMsg{
		accounts: []LoginUsage{
			{Provider: "openai", Name: "ChatGPT (Codex)", Windows: []LoginUsageWindow{
				{Key: "seven_day", Label: "7d all models", Utilization: 95, TokensUsed: 0},
			}},
		},
	}
	app.overview.loadedAt = time.Now()
	app.overview.setSize(120, 40)

	view := ansi.Strip(app.overview.View().Content)
	if !strings.Contains(view, "5% left") || !strings.Contains(view, " 95% used") {
		t.Fatalf("Home capacity view missing the honest percent:\n%s", view)
	}
	if strings.Contains(view, "routed via Prowl") || strings.Contains(view, "token totals not measured") {
		t.Fatalf("Home capacity invented a token figure with no measured spend:\n%s", view)
	}
}

// TestHomeWithoutAccountsHintsProviders proves the capacity block collapses to
// a single pointer at where to connect a subscription when none is connected,
// rather than drawing a meaningless full bar.
func TestHomeWithoutAccountsHintsProviders(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.overview.data = overviewMsg{strategy: "smartest"}
	app.overview.loadedAt = time.Now()
	app.overview.setSize(120, 40)

	view := ansi.Strip(app.overview.View().Content)
	if !strings.Contains(view, "Connect a subscription on Providers (3) to see its allowance here.") {
		t.Fatalf("Home did not hint where to connect a subscription:\n%s", view)
	}
	// The section keeps its heading so the page shape is stable; only the
	// bar and its percentage must be absent.
	if strings.Contains(view, "% left") || strings.Contains(view, "━") {
		t.Fatalf("Home drew a capacity bar with no accounts connected:\n%s", view)
	}
}
func TestProjectsViewKeepsListRangeVisible(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.tab = TabProjects
	app.projects.data = projectsLoadedMsg{projects: []projectRow{{
		root: "/work/prowl-agent", state: "ready",
	}}}
	app.projects.loaded = true
	app.projects.buildRows()
	_, _ = app.Update(tea.WindowSizeMsg{Width: 100, Height: 24})

	view := ansi.Strip(app.View().Content)
	if !strings.Contains(view, "1-1 of 1") {
		t.Fatalf("project list range counter was clipped from the rendered app:\n%s", view)
	}
}

func TestProjectWithoutQueriesDoesNotClaimSavings(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.projects.data = projectsLoadedMsg{projects: []projectRow{{
		root: "/work/empty", state: "ready", status: query.Status{},
	}}}
	app.projects.buildRows()
	if got := app.projects.list.rows[0].cells[5]; got != "-" {
		t.Fatalf("saved-token cell = %q; want em dash before measured queries", got)
	}
	if report := ansi.Strip(projectStatusReport(app.projects.data.projects[0])); !strings.Contains(report, "none yet") {
		t.Fatalf("empty project report made no explicit zero-data statement:\n%s", report)
	}
}

func TestDetailOverlayEnterNeverChoosesASecondaryAction(t *testing.T) {
	overlay := newDetailOverlay("Account", "Details", "Ready", []action{
		{Key: "space", Label: "Withdraw from pool"},
		{Key: "d", Label: "Forget", Dangerous: true},
	})

	model, cmd := overlay.Update(syntheticKey("enter"))
	if model == nil || cmd != nil {
		t.Fatalf("Enter chose a secondary detail action: model=%T cmd=%v", model, cmd != nil)
	}

	model, cmd = overlay.Update(syntheticKey("space"))
	if model != nil || cmd == nil {
		t.Fatalf("explicit action key did not choose the action: model=%T cmd=%v", model, cmd != nil)
	}
	msg, ok := cmd().(actionChosenMsg)
	if !ok || msg.Key != "space" {
		t.Fatalf("chosen action = %#v; want space", msg)
	}
}

func TestDetailOverlayFitsShortTerminalAndClicksWhereDrawn(t *testing.T) {
	const width, height = 80, 12
	overlay := newDetailOverlay(
		"Claude Pro / Max",
		"Subscription allowance",
		strings.Repeat("allowance row\n", 12),
		[]action{{Key: "space", Label: "Enrol", Primary: true}},
	)
	_, _ = overlay.Update(tea.WindowSizeMsg{Width: width, Height: height})
	modal := overlay.View().Content
	rendered := ansi.Strip(mergeOverlay("", modal, width, height).Content)
	if !strings.Contains(rendered, "esc Close") {
		t.Fatalf("short terminal clipped the close control:\n%s", rendered)
	}

	var clickX, clickY = -1, -1
	for y, line := range strings.Split(rendered, "\n") {
		if x := strings.Index(line, "space Enrol"); x >= 0 {
			clickX, clickY = x-1, y // include the chip's left padding cell
			break
		}
	}
	if clickX < 0 {
		t.Fatalf("could not locate rendered primary action:\n%s", rendered)
	}
	model, cmd := overlay.Update(tea.MouseClickMsg{X: clickX, Y: clickY})
	if model != nil || cmd == nil {
		t.Fatalf("click on rendered action missed: model=%T cmd=%v", model, cmd != nil)
	}
	if msg, ok := cmd().(actionChosenMsg); !ok || msg.Key != "space" {
		t.Fatalf("rendered action click chose %#v; want space", msg)
	}
}
