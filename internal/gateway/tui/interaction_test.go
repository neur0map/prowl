package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestListPointerSelectsThenActivates(t *testing.T) {
	view := newList("Search")
	view.width = 60
	view.height = 10
	view.setHeaders("Name", "State")
	view.setRows([]row{
		{cells: []string{"one", "ready"}, key: "one"},
		{cells: []string{"two", "ready"}, key: "two"},
	})
	view.setOrigin(10, 5)

	secondRowY := view.rowStartY() + 1
	handled, activate := view.mouse(tea.MouseClickMsg{X: 12, Y: secondRowY})
	if !handled || activate {
		t.Fatalf("first click = handled %v, activate %v; want select only", handled, activate)
	}
	if selected := view.selected(); selected == nil || selected.key != "two" {
		t.Fatalf("selected = %#v; want second row", selected)
	}
	view.startSearch()
	handled, activate = view.mouse(tea.MouseClickMsg{X: 12, Y: secondRowY})

	if !handled || !activate {
		t.Fatalf("second click = handled %v, activate %v; want activation", handled, activate)
	}
	if view.isSearching() {
		t.Fatal("activating a search result left text entry focused")
	}
}

func TestListPointerMotionTracksHoveredRow(t *testing.T) {
	view := newList("Search")
	view.width = 60
	view.height = 10
	view.setHeaders("Name", "State")
	view.setRows([]row{
		{cells: []string{"one", "ready"}, key: "one"},
		{cells: []string{"two", "ready"}, key: "two"},
	})
	view.setOrigin(4, 3)

	handled, activate := view.mouse(tea.MouseMotionMsg{X: 8, Y: view.rowStartY() + 1})
	if !handled || activate {
		t.Fatalf("hover = handled %v, activate %v; want selection only", handled, activate)
	}
	if selected := view.selected(); selected == nil || selected.key != "two" {
		t.Fatalf("selected = %#v; want hovered row", selected)
	}
}

func TestPointerNavigationAndActionOverflow(t *testing.T) {
	app := New(&Client{BaseURL: "http://127.0.0.1:8788"}, true, "test")
	_, _ = app.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = app.View()

	var providerHit *hitRegion
	for i := range app.hits {
		if app.hits[i].kind == hitNavigation && app.hits[i].tab == TabProviders {
			providerHit = &app.hits[i]
			break
		}
	}
	if providerHit == nil {
		t.Fatal("provider navigation has no pointer target")
	}
	_, _ = app.Update(tea.MouseClickMsg{X: providerHit.x, Y: providerHit.y})
	if app.tab != TabProviders {
		t.Fatalf("tab = %v; want Providers", app.tab)
	}

	app.tab = TabKeys
	app.applyLayout()
	_ = app.View()
	var moreHit *hitRegion
	for i := range app.hits {
		if app.hits[i].kind == hitAction && app.hits[i].key == "__more__" {
			moreHit = &app.hits[i]
			break
		}
	}
	if moreHit == nil {
		t.Fatal("overflowed actions have no More pointer target")
	}
	_, _ = app.Update(tea.MouseClickMsg{X: moreHit.x, Y: moreHit.y})
	if _, ok := app.overlay.(*actionOverlay); !ok {
		t.Fatalf("overlay = %T; want actionOverlay", app.overlay)
	}
}

func TestFormPointerMovesFocusAndCancels(t *testing.T) {
	form := newForm("Add endpoint", "Base URL", "API key")
	_, _ = form.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_ = form.View()

	second := form.fieldHits[1]
	model, _ := form.Update(tea.MouseClickMsg{X: second.x, Y: second.y})
	if model == nil || form.cursor != 1 {
		t.Fatalf("pointer focus = model %T cursor %d; want second field", model, form.cursor)
	}

	cancel := form.cancelHit
	model, _ = form.Update(tea.MouseClickMsg{X: cancel.x, Y: cancel.y})
	if model != nil {
		t.Fatalf("cancel click left overlay open as %T", model)
	}
}

func TestOAuthSignInStartsAndOpensBrowser(t *testing.T) {
	var started bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/signin" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode sign-in body: %v", err)
		}
		if body["provider"] != "openai" {
			t.Fatalf("provider = %q; want openai", body["provider"])
		}
		started = true
		_ = json.NewEncoder(w).Encode(SignInSession{
			ID: "flow-1", Provider: "openai", URL: "https://example.test/oauth",
			UserCode: "ABCD-EFGH", State: "pending",
		})
	}))
	defer server.Close()

	opened := ""
	originalOpenURL := openURL
	openURL = func(rawURL string) error {
		opened = rawURL
		return nil
	}
	t.Cleanup(func() { openURL = originalOpenURL })

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	app.logins.data.platforms = []SignInPlatform{{ID: "openai", Name: "ChatGPT", RoutesTo: "openai"}}
	app.logins.loaded = true
	app.logins.buildRows()

	_, start := app.logins.Update(tea.KeyPressMsg{Text: "s", Code: 's'})
	if start == nil {
		t.Fatal("sign-in key returned no command")
	}
	msg := start()
	if !started {
		t.Fatal("sign-in command did not call POST /api/signin")
	}
	startedMsg, ok := msg.(signInStartedMsg)
	if !ok {
		t.Fatalf("sign-in command returned %T; want signInStartedMsg", msg)
	}

	_, follow := app.logins.Update(startedMsg)
	followMsg := follow()
	batch, ok := followMsg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("started flow returned %T; want poll/browser batch", followMsg)
	}
	if len(batch) != 2 {
		t.Fatalf("started flow commands = %d; want one poll and one browser open", len(batch))
	}
	for _, cmd := range batch {
		_ = cmd()
	}
	if opened != "https://example.test/oauth" {
		t.Fatalf("opened URL = %q; want OAuth URL", opened)
	}
	if app.logins.active == nil || app.logins.active.ID != "flow-1" {
		t.Fatalf("active session = %#v; want flow-1", app.logins.active)
	}
}

func TestModelsTabControlsSelectedModel(t *testing.T) {
	var path string
	var patch map[string]bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Method != http.MethodPatch {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			t.Fatalf("decode model patch: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer server.Close()

	app := New(&Client{BaseURL: server.URL, HTTP: server.Client()}, true, "test")
	app.routing.defaultModels = true
	app.routing.loaded = true
	app.routing.data.models = []ModelRow{
		{ID: 41, Platform: "anthropic", ModelID: "claude-opus", DisplayName: "Claude Opus", Enabled: true, FallbackEnabled: true, Available: true},
		{ID: 42, Platform: "openai", ModelID: "gpt-codex", DisplayName: "GPT Codex", Available: true},
	}
	app.routing.buildRows()
	app.routing.list.move(1)

	_, command := app.routing.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	if command == nil {
		t.Fatal("selected model returned no update command")
	}
	if _, ok := command().(doneMsg); !ok {
		t.Fatal("selected model did not complete its update")
	}
	if path != "/api/models/42" {
		t.Fatalf("patched %q; want selected model /api/models/42", path)
	}
	if !patch["fallbackEnabled"] || !patch["enabled"] {
		t.Fatalf("model selection patch = %#v; want selected and catalogue-enabled", patch)
	}
}

func TestConsoleDisablesMouseCaptureForNativeSelection(t *testing.T) {
	app := New(&Client{BaseURL: "http://127.0.0.1:8788"}, true, "test")
	_, _ = app.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if got := app.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("mouse mode = %v; want MouseModeNone so the terminal keeps native text selection without Shift", got)
	}
}

func TestHomeDaemonToggleControlsExitPolicy(t *testing.T) {
	app := New(&Client{BaseURL: "http://127.0.0.1:8788"}, true, "test")
	if !app.DaemonManageable || app.KeepGateway {
		t.Fatalf("new owned console policy = manageable %v keep %v; want manageable and off", app.DaemonManageable, app.KeepGateway)
	}

	_, _ = app.Update(tea.KeyPressMsg{Text: "d", Code: 'd'})
	if !app.KeepGateway {
		t.Fatal("d did not enable keeping the gateway alive")
	}
	if !strings.Contains(app.toast.Text, "stay running") {
		t.Fatalf("enable toast = %q", app.toast.Text)
	}
	foundOn := false
	for _, item := range app.overview.actions() {
		foundOn = foundOn || item.Key == "d" && strings.Contains(item.Label, "on")
	}
	if !foundOn {
		t.Fatal("Home actions do not expose the enabled daemon switch")
	}

	_, _ = app.Update(tea.KeyPressMsg{Text: "d", Code: 'd'})
	if app.KeepGateway {
		t.Fatal("second d did not disable keeping the gateway alive")
	}
	if !strings.Contains(app.toast.Text, "will stop") {
		t.Fatalf("disable toast = %q", app.toast.Text)
	}

	app.DaemonManageable = false
	_, _ = app.Update(tea.KeyPressMsg{Text: "d", Code: 'd'})
	if app.KeepGateway {
		t.Fatal("unmanaged gateway unexpectedly became managed")
	}
	if !strings.Contains(app.toast.Text, "managed externally") {
		t.Fatalf("unmanaged toast = %q", app.toast.Text)
	}
}

func TestLoadedScreenStopsSpinnerRedrawLoop(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.tab = TabRouting
	app.routing.loaded = true

	_, cmd := app.Update(app.spinner.Tick())
	if cmd != nil {
		t.Fatal("a loaded screen kept scheduling spinner redraws")
	}

	app.routing.loaded = false
	_, cmd = app.Update(app.spinner.Tick())
	if cmd == nil {
		t.Fatal("an unloaded screen stopped the visible loading spinner")
	}
}

func TestShellNeverWrapsTerminalEdge(t *testing.T) {
	const width, height = 120, 36
	app := New(&Client{BaseURL: "http://127.0.0.1:8788"}, true, "test")
	_, _ = app.Update(tea.WindowSizeMsg{Width: width, Height: height})
	view := app.View().Content
	lines := strings.Split(view, "\n")
	if len(lines) != height {
		t.Fatalf("rendered rows = %d; want %d", len(lines), height)
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got >= width {
			t.Fatalf("row %d width = %d; want less than terminal width %d", i, got, width)
		}
	}
	if top := strings.TrimSpace(ansi.Strip(lines[height-3])); !strings.HasSuffix(top, "╮") {
		t.Fatalf("action dock lost rounded corner: %q", top)
	}
}

func usageSeriesFixture() []UsageSeriesPoint {
	pts := make([]UsageSeriesPoint, 0, 8)
	now := time.Now()
	for i := 0; i < 8; i++ {
		pts = append(pts, UsageSeriesPoint{
			Start:             now.Add(-time.Duration(8-i) * time.Hour).Format(time.RFC3339),
			Requests:          int64(i%4 + 1),
			InputTokens:       int64((i + 1) * 10),
			OutputTokens:      int64((i + 1) * 3),
			EstimatedRequests: int64(i % 2),
			CostUSD:           float64(i) * 0.1,
			CostKnownRequests: int64(i % 3),
		})
	}
	return pts
}

func TestActivityFillsHeightWithHonestTrendModelAndRecentPanels(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.usage.loaded = true
	app.usage.width = 196
	app.usage.height = 50
	app.usage.window = "24h"
	app.usage.data = usageLoadedMsg{
		summary: UsageSummary{
			Requests: 3, Successes: 3, InputTokens: 95, OutputTokens: 19,
			ExactRequests: 1, EstimatedRequests: 1, UnavailableRequests: 1,
			CostUSD: 1.2, CostKnownRequests: 2, UnknownCostRequests: 1,
			AvgLatencyMs: 500,
		},
		series: usageSeriesFixture(),
		models: []UsageModel{
			{
				Platform: "openai", Model: "gpt-5.6-sol", Requests: 1,
				InputTokens: 90, OutputTokens: 16, EstimatedRequests: 1,
				CostUSD: 0.9, CostKnownRequests: 1, AvgMs: 700,
			},
			{
				Platform: "anthropic", Model: "claude-sonnet", Requests: 1,
				InputTokens: 5, OutputTokens: 3, CostUSD: 0.3, CostKnownRequests: 1, AvgMs: 300,
			},
		},
		requests: []UsageRequestRow{
			{
				CreatedAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
				Platform:  "openai", Model: "gpt-5.6-sol", Outcome: "success",
				InputTokens: 90, OutputTokens: 16, UsageQuality: "estimated", Estimated: true,
				CostUSD: 0.9, CostKnown: true,
				LatencyMs: 700, Attempts: 2, Class: "coding", Effort: "high",
			},
			{
				CreatedAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
				Platform:  "anthropic", Model: "claude-sonnet", Outcome: "success",
				InputTokens: 5, OutputTokens: 3, UsageQuality: "exact",
				CostUSD: 0.3, CostKnown: true, LatencyMs: 300, Attempts: 1,
			},
			{
				CreatedAt: time.Now().Add(-3 * time.Minute).Format(time.RFC3339),
				Platform:  "mistral", Model: "mixtral", Outcome: "success",
				UsageQuality: "unavailable", CostKnown: false, LatencyMs: 120, Attempts: 1,
			},
		},
	}

	content := app.usage.View().Content
	lines := strings.Split(content, "\n")
	if len(lines) < 45 {
		t.Fatalf("Activity rendered %d rows at 196x50; want a materially tall layout", len(lines))
	}
	view := ansi.Strip(content)
	for _, want := range []string{
		"Trend · 24h",           // chronological graph panel
		"Model usage · 24h",     // per-model rollup panel
		"Recent requests · 24h", // recent trail panel
		"OpenAI / Codex · gpt-5.6-sol",
		"Anthropic / Claude · claude-sonnet",
		"~106",            // estimated model token total
		"~90 in / 16 out", // estimated request split
		"5 in / 3 out",    // exact request split
		"$1.20",           // spend headline (only known costs)
		"$0.90",           // per-row / per-model known cost
		"unpriced",        // unknown cost disclosed, not zeroed
		"1 exact",         // quality legend
		"1 est",
		"1 n/a",
		"700ms",
		"coding · high ×2",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("Activity view did not render %q:\n%s", want, view)
		}
	}

	// Narrow but tall: the graph stacks over the rollup over the trail, and the
	// honest token split survives the tighter columns.
	app.usage.width = 120
	narrow := ansi.Strip(app.usage.View().Content)
	for _, want := range []string{
		"Trend · 24h",
		"Model usage · 24h",
		"OpenAI / Codex · gpt-5.6-sol",
		"Anthropic / Claude · claude-sonnet",
		"~90 in / 16 out",
	} {
		if !strings.Contains(narrow, want) {
			t.Fatalf("narrow Activity view did not render %q:\n%s", want, narrow)
		}
	}
}

func TestHomeShowsUsageTrendAfterSummary(t *testing.T) {
	app := New(&Client{}, true, "test")
	app.overview.width = 120
	app.overview.height = 40
	app.overview.loadedAt = time.Now()
	app.overview.data = overviewMsg{
		usageSummary: UsageSummary{
			Requests: 3, InputTokens: 40, OutputTokens: 12,
			CostUSD: 0.5, CostKnownRequests: 2, UnknownCostRequests: 1,
		},
		usageSeries: usageSeriesFixture(),
		requests: []UsageRequestRow{
			{
				CreatedAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
				Platform:  "openai", Model: "gpt-5.6-sol", Outcome: "success",
			},
		},
	}

	view := ansi.Strip(app.overview.View().Content)
	for _, want := range []string{
		"Traffic · 24h", // the live 24h graph panel replaced the bare route list
		"$0.50 spent",   // spend summary, known costs only
		"priced",        // unknown-cost disclosure
		"3 requests",    // token/request summary line
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("Home view did not render %q:\n%s", want, view)
		}
	}
}
