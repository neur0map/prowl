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
	"github.com/charmbracelet/x/ansi"

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
	accounts        []LoginUsage
	usageErr        error
	usageFetched    bool
	err             error
}

// keyRegeneratedMsg carries the freshly minted unified key back from the
// regenerate confirm. It is launched from an overlay, so app.Update dismisses
// the owning confirm (matched by op) before routing it here.
type keyRegeneratedMsg struct {
	key string
	op  uint64
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
	usageFetchedAt  time.Time
}

func (m *overviewModel) Init() tea.Cmd { return nil }

func (m *overviewModel) setSize(w, h int) { m.width, m.height = w, h }

func (m *overviewModel) load() tea.Cmd {
	c := m.app.Client
	refreshSavings := m.savingsLoadedAt.IsZero() || time.Since(m.savingsLoadedAt) >= time.Minute
	cachedSavings := m.data.savings
	cachedProjects := m.data.savingsProjects
	// The allowance read is the heaviest call on this page and the numbers
	// move slowly, so the 5s tick reuses the last accounts until a minute
	// has passed since the last real fetch.
	refreshUsage := m.usageFetchedAt.IsZero() || time.Since(m.usageFetchedAt) >= time.Minute
	cachedAccounts := m.data.accounts
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
		out.accounts = cachedAccounts
		if refreshUsage {
			out.usageFetched = true
			var env struct {
				Accounts []LoginUsage `json:"accounts"`
			}
			if err := c.Get(ctx, "/api/logins/usage", &env); err != nil {
				out.accounts = nil
				out.usageErr = err
			} else {
				out.accounts = env.Accounts
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
		if msg.usageFetched {
			m.usageFetchedAt = m.loadedAt
		}
		return m, nil
	case keyRegeneratedMsg:
		// Show the new key straight away so the operator can copy it; the key
		// itself is set here rather than waiting on a reload so there is no
		// window where Home renders the stale credential.
		m.data.unifiedKey = msg.key
		m.revealed = true
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "v":
			m.revealed = !m.revealed
			return m, nil
		case "g":
			return m, m.confirmRegenerateKey()
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

	// The page is a column of blocks with a blank line between them and a
	// two-column inset, so each block reads on its own instead of the whole
	// top half arriving as one wall.
	inner := max(m.width-homeInset*2, 20)
	blocks := []string{
		m.statsRow(inner),
		strings.Join(m.capacityLines(inner), "\n"),
	}

	endpoint := section("Endpoint") + "\n\n" +
		keyRow("Project", stHead.Render(m.data.project.Name), 12) + "\n" +
		keyRow("Index", m.projectIndexSummary(), 12) + "\n" +
		keyRow("Base URL", stKey.Render(m.app.Client.BaseURL+"/v1"), 12) + "\n" +
		keyRow("API key", keyShown, 12) + "\n" +
		keyRow("After exit", m.gatewayExitState(), 12) + "\n" +
		stFaint.Render(ownership)
	readiness := section("Readiness") + "\n\n" + strings.Join(m.checklist(), "\n")

	renderCol := func(body string, w int) string {
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			lines[i] = padRight(truncate(ln, w), w)
		}
		return strings.Join(lines, "\n")
	}
	if m.height < 22 || m.width < 82 {
		blocks = append(blocks, renderCol(endpoint, inner), renderCol(readiness, inner))
	} else {
		leftW := max(inner*55/100, 34)
		rightW := max(inner-leftW-4, 26)
		blocks = append(blocks, lipgloss.JoinHorizontal(lipgloss.Top,
			renderCol(endpoint, leftW), "    ", renderCol(readiness, rightW)))
	}

	used := lipgloss.Height(strings.Join(blocks, "\n\n")) + 1
	blocks = append(blocks, m.trafficPanel(inner, max(m.height-used-1, 3)))
	return tea.NewView(inset(strings.Join(blocks, "\n\n"), homeInset))
}

// homeInset is the left margin every Home block shares.
const homeInset = 2

func inset(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// statsRow is the four numbers Home leads with, each with its own small-caps
// label above it, spaced across the width like a dashboard stat bar.
func (m *overviewModel) statsRow(width int) string {
	items := []metric{
		{"Tokens saved", m.savingsValue(), m.savingsDetail()},
		{"Catalogue", fmt.Sprintf("%d models", m.data.modelCount), fmt.Sprintf("%d providers", len(m.data.providers))},
		{"Strategy", strategyDisplayName(m.data.strategy), "console default"},
		{"Runtime", "online", ansiPlain(m.gatewayExitState())},
	}
	return metricStrip(items, width)
}

// capacityLines is Home's subscription allowance block: a bar of remaining
// headroom (100 minus the busiest window across accounts) led by whichever
// window is closest to its limit, then one line per account window.
//
// It deliberately shows percentages, not token totals: Claude and ChatGPT
// report only a used-percent, never an absolute token ceiling, and that
// percent counts usage outside Prowl too, so any "N of M tokens" figure would
// be fabricated. Where the gateway actually measured tokens it routed to an
// account, that count is shown as an observation, not a ceiling.
func (m *overviewModel) capacityLines(width int) []string {
	if len(m.data.accounts) == 0 {
		return []string{section("Subscription capacity"), "",
			stFaint.Render("Connect a subscription on Providers (3) to see its allowance here.")}
	}
	remaining, tight, hasTight := m.capacity()
	label := ""
	if hasTight {
		label = tight
	}
	head := joinEdges(section("Subscription capacity"), stSubtle.Render(label), width)

	if remaining < 0 {
		remaining = 0
	}
	pct := fmt.Sprintf("%3.0f%% left", remaining)
	pctStyle := stGood
	switch {
	case remaining < 15:
		pctStyle = stBad
	case remaining < 40:
		pctStyle = stWarn
	}
	barW := max(width-lipgloss.Width(pct)-2, 1)
	lines := []string{head, "", bar(remaining/100, barW) + "  " + pctStyle.Render(pct), ""}
	for i, a := range m.data.accounts {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, m.accountLines(a, width)...)
	}
	return lines
}

// capacity returns the remaining headroom for the single bar and a summary of
// the window closest to its limit - the one an operator actually needs to
// watch. remaining is 100 minus the busiest window's used-percent.
func (m *overviewModel) capacity() (remaining float64, tightest string, ok bool) {
	maxUtil := 0.0
	var best LoginUsageWindow
	var bestAccount string
	for _, a := range m.data.accounts {
		pw, has := primaryWindow(a)
		if !has {
			continue
		}
		if pw.Utilization > maxUtil {
			maxUtil = pw.Utilization
		}
		if !ok || pw.Utilization > best.Utilization {
			best, bestAccount, ok = pw, accountName(a), true
		}
	}
	remaining = 100 - maxUtil
	if ok {
		label := best.Label
		if label == "" {
			label = windowShort(best.Key)
		}
		tightest = fmt.Sprintf("%s %s at %.0f%% used", bestAccount, label, best.Utilization)
		if strings.TrimSpace(best.ResetsAt) != "" {
			tightest += " · resets " + humanReset(best.ResetsAt)
		}
	}
	return remaining, tightest, ok
}

// primaryWindow is the account's window closest to its ceiling - the honest
// bound on how much subscription is left, and the one the bar answers to.
func primaryWindow(a LoginUsage) (LoginUsageWindow, bool) {
	var best LoginUsageWindow
	found := false
	for _, w := range a.Windows {
		if !found || w.Utilization > best.Utilization {
			best, found = w, true
		}
	}
	return best, found
}

func accountName(a LoginUsage) string {
	if a.Name != "" {
		return a.Name
	}
	return a.Provider
}

// accountLines is one account's block: its name, then one aligned line per
// window so the eye compares bars down a column instead of across a sentence.
func (m *overviewModel) accountLines(a LoginUsage, width int) []string {
	head := stHead.Render(accountName(a))
	switch {
	case a.NeedsSignIn:
		return []string{head + stFaint.Render("  ·  ") + stWarn.Render("sign in again")}
	case a.Error != "":
		return []string{head + stFaint.Render("  ·  ") + stBad.Render("allowance report offline")}
	case a.Balance != nil:
		unit := a.Unit
		if unit == "" {
			unit = "credits"
		}
		return []string{head + stFaint.Render("  ·  ") + stKey.Render(trimNum(*a.Balance)) + stFaint.Render(" "+unit+" left")}
	case len(a.Windows) == 0:
		return []string{head + stFaint.Render("  ·  no allowance windows")}
	}
	lines := []string{head}
	for _, w := range a.Windows {
		lines = append(lines, truncate(windowLine(w), width))
	}
	return lines
}

// windowLine is `  <label>  <bar>  <used>  <resets>  <routed>` with fixed
// columns. Routed tokens are what THIS gateway sent to the account in the
// window - an observation shown only when measured, never dressed up as the
// window's ceiling.
func windowLine(w LoginUsageWindow) string {
	label := w.Label
	if label == "" {
		label = windowShort(w.Key)
	}
	used := fmt.Sprintf("%3.0f%% used", w.Utilization)
	line := "  " + padRight(stSubtle.Render(label), 16) + bar(w.Utilization/100, 24) + "  " + stSubtle.Render(used)
	if strings.TrimSpace(w.ResetsAt) != "" {
		line += stFaint.Render("  ·  resets " + humanReset(w.ResetsAt))
	}
	if w.TokensUsed > 0 {
		line += stFaint.Render("  ·  " + humanTokens(float64(w.TokensUsed)) + " routed via Prowl")
	}
	return line
}

// humanTokens renders a token count with a k/M suffix, e.g. 397358 -> "397k",
// 1_500_000 -> "1.5M".
func humanTokens(n float64) string {
	switch {
	case n >= 1_000_000:
		return trimNum(n/1_000_000) + "M"
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", n/1_000)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}

func ansiPlain(s string) string { return ansi.Strip(s) }

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
	avail := max(width, 12)
	summary := m.data.usageSummary
	series := m.data.usageSeries
	// The section header and its spacer take two rows of the budget.
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
	for i := range body {
		body[i] = truncate(body[i], width)
	}
	return section(title) + "\n\n" + strings.Join(body, "\n")
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
	actions = append(actions, action{Key: "g", Label: "Regenerate key", Dangerous: true})
	if m.app.DaemonManageable {
		daemonLabel := "Keep running: off"
		if m.app.KeepGateway {
			daemonLabel = "Keep running: on"
		}
		actions = append(actions, action{Key: "d", Label: daemonLabel})
	}
	return actions
}

// confirmRegenerateKey rotates the unified inference key behind a confirm, since
// it is destructive: the old key stops authenticating the instant the new one
// is stored, so every harness holding the old value must be repointed. The new
// key is revealed on success so the operator can copy it, and Home reloads to
// show it (and the 5s Home tick keeps it fresh even without the reload).
func (m *overviewModel) confirmRegenerateKey() tea.Cmd {
	c := m.app.Client
	m.app.overlay = &confirmOverlay{
		question: "Regenerate the unified API key? The current key stops working immediately and every coding harness must be repointed with the new one (Setup re-injects it).",
		verb:     "Regenerate",
		yes: func() tea.Msg {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var out APIKeyResponse
				if err := c.Post(ctx, "/api/settings/api-key/regenerate", nil, &out); err != nil {
					return errMsg{Screen: "overview", Err: err}
				}
				return keyRegeneratedMsg{key: out.APIKey}
			}
		},
	}
	return nil
}

func (m *overviewModel) checklist() []string {
	type step struct {
		done bool
		hint string
	}
	steps := []step{
		{m.data.enabledKeys > 0, "Connect a provider (3 · key or sign-in)"},
		{m.data.modelCount > 5, "Build a routing set and pick its models (2)"},
		{m.data.signedIn > 0, "Add a subscription account (optional)"},
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
	out = append(out, mark+"  Connect at least one coding harness (6)")
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
	var t time.Time
	var err error
	// The API emits RFC 3339 for most timestamps and SQLite's own datetime
	// text for key health checks; a value in neither form is shown as-is
	// rather than hidden.
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err = time.Parse(layout, iso); err == nil {
			break
		}
	}
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
