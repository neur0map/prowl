package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/neur0map/prowl/internal/query"
	internalsavings "github.com/neur0map/prowl/internal/savings"
	"github.com/neur0map/prowl/internal/store"
	"github.com/neur0map/prowl/internal/workspace"
)

// Overview is the landing screen: a stat strip over the gateway's live state,
// the credential a harness would hold, and a checklist that says - from real
// counts, never from guesses - what still needs doing before this routes.

type overviewMsg struct {
	providers       []KeyProvider
	enabledKeys     int
	modelCount      int
	strategy        string
	requests        []UsageRequestRow
	usageSummary    UsageSummary
	usageSeries     []UsageSeriesPoint
	unifiedKey      string
	signedIn        int
	project         projectOverview
	savings         query.Savings
	savingsProjects int
	savingsLoaded   bool
	err             error
}

type projectOverview struct {
	Name, State    string
	Files, Symbols int64
}

type overviewModel struct {
	app             *App
	width           int
	height          int
	data            overviewMsg
	revealed        bool
	loading         bool
	loadedAt        time.Time
	savingsLoadedAt time.Time
}

func (m *overviewModel) Init() tea.Cmd { return nil }

func (m *overviewModel) setSize(w, h int) { m.width, m.height = w, h }

func (m *overviewModel) load() tea.Cmd {
	c := m.app.Client
	refreshSavings := m.savingsLoadedAt.IsZero() || time.Since(m.savingsLoadedAt) >= time.Minute
	cachedSavings := m.data.savings
	cachedProjects := m.data.savingsProjects
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		var out overviewMsg

		var provEnv ProvidersEnvelope
		if err := c.Get(ctx, "/api/keys/providers", &provEnv); err != nil {
			out.err = err
			return out
		}
		out.providers = provEnv.Providers
		// The key count comes from the vault itself, not the provider
		// checklist: custom endpoints are deliberately absent from the
		// checklist and must still count as capacity the pool holds.
		var keys []KeyView
		if err := c.Get(ctx, "/api/keys", &keys); err == nil {
			for _, k := range keys {
				if k.Enabled {
					out.enabledKeys++
				}
			}
		}

		var models []ModelRow
		if err := c.Get(ctx, "/api/models", &models); err == nil {
			for _, mo := range models {
				if mo.Enabled {
					out.modelCount++
				}
			}
		}
		var routing RoutingState
		if err := c.Get(ctx, "/api/fallback/routing", &routing); err == nil {
			out.strategy = routing.Strategy
		}
		var usage struct {
			Requests []UsageRequestRow `json:"requests"`
		}
		if err := c.Get(ctx, "/api/usage/requests?limit=5", &usage); err == nil {
			out.requests = usage.Requests
		}
		var usageWindow UsageResponse
		if err := c.Get(ctx, "/api/usage/summary?window=24h", &usageWindow); err == nil {
			out.usageSummary = usageWindow.Summary
			out.usageSeries = usageWindow.Series
		}
		var key APIKeyResponse
		if err := c.Get(ctx, "/api/settings/api-key", &key); err == nil {
			out.unifiedKey = key.APIKey
		}
		var platforms struct {
			Platforms []SignInPlatform `json:"platforms"`
		}
		if err := c.Get(ctx, "/api/logins/platforms", &platforms); err == nil {
			for _, p := range platforms.Platforms {
				if p.SignedIn {
					out.signedIn++
				}
			}
		}
		out.savings = cachedSavings
		out.savingsProjects = cachedProjects
		if refreshSavings {
			projects, combined := internalsavings.Aggregate()
			out.savings = combined
			out.savingsProjects = len(projects)
			out.savingsLoaded = true
		}
		out.project = currentProjectOverview()
		return out
	}
}

func currentProjectOverview() projectOverview {
	cwd, err := os.Getwd()
	if err != nil {
		return projectOverview{Name: "unknown", State: "unavailable"}
	}
	project := projectOverview{Name: filepath.Base(cwd), State: "not indexed"}
	ws, err := workspace.Resolve(cwd)
	if err != nil {
		return project
	}
	project.Name = filepath.Base(ws.Root)
	if _, err := os.Stat(ws.DB); err != nil {
		return project
	}
	db, err := store.Open(ws.DB)
	if err != nil {
		project.State = "unavailable"
		return project
	}
	defer db.Close()
	status, err := query.New(db).Status()
	if err != nil {
		project.State = "unavailable"
		return project
	}
	project.State = "ready"
	if status.Semantic.Remaining > 0 {
		project.State = "semantic building"
	}
	project.Files = int64(status.Counts.Files)
	project.Symbols = int64(status.Counts.Symbols)
	return project
}

func (m *overviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case overviewMsg:
		m.data = msg
		m.loading = false
		m.loadedAt = time.Now()
		if msg.savingsLoaded {
			m.savingsLoadedAt = m.loadedAt
		}
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "v":
			m.revealed = !m.revealed
			return m, nil
		case "d":
			if !m.app.DaemonManageable {
				return m, m.app.showToast("bad", "This gateway is managed externally")
			}
			m.app.KeepGateway = !m.app.KeepGateway
			if m.app.KeepGateway {
				return m, m.app.showToast("ok", "Gateway will stay running after this console closes")
			}
			return m, m.app.showToast("info", "Gateway will stop when this console closes")
		}
	}
	return m, nil
}

func (m *overviewModel) View() tea.View {
	if m.loading && m.loadedAt.IsZero() {
		return tea.NewView(roundedPanel(
			"Gateway",
			m.app.spinner.View()+" "+stSubtle.Render("Reading live gateway state"),
			m.width,
		))
	}
	if m.data.err != nil {
		return tea.NewView(roundedPanel(
			"Gateway unreachable",
			stBad.Render("● "+m.data.err.Error())+"\n"+
				stFaint.Render("Start it with `prowl gateway up`, then reload."),
			m.width,
		))
	}

	ownership := "attached gateway"
	if m.app.Owned {
		ownership = "this terminal session"
	} else if m.app.DaemonManageable {
		ownership = "attached daemon"
	}
	keyShown := maskKey(m.data.unifiedKey)
	if m.revealed {
		keyShown = stWarn.Render(m.data.unifiedKey)
	}

	if m.height < 19 || m.width < 82 {
		hero := roundedPanel(
			"Prowl online",
			keyRow("Project", stHead.Render(m.data.project.Name), 11)+"\n"+
				keyRow("Index", m.projectIndexSummary(), 11)+"\n"+
				keyRow("Est. saved", m.savingsSummary(), 11)+"\n"+
				keyRow("Base URL", stKey.Render(m.app.Client.BaseURL+"/v1"), 11)+"\n"+
				keyRow("API key", keyShown, 11)+"\n"+
				keyRow("Runtime", stGood.Render("● online")+"  "+stFaint.Render(ownership), 11)+"\n"+
				keyRow("After exit", m.gatewayExitState(), 11),
			m.width,
		)
		facts := "  " + stGood.Render(m.savingsValue()) + stFaint.Render(" saved across projects") +
			"   " + stHead.Render(fmt.Sprintf("%d", len(m.data.providers))) + stFaint.Render(" providers") +
			"   " + stHead.Render(fmt.Sprintf("%d", m.data.enabledKeys)) + stFaint.Render(" active keys") +
			"   " + stHead.Render(fmt.Sprintf("%d", m.data.modelCount)) + stFaint.Render(" models")
		next := "Everything required is connected."
		for _, item := range m.checklist() {
			if strings.Contains(item, "○") {
				next = item
				break
			}
		}
		return tea.NewView(hero + "\n" + truncate(facts, m.width) + "\n" +
			roundedPanel("Next move", next, m.width))
	}

	stats := metricStrip([]metric{
		{"Est. tokens saved", m.savingsValue(), m.savingsDetail()},
		{"Capacity", fmt.Sprintf("%d providers", len(m.data.providers)), fmt.Sprintf("%d active keys · %d models", m.data.enabledKeys, m.data.modelCount)},
		{"Routing", m.data.strategy, "active request policy"},
		{"Runtime", "online", m.gatewayExitState()},
	}, m.width)

	leftW := max(m.width*3/5, 34)
	rightW := max(m.width-leftW-1, 26)
	connect := roundedPanel(
		"Prowl online",
		keyRow("Project", stHead.Render(m.data.project.Name), 11)+"\n"+
			keyRow("Index", m.projectIndexSummary(), 11)+"\n"+
			keyRow("Base URL", stKey.Render(m.app.Client.BaseURL+"/v1"), 11)+"\n"+
			keyRow("API key", keyShown, 11)+"\n"+
			keyRow("Runtime", stGood.Render("● online"), 11)+"\n"+
			keyRow("After exit", m.gatewayExitState(), 11)+"\n"+
			stFaint.Render(ownership+" · use this endpoint from any connected harness"),
		leftW,
	)
	ready := roundedPanel("Readiness", strings.Join(m.checklist(), "\n"), rightW)
	middle := lipgloss.JoinHorizontal(lipgloss.Top, connect, " ", ready)

	used := lipgloss.Height(stats) + 1 + lipgloss.Height(middle) + 1
	traffic := m.trafficPanel(m.width, max(m.height-used-2, 1))
	return tea.NewView(stats + "\n" + middle + "\n" + traffic)
}

func (m *overviewModel) savingsValue() string {
	if m.data.savings.Queries == 0 {
		return "not measured"
	}
	return "~" + compactTokens(m.data.savings.SavedTokens)
}

func (m *overviewModel) savingsDetail() string {
	if m.data.savings.Queries == 0 {
		return "across all indexed projects"
	}
	projects := "projects"
	if m.data.savingsProjects == 1 {
		projects = "project"
	}
	return fmt.Sprintf("%s answers · %d %s", humanInt(m.data.savings.Queries), m.data.savingsProjects, projects)
}

func (m *overviewModel) savingsSummary() string {
	value := m.savingsValue()
	if m.data.savings.Queries == 0 {
		return stFaint.Render(value)
	}
	return stGood.Render(value) + stFaint.Render(" · all projects")
}

func (m *overviewModel) projectIndexSummary() string {
	switch m.data.project.State {
	case "ready":
		return stGood.Render("● ready") + stFaint.Render(fmt.Sprintf(
			" · %s files · %s symbols",
			humanInt(m.data.project.Files), humanInt(m.data.project.Symbols)))
	case "semantic building":
		return stWarn.Render("● semantic building") + stFaint.Render(fmt.Sprintf(
			" · %s files · %s symbols",
			humanInt(m.data.project.Files), humanInt(m.data.project.Symbols)))
	case "not indexed":
		return stWarn.Render("○ not indexed") + stFaint.Render(" · run `prowl init`")
	default:
		return stBad.Render("● " + m.data.project.State)
	}
}

// trafficPanel is Home's compact live view of the last 24h: a chronological
// request graph with a spend and token summary when there is traffic, and the
// freshest routes beneath it. Spend never appears as $0.00 when no price was
// reported.
func (m *overviewModel) trafficPanel(width, rows int) string {
	rows = max(rows, 3)
	avail := max(width-4, 12)
	summary := m.data.usageSummary
	series := m.data.usageSeries
	contentBudget := rows - 2
	var body []string
	hasTrend := summary.Requests > 0 && len(series) > 0
	if hasTrend {
		n, colW := columnPlan(len(series), avail)
		reqCols := resample(seriesRequests(series), n)
		chartH := min(max(contentBudget-2-min(len(m.data.requests), 3), 1), 4)
		if chartH > contentBudget-2 {
			chartH = max(contentBudget-2, 1)
		}
		body = append(body, sparkColumns(reqCols, chartH, colW, rgbEmberDim, rgbGold)...)
		body = append(body, joinEdges(
			stKey.Render(homeSpend(summary)),
			stSubtle.Render(humanInt(summary.InputTokens+summary.OutputTokens)+" tokens")+
				stFaint.Render(" · ")+stSubtle.Render(humanInt(summary.Requests)+" requests"),
			avail))
		body = append(body, joinEdges(
			stFaint.Render(relTime(series[0].Start)+" ago"),
			stFaint.Render("now"), avail))
	}
	remaining := contentBudget - len(body)
	for _, request := range m.data.requests {
		if remaining <= 0 {
			break
		}
		via := request.Platform
		if request.RoutedFrom != "" {
			via = request.Platform + " ← " + request.RoutedFrom
		}
		body = append(body, fmt.Sprintf("%s  %s  %s  %s",
			stFaint.Render(padRight(relTime(request.CreatedAt), 5)),
			padRight(truncate(via, 22), 22),
			padRight(truncate(request.Model, 26), 26),
			pill(request.Outcome)))
		remaining--
	}
	if len(body) == 0 {
		body = append(body,
			stFaint.Render("No traffic yet."),
			stSubtle.Render("Add a credential, then send a request to the base URL."))
	}
	title := "Recent routes"
	if hasTrend {
		title = "Traffic · 24h"
	}
	return roundedPanel(title, strings.Join(body, "\n"), width)
}

// homeSpend is the one-line spend headline for Home; unpriced requests are
// disclosed, never valued at zero.
func homeSpend(s UsageSummary) string {
	if s.CostKnownRequests <= 0 {
		return "cost not reported"
	}
	txt := "$" + fmtUSD(s.CostUSD) + " spent"
	if s.UnknownCostRequests > 0 {
		txt += " · " + humanInt(s.CostKnownRequests) + " of " + humanInt(s.Requests) + " priced"
	}
	return txt
}

func (m *overviewModel) gatewayExitState() string {
	if !m.app.DaemonManageable {
		return stFaint.Render("managed externally")
	}
	if m.app.KeepGateway {
		return stGood.Render("● stays running")
	}
	return stWarn.Render("○ stops with console")
}

func (m *overviewModel) actions() []action {
	label := "Reveal key"
	if m.revealed {
		label = "Hide key"
	}
	actions := []action{{Key: "v", Label: label, Primary: true}}
	if m.app.DaemonManageable {
		daemonLabel := "Keep running: off"
		if m.app.KeepGateway {
			daemonLabel = "Keep running: on"
		}
		actions = append(actions, action{Key: "d", Label: daemonLabel})
	}
	return actions
}

func (m *overviewModel) checklist() []string {
	type step struct {
		done bool
		hint string
	}
	steps := []step{
		{m.data.enabledKeys > 0, "Add a provider credential"},
		{m.data.modelCount > 5, "Choose models for routing"},
		{m.data.signedIn > 0, "Connect a subscription (optional)"},
	}
	var out []string
	for _, step := range steps {
		mark := stFaint.Render("○")
		if step.done {
			mark = stGood.Render("●")
		}
		out = append(out, mark+"  "+step.hint)
	}
	injected := injectTargets(m.app.Client.Home)
	mark := stFaint.Render("○")
	if len(injected) > 0 {
		mark = stGood.Render("●")
	}
	out = append(out, mark+"  Connect at least one coding harness")
	return out
}

func maskKey(k string) string {
	if len(k) <= 12 {
		return stSubtle.Render(k)
	}
	return stSubtle.Render(k[:len("prowlag-")+2]) + stFaint.Render(strings.Repeat("•", 24)) +
		stSubtle.Render(k[len(k)-4:])
}

func relTime(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
