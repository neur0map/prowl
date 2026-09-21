package tui

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Tab is one section of the console.
type Tab int

const (
	TabOverview Tab = iota
	TabRouting
	TabProviders
	TabUsage
	TabProjects
	TabSetup
	TabToolkit
)

// tabMeta is what the sidebar and page heading show for a section. group is
// the sidebar heading it sits under; an empty group is the top-level entry.
type tabMeta struct {
	name  string
	desc  string
	glyph string
	group string
}

var tabsMeta = []tabMeta{
	{"Home", "Project intelligence and gateway readiness at a glance.", "◉", ""},
	{"Routing", "Sets of models and the strategy each one routes with.", "⌁", "Gateway"},
	{"Providers", "Connect API keys and subscription accounts. Only connected providers offer models.", "◇", "Gateway"},
	{"Activity", "Which provider and model served each request, with tokens, latency and outcomes.", "▥", "Gateway"},
	{"Projects", "Every indexed project, its coverage and freshness.", "▦", "Workspace"},
	{"Setup", "Connect coding harnesses to Prowl safely.", "△", "Workspace"},
	{"Toolkit", "Every Prowl function and its copyable command.", "◫", "Workspace"},
}

func (t Tab) name() string { return tabsMeta[t].name }

type errMsg struct {
	Screen string
	Err    error
	// op is the id of the overlay operation that produced this error (0 when
	// the error did not originate from a confirm/form). dismissActiveOverlay
	// closes an overlay only for its own op, so an unrelated failure cannot.
	op uint64
}

func (e errMsg) String() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

type toastMsg struct {
	Text    string
	Kind    string
	Expires time.Time
}

// toastExpiredMsg is the deadline signal for a specific toast, carrying the
// exact expiry it was armed for. Update clears the toast only when that expiry
// still matches what is on screen, so a newer toast is never yanked away by an
// earlier toast's timer. The captured value keeps the timer goroutine from
// reading mutable model state.
type toastExpiredMsg struct{ expires time.Time }

type tickMsg time.Time

func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type doneMsg struct {
	Tab  Tab
	Text string
	// op is the id of the overlay operation that produced this result (0 when
	// it was not launched from a confirm/form), matched in dismissActiveOverlay
	// so only the owning overlay is dismissed.
	op uint64
}

// overlayOpSeq assigns each overlay-launched operation a process-unique id, so a
// completion can be matched to the exact confirm/form that started it. A
// background completion from an unrelated operation therefore cannot dismiss a
// confirm or form the operator is still waiting on.
var overlayOpSeq atomic.Uint64

func nextOverlayOp() uint64 { return overlayOpSeq.Add(1) }

// tagOverlayOp stamps a result message with the id of the overlay operation
// that produced it. Messages that can complete a form or confirm carry the tag;
// unrelated asynchronous messages pass through unchanged.
func tagOverlayOp(msg tea.Msg, op uint64) tea.Msg {
	switch m := msg.(type) {
	case doneMsg:
		m.op = op
		return m
	case setCreatedMsg:
		m.op = op
		return m
	case keyRegeneratedMsg:
		m.op = op
		return m
	case errMsg:
		m.op = op
		return m
	}
	return msg
}

type refreshMsg struct{ Tab Tab }
type closeOverlayMsg struct{ Reload Tab }
type actionChosenMsg struct{ Key string }

type action struct {
	Key       string
	Label     string
	Primary   bool
	Dangerous bool
}

type actionProvider interface{ actions() []action }

type hitKind int

const (
	hitNavigation hitKind = iota
	hitAction
)

type hitRegion struct {
	x, y, w, h int
	kind       hitKind
	tab        Tab
	key        string
}

func (r hitRegion) contains(x, y int) bool {
	return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h
}

// App is the gateway console's root model.
type App struct {
	Client           *Client
	Owned            bool
	DaemonManageable bool
	KeepGateway      bool
	Version          string

	width, height int
	tab           Tab
	tabs          []Tab

	overview  overviewModel
	routing   routingModel
	providers providersModel
	usage     usageModel
	projects  projectsModel
	setup     setupModel
	toolkit   toolkitModel

	spinner spinner.Model
	overlay tea.Model
	toast   toastMsg

	hits                       []hitRegion
	hover                      hitRegion
	hoverActive                bool
	sideW                      int
	bodyX, bodyY, bodyW, bodyH int
	quitting, ready            bool
	motionPhase                int
	visited                    map[Tab]bool
}

// New builds the console over a live client.
func New(client *Client, owned bool, version string) *App {
	a := &App{
		Client:           client,
		Owned:            owned,
		DaemonManageable: owned,
		Version:          version,
		tabs:             []Tab{TabOverview, TabRouting, TabProviders, TabUsage, TabProjects, TabSetup, TabToolkit},
		visited:          map[Tab]bool{TabOverview: true},
		spinner: spinner.New(
			spinner.WithSpinner(spinner.Line),
			spinner.WithStyle(stWarn),
		),
	}
	a.overview = overviewModel{app: a}
	a.routing = newRoutingModel(a)
	a.providers = newProvidersModel(a)
	a.usage = usageModel{app: a}
	a.projects = projectsModel{app: a, list: newList("Search projects")}
	a.projects.list.setHeaders("Project", "Index", "Files", "Symbols", "Edges", "Est. saved", "Updated")
	a.toolkit = newToolkitModel(a)
	a.setup = setupModel{app: a, list: newList("Search harnesses")}
	return a
}

func (a *App) Init() tea.Cmd {
	return tea.Batch(a.refreshCmd(a.tab), tickEvery(5*time.Second), a.spinner.Tick, firstRunTutorial())
}

func (a *App) screen() tea.Model { return a.screenFor(a.tab) }

func (a *App) refreshCmd(tab Tab) tea.Cmd {
	s := a.screenFor(tab)
	if loader, ok := s.(interface{ load() tea.Cmd }); ok {
		return loader.load()
	}
	return nil
}

func (a *App) screenFor(tab Tab) tea.Model {
	switch tab {
	case TabOverview:
		return &a.overview
	case TabRouting:
		return &a.routing
	case TabProviders:
		return &a.providers
	case TabUsage:
		return &a.usage
	case TabProjects:
		return &a.projects
	case TabSetup:
		return &a.setup
	case TabToolkit:
		return &a.toolkit
	}
	return nil
}

func (a *App) screenLoading(tab Tab) bool {
	switch tab {
	case TabOverview:
		return a.overview.loadedAt.IsZero()
	case TabRouting:
		return !a.routing.loaded
	case TabProviders:
		return !a.providers.loaded
	case TabUsage:
		return !a.usage.loaded
	case TabProjects:
		return !a.projects.loaded
	case TabSetup:
		return !a.setup.loaded
	default:
		return false
	}
}

func (a *App) currentList() *list {
	switch a.tab {
	case TabRouting:
		return a.routing.activeList()
	case TabProviders:
		return &a.providers.list
	case TabProjects:
		return &a.projects.list
	case TabToolkit:
		return &a.toolkit.list
	case TabSetup:
		return &a.setup.list
	default:
		return nil
	}
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case spinner.TickMsg:
		if !a.screenLoading(a.tab) {
			return a, nil
		}
		var cmd tea.Cmd
		a.spinner, cmd = a.spinner.Update(msg)
		return a, cmd
	case tea.WindowSizeMsg:
		a.width, a.height, a.ready = msg.Width, msg.Height, true
		a.applyLayout()
		if a.overlay != nil {
			m, cmd := a.overlay.Update(msg)
			a.overlay = m
			return a, cmd
		}
		return a, nil
	case toastMsg:
		a.toast = msg
		// A cleared or already-expired toast must not schedule another tick:
		// tea.Tick on a zero/past deadline fires immediately and would spin.
		if msg.Text == "" || msg.Expires.IsZero() {
			return a, nil
		}
		// Capture the expiry for the timer instead of reading a.toast from the
		// tick goroutine (a data race): Update decides staleness when the
		// deadline signal arrives.
		expires := msg.Expires
		return a, tea.Tick(time.Until(expires), func(time.Time) tea.Msg {
			return toastExpiredMsg{expires: expires}
		})
	case toastExpiredMsg:
		// Clear only the toast this deadline was armed for; a newer toast with a
		// different expiry supersedes it and this completion is stale.
		if a.toast.Expires.Equal(msg.expires) {
			a.toast = toastMsg{}
		}
		return a, nil
	case errMsg:
		a.dismissActiveOverlay(msg.op)
		// The failing action left its owning screen busy; clear it so the
		// screen stays responsive instead of stuck behind a spinner.
		switch msg.Screen {
		case "providers":
			a.providers.busy = ""
		case "setup":
			a.setup.busy = ""
		case "routing":
			a.routing.busy = ""
		}
		return a, a.showToast("bad", msg.String())
	case doneMsg:
		a.dismissActiveOverlay(msg.op)
		if msg.Text != "" {
			cmds = append(cmds, a.showToast("ok", msg.Text))
		}
		cmds = append(cmds, a.refreshCmd(msg.Tab))
		return a, tea.Batch(cmds...)
	case setCreatedMsg:
		a.dismissActiveOverlay(msg.op)
		_, cmd := a.routing.Update(msg)
		return a, tea.Batch(
			a.showToast("ok", "model set created"),
			cmd,
		)
	case keyRegeneratedMsg:
		a.dismissActiveOverlay(msg.op)
		_, cmd := a.overview.Update(msg)
		return a, tea.Batch(
			a.showToast("ok", "API key regenerated - repoint your harnesses (Setup re-injects it)"),
			cmd,
		)
	case closeOverlayMsg:
		a.overlay = nil
		if msg.Reload >= 0 {
			return a, a.refreshCmd(msg.Reload)
		}
		return a, nil
	case providersLoadedMsg, providerUsageLoadedMsg, providerDetailMsg, revealMsg, revealExpiredMsg,
		signInStartedMsg, signInStartFailedMsg, signInPolledMsg, signInPollErrMsg, signInTickMsg, signInCancelledMsg:
		// Provider work keeps running while the operator browses another
		// section or has a manage overlay open. Route the asynchronous results
		// back to the owning model instead of letting the current screen or
		// overlay swallow them.
		_, cmd := a.providers.Update(msg)
		return a, cmd
	case overviewMsg:
		_, cmd := a.overview.Update(msg)
		return a, cmd
	case projectsLoadedMsg:
		_, cmd := a.projects.Update(msg)
		return a, cmd
	case routingLoadedMsg, setEditLoadedMsg:
		_, cmd := a.routing.Update(msg)
		return a, cmd
	case usageLoadedMsg:
		_, cmd := a.usage.Update(msg)
		return a, cmd
	case actionChosenMsg:
		a.overlay = nil
		return a, a.activate(msg.Key)
	case tickMsg:
		if a.toast.Text != "" && a.toast.Expires.Before(time.Time(msg)) {
			a.toast = toastMsg{}
		}
		if a.tab == TabOverview {
			cmds = append(cmds, a.refreshCmd(TabOverview))
		}
		cmds = append(cmds, tickEvery(5*time.Second))
		return a, tea.Batch(cmds...)

	}

	if a.overlay != nil {
		m, cmd := a.overlay.Update(msg)
		a.overlay = m
		return a, cmd
	}

	if motion, ok := msg.(tea.MouseMotionMsg); ok {
		a.updateHover(motion.X, motion.Y)
		if l := a.currentList(); l != nil {
			_, _ = l.mouse(motion)
		}
		return a, nil
	}

	if click, ok := msg.(tea.MouseClickMsg); ok {
		if cmd, handled := a.handleClick(click); handled {
			return a, cmd
		}
	}
	if wheel, ok := msg.(tea.MouseWheelMsg); ok {
		if l := a.currentList(); l != nil {
			if handled, _ := l.mouse(wheel); handled {
				return a, nil
			}
		}
	}

	if key, ok := msg.(tea.KeyPressMsg); ok {
		if s, searchable := a.screen().(interface{ searching() bool }); searchable && s.searching() {
			if key.String() == "ctrl+c" {
				a.quitting = true
				return a, tea.Quit
			}
			m, cmd := a.screen().Update(msg)
			_ = m
			return a, cmd
		}
		switch k := key.String(); {
		case k == "ctrl+c" || k == "q":
			a.quitting = true
			return a, tea.Quit
		case k == "tab":
			return a, a.switchTab(1)
		case k == "shift+tab":
			return a, a.switchTab(-1)
		case len(k) == 1 && k[0] >= '1' && k[0] <= '9':
			idx := int(k[0] - '1')
			if idx < len(a.tabs) {
				return a, a.navigateTo(a.tabs[idx])
			}
		case k == "ctrl+r":
			return a, a.refreshCmd(a.tab)
		case k == "?":
			a.overlay = newHelpOverlay()
			a.sizeOverlay()
			return a, nil
		case k == ".":
			return a, a.activate(".")
		}
	}

	before := a.overlay
	m, cmd := a.screen().Update(msg)
	_ = m
	if before == nil && a.overlay != nil {
		a.sizeOverlay()
	}
	return a, cmd
}

func (a *App) switchTab(delta int) tea.Cmd {
	idx := (indexOf(a.tabs, a.tab) + delta + len(a.tabs)) % len(a.tabs)
	return a.navigateTo(a.tabs[idx])
}

func (a *App) navigateTo(tab Tab) tea.Cmd {
	if tab == a.tab {
		return nil
	}
	a.tab = tab
	a.motionPhase = int(tab) * 11
	a.hoverActive = false
	a.applyLayout()
	cmds := []tea.Cmd{a.refreshCmd(a.tab)}
	if a.screenLoading(tab) {
		cmds = append(cmds, a.spinner.Tick)
	}
	if !a.visited[tab] {
		a.visited[tab] = true
		if tip := tipFor(tab); tip != "" {
			cmds = append(cmds, func() tea.Msg { return newToast("tip", tip) })
		}
	}
	return tea.Batch(cmds...)
}

func (a *App) handleClick(click tea.MouseClickMsg) (tea.Cmd, bool) {
	for i := len(a.hits) - 1; i >= 0; i-- {
		h := a.hits[i]
		if !h.contains(click.X, click.Y) {
			continue
		}
		switch h.kind {
		case hitNavigation:
			return a.navigateTo(h.tab), true
		case hitAction:
			return a.activate(h.key), true
		}
	}
	if l := a.currentList(); l != nil {
		handled, activate := l.mouse(click)
		if activate {
			return a.activate("enter"), true
		}
		return nil, handled
	}
	return nil, false
}

func (a *App) activate(key string) tea.Cmd {
	switch key {
	case "ctrl+r":
		return a.refreshCmd(a.tab)
	case "?":
		a.overlay = newHelpOverlay()
		a.sizeOverlay()
		return nil
	case "q":
		a.quitting = true
		return tea.Quit
	case ".", "__more__":
		if provider, ok := a.screen().(actionProvider); ok {
			a.overlay = newActionOverlay(provider.actions())
			a.sizeOverlay()
		}
		return nil
	}
	before := a.overlay
	m, cmd := a.screen().Update(syntheticKey(key))
	_ = m
	if before == nil && a.overlay != nil {
		a.sizeOverlay()
	}
	return cmd
}

func syntheticKey(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	r := []rune(s)
	if len(r) == 0 {
		return tea.KeyPressMsg{}
	}
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func (a *App) sizeOverlay() {
	if a.overlay == nil || a.width == 0 {
		return
	}
	m, _ := a.overlay.Update(tea.WindowSizeMsg{Width: a.width, Height: a.height})
	a.overlay = m
}

func indexOf(haystack []Tab, needle Tab) int {
	for i, t := range haystack {
		if t == needle {
			return i
		}
	}
	return 0
}

func (a *App) showToast(kind, text string) tea.Cmd {
	a.toast = toastMsg{Text: text, Kind: kind, Expires: time.Now().Add(6 * time.Second)}
	return nil
}

// dismissActiveOverlay closes only the overlay that launched the operation whose
// result just arrived: a confirming confirmOverlay or a submitting form whose
// own op id matches the completion. A passive overlay the operator opened
// meanwhile - help, a manage panel, a picker - has no op and is left
// untouched, and a completion from a *different* in-flight operation (op
// mismatch) is ignored, so an unrelated background success or error can never
// yank a confirm or form off the screen.
func (a *App) dismissActiveOverlay(op uint64) {
	switch o := a.overlay.(type) {
	case *confirmOverlay:
		if o.running && o.op == op {
			a.overlay = nil
		}
	case *formOverlay:
		if o.submitting && o.op == op {
			a.overlay = nil
		}
	}
}

func (a *App) updateHover(x, y int) {
	a.hoverActive = false
	for i := len(a.hits) - 1; i >= 0; i-- {
		if a.hits[i].contains(x, y) {
			a.hover = a.hits[i]
			a.hoverActive = true
			return
		}
	}
}

func (a *App) hovered(kind hitKind, tab Tab, key string) bool {
	if !a.hoverActive || a.hover.kind != kind {
		return false
	}
	if kind == hitNavigation {
		return a.hover.tab == tab
	}
	return a.hover.key == key
}

// Layout: a two-row header, a sidebar beside the content, and a two-row
// footer. The content opens with a two-row page heading, so the page body
// starts at bodyY and the chrome costs six rows in total.
const (
	headerH     = 2
	headingH    = 2
	footerH     = 2
	sideWide    = 15
	sideNarrow  = 4
	sideCollaps = 96 // below this width the sidebar shows glyphs only
)

func (a *App) applyLayout() {
	if a.width == 0 || a.height == 0 {
		return
	}
	a.sideW = sideWide
	if a.width < sideCollaps {
		a.sideW = sideNarrow
	}
	a.bodyX = a.sideW + 2
	a.bodyY = headerH + headingH
	a.bodyW = max(a.width-a.bodyX-1, 20)
	a.bodyH = max(a.height-headerH-headingH-footerH, 4)
	if resizable, ok := a.screen().(interface{ setSize(int, int) }); ok {
		resizable.setSize(a.bodyW, a.bodyH)
	}
}

func (a *App) View() tea.View {
	if !a.ready {
		v := tea.NewView(brandText("PROWL", a.motionPhase) + " " + stSubtle.Render("starting gateway…"))
		v.AltScreen = true
		v.BackgroundColor = themeBackground()
		v.ForegroundColor = themeForeground()
		return v
	}

	a.hits = a.hits[:0]
	a.applyLayout()
	canvasW := max(a.width-1, 1)

	content := a.pageHeading() + "\n" + fillLines(a.screen().View().Content, a.bodyW, a.bodyH)
	contentH := headingH + a.bodyH
	contentLines := strings.Split(fillLines(content, a.bodyW, contentH), "\n")
	sideLines := strings.Split(a.sidebar(contentH), "\n")
	sep := stFaint.Render("│")
	middle := make([]string, 0, contentH)
	for i := range contentH {
		middle = append(middle, sideLines[i]+sep+" "+contentLines[i])
	}

	view := strings.Join([]string{
		a.header(),
		strings.Join(middle, "\n"),
		a.footer(),
	}, "\n")
	view = fillLines(view, canvasW, a.height)
	if a.overlay != nil {
		if ov, ok := a.overlay.(interface{ View() tea.View }); ok {
			view = mergeOverlay(view, ov.View().Content, a.width, a.height).Content
		}
	}

	v := tea.NewView(view)
	v.AltScreen = true
	// Native mouse capture stays off so the terminal keeps its own text
	// selection and copy - no Shift dance required. Every screen action is
	// reachable from the keyboard, so nothing is lost by ceding the pointer.
	v.MouseMode = tea.MouseModeNone
	v.BackgroundColor = themeBackground()
	v.ForegroundColor = themeForeground()
	v.WindowTitle = "Prowl"
	return v
}

// header is the brand row and the rule beneath it: who this is, where it is
// listening, and whether it is alive - the facts a dashboard keeps in its top
// bar.
func (a *App) header() string {
	width := max(a.width-1, 8)
	mode := "attached"
	if a.Owned {
		mode = "session"
	}
	brand := " " + brandText("PROWL", a.motionPhase) + "  " + stSubtle.Render("control plane")
	endpoint := strings.TrimPrefix(a.Client.BaseURL, "http://127.0.0.1")
	status := stGood.Render("● online") + stFaint.Render("  ·  ") +
		stSubtle.Render(endpoint) + stFaint.Render("  ·  "+mode+"  ·  "+a.Version) + " "
	return joinEdges(brand, status, width) + "\n" + " " + gradientRule(width-1, a.motionPhase)
}

// sidebar is the section list: grouped, with the active section lit and its
// number beside it so the keyboard shortcut is never a guess. Below the
// collapse width only the glyphs remain.
func (a *App) sidebar(height int) string {
	wide := a.sideW == sideWide
	lines := make([]string, 0, height)
	y := headerH
	lastGroup := ""
	for _, tab := range a.tabs {
		meta := tabsMeta[tab]
		if meta.group != lastGroup {
			lastGroup = meta.group
			if wide {
				lines = append(lines, "", " "+stFaint.Render(strings.ToUpper(meta.group)))
			} else {
				lines = append(lines, "", "")
			}
			y += 2
		}
		number := fmt.Sprintf("%d", indexOf(a.tabs, tab)+1)
		var label string
		switch {
		case wide:
			label = padRight(" "+meta.glyph+" "+meta.name, a.sideW-2) + stFaint.Render(number)
		default:
			label = " " + meta.glyph + " "
		}
		active := tab == a.tab
		hovered := a.hovered(hitNavigation, tab, "")
		var rendered string
		switch {
		case active:
			text := padRight(" "+meta.glyph+" "+meta.name, a.sideW-2)
			if !wide {
				text = " " + meta.glyph + " "
			}
			rendered = brandText("▌", a.motionPhase) +
				lipgloss.NewStyle().Background(colorRaised).Render(brandText(text, a.motionPhase+3))
			if wide {
				rendered += lipgloss.NewStyle().Background(colorRaised).Foreground(colorGold).Render(number)
			}
		case hovered:
			rendered = " " + lipgloss.NewStyle().Background(colorShoji).Foreground(colorMoon).Render(label)
		default:
			rendered = " " + lipgloss.NewStyle().Foreground(colorMist).Render(label)
		}
		lines = append(lines, padRight(rendered, a.sideW))
		a.hits = append(a.hits, hitRegion{x: 0, y: y, w: a.sideW, h: 1, kind: hitNavigation, tab: tab})
		y++
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = padRight(truncate(lines[i], a.sideW), a.sideW)
	}
	return strings.Join(lines[:height], "\n")
}

// pageHeading names the section and describes it in one line; a toast takes
// over that line while it lasts, so transient messages never push the body.
func (a *App) pageHeading() string {
	meta := tabsMeta[a.tab]
	heading := meta.glyph + "  " + meta.name
	// A screen may name where it has drilled to; the heading shows it as a
	// breadcrumb so the title carries the operator's location, not just the
	// section.
	if c, ok := a.screen().(interface{ crumb() string }); ok {
		if cr := c.crumb(); cr != "" {
			heading += " › " + cr
		}
	}
	title := brandText(heading, a.motionPhase)
	context := ""
	if s, ok := a.screen().(interface{ headline() string }); ok {
		context = stSubtle.Render(s.headline())
	}
	line1 := joinEdges(title, context+" ", a.bodyW)
	line2 := stSubtle.Render(truncate(meta.desc, max(a.bodyW-1, 1)))
	if a.toast.Text != "" {
		line2 = pillSuffix(a.toast.Kind) + " " + truncate(a.toast.Text, max(a.bodyW-3, 1))
	}
	return padRight(line1, a.bodyW) + "\n" + padRight(line2, a.bodyW)
}

func joinEdges(left, right string, width int) string {
	gap := max(width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return truncate(left+strings.Repeat(" ", gap)+right, width)
}

// footer keeps the current screen's keybinds visible. Toasts intentionally
// render in pageHeading so transient messages never replace these controls.
func (a *App) footer() string {
	width := max(a.width-1, 8)
	top := " " + stFaint.Render(strings.Repeat("─", width-1))

	globals := []action{
		{Key: "?", Label: "Help"},
		{Key: "q", Label: "Quit"},
	}
	globalParts := make([]string, 0, len(globals))
	for _, item := range globals {
		globalParts = append(globalParts, actionChip(
			item.Key,
			item.Label,
			false,
			false,
			a.hovered(hitAction, 0, item.Key),
		))
	}
	globalLine := strings.Join(globalParts, " ")
	globalW := lipgloss.Width(globalLine)
	globalX := max(width-globalW-1, 1)

	var left strings.Builder
	left.WriteString(" ")
	x := 1
	if provider, ok := a.screen().(actionProvider); ok {
		screenActions := provider.actions()
		more := actionChip(".", "More", false, false, a.hovered(hitAction, 0, "__more__"))
		moreW := lipgloss.Width(more)
		shown := 0
		for i, item := range screenActions {
			rendered := actionChip(
				item.Key,
				item.Label,
				item.Primary,
				item.Dangerous,
				a.hovered(hitAction, 0, item.Key),
			)
			w := lipgloss.Width(rendered)
			reserve := 0
			if i < len(screenActions)-1 {
				reserve = moreW + 1
			}
			if x+w+1+reserve >= globalX {
				break
			}
			left.WriteString(rendered)
			left.WriteString(" ")
			a.hits = append(a.hits, hitRegion{x: x, y: a.height - 1, w: w, h: 1, kind: hitAction, key: item.Key})
			x += w + 1
			shown++
		}
		if shown < len(screenActions) && x+moreW+1 < globalX {
			left.WriteString(more)
			left.WriteString(" ")
			a.hits = append(a.hits, hitRegion{x: x, y: a.height - 1, w: moreW, h: 1, kind: hitAction, key: "__more__"})
		}
	}
	gap := max(globalX-lipgloss.Width(left.String()), 1)
	line := left.String() + strings.Repeat(" ", gap) + globalLine

	x = globalX
	for i, item := range globals {
		renderedW := lipgloss.Width(globalParts[i])
		a.hits = append(a.hits, hitRegion{x: x, y: a.height - 1, w: renderedW, h: 1, kind: hitAction, key: item.Key})
		x += renderedW + 1
	}
	return top + "\n" + padRight(truncate(line, width), width)
}

func pillSuffix(kind string) string {
	switch kind {
	case "ok":
		return stGood.Render("●")
	case "bad":
		return stBad.Render("●")
	default:
		return stWarn.Render("●")
	}
}
