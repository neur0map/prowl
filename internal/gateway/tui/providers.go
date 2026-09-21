package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Providers is the free-tier storefront: every provider the gateway knows,
// what it costs, how much you'd get for an email address, and what it takes
// to sign up - the same information the reference dashboard showed as a dense
// table with badges, kept dense and scannable here.
//
// A row is browsable whether or not the gateway can serve it: Enter opens a
// read-only drill-down (never a mutation), o sends you to the signup page, and
// a adds a credential - but only where an adapter exists, and for a keyless
// provider it enables the endpoint in one action instead of demanding a key
// the provider never asked for. Two cycle filters (access and readiness) keep
// the long directory navigable, and their state is always on screen.

// accessFilters and readyFilters are the two cycle axes. The first entry of
// each ("all") is the neutral state, so a zero-valued model shows everything.
var accessFilters = []string{"all", "free", "credits", "subscription", "local", "paid"}
var readyFilters = []string{"all", "ready", "connected", "setup", "unavailable"}

type providersLoadedMsg struct {
	providers []DirectoryProvider
	counts    DirectoryCounts
	err       error
}

// providerDetailMsg carries a selected provider plus its quota reading back to
// the screen, which then opens the drill-down overlay. The probe is fetched
// off the render path so the modal opens with the number already in hand.
type providerDetailMsg struct {
	provider DirectoryProvider
	probe    probeReading
	err      error
}

// probeReading mirrors the informational quota response. The reading is
// read-only: it reports what the provider published, never a fabricated limit.
type probeReading struct {
	Kind      string  `json:"kind"`
	Published bool    `json:"published"`
	Message   string  `json:"message"`
	Limit     *int64  `json:"limit"`
	Remaining *int64  `json:"remaining"`
	ResetAt   *string `json:"resetAt"`
	Window    string  `json:"window"`
}

type providersModel struct {
	app       *App
	width     int
	height    int
	list      list
	data      providersLoadedMsg
	loaded    bool
	accessIdx int
	readyIdx  int
}

func (m *providersModel) Init() tea.Cmd { return nil }

func (m *providersModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.list.width = w
	m.list.height = max(h-2, 4)
}

func (m *providersModel) load() tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var resp DirectoryResponse
		if err := c.Get(ctx, "/api/providers/directory", &resp); err != nil {
			return providersLoadedMsg{err: err}
		}
		return providersLoadedMsg{providers: resp.Providers, counts: resp.Counts}
	}
}

func (m *providersModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case providersLoadedMsg:
		m.data = msg
		m.loaded = true
		m.buildRows()
		return m, nil

	case providerDetailMsg:
		m.app.overlay = newDetailOverlay(
			msg.provider.Name,
			detailSubtitle(msg.provider),
			m.detailBody(msg.provider, msg.probe, msg.err),
			detailActions(msg.provider),
		)
		return m, nil

	case tea.KeyPressMsg:
		if m.list.typeFilter(msg) {
			m.buildRows()
			return m, nil
		}
		switch msg.String() {
		case "up", "k":
			m.list.move(-1)
		case "down", "j":
			m.list.move(1)
		case "pgup":
			m.list.page(-1)
		case "pgdn":
			m.list.page(1)
		case "home", "g":
			m.list.top()
		case "end", "G":
			m.list.bot()
		case "/":
			m.list.startSearch()
		case "f":
			m.accessIdx = (m.accessIdx + 1) % len(accessFilters)
			m.buildRows()
		case "r":
			m.readyIdx = (m.readyIdx + 1) % len(readyFilters)
			m.buildRows()
		case "enter":
			if r := m.list.selected(); r != nil {
				return m, m.openDetail(r.key.(DirectoryProvider))
			}
		case "o":
			if r := m.list.selected(); r != nil {
				return m, m.openSignup(r.key.(DirectoryProvider))
			}
		case "a":
			if r := m.list.selected(); r != nil {
				return m, m.addCredential(r.key.(DirectoryProvider))
			}
		}
	}
	return m, nil
}

// buildRows renders the directory as a table honouring both cycle filters:
//
//	name                 access   free  ctx     setup    state
func (m *providersModel) buildRows() {
	// Free-tier-first, then by class cost, then name: the ordering is the
	// product's opinion - a user with no budget should see what they can use
	// now at the top, not an alphabetical wall.
	order := map[string]int{"free": 0, "credits": 1, "oauth": 2, "local": 3, "paid": 4}
	provs := append([]DirectoryProvider(nil), m.data.providers...)
	sortProviders(provs, order)
	rows := make([]row, 0, len(provs))
	for _, p := range provs {
		if !m.passesAccess(p) || !m.passesReady(p) {
			continue
		}
		rows = append(rows, row{
			cells: []string{
				p.Name,
				accessWord(p),
				fmt.Sprintf("%d", p.FreeModels),
				contextLabel(p.MaxContext),
				frictionLabel(p.Friction),
				stateCell(p),
			},
			styles: []func(string) string{
				nil,
				nil,
				func(s string) string {
					if s == "0" {
						return stFaint.Render(s)
					}
					return stHead.Render(s)
				},
				nil, nil, nil,
			},
			key: p,
		})
	}
	m.list.empty = "No providers match these filters. Press f or r to widen them."
	m.list.setRows(rows)
	if m.list.place == "" {
		m.list.place = "search providers…"
	}
}

func (m *providersModel) passesAccess(p DirectoryProvider) bool {
	want := accessFilters[m.accessIdx]
	return want == "all" || accessOf(p) == want
}

func (m *providersModel) passesReady(p DirectoryProvider) bool {
	want := readyFilters[m.readyIdx]
	return want == "all" || readinessOf(p) == want
}

func sortProviders(p []DirectoryProvider, order map[string]int) {
	// stable sort by (class order, free models desc, name)
	for i := 1; i < len(p); i++ {
		for j := i; j > 0; j-- {
			a, b := order[classOf(p[j])], order[classOf(p[j-1])]
			if a < b || (a == b && (p[j].FreeModels > p[j-1].FreeModels ||
				(p[j].FreeModels == p[j-1].FreeModels && p[j].Name < p[j-1].Name))) {
				p[j], p[j-1] = p[j-1], p[j]
			} else {
				break
			}
		}
	}
}

func classOf(p DirectoryProvider) string {
	switch p.Class {
	case "free", "permanent-free":
		return "free"
	case "credits", "renewable-credits":
		return "credits"
	case "oauth":
		return "oauth"
	case "local":
		return "local"
	default:
		return "paid"
	}
}

// accessOf maps a provider onto the access vocabulary the filter and modal use
// ("subscription", not the internal "oauth"), so the word the user cycles to
// is the word they see on the row.
func accessOf(p DirectoryProvider) string {
	if classOf(p) == "oauth" {
		return "subscription"
	}
	return classOf(p)
}

func accessWord(p DirectoryProvider) string {
	switch accessOf(p) {
	case "free":
		return stGood.Render("free")
	case "credits":
		return stWarn.Render("credits")
	case "subscription":
		return stWarn.Render("subscription")
	case "local":
		return stSubtle.Render("local")
	default:
		return stBad.Render("paid")
	}
}

// readinessOf partitions every provider into exactly one readiness bucket, the
// same four the readiness filter cycles through. It never calls an adapter-only
// provider ready: that honesty lives in the API's Ready flag, which this reads.
func readinessOf(p DirectoryProvider) string {
	switch {
	case !p.Adapter:
		return "unavailable"
	case p.Ready:
		return "ready"
	case p.Configured:
		return "connected"
	default:
		return "setup"
	}
}

func contextLabel(n int) string {
	switch {
	case n == 0:
		return stFaint.Render("-")
	case n >= 1_000_000:
		return fmt.Sprintf("%dM", n/1_000_000)
	default:
		return fmt.Sprintf("%dk", n/1000)
	}
}

func frictionLabel(f string) string {
	switch f {
	case "none":
		return stGood.Render("no signup")
	case "registration":
		return stSubtle.Render("email")
	case "phone":
		return stWarn.Render("phone")
	case "card":
		return stBad.Render("card")
	case "":
		return stFaint.Render("-")
	default:
		return f
	}
}

// stateCell folds readiness and the key-health tally into the State column so a
// row reports not just "has a key" but whether that key can serve.
func stateCell(p DirectoryProvider) string {
	switch readinessOf(p) {
	case "ready":
		badge := stGood.Render("● ready")
		if p.HealthyKeyCount > 0 {
			badge += stFaint.Render(fmt.Sprintf(" %d✓", p.HealthyKeyCount))
		}
		return badge
	case "connected":
		if p.ErrorKeyCount > 0 {
			return stBad.Render("● key error") + stFaint.Render(fmt.Sprintf(" %d✗", p.ErrorKeyCount))
		}
		// A key is on file but nothing routes yet - usually no synced models.
		return stWarn.Render("● connected")
	case "setup":
		if p.Keyless {
			return stSubtle.Render("● keyless")
		}
		return stSubtle.Render("● needs key")
	default:
		return stFaint.Render("● no adapter")
	}
}

// openDetail fetches the provider's quota reading, then hands both to the
// screen to raise the drill-down. It never mutates gateway state.
func (m *providersModel) openDetail(p DirectoryProvider) tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var pr probeReading
		err := c.Get(ctx, "/api/providers/"+p.ID+"/probe", &pr)
		return providerDetailMsg{provider: p, probe: pr, err: err}
	}
}

// detailActions is the modal's action set. Opening the signup page is the
// explicit primary action: Enter may open a browser, but it must never add a
// credential or mutate gateway state.
func detailActions(p DirectoryProvider) []action {
	acts := []action{{Key: "o", Label: "Open signup", Primary: true}}
	if p.Adapter {
		label := "Add credential"
		if p.Keyless {
			label = "Enable (keyless)"
		}
		acts = append(acts, action{Key: "a", Label: label})
	}
	return acts
}

func detailSubtitle(p DirectoryProvider) string {
	friction := p.Friction
	if friction == "" {
		friction = "signup unknown"
	}
	return fmt.Sprintf("%s · %s · %s", accessOf(p), readinessOf(p), friction)
}

// detailBody is the rich drill-down: access, setup friction, adapter, the model
// catalogue, key health, modalities, the quota probe, notes and the links.
func (m *providersModel) detailBody(p DirectoryProvider, pr probeReading, probeErr error) string {
	const kw = 12
	var b strings.Builder
	line := func(k, v string) { b.WriteString(keyRow(k, v, kw) + "\n") }

	line("Access", accessWord(p))
	line("Setup", frictionLabel(p.Friction))
	if p.Adapter {
		line("Adapter", stGood.Render(p.Platform))
	} else {
		line("Adapter", stBad.Render("none - cannot route here yet"))
	}
	line("Models", modelCatalogueLabel(p))
	line("Readiness", readinessDetail(p))
	line("Keys", keyHealthLine(p))
	if len(p.Modalities) > 0 {
		line("Modalities", stSubtle.Render(strings.Join(p.Modalities, ", ")))
	}

	b.WriteString("\n" + section("Quota") + "\n")
	b.WriteString(probeText(pr, probeErr) + "\n")

	if strings.TrimSpace(p.Note) != "" {
		b.WriteString("\n" + section("Notes") + "\n")
		b.WriteString(stSubtle.Render(p.Note) + "\n")
	}

	b.WriteString("\n" + section("Links") + "\n")
	if p.APIKeyURL != "" {
		line("Signup", stSubtle.Render(p.APIKeyURL))
	} else {
		line("Signup", stFaint.Render("none published"))
	}
	if p.DocsURL != "" {
		line("Docs", stSubtle.Render(p.DocsURL))
	}
	return b.String()
}

func modelCatalogueLabel(p DirectoryProvider) string {
	if p.ModelCount > 0 {
		return stHead.Render(fmt.Sprintf("%d served here", p.ModelCount)) +
			stFaint.Render(fmt.Sprintf("  ·  %d free advertised", p.FreeModels))
	}
	if p.FreeModels > 0 {
		return stFaint.Render(fmt.Sprintf("none synced yet  ·  %d free advertised", p.FreeModels))
	}
	return stFaint.Render("none")
}

func readinessDetail(p DirectoryProvider) string {
	switch readinessOf(p) {
	case "ready":
		return stGood.Render("ready - routable now")
	case "connected":
		if p.ErrorKeyCount > 0 {
			return stBad.Render("connected but keys are erroring")
		}
		return stWarn.Render("connected - no served model yet")
	case "setup":
		if p.Keyless {
			return stSubtle.Render("one action away - keyless, press a to enable")
		}
		return stSubtle.Render("needs a key - press a to add one")
	default:
		return stFaint.Render("no adapter - browse only, cannot route")
	}
}

func keyHealthLine(p DirectoryProvider) string {
	if p.KeyCount == 0 {
		if p.Keyless {
			return stFaint.Render("keyless - no credential stored yet")
		}
		return stFaint.Render("no credentials")
	}
	parts := []string{stSubtle.Render(fmt.Sprintf("%d enabled", p.EnabledKeyCount))}
	if p.HealthyKeyCount > 0 {
		parts = append(parts, stGood.Render(fmt.Sprintf("%d healthy", p.HealthyKeyCount)))
	}
	if p.ErrorKeyCount > 0 {
		parts = append(parts, stBad.Render(fmt.Sprintf("%d error", p.ErrorKeyCount)))
	}
	return strings.Join(parts, "  ")
}

// probeText renders the quota reading exactly as the provider published it: a
// real remaining figure when one exists, otherwise the provider's own reason
// for having none. It never invents a number.
func probeText(pr probeReading, err error) string {
	if err != nil {
		return stBad.Render("could not read quota: " + err.Error())
	}
	if pr.Published && pr.Remaining != nil {
		window := pr.Window
		if window == "" {
			window = "left"
		} else {
			window += " left"
		}
		head := stHead.Render(fmt.Sprintf("%d %s", *pr.Remaining, window))
		if pr.Limit != nil {
			head += stFaint.Render(fmt.Sprintf(" of %d", *pr.Limit))
		}
		if pr.ResetAt != nil {
			head += stFaint.Render("  ·  resets " + *pr.ResetAt)
		}
		if pr.Message != "" {
			return head + "\n" + stFaint.Render(pr.Message)
		}
		return head
	}
	if pr.Message != "" {
		return stSubtle.Render(pr.Message)
	}
	return stFaint.Render("no published quota")
}

// openSignup sends the user to the provider's signup page, falling back to its
// docs when there is no signup URL. It opens a browser; it changes nothing.
func (m *providersModel) openSignup(p DirectoryProvider) tea.Cmd {
	url := p.APIKeyURL
	if url == "" {
		url = p.DocsURL
	}
	if url == "" {
		return func() tea.Msg {
			return newToast("bad", p.Name+" publishes no signup or docs link to open")
		}
	}
	name := p.Name
	return func() tea.Msg {
		if err := openURL(url); err != nil {
			return newToast("bad", "Could not open the browser: "+err.Error())
		}
		return newToast("ok", "Opened "+name+" in your browser")
	}
}

// addCredential is the only mutating action here. It refuses to pretend an
// unsupported provider can take a key, and it never shows a required-key form
// for a keyless provider - that enables the endpoint in one action instead.
func (m *providersModel) addCredential(p DirectoryProvider) tea.Cmd {
	if !p.Adapter {
		return func() tea.Msg {
			return newToast("bad", p.Name+" has no adapter yet, so it cannot accept a key")
		}
	}
	if p.Keyless {
		return m.enableKeyless(p)
	}
	return m.openAdd(p)
}

// enableKeyless posts the no-key sentinel directly: a keyless provider needs a
// row so routing sees it as configured, but it never needs a secret from the
// user, so demanding one would be a lie.
func (m *providersModel) enableKeyless(p DirectoryProvider) tea.Cmd {
	c := m.app.Client
	platform := platformID(p)
	name := p.Name
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		body := map[string]string{"platform": platform, "key": "", "label": ""}
		var resp struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		}
		if err := c.Post(ctx, "/api/keys", body, &resp); err != nil {
			return errMsg{Screen: "providers", Err: err}
		}
		return doneMsg{Tab: TabProviders, Text: name + " enabled (keyless)"}
	}
}

// openAdd starts a credential form for the selected directory provider. The
// provider id is fixed by selection so the user only enters the secret.
func (m *providersModel) openAdd(p DirectoryProvider) tea.Cmd {
	f := newForm("Add credential - "+p.Name, "API key", "Label")
	f.secret(0)
	f.optional(1)
	platform := platformID(p)
	f.submit = func(values []string) tea.Msg {
		c := m.app.Client
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		body := map[string]string{"platform": platform, "key": values[0], "label": values[1]}
		var resp struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		}
		if err := c.Post(ctx, "/api/keys", body, &resp); err != nil {
			return errMsg{Screen: "providers", Err: err}
		}
		return doneMsg{Tab: TabProviders, Text: fmt.Sprintf("%s key added (%s)", p.Name, statusWord(resp.Status))}
	}
	f.after = func([]string) string { return p.Name + " credential added" }
	m.app.overlay = f
	return f.Init()
}

func platformID(p DirectoryProvider) string {
	if p.Platform != "" {
		return p.Platform
	}
	return p.ID
}

func statusWord(s string) string {
	if s == "" {
		return "unknown health"
	}
	return s
}

// filterBar shows both cycle filters and the key hints, so the active lens is
// never a mystery. Non-neutral filters are highlighted; "all" stays quiet.
func (m *providersModel) filterBar() string {
	seg := func(label, val string, active bool) string {
		shown := stSubtle.Render(val)
		if active {
			shown = stHead.Render(val)
		}
		return stFaint.Render(label+" ") + shown
	}
	left := seg("Access", accessFilters[m.accessIdx], m.accessIdx != 0) +
		stFaint.Render("  ·  ") +
		seg("Readiness", readyFilters[m.readyIdx], m.readyIdx != 0)
	hint := stFaint.Render("f access · r readiness · enter details · o signup · a add")
	return truncate(left+stFaint.Render("   ")+hint, max(m.width, 8))
}

func (m *providersModel) View() tea.View {
	if !m.loaded {
		return tea.NewView(roundedPanel(
			"Provider directory",
			m.app.spinner.View()+" "+stSubtle.Render("Reading compatible endpoints"),
			m.width,
		))
	}
	if m.data.err != nil {
		return tea.NewView(roundedPanel(
			"Provider directory unavailable",
			stBad.Render("● "+m.data.err.Error()),
			m.width,
		))
	}
	counts := m.data.counts
	var b strings.Builder
	b.WriteString(metricStrip([]metric{
		{"Directory", fmt.Sprintf("%d", counts.Total), "production adapters"},
		{"Ready", fmt.Sprintf("%d", counts.Ready), "routable now"},
		{"Free models", fmt.Sprintf("%d", counts.FreeModels), "no-cost options"},
		{"Connected", fmt.Sprintf("%d", counts.Configured), "with credentials"},
	}, m.width))
	b.WriteString("\n")
	b.WriteString(m.filterBar() + "\n")
	localY := strings.Count(b.String(), "\n")
	m.list.height = max(m.height-localY, 5)
	m.list.setOrigin(m.app.bodyX, m.app.bodyY+localY)
	b.WriteString(m.list.render())
	return tea.NewView(b.String())
}

func (m *providersModel) actions() []action {
	return []action{
		{Key: "enter", Label: "Details", Primary: true},
		{Key: "o", Label: "Open signup"},
		{Key: "a", Label: "Add credential"},
		{Key: "f", Label: "Filter access"},
		{Key: "r", Label: "Filter readiness"},
		{Key: "/", Label: "Search"},
	}
}

func (m *providersModel) searching() bool { return m.list.isSearching() }
