package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Usage answers "what did this gateway actually do": the window totals, a
// chronological traffic/token/cost graph, the per-model rollup, and the recent
// request trail - the events the router decided on that a human would want to
// see after the fact. Every token and dollar is drawn from the shared
// accounting contract, so an exact figure, an estimate (~) and an unreported
// value (-) never look alike.

type usageLoadedMsg struct {
	// gen is the monotonically increasing id of the load that produced this
	// response. Update accepts a response only when gen still matches the latest
	// dispatched load, which rejects a stale response even after the operator
	// cycles the window back to a value an in-flight load also fetched (ABA):
	// window equality alone would wrongly accept that earlier response.
	gen uint64
	// window is the range this response was fetched for, checked alongside gen
	// so an accepted response can never disagree with the chosen window.
	window    string
	summary   UsageSummary
	platforms []UsagePlatform
	models    []UsageModel
	series    []UsageSeriesPoint
	requests  []UsageRequestRow
	logs      []ServerLog
	err       error
}

type usageModel struct {
	app    *App
	width  int
	height int
	tab    int // 0 requests, 1 logs
	scroll int
	window string
	// loadGen is bumped on every load() so each dispatch carries a unique id;
	// only the newest load's response is accepted (see usageLoadedMsg.gen).
	loadGen uint64
	data    usageLoadedMsg
	loaded  bool
}

func (m *usageModel) Init() tea.Cmd { return nil }

func (m *usageModel) setSize(w, h int) { m.width, m.height = w, h }

// usageWindowOr resolves the empty default to the concrete "24h" range, so the
// window a load fetched and the window Update compares against are the same
// token even before the operator has ever pressed w.
func usageWindowOr(window string) string {
	if window == "" {
		return "24h"
	}
	return window
}

func (m *usageModel) load() tea.Cmd {
	c := m.app.Client
	window := usageWindowOr(m.window)
	m.loadGen++
	gen := m.loadGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out := usageLoadedMsg{gen: gen, window: window}
		var usage UsageResponse
		if err := c.Get(ctx, "/api/usage/summary?window="+window, &usage); err != nil {
			out.err = err
			return out
		}
		out.summary = usage.Summary
		out.platforms = usage.Platforms
		out.models = usage.Models
		out.series = usage.Series
		var reqs struct {
			Requests []UsageRequestRow `json:"requests"`
		}
		if err := c.Get(ctx, "/api/usage/requests?limit=40", &reqs); err == nil {
			out.requests = reqs.Requests
		}
		var logs LogsResponse
		if err := c.Get(ctx, "/api/logs?limit=40", &logs); err == nil {
			out.logs = logs.Logs
		}
		return out
	}
}

func (m *usageModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case usageLoadedMsg:
		// Drop a response from a superseded load: pressing w bumps loadGen and
		// dispatches a fresh load, so only the newest load's response is
		// accepted. Generation is what defeats the ABA case (24h→7d→24h): an
		// in-flight first 24h load carries an older gen and is rejected even
		// though its window matches again. Window is checked too so an accepted
		// response can never disagree with the chosen range.
		if msg.gen != m.loadGen || msg.window != usageWindowOr(m.window) {
			return m, nil
		}
		m.data = msg
		m.loaded = true
		m.scroll = min(m.scroll, max(m.itemCount()-1, 0))
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k":
			m.scroll = max(m.scroll-1, 0)
		case "down", "j":
			m.scroll = min(m.scroll+1, max(m.itemCount()-1, 0))
		case "pgup":
			m.scroll = max(m.scroll-max(m.height/2, 1), 0)
		case "pgdn":
			m.scroll = min(m.scroll+max(m.height/2, 1), max(m.itemCount()-1, 0))
		case "home", "g":
			m.scroll = 0
		case "end", "G":
			m.scroll = max(m.itemCount()-1, 0)
		case "l":
			m.tab = 1 - m.tab
			m.scroll = 0
		case "w":
			switch m.window {
			case "", "24h":
				m.window = "7d"
			case "7d":
				m.window = "30d"
			default:
				m.window = "24h"
			}
			m.scroll = 0
			return m, m.load()
		case "d":
			if m.tab != 1 {
				return m, nil
			}
			c := m.app.Client
			m.app.overlay = &confirmOverlay{
				question: "Clear every recorded server event? Request usage is kept; only the diagnostic event log is removed.",
				yes: func() tea.Msg {
					// Return the DELETE as a command so runYes runs it off the
					// Update loop, never blocking on I/O inside Update.
					return func() tea.Msg {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						var out map[string]any
						if err := c.Delete(ctx, "/api/logs", &out); err != nil {
							return errMsg{Screen: "usage", Err: err}
						}
						return doneMsg{Tab: TabUsage, Text: "server event log cleared"}
					}
				},
			}
			return m, m.app.overlay.Init()
		}
	}
	return m, nil
}

func (m *usageModel) itemCount() int {
	if m.tab == 1 {
		return len(m.data.logs)
	}
	return len(m.data.requests)
}

func (m *usageModel) View() tea.View {
	if !m.loaded {
		return tea.NewView(roundedPanel(
			"Usage",
			m.app.spinner.View()+" "+stSubtle.Render("Reading request history"),
			m.width,
		))
	}
	if m.data.err != nil {
		return tea.NewView(roundedPanel(
			"Usage unavailable",
			stBad.Render("● "+m.data.err.Error()),
			m.width,
		))
	}

	stats := m.statStrip()
	remaining := max(m.height-lipgloss.Height(stats)-1, 3)
	// Tall: a full-width chronological graph over the model rollup over the
	// recent trail. Full width keeps the model bars wide and every dense recent
	// column - cost, route, retries - legible instead of truncated, and the
	// stacked frames spend a big terminal's height on signal.
	if remaining >= 18 {
		trendH := max(remaining*2/5, 10)
		trendH = min(trendH, remaining-14)
		leftover := max(remaining-trendH-2, 12)
		modelH := max(leftover*2/5, 6)
		recentH := max(leftover-modelH, 6)
		trend := fixedPanel("Trend · "+m.windowText(), m.trendBody(trendH-2, m.width), m.width, trendH)
		models := fixedPanel("Model usage · "+m.windowText(), m.modelUsageBody(modelH-2, m.width), m.width, modelH)
		activity := fixedPanel(m.activityTitle(), m.activityBody(recentH-2, m.width), m.width, recentH)
		return tea.NewView(stats + "\n" + trend + "\n" + models + "\n" + activity)
	}

	// Mid-height: drop the graph, keep the rollup over the trail - both full
	// width so their columns still breathe.
	if remaining >= 12 {
		modelHeight := min(max(remaining/3, 5), 8)
		activityHeight := max(remaining-modelHeight-1, 4)
		models := fixedPanel("Model usage · "+m.windowText(), m.modelUsageBody(modelHeight-2, m.width), m.width, modelHeight)
		activity := fixedPanel(m.activityTitle(), m.activityBody(activityHeight-2, m.width), m.width, activityHeight)
		return tea.NewView(stats + "\n" + models + "\n" + activity)
	}

	// Cramped: the trail alone.
	activity := fixedPanel(m.activityTitle(), m.activityBody(remaining-2, m.width), m.width, remaining)
	return tea.NewView(stats + "\n" + activity)
}

// statStrip is the honest window headline: request quality mix, token split,
// spend, reliability and speed. Spend is a first-class card - never hidden.
func (m *usageModel) statStrip() string {
	summary := m.data.summary
	successRate := 0.0
	if summary.Requests > 0 {
		successRate = float64(summary.Successes) / float64(summary.Requests) * 100
	}
	tokenDetail := fmt.Sprintf("%s in · %s out",
		humanInt(summary.InputTokens), humanInt(summary.OutputTokens))
	costValue, costDetail := m.costHeadline()
	metrics := []metric{
		{"Requests", humanInt(summary.Requests), m.qualityDetail()},
		{"Tokens", humanInt(summary.InputTokens + summary.OutputTokens), tokenDetail},
		{"Cost", costValue, costDetail},
		{"Success", fmt.Sprintf("%.0f%%", successRate), humanInt(summary.Failures) + " failed"},
	}
	if m.width >= 130 {
		metrics = append(metrics, metric{
			"Speed", fmt.Sprintf("%dms", summary.AvgLatencyMs),
			fmt.Sprintf("%.0f%% failover", summary.FailoverRate*100),
		})
	}
	return metricStrip(metrics, m.width)
}

// qualityDetail names how many of the window's requests carry exact,
// estimated or unreported token counts - the words behind the graph legend.
func (m *usageModel) qualityDetail() string {
	s := m.data.summary
	var parts []string
	if s.ExactRequests > 0 {
		parts = append(parts, humanInt(s.ExactRequests)+" exact")
	}
	if s.EstimatedRequests > 0 {
		parts = append(parts, "~"+humanInt(s.EstimatedRequests)+" est")
	}
	if s.UnavailableRequests > 0 {
		parts = append(parts, "-"+humanInt(s.UnavailableRequests)+" n/a")
	}
	if len(parts) == 0 {
		return "window " + m.windowText()
	}
	return strings.Join(parts, " · ")
}

// costHeadline is the spend figure and how much of it is actually known.
// Unpriced requests are counted, never silently valued at zero.
func (m *usageModel) costHeadline() (value, detail string) {
	s := m.data.summary
	if s.CostKnownRequests <= 0 {
		return "not reported", "no provider priced a request"
	}
	detail = humanInt(s.CostKnownRequests) + " priced"
	if s.UnknownCostRequests > 0 {
		detail += " · " + humanInt(s.UnknownCostRequests) + " unpriced"
	}
	return "$" + fmtUSD(s.CostUSD), detail
}

func (m *usageModel) activityTitle() string {
	if m.tab == 1 {
		return "Server events"
	}
	return "Recent requests · " + m.windowText()
}

// trendBody draws the chronological graph: request bars over time (ember→gold),
// a matcha token strip beneath when there is room, a cost headline, a time
// axis and a quality legend - the traffic/token/cost story in one panel.
func (m *usageModel) trendBody(rows, width int) string {
	rows = max(rows, 1)
	avail := max(width-4, 12)
	s := m.data.summary
	series := m.data.series
	if len(series) == 0 || s.Requests == 0 {
		return stFaint.Render("No traffic yet.") + "\n" +
			stSubtle.Render("Requests appear here after the first routed call.")
	}

	lines := make([]string, 0, rows)
	costValue, costDetail := m.costHeadline()
	headline := stKey.Render(costValue)
	if costDetail != "" {
		headline += stFaint.Render("  ·  " + costDetail)
	}
	totals := stSubtle.Render(humanInt(s.InputTokens+s.OutputTokens)+" tokens") +
		stFaint.Render(" · ") + stSubtle.Render(humanInt(s.Requests)+" requests")
	lines = append(lines, joinEdges(headline, totals, avail))

	tokenStrip := rows >= 10
	bottom := 2 // axis + legend
	if tokenStrip {
		bottom++
	}
	chartH := max(rows-1-bottom, 1)

	n, colW := columnPlan(len(series), avail)
	reqCols := resample(seriesRequests(series), n)
	lines = append(lines, sparkColumns(reqCols, chartH, colW, rgbEmberDim, rgbGold)...)
	if tokenStrip {
		tokCols := resample(seriesTokens(series), n)
		lines = append(lines, sparkColumns(tokCols, 1, colW, rgbMatchaDim, rgbMatcha)...)
	}

	first := series[0].Start
	lines = append(lines, joinEdges(
		stFaint.Render(relTime(first)+" ago"),
		stFaint.Render("now"), avail))

	key := stWarn.Render("▇") + stFaint.Render(" requests")
	if tokenStrip {
		key += "   " + lipgloss.NewStyle().Foreground(colorMatcha).Render("▁") + stFaint.Render(" tokens")
	}
	lines = append(lines, joinEdges(key, m.qualityLegend(), avail))
	return strings.Join(lines, "\n")
}

// qualityLegend spells out the exact/estimated/unavailable split with the same
// marks the rows use, so colour is never the only carrier.
func (m *usageModel) qualityLegend() string {
	s := m.data.summary
	return stGood.Render("●") + stSubtle.Render(" "+humanInt(s.ExactRequests)+" exact") + "  " +
		stWarn.Render("~") + stSubtle.Render(" "+humanInt(s.EstimatedRequests)+" est") + "  " +
		stFaint.Render("-") + stSubtle.Render(" "+humanInt(s.UnavailableRequests)+" n/a")
}

func (m *usageModel) modelUsageBody(rows, width int) string {
	if len(m.data.models) == 0 {
		return stFaint.Render("No routed models in this window.")
	}
	rows = max(rows, 1)
	width = max(width-4, 1)
	maxTokens := int64(1)
	for _, model := range m.data.models {
		maxTokens = max(maxTokens, model.InputTokens+model.OutputTokens)
	}

	header := rows >= 3
	if header {
		rows--
	}
	barW := max(min(width/3, 22), 8)
	tokenW := 8
	costW := 8
	requestW := 6
	nameW := max(width-barW-tokenW-costW-requestW-10, 10)
	out := make([]string, 0, rows+1)
	if header {
		out = append(out, stFaint.Render(
			padRight("Provider · model", nameW)+"  "+
				padRight("Share", barW)+"  "+
				padRight("Tokens", tokenW)+"  "+
				padRight("Cost", costW)+"  Reqs",
		))
	}
	for _, model := range m.data.models[:min(len(m.data.models), rows)] {
		tokens := model.InputTokens + model.OutputTokens
		requests := fmt.Sprintf("%d×", model.Requests)
		if model.Errors > 0 {
			requests = stWarn.Render(fmt.Sprintf("%d×/%d!", model.Requests, model.Errors))
		}
		name := providerDisplayName(model.Platform) + " · " + model.Model
		out = append(out, fmt.Sprintf("%s  %s  %s  %s  %s",
			padRight(truncate(name, nameW), nameW),
			bar(float64(tokens)/float64(maxTokens), barW),
			padRight(modelTokenText(model), tokenW),
			padRight(modelCostText(model), costW),
			padRight(requests, requestW)))
	}
	return strings.Join(out, "\n")
}

func (m *usageModel) activityBody(rows, width int) string {
	rows = max(rows, 1)
	avail := max(width-4, 1)
	if m.tab == 0 {
		if len(m.data.requests) == 0 {
			return stFaint.Render("No traffic yet.") + "\n" +
				stSubtle.Render("Requests appear here after the first routed call.")
		}
		header := rows >= 3
		if header {
			rows--
		}
		m.scroll = min(m.scroll, max(len(m.data.requests)-rows, 0))
		end := min(m.scroll+rows, len(m.data.requests))

		const (
			whenW    = 5
			resultW  = 12
			tokenW   = 17
			latencyW = 8
		)
		free := avail - whenW - resultW - tokenW - latencyW - 8
		costW := 0
		if free >= 24 {
			costW = 8
			free -= costW + 2
		}
		classW := 0
		if free >= 22 {
			classW = min(free-14, 18)
			free -= classW + 2
		}
		nameW := max(free, 12)

		out := make([]string, 0, rows+1)
		if header {
			cells := []string{
				padRight("When", whenW),
				padRight("Provider · model", nameW),
				padRight("Result", resultW),
				padRight("Tokens", tokenW),
			}
			if costW > 0 {
				cells = append(cells, padRight("Cost", costW))
			}
			cells = append(cells, padRight("Latency", latencyW))
			if classW > 0 {
				cells = append(cells, "Route")
			}
			out = append(out, stFaint.Render(strings.Join(cells, "  ")))
		}
		for _, request := range m.data.requests[m.scroll:end] {
			classification := strings.Trim(strings.Join([]string{request.Class, request.Effort}, " · "), " ·")
			if classification == "" {
				classification = "-"
			}
			if request.Attempts > 1 {
				classification += fmt.Sprintf(" ×%d", request.Attempts)
			}
			name := providerDisplayName(request.Platform) + " · " + request.Model
			cells := []string{
				stFaint.Render(padRight(relTime(request.CreatedAt), whenW)),
				padRight(truncate(name, nameW), nameW),
				padRight(pill(request.Outcome), resultW),
				padRight(requestTokenSplit(request), tokenW),
			}
			if costW > 0 {
				cells = append(cells, padRight(requestCost(request), costW))
			}
			cells = append(cells, padRight(fmt.Sprintf("%dms", request.LatencyMs), latencyW))
			if classW > 0 {
				cells = append(cells, stFaint.Render(truncate(classification, classW)))
			}
			out = append(out, strings.Join(cells, "  "))
		}
		return strings.Join(out, "\n")
	}
	if len(m.data.logs) == 0 {
		return stGood.Render("● Router quiet") + "\n" +
			stFaint.Render("No warnings or errors in this window.")
	}
	m.scroll = min(m.scroll, max(len(m.data.logs)-rows, 0))
	end := min(m.scroll+rows, len(m.data.logs))
	var out []string
	for _, entry := range m.data.logs[m.scroll:end] {
		out = append(out, fmt.Sprintf("%s  %s  %s",
			stFaint.Render(time.UnixMilli(entry.TS).Format("15:04:05")),
			padRight(pill(entry.Level), 11),
			truncate(entry.Provider+" "+entry.Event+": "+entry.Message, max(avail-25, 16))))
	}
	return strings.Join(out, "\n")
}

func (m *usageModel) actions() []action {
	view := "Server events"
	if m.tab == 1 {
		view = "Requests"
	}
	actions := []action{
		{Key: "l", Label: view, Primary: true},
		{Key: "w", Label: "Window " + m.windowText()},
	}
	if m.tab == 1 {
		actions = append(actions, action{Key: "d", Label: "Clear events", Dangerous: true})
	}
	return actions
}

func (m *usageModel) windowText() string {
	if m.window == "" {
		return "24h"
	}
	return m.window
}

// fixedPanel frames body at an exact height, padding short content with blank
// rows and clipping overflow, so a panel spans the height the layout gave it
// instead of collapsing to its content. This is what lets a big terminal read
// as dense panels rather than a stack hugging the top edge.
func fixedPanel(title, body string, width, height int) string {
	inner := max(height-2, 1)
	lines := strings.Split(body, "\n")
	for len(lines) < inner {
		lines = append(lines, "")
	}
	if len(lines) > inner {
		lines = lines[:inner]
	}
	return roundedPanel(title, strings.Join(lines, "\n"), width)
}

// ── shared accounting formatters ─────────────────────────────────────────────

// usageQualityOf reads a request's declared accounting quality, falling back to
// the token count and the legacy estimated flag for rows recorded before the
// field existed.
func usageQualityOf(r UsageRequestRow) string {
	switch r.UsageQuality {
	case "exact", "estimated", "unavailable":
		return r.UsageQuality
	}
	if r.InputTokens+r.OutputTokens <= 0 {
		return "unavailable"
	}
	if r.Estimated {
		return "estimated"
	}
	return "exact"
}

// requestTokenSplit renders a request's in/out tokens with an honest marker:
// exact plain, estimated ~, unreported -.
func requestTokenSplit(r UsageRequestRow) string {
	switch usageQualityOf(r) {
	case "unavailable":
		return stFaint.Render("-")
	case "estimated":
		return fmt.Sprintf("~%s in / %s out", humanInt(r.InputTokens), humanInt(r.OutputTokens))
	default:
		return fmt.Sprintf("%s in / %s out", humanInt(r.InputTokens), humanInt(r.OutputTokens))
	}
}

// requestCost shows a per-request price only when the provider actually
// reported one; an unknown price is a dash, never a misleading $0.00.
func requestCost(r UsageRequestRow) string {
	if !r.CostKnown {
		return stFaint.Render("-")
	}
	return "$" + fmtUSD(r.CostUSD)
}

// modelTokenText totals a model's tokens with an estimated marker; no tokens is
// an honest dash.
func modelTokenText(m UsageModel) string {
	tokens := m.InputTokens + m.OutputTokens
	if tokens <= 0 {
		return stFaint.Render("-")
	}
	if m.EstimatedRequests > 0 {
		return "~" + humanInt(tokens)
	}
	return humanInt(tokens)
}

// modelCostText shows a model's spend only for its priced requests.
func modelCostText(m UsageModel) string {
	if m.CostKnownRequests <= 0 {
		return stFaint.Render("-")
	}
	return "$" + fmtUSD(m.CostUSD)
}

// fmtUSD prints a dollar amount with enough precision that sub-cent spend does
// not round away to zero.
func fmtUSD(v float64) string {
	switch {
	case v <= 0:
		return "0.00"
	case v < 0.01:
		return fmt.Sprintf("%.4f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

// ── chronological chart primitives ───────────────────────────────────────────

var barGlyphs = []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

var (
	rgbMatcha    = rgb{143, 168, 121}
	rgbMatchaDim = rgb{99, 122, 82}
)

// seriesRequests and seriesTokens project a chronological series onto one
// dimension for charting.
func seriesRequests(series []UsageSeriesPoint) []float64 {
	out := make([]float64, len(series))
	for i, p := range series {
		out[i] = float64(p.Requests)
	}
	return out
}

func seriesTokens(series []UsageSeriesPoint) []float64 {
	out := make([]float64, len(series))
	for i, p := range series {
		out[i] = float64(p.InputTokens + p.OutputTokens)
	}
	return out
}

// columnPlan decides how many buckets to draw and how many cells wide each one
// is, so a short series still fills a wide canvas and a long one downsamples to
// fit rather than overflow.
func columnPlan(count, avail int) (n, colW int) {
	if count < 1 || avail < 1 {
		return 0, 1
	}
	if count >= avail {
		return avail, 1
	}
	colW = avail / count
	if colW < 1 {
		colW = 1
	}
	if colW > 6 {
		colW = 6
	}
	return count, colW
}

// resample buckets values into n groups by summation, preserving chronological
// order. Fewer values than groups pass through unchanged.
func resample(values []float64, n int) []float64 {
	if n <= 0 || len(values) == 0 {
		return nil
	}
	if len(values) <= n {
		return values
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		lo := i * len(values) / n
		hi := (i + 1) * len(values) / n
		if hi <= lo {
			hi = lo + 1
		}
		var sum float64
		for j := lo; j < hi && j < len(values); j++ {
			sum += values[j]
		}
		out[i] = sum
	}
	return out
}

// sparkColumns renders a chronological bar chart: one column per bucket, each
// colW cells wide and height rows tall, drawn bottom-up with eighth-block
// glyphs so short bars still read. Colour sweeps from→to bottom-to-top. It
// returns exactly height lines, top row first.
func sparkColumns(values []float64, height, colW int, from, to rgb) []string {
	if height < 1 {
		height = 1
	}
	if colW < 1 {
		colW = 1
	}
	maxV := 0.0
	for _, v := range values {
		if v > maxV {
			maxV = v
		}
	}
	lines := make([]string, height)
	for r := 0; r < height; r++ {
		floor := float64(height - 1 - r)
		tint := 1.0
		if height > 1 {
			tint = 1 - float64(r)/float64(height-1)
		}
		style := lipgloss.NewStyle().Foreground(mixRGB(from, to, tint))
		var b strings.Builder
		for _, v := range values {
			level := 0.0
			if maxV > 0 {
				level = v / maxV * float64(height)
			}
			portion := level - floor
			var g rune
			switch {
			case portion >= 1:
				g = '█'
			case portion <= 0:
				g = ' '
			default:
				idx := int(portion*8 + 0.5)
				if idx < 1 {
					idx = 1
				}
				if idx > 8 {
					idx = 8
				}
				g = barGlyphs[idx]
			}
			for c := 0; c < colW; c++ {
				b.WriteRune(g)
			}
		}
		lines[r] = style.Render(b.String())
	}
	return lines
}
