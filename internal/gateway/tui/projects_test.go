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
	for _, want := range []string{"Est. tokens saved", "~4.2M", "151 answers · 3 projects"} {
		if !strings.Contains(view, want) {
			t.Fatalf("Home overview missing %q:\n%s", want, view)
		}
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
