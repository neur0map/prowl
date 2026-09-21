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

// Tab is one screen of the console.
type Tab int

const (
	TabOverview Tab = iota
	TabProjects
	TabProviders
	TabKeys
	TabLogins
	TabRouting
	TabUsage
	TabToolkit
	TabSetup
)

type tabMeta struct {
	name  string
	short string
	desc  string
	glyph string
}

var tabsMeta = []tabMeta{
	{"Overview", "Home", "Project intelligence and gateway readiness at a glance.", "◉"},
	{"Projects", "Projects", "See every indexed project, its coverage and freshness.", "▦"},
	{"Providers", "Providers", "Browse service endpoints and add bring-your-own API keys.", "◇"},
	{"Credentials", "Keys", "Manage individual API keys, health and cooldowns.", "◆"},
	{"Accounts", "Accounts", "Connect subscription accounts and use their models directly.", "◎"},
	{"Models", "Models", "Build model sets and choose how Prowl routes each request.", "⌁"},
	{"Activity", "Activity", "See which provider and model served each request, with tokens, latency and outcomes.", "▥"},
	{"Toolkit", "Toolkit", "Understand every Prowl function and copy its command.", "◫"},
	{"Setup", "Setup", "Connect coding harnesses to Prowl safely.", "△"},
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
	projects  projectsModel
	providers providersModel
	keys      keysModel
	logins    loginsModel
	routing   routingModel
	usage     usageModel
	toolkit   toolkitModel
	setup     setupModel

	spinner spinner.Model
	overlay tea.Model
	toast   toastMsg

	hits                       []hitRegion
	hover                      hitRegion
	hoverActive                bool
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
		tabs:             []Tab{TabOverview, TabProjects, TabProviders, TabKeys, TabLogins, TabRouting, TabUsage, TabToolkit, TabSetup},
		visited:          map[Tab]bool{TabOverview: true},
		spinner: spinner.New(
			spinner.WithSpinner(spinner.Line),
			spinner.WithStyle(stWarn),
		),
	}
	a.overview = overviewModel{app: a}
	a.projects = projectsModel{app: a, list: newList("Search projects")}
	a.projects.list.setHeaders("Project", "Index", "Files", "Symbols", "Edges", "Est. saved", "Updated")
	a.providers = providersModel{app: a, list: newList("Search providers")}
	a.providers.list.setHeaders("Provider", "Access", "Free", "Context", "Setup", "State")
	a.keys = keysModel{app: a, list: newList("Search credentials")}
	a.keys.list.setHeaders("Credential", "Health", "State", "Identity", "Traffic", "Cooldown")
	a.logins = loginsModel{app: a, list: newList("Search accounts")}
	a.logins.list.setHeaders("Account", "Connection", "Routing", "Allowance", "Identity")
	a.routing = routingModel{app: a, list: newList("Search model sets")}
	a.routing.list.setHeaders("Model set", "Models", "State")
	a.usage = usageModel{app: a}
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
	case TabProjects:
		return &a.projects
	case TabProviders:
		return &a.providers
	case TabKeys:
		return &a.keys
	case TabLogins:
		return &a.logins
	case TabRouting:
		return &a.routing
	case TabUsage:
		return &a.usage
	case TabToolkit:
		return &a.toolkit
	case TabSetup:
		return &a.setup
	}
	return nil
}

func (a *App) screenLoading(tab Tab) bool {
	switch tab {
	case TabOverview:
		return a.overview.loadedAt.IsZero()
	case TabProjects:
		return !a.projects.loaded
	case TabProviders:
		return !a.providers.loaded
	case TabKeys:
		return !a.keys.loaded
	case TabLogins:
		return !a.logins.loaded
	case TabRouting:
		return !a.routing.loaded
	case TabUsage:
		return !a.usage.loaded
	case TabToolkit:
		return false
	case TabSetup:
		return !a.setup.loaded
	default:
		return false
	}
}

func (a *App) currentList() *list {
	switch a.tab {
	case TabProjects:
		return &a.projects.list
	case TabProviders:
		return &a.providers.list
	case TabKeys:
		return &a.keys.list
	case TabLogins:
		return &a.logins.list
	case TabRouting:
		return &a.routing.list
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
		case "keys":
			a.keys.busy = ""
		case "setup":
			a.setup.busy = ""
		case "logins":
			a.logins.busy = ""
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
	case closeOverlayMsg:
		a.overlay = nil
		if msg.Reload >= 0 {
			return a, a.refreshCmd(msg.Reload)
		}
		return a, nil
	case loginsLoadedMsg, loginsUsageLoadedMsg, signInStartedMsg, signInStartFailedMsg, signInPolledMsg, signInPollErrMsg, signInTickMsg, signInCancelledMsg:
		// Login flows keep polling while the operator browses another tab or
		// opens an account detail. Route their asynchronous results back to the
		// owning model instead of letting the current screen or overlay swallow
		// them.
		_, cmd := a.logins.Update(msg)
		if _, usage := msg.(loginsUsageLoadedMsg); usage && a.tab == TabLogins {
			if _, detailOpen := a.overlay.(*detailOverlay); detailOpen {
				if selected := a.logins.list.selected(); selected != nil {
					row, ok := selected.key.(loginRowData)
					if ok && row.platform.SignedIn {
						a.overlay = a.logins.accountDetail(row)
						a.sizeOverlay()
					}
				}
			}
		}
		return a, cmd
	case overviewMsg:
		_, cmd := a.overview.Update(msg)
		return a, cmd
	case projectsLoadedMsg:
		_, cmd := a.projects.Update(msg)
		return a, cmd
	case providersLoadedMsg:
		_, cmd := a.providers.Update(msg)
		return a, cmd
	case keysLoadedMsg, revealMsg, revealExpiredMsg:
		_, cmd := a.keys.Update(msg)
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
		case k == "tab" || k == "right":
			return a, a.switchTab(1)
		case k == "shift+tab" || k == "left":
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
// meanwhile - help, the account detail, a picker - has no op and is left
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

func (a *App) applyLayout() {
	if a.width == 0 || a.height == 0 {
		return
	}
	const headerH = 5
	const headingH = 3
	a.bodyX = 1
	a.bodyY = headerH + headingH
	a.bodyW = max(a.width-3, 20)
	a.bodyH = max(a.height-headerH-headingH, 4)
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
	body := fillLines(a.screen().View().Content, a.bodyW, a.bodyH)
	bodyLines := strings.Split(body, "\n")
	for i := range bodyLines {
		bodyLines[i] = " " + bodyLines[i]
	}
	body = strings.Join(bodyLines, "\n")
	content := strings.Join([]string{
		a.header(),
		a.pageHeading(),
		body,
	}, "\n")
	content = fillLines(content, max(a.width-1, 1), a.height)
	if a.overlay != nil {
		if ov, ok := a.overlay.(interface{ View() tea.View }); ok {
			content = mergeOverlay(content, ov.View().Content, a.width, a.height).Content
		}
	}

	v := tea.NewView(content)
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

func (a *App) header() string {
	width := max(a.width-1, 8)
	inner := width - 2
	mode := "attached"
	if a.Owned {
		mode = "session"
	}
	brand := " " + brandText("PROWL", a.motionPhase) + stFaint.Render(" / ") + stHead.Render("control plane")
	status := stGood.Render("● online")
	brandRow := joinEdges(brand, status+" ", inner)

	endpoint := strings.TrimPrefix(a.Client.BaseURL, "http://127.0.0.1")
	meta := " " + stSubtle.Render(endpoint)
	build := stFaint.Render(mode+"  "+a.Version) + " "
	metaRow := joinEdges(meta, build, inner)

	return strings.Join([]string{
		stFaint.Render("╭") + gradientRule(inner, a.motionPhase) + stFaint.Render("╮"),
		frameRow(brandRow, inner),
		frameRow(metaRow, inner),
		frameRow(a.navigation(inner), inner),
		stFaint.Render("╰") + gradientRule(inner, a.motionPhase+inner/3) + stFaint.Render("╯"),
	}, "\n")
}

func (a *App) navigation(width int) string {
	labels := make([]string, len(a.tabs))
	total := 1
	for i, tab := range a.tabs {
		meta := tabsMeta[tab]
		labels[i] = meta.glyph + " " + meta.short
		total += lipgloss.Width(labels[i]) + 3
	}
	if total > width {
		total = 1
		for i, tab := range a.tabs {
			meta := tabsMeta[tab]
			if tab == a.tab {
				labels[i] = meta.glyph + " " + meta.short
			} else {
				labels[i] = meta.glyph
			}
			total += lipgloss.Width(labels[i]) + 3
		}
	}

	var out strings.Builder
	out.WriteString(" ")
	x := 1
	for i, tab := range a.tabs {
		label := labels[i]
		active := tab == a.tab
		hovered := a.hovered(hitNavigation, tab, "")
		var rendered string
		switch {
		case active:
			rendered = lipgloss.NewStyle().
				Background(colorRaised).
				Padding(0, 1).
				Render(brandText(label, a.motionPhase+i))
		case hovered:
			rendered = lipgloss.NewStyle().
				Background(colorShoji).
				Foreground(colorMoon).
				Padding(0, 1).
				Render(label)
		default:
			rendered = lipgloss.NewStyle().
				Foreground(colorMist).
				Padding(0, 1).
				Render(label)
		}
		w := lipgloss.Width(rendered)
		if x+w > width {
			break
		}
		out.WriteString(rendered)
		out.WriteString(" ")
		a.hits = append(a.hits, hitRegion{x: 1 + x, y: 3, w: w, h: 1, kind: hitNavigation, tab: tab})
		x += w + 1
	}
	return padRight(out.String(), width)
}

func (a *App) pageHeading() string {
	meta := tabsMeta[a.tab]
	canvasW := max(a.width-1, 8)
	title := " " + brandText(meta.glyph+"  "+meta.name, a.motionPhase)
	position := stFaint.Render(fmt.Sprintf("%02d / %02d", indexOf(a.tabs, a.tab)+1, len(a.tabs))) + " "
	line1 := joinEdges(title, position, canvasW)
	line2 := " " + stSubtle.Render(truncate(meta.desc, max(canvasW-2, 1)))
	line3 := " " + gradientRule(max(canvasW-2, 1), a.motionPhase*2)
	if a.toast.Text != "" {
		line3 = " " + pillSuffix(a.toast.Kind) + " " +
			truncate(a.toast.Text, max(canvasW-5, 1))
	}
	line3 = padRight(line3, canvasW)
	return strings.Join([]string{
		padRight(line1, canvasW),
		padRight(line2, canvasW),
		padRight(line3, canvasW),
	}, "\n")
}

func joinEdges(left, right string, width int) string {
	gap := max(width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return truncate(left+strings.Repeat(" ", gap)+right, width)
}

func frameRow(content string, width int) string {
	return stFaint.Render("│") + padRight(truncate(content, width), width) + stFaint.Render("│")
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
