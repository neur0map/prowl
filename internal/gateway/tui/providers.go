package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Providers is the one place a provider is connected, watched and
// disconnected, whatever it authenticates with: a pasted API key, a keyless
// endpoint or a browser sign-in to a subscription. The table leads with what
// is connected - those are the providers whose models the Models page offers -
// and the rest of the directory follows, so "what can I use now" and "what
// could I add" are two sections of one list rather than three screens.
//
// Enter opens a provider's manage panel (credentials, health, allowance,
// links); c connects; space pauses or resumes; d disconnects. Every mutation
// goes through the same verbs no matter how the provider authenticates.

type providersLoadedMsg struct {
	directory []DirectoryProvider
	counts    DirectoryCounts
	keys      []KeyView
	activity  map[int64]keyActivityRow
	platforms []SignInPlatform
	logins    []LoginRow
	err       error
}

type providerUsageLoadedMsg struct {
	accounts []LoginUsage
	err      error
}

// providerDetailMsg carries the quota reading the manage panel shows. The
// probe is fetched off the render path, so the panel opens at once and the
// number fills in when it arrives.
type providerDetailMsg struct {
	platform string
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

type keyActivityRow struct {
	KeyID         int64  `json:"keyId"`
	Served        int64  `json:"served"`
	Failed        int64  `json:"failed"`
	Tokens        int64  `json:"tokens"`
	LastError     string `json:"lastError"`
	CoolingUntil  *int64 `json:"coolingUntil"`
	CoolingModels int    `json:"coolingModels"`
}

type revealMsg struct {
	platform string
	value    string
	until    time.Time
}

// revealExpiredMsg is a reveal's hide deadline, tagged with the generation it
// was armed for so a superseded reveal's timer is ignored (see revealGen).
type revealExpiredMsg struct{ gen int }

// providerRow joins everything the console knows about one provider: its
// directory entry, its stored keys, its sign-in state and routing enrolment.
type providerRow struct {
	dir   DirectoryProvider
	keys  []KeyView
	login *SignInPlatform
	enrol *LoginRow
}

func (p providerRow) platform() string {
	if p.dir.Platform != "" {
		return p.dir.Platform
	}
	return p.dir.ID
}

// signedIn reports a stored subscription login for this provider.
func (p providerRow) signedIn() bool { return p.login != nil && p.login.SignedIn }

// connected is "something is on file": a key row or a stored login.
func (p providerRow) connected() bool { return len(p.keys) > 0 || p.signedIn() }

// accountKind reports whether the provider connects by browser sign-in.
func (p providerRow) accountKind() bool { return p.login != nil }

// enrolled reports whether the login contributes routing capacity.
func (p providerRow) enrolled() bool { return p.enrol != nil && p.enrol.Enrolled }

// loginKeyID is the pool row a login owns, so it can be told apart from a
// pasted key on the same platform.
func (p providerRow) loginKeyID() int64 {
	if p.enrol != nil {
		return p.enrol.KeyID
	}
	return 0
}

// pastedKeys are the API keys the operator added by hand, excluding the pool
// row a sign-in manages on its own.
func (p providerRow) pastedKeys() []KeyView {
	out := make([]KeyView, 0, len(p.keys))
	for _, k := range p.keys {
		if k.ID != p.loginKeyID() {
			out = append(out, k)
		}
	}
	return out
}

const (
	filterConnection = "connection"
	filterFriction   = "setup"
)

type providersModel struct {
	app     *App
	width   int
	height  int
	list    list
	data    providersLoadedMsg
	loaded  bool
	busy    string
	filters filterState

	// usage holds the last allowance read per lowercase provider id;
	// usageLoaded marks the first read done so the status shows a pending
	// marker until then.
	usage       map[string]LoginUsage
	usageLoaded bool
	usageErr    error

	signin signInFlow

	reveal revealMsg
	// revealGen tags each shown secret so an earlier reveal's hide timer cannot
	// clear a newer one: only the tick carrying the current generation hides.
	revealGen int

	// probes caches the quota reading per platform for the manage panel.
	probes map[string]providerDetailMsg
}

func newProvidersModel(app *App) providersModel {
	m := providersModel{
		app:     app,
		list:    newList("Search providers"),
		filters: filterState{},
		usage:   map[string]LoginUsage{},
		probes:  map[string]providerDetailMsg{},
	}
	m.list.setHeaders("Provider", "Access", "Models", "Connection", "Status")
	m.list.setWeights(6, 4, 2, 6, 6)
	return m
}

func (m *providersModel) Init() tea.Cmd { return nil }

func (m *providersModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.list.width = w
}

func (m *providersModel) load() tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var out providersLoadedMsg
		var dir DirectoryResponse
		if err := c.Get(ctx, "/api/providers/directory", &dir); err != nil {
			out.err = err
			return out
		}
		out.directory, out.counts = dir.Providers, dir.Counts
		if err := c.Get(ctx, "/api/keys", &out.keys); err != nil {
			out.err = err
			return out
		}
		var platforms struct {
			Platforms []SignInPlatform `json:"platforms"`
		}
		if err := c.Get(ctx, "/api/logins/platforms", &platforms); err != nil {
			out.err = err
			return out
		}
		out.platforms = platforms.Platforms
		var logins struct {
			Logins []LoginRow `json:"logins"`
		}
		if err := c.Get(ctx, "/api/logins", &logins); err != nil {
			out.err = err
			return out
		}
		out.logins = logins.Logins
		// Activity is decoration on the manage panel; a failed read must not
		// take the page down with it.
		out.activity = map[int64]keyActivityRow{}
		var env struct {
			Activity []keyActivityRow `json:"activity"`
		}
		if err := c.Get(ctx, "/api/keys/activity", &env); err == nil {
			for _, a := range env.Activity {
				out.activity[a.KeyID] = a
			}
		}
		return out
	}
}

func (m *providersModel) loadUsage() tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var env struct {
			Accounts []LoginUsage `json:"accounts"`
		}
		if err := c.Get(ctx, "/api/logins/usage", &env); err != nil {
			return providerUsageLoadedMsg{err: err}
		}
		return providerUsageLoadedMsg{accounts: env.Accounts}
	}
}

func (m *providersModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handled, cmd, done, err := m.signin.update(m.app.Client, msg); handled {
		switch {
		case err != nil:
			m.busy = ""
			return m, tea.Batch(cmd, m.app.showToast("bad", err.Error()))
		case done != "":
			m.busy = ""
			return m, tea.Batch(cmd, m.load(), func() tea.Msg {
				return doneMsg{Tab: TabProviders, Text: done + " connected"}
			})
		}
		if _, started := msg.(signInStartedMsg); started {
			m.busy = ""
		}
		return m, cmd
	}

	switch msg := msg.(type) {
	case providersLoadedMsg:
		m.data = msg
		m.loaded = true
		m.busy = ""
		if msg.err == nil {
			if m.signin.connected != "" {
				if r := m.rowFor(m.signin.connected); r != nil && r.enrolled() {
					m.signin.connected = ""
				}
			}
		}
		m.buildRows()
		m.refreshManageOverlay()
		return m, m.loadUsage()

	case providerUsageLoadedMsg:
		m.usageLoaded = true
		m.usageErr = msg.err
		if msg.err == nil {
			m.usage = map[string]LoginUsage{}
			for _, a := range msg.accounts {
				m.usage[strings.ToLower(a.Provider)] = a
			}
		}
		m.buildRows()
		m.refreshManageOverlay()
		return m, nil

	case providerDetailMsg:
		m.probes[msg.platform] = msg
		m.refreshManageOverlay()
		return m, nil

	case filterAppliedMsg:
		m.filters = msg.state
		m.list.cursor, m.list.offset = 0, 0
		m.buildRows()
		return m, nil

	case revealMsg:
		// A reveal that arrives already expired (a slow fetch) hides at once and
		// schedules nothing; a tick on a past deadline would fire immediately.
		if msg.until.IsZero() || !time.Now().Before(msg.until) {
			m.reveal = revealMsg{}
			return m, nil
		}
		m.reveal = msg
		m.revealGen++
		gen := m.revealGen
		m.refreshManageOverlay()
		// The tick carries the generation instead of reading m.reveal from the
		// goroutine, so an earlier reveal's timer cannot hide this newer secret.
		return m, tea.Tick(time.Until(msg.until), func(time.Time) tea.Msg {
			return revealExpiredMsg{gen: gen}
		})
	case revealExpiredMsg:
		// Hide only if this deadline belongs to the secret currently shown; a
		// stale generation (a superseded reveal) is ignored.
		if msg.gen == m.revealGen {
			m.reveal = revealMsg{}
			m.refreshManageOverlay()
		}
		return m, nil

	case tea.KeyPressMsg:
		if m.signin.active != nil {
			// A sign-in is in progress: only its own controls are live. Block
			// the list navigation and provider actions underneath so a stray
			// key cannot connect, forget, or start a second flow mid sign-in.
			if msg.String() == "esc" {
				m.busy = "cancelling sign-in…"
			}
			return m, m.signin.key(m.app.Client, msg.String())
		}
		if m.list.typeFilter(msg) {
			m.buildRows()
			return m, nil
		}
		switch msg.String() {
		case "up", "k":
			m.list.move(-1)
			return m, nil
		case "down", "j":
			m.list.move(1)
			return m, nil
		case "pgup":
			m.list.page(-1)
			return m, nil
		case "pgdn":
			m.list.page(1)
			return m, nil
		case "home", "g":
			m.list.top()
			return m, nil
		case "end", "G":
			m.list.bot()
			return m, nil
		case "/":
			m.list.startSearch()
			return m, nil
		case "left", "h", "right", "l":
			if r := m.list.selected(); r != nil && r.header {
				m.list.toggleGroup()
			}
			return m, nil
		case "x":
			if m.signin.connected != "" {
				m.signin.connected = ""
			}
			return m, nil
		}
		if m.busy != "" {
			return m, nil
		}
		switch msg.String() {
		case "f":
			return m, m.openFilters()
		case "a":
			return m, m.openCustomForm()
		}
		r := m.list.selected()
		if r == nil {
			return m, nil
		}
		if r.header {
			if msg.String() == "enter" {
				m.list.toggleGroup()
			}
			return m, nil
		}
		p, ok := r.key.(providerRow)
		if !ok {
			return m, nil
		}
		switch msg.String() {
		case "enter":
			return m, m.openManage(p)
		case "c":
			return m, m.connect(p)
		case "space":
			return m, m.pauseResume(p)
		case "d":
			return m, m.disconnect(p)
		case "o":
			return m, m.openSignup(p.dir)
		}
	}
	return m, nil
}

// ── rows ────────────────────────────────────────────────────────────────────

// rows joins the directory with keys, logins and enrolments. A stored login
// the directory does not list (an adapter that lost its directory entry)
// still gets a row: hiding usable capacity is worse than a row with less
// detail.
func (m *providersModel) rows() []providerRow {
	keysByPlatform := map[string][]KeyView{}
	for _, k := range m.data.keys {
		keysByPlatform[k.Platform] = append(keysByPlatform[k.Platform], k)
	}
	loginByPlatform := map[string]*SignInPlatform{}
	for i := range m.data.platforms {
		p := &m.data.platforms[i]
		target := p.RoutesTo
		if target == "" {
			target = p.ID
		}
		loginByPlatform[strings.ToLower(target)] = p
	}
	enrolByPlatform := map[string]*LoginRow{}
	for i := range m.data.logins {
		enrolByPlatform[strings.ToLower(m.data.logins[i].ID)] = &m.data.logins[i]
	}

	out := make([]providerRow, 0, len(m.data.directory)+len(m.data.platforms))
	seen := map[string]bool{}
	for _, d := range m.data.directory {
		platform := d.Platform
		if platform == "" {
			platform = d.ID
		}
		seen[platform] = true
		r := providerRow{dir: d, keys: keysByPlatform[platform]}
		if login := loginByPlatform[strings.ToLower(platform)]; login != nil {
			r.login = login
			r.enrol = enrolByPlatform[strings.ToLower(login.ID)]
		}
		out = append(out, r)
	}
	for _, p := range m.data.platforms {
		target := p.RoutesTo
		if target == "" || seen[target] || !p.SignedIn {
			continue
		}
		seen[target] = true
		login := p
		out = append(out, providerRow{
			dir:   DirectoryProvider{ID: p.ID, Name: p.Name, Class: "oauth", Platform: target, Adapter: true, Routable: true},
			keys:  keysByPlatform[target],
			login: &login,
			enrol: enrolByPlatform[strings.ToLower(p.ID)],
		})
	}
	return out
}

func (m *providersModel) rowFor(platform string) *providerRow {
	platform = strings.ToLower(platform)
	for _, r := range m.rows() {
		if strings.ToLower(r.platform()) == platform || (r.login != nil && strings.ToLower(r.login.ID) == platform) {
			return &r
		}
	}
	return nil
}

func (m *providersModel) passesFilters(p providerRow) bool {
	switch m.filters.radio(filterConnection) {
	case "connected":
		if !p.connected() {
			return false
		}
	case "available":
		if p.connected() {
			return false
		}
	}
	if want := m.filters.radio(filterAccess); want != "" && want != "all" && accessOf(p.dir) != want {
		return false
	}
	if want := m.filters.radio(filterFriction); want != "" && want != "all" && frictionOf(p.dir) != want {
		return false
	}
	return true
}

func (m *providersModel) buildRows() {
	all := m.rows()
	// Free-tier-first inside each section, then by class cost, then name: a
	// user with no budget should see what they can use now at the top.
	order := map[string]int{"free": 0, "credits": 1, "oauth": 2, "local": 3, "paid": 4}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := order[classOf(all[i].dir)], order[classOf(all[j].dir)]
		if a != b {
			return a < b
		}
		if all[i].dir.FreeModels != all[j].dir.FreeModels {
			return all[i].dir.FreeModels > all[j].dir.FreeModels
		}
		return strings.ToLower(all[i].dir.Name) < strings.ToLower(all[j].dir.Name)
	})

	var connected, available []providerRow
	for _, p := range all {
		if !m.passesFilters(p) {
			continue
		}
		if p.connected() {
			connected = append(connected, p)
		} else {
			available = append(available, p)
		}
	}
	rows := make([]row, 0, len(all)+2)
	if len(connected) > 0 || m.filters.radio(filterConnection) != "available" {
		rows = append(rows, row{
			id: "group:connected", header: true, group: "connected", key: "connected",
			cells:   []string{"Connected"},
			summary: fmt.Sprintf("%d providers offering models", len(connected)),
		})
		for _, p := range connected {
			rows = append(rows, m.providerListRow(p, "connected"))
		}
	}
	if len(available) > 0 || m.filters.radio(filterConnection) != "connected" {
		rows = append(rows, row{
			id: "group:available", header: true, group: "available", key: "available",
			cells:   []string{"Available to connect"},
			summary: fmt.Sprintf("%d providers", len(available)),
		})
		for _, p := range available {
			rows = append(rows, m.providerListRow(p, "available"))
		}
	}
	if len(connected)+len(available) == 0 {
		rows = rows[:0]
		m.list.empty = "No providers match these filters."
		m.list.emptyHint = "Press f to widen them, or x inside the filter panel to reset every lens."
	}
	m.list.setRows(rows)
}

func (m *providersModel) providerListRow(p providerRow, group string) row {
	connection, status := m.connectionCells(p)
	// Connected providers count what they serve here; the rest show the
	// free tier the directory advertises, so the column always means "what
	// you would get".
	models := fmt.Sprintf("%d", p.dir.ModelCount)
	switch {
	case p.enrolled():
		models = fmt.Sprintf("%d", p.enrol.Models)
	case !p.connected():
		models = fmt.Sprintf("%d", p.dir.FreeModels)
	}
	if models == "0" {
		models = "-"
	}
	return row{
		id:    "provider:" + p.platform(),
		cells: []string{p.dir.Name, accessOf(p.dir), models, connection, status},
		styles: []func(string) string{
			func(s string) string {
				if p.connected() {
					return stHead.Render(s)
				}
				return stSubtle.Render(s)
			},
			accessWord,
			func(s string) string {
				if s == "-" {
					return stFaint.Render(s)
				}
				return stSubtle.Render(s)
			},
			func(s string) string { return stSubtle.Render(s) },
			statusStyle,
		},
		key:   p,
		group: group,
	}
}

// keylessSentinelMask is what the vault renders for the "no-key" sentinel a
// keyless provider stores (maskKey("no-key") in the gateway): four asterisks
// plus the secret's last two letters. It lets a connected keyless row tell a
// free-tier placeholder apart from a real pasted key without a separate flag.
const keylessSentinelMask = "****ey"

// connectionCells is the Connection and Status pair for a row: how the
// provider is (or could be) connected, and whether that connection can serve.
func (m *providersModel) connectionCells(p providerRow) (connection, status string) {
	if !p.connected() {
		switch {
		case p.accountKind():
			return "browser sign-in", "-"
		case p.dir.Keyless:
			return "keyless · key optional", "-"
		default:
			return "API key · " + frictionOf(p.dir), "-"
		}
	}
	if p.signedIn() {
		who := "signed in"
		if account := maskAccount(p.login.Account); account != "" {
			who += " · " + account
		}
		if extra := len(p.pastedKeys()); extra > 0 {
			who += fmt.Sprintf(" · +%d key", extra)
		}
		return who, m.accountStatus(p)
	}
	keys := p.keys
	switch {
	case p.dir.Keyless && len(keys) == 1 && keys[0].MaskedKey == keylessSentinelMask:
		connection = "free tier · no key"
	case len(keys) == 1:
		connection = "1 key · " + keys[0].MaskedKey
	default:
		connection = fmt.Sprintf("%d keys", len(keys))
	}
	return connection, keyStatus(keys, m.data.activity)
}

func (m *providersModel) accountNeedsSignIn(p providerRow) bool {
	if p.login == nil {
		return false
	}
	if p.login.Broken {
		return true
	}
	usage, ok := m.usage[strings.ToLower(p.login.ID)]
	return ok && usage.NeedsSignIn
}

func (m *providersModel) accountStatus(p providerRow) string {
	switch {
	case m.accountNeedsSignIn(p):
		return "sign in again"
	case p.login.RoutesTo == "":
		return "not routable yet"
	case p.enrolled():
		status := "routing"
		var u *LoginUsage
		if usage, ok := m.usage[strings.ToLower(p.login.ID)]; ok {
			u = &usage
		}
		if allowance := allowanceSummary(p.login.ID, u, m.usageLoaded, m.usageErr); allowance != "" {
			status += " · " + allowance
		}
		return status
	default:
		return "paused"
	}
}

// keyStatus folds a platform's key health into one word plus a cooldown.
func keyStatus(keys []KeyView, activity map[int64]keyActivityRow) string {
	enabled, healthy, errored, unknown := 0, 0, 0, 0
	cooling := ""
	for _, k := range keys {
		if !k.Enabled {
			continue
		}
		enabled++
		switch k.Status {
		case "healthy":
			healthy++
		case "error":
			errored++
		default:
			unknown++
		}
		if act, ok := activity[k.ID]; ok && act.CoolingUntil != nil && *act.CoolingUntil > time.Now().Unix() {
			left := time.Until(time.Unix(*act.CoolingUntil, 0)).Round(time.Second)
			cooling = " · cooling " + left.String()
		}
	}
	switch {
	case enabled == 0:
		return "paused"
	case errored > 0 && healthy == 0:
		return "key error" + cooling
	case healthy > 0:
		return "healthy" + cooling
	case unknown > 0:
		return "unchecked" + cooling
	}
	return "-"
}

func statusStyle(s string) string {
	switch {
	case s == "-" || s == "":
		return stFaint.Render(s)
	case strings.HasPrefix(s, "healthy"), strings.HasPrefix(s, "routing"):
		return stGood.Render(s)
	case strings.HasPrefix(s, "key error"), s == "sign in again", strings.Contains(s, "offline"):
		return stBad.Render(s)
	case s == "paused", strings.HasPrefix(s, "unchecked"), s == "not routable yet":
		return stWarn.Render(s)
	default:
		return stSubtle.Render(s)
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

// accessOf maps a provider onto the access vocabulary the filter and rows use
// ("subscription", not the internal "oauth"), so the word the user filters by
// is the word they see on the row.
func accessOf(p DirectoryProvider) string {
	if classOf(p) == "oauth" {
		return "subscription"
	}
	return classOf(p)
}

func accessWord(access string) string {
	switch access {
	case "free":
		return stGood.Render(access)
	case "credits":
		return stWarn.Render(access)
	case "subscription":
		return stKey.Render(access)
	case "local":
		return stSubtle.Render(access)
	case "paid":
		return stBad.Render(access)
	default:
		return stFaint.Render(access)
	}
}

// frictionOf is the signup effort in the words the filter offers.
func frictionOf(p DirectoryProvider) string {
	switch p.Friction {
	case "none":
		return "no signup"
	case "registration":
		return "email signup"
	case "phone":
		return "phone signup"
	case "card":
		return "card required"
	default:
		return "signup"
	}
}

func contextLabel(n int) string {
	switch {
	case n == 0:
		return "-"
	case n >= 1_000_000:
		return fmt.Sprintf("%dM", n/1_000_000)
	default:
		return fmt.Sprintf("%dk", n/1000)
	}
}

func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

// ── filters ─────────────────────────────────────────────────────────────────

func (m *providersModel) filterSections() []filterSection {
	all := m.rows()
	connected, available := 0, 0
	access := map[string]int{}
	friction := map[string]int{}
	for _, p := range all {
		if p.connected() {
			connected++
		} else {
			available++
		}
		access[accessOf(p.dir)]++
		friction[frictionOf(p.dir)]++
	}
	return []filterSection{
		{id: filterConnection, title: "Connection", radio: true, options: []filterOption{
			{id: "all", label: "Everything", count: len(all)},
			{id: "connected", label: "Connected", count: connected},
			{id: "available", label: "Available to connect", count: available},
		}},
		{id: filterAccess, title: "Access", radio: true, options: []filterOption{
			{id: "all", label: "Any", count: len(all)},
			{id: "free", label: "Free", count: access["free"]},
			{id: "credits", label: "Free credits", count: access["credits"]},
			{id: "subscription", label: "Subscription", count: access["subscription"]},
			{id: "paid", label: "Paid", count: access["paid"]},
			{id: "local", label: "Local", count: access["local"]},
		}},
		{id: filterFriction, title: "Signup effort", radio: true, options: []filterOption{
			{id: "all", label: "Any", count: len(all)},
			{id: "no signup", label: "No signup", count: friction["no signup"]},
			{id: "email signup", label: "Email", count: friction["email signup"]},
			{id: "phone signup", label: "Phone", count: friction["phone signup"]},
			{id: "card required", label: "Card", count: friction["card required"]},
		}},
	}
}

func (m *providersModel) openFilters() tea.Cmd {
	overlay := newFilterOverlay("Filter providers", m.filterSections(), m.filters)
	m.app.overlay = overlay
	return overlay.Init()
}

// ── actions ─────────────────────────────────────────────────────────────────

// connect starts whatever this provider's connection is: a browser sign-in for
// an account that needs one, or the credential form for everything else. The
// form keeps the key optional for a keyless provider, so the same c both
// enables the free tier and, later, adds or replaces a key.
func (m *providersModel) connect(p providerRow) tea.Cmd {
	if !p.dir.Adapter {
		return func() tea.Msg {
			return newToast("bad", p.dir.Name+" has no adapter yet, so it cannot be connected")
		}
	}
	if p.accountKind() && (!p.signedIn() || m.accountNeedsSignIn(p)) {
		return m.beginSignIn(*p.login)
	}
	return m.openAddKey(p)
}

func (m *providersModel) beginSignIn(p SignInPlatform) tea.Cmd {
	cmd := m.signin.begin(m.app.Client, p.ID)
	if cmd != nil {
		m.busy = "starting the sign-in flow…"
		m.app.overlay = nil
	}
	return cmd
}

// pauseResume withholds or restores a connected provider's capacity without
// forgetting anything: a login is unenrolled/enrolled, keys are disabled or
// enabled together.
func (m *providersModel) pauseResume(p providerRow) tea.Cmd {
	c := m.app.Client
	if !p.connected() {
		return func() tea.Msg { return newToast("tip", "Connect "+p.dir.Name+" first (press c).") }
	}
	if p.signedIn() {
		if p.login.RoutesTo == "" {
			return func() tea.Msg {
				return newToast("bad", p.dir.Name+" cannot be used for routing: no routing adapter is available yet")
			}
		}
		id := p.login.ID
		name := p.dir.Name
		if p.enrolled() {
			m.busy = "pausing " + name + "…"
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var out map[string]any
				if err := c.Delete(ctx, fmt.Sprintf("/api/logins/%s/enroll", id), &out); err != nil {
					return errMsg{Screen: "providers", Err: err}
				}
				return doneMsg{Tab: TabProviders, Text: name + " paused; its models leave routing"}
			}
		}
		m.busy = "resuming " + name + "…"
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			var out struct {
				Models int `json:"models"`
			}
			if err := c.Post(ctx, fmt.Sprintf("/api/logins/%s/enroll", id), map[string]any{}, &out); err != nil {
				return errMsg{Screen: "providers", Err: err}
			}
			return doneMsg{Tab: TabProviders, Text: fmt.Sprintf("%s routing %d models", name, out.Models)}
		}
	}
	anyEnabled := false
	for _, k := range p.keys {
		if k.Enabled {
			anyEnabled = true
		}
	}
	return m.setKeysEnabled(p.dir.Name, p.keys, !anyEnabled)
}

func (m *providersModel) setKeysEnabled(name string, keys []KeyView, enabled bool) tea.Cmd {
	c := m.app.Client
	if enabled {
		m.busy = "resuming " + name + "…"
	} else {
		m.busy = "pausing " + name + "…"
	}
	ids := make([]int64, 0, len(keys))
	for _, k := range keys {
		if k.Enabled != enabled {
			ids = append(ids, k.ID)
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, id := range ids {
			var out map[string]any
			if err := c.Patch(ctx, fmt.Sprintf("/api/keys/%d", id), map[string]any{"enabled": enabled}, &out); err != nil {
				return errMsg{Screen: "providers", Err: err}
			}
		}
		if enabled {
			return doneMsg{Tab: TabProviders, Text: name + " resumed"}
		}
		return doneMsg{Tab: TabProviders, Text: name + " paused; its models leave routing"}
	}
}

// disconnect forgets the provider's connection: the stored login (and its
// pool row) and/or every pasted key. It always confirms first.
func (m *providersModel) disconnect(p providerRow) tea.Cmd {
	if !p.connected() {
		return nil
	}
	c := m.app.Client
	name := p.dir.Name
	var question string
	switch {
	case p.signedIn() && len(p.pastedKeys()) > 0:
		question = fmt.Sprintf("Disconnect %s? The stored login and %d pasted key(s) are removed and its models leave routing. Your actual account is untouched.", name, len(p.pastedKeys()))
	case p.signedIn():
		question = fmt.Sprintf("Forget the %s login? Its models leave routing. Your actual account is untouched; only this gateway's stored token is deleted.", name)
	case p.dir.Keyless:
		question = fmt.Sprintf("Disable %s? Routing stops using it immediately.", name)
	default:
		question = fmt.Sprintf("Remove every %s key (%d)? Routing stops using them immediately.", name, len(p.keys))
	}
	loginID := ""
	if p.signedIn() {
		loginID = p.login.ID
	}
	keyIDs := make([]int64, 0, len(p.keys))
	for _, k := range p.pastedKeys() {
		keyIDs = append(keyIDs, k.ID)
	}
	m.app.overlay = &confirmOverlay{
		verb:     "Disconnect",
		question: question,
		yes: func() tea.Msg {
			// Return the work as a command so it runs off the Update loop,
			// never blocking on I/O inside Update.
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				var out map[string]any
				if loginID != "" {
					if err := c.Delete(ctx, fmt.Sprintf("/api/logins/%s/forget", loginID), &out); err != nil {
						return errMsg{Screen: "providers", Err: err}
					}
				}
				for _, id := range keyIDs {
					if err := c.Delete(ctx, fmt.Sprintf("/api/keys/%d", id), &out); err != nil {
						return errMsg{Screen: "providers", Err: err}
					}
				}
				return doneMsg{Tab: TabProviders, Text: name + " disconnected"}
			}
		},
	}
	return m.app.overlay.Init()
}

// openAddKey opens the credential form for a provider. The platform is fixed by
// selection, so the operator only supplies the secret. A keyless provider makes
// the key optional - an empty submit keeps the free tier - and, because the
// API's add path only re-enables an existing sentinel and ignores a freshly
// typed key, a keyless provider that already has rows on file is cleared first
// so a real key replaces the placeholder. Both happen in one command so the
// operator sees a single result.
func (m *providersModel) openAddKey(p providerRow) tea.Cmd {
	name := p.dir.Name
	platform := platformID(p.dir)
	keyless := p.dir.Keyless
	c := m.app.Client

	var f *formOverlay
	if keyless {
		f = newForm("Connect · "+name, "API key", "Label")
		f.secret(0)
		f.optional(0)
		f.optional(1)
		f.hint("A key is optional here - leave it empty to use the free tier.")
	} else {
		f = newForm("Add API key · "+name, "API key", "Label")
		f.secret(0)
		f.optional(1)
	}

	// A keyless provider only ever keeps one row, so replacing its key means
	// deleting what is on file before the new POST; the ids are captured now so
	// the submit runs entirely off the Update loop.
	var replace []int64
	if keyless {
		for _, k := range p.keys {
			replace = append(replace, k.ID)
		}
	}

	f.submit = func(values []string) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var out map[string]any
		for _, id := range replace {
			if err := c.Delete(ctx, fmt.Sprintf("/api/keys/%d", id), &out); err != nil {
				return errMsg{Screen: "providers", Err: err}
			}
		}
		body := map[string]string{"platform": platform, "key": values[0], "label": values[1]}
		var resp struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		}
		if err := c.Post(ctx, "/api/keys", body, &resp); err != nil {
			return errMsg{Screen: "providers", Err: err}
		}
		if keyless && values[0] == "" {
			return doneMsg{Tab: TabProviders, Text: name + " enabled on its free tier; add a key any time with c"}
		}
		return doneMsg{Tab: TabProviders, Text: fmt.Sprintf("%s key added (%s)", name, statusWord(resp.Status))}
	}
	m.app.overlay = f
	return f.Init()
}

func (m *providersModel) openCustomForm() tea.Cmd {
	f := newForm("Register a custom OpenAI-compatible endpoint",
		"Base URL", "API key", "Label")
	f.secret(1)
	f.optional(2)
	c := m.app.Client
	f.submit = func(values []string) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		body := map[string]string{"baseUrl": values[0], "apiKey": values[1], "label": values[2]}
		var out struct {
			ID     int64 `json:"id"`
			Models int   `json:"models"`
		}
		if err := c.Post(ctx, "/api/keys/custom", body, &out); err != nil {
			return errMsg{Screen: "providers", Err: err}
		}
		return doneMsg{Tab: TabProviders, Text: fmt.Sprintf("endpoint registered with %d models", out.Models)}
	}
	m.app.overlay = f
	return f.Init()
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

// ── view ────────────────────────────────────────────────────────────────────

func (m *providersModel) headline() string {
	if !m.loaded || m.data.err != nil {
		return ""
	}
	connected := 0
	for _, p := range m.rows() {
		if p.connected() {
			connected++
		}
	}
	return fmt.Sprintf("%d connected · %d in the directory", connected, len(m.data.directory))
}

func (m *providersModel) View() tea.View {
	if !m.loaded && m.signin.active == nil {
		return tea.NewView(" " + m.app.spinner.View() + " " + stSubtle.Render("Reading providers, credentials and accounts"))
	}
	if m.data.err != nil {
		return tea.NewView(" " + stBad.Render("● "+m.data.err.Error()))
	}
	// One information line, no controls: the keys live in the footer, so a
	// second row of key letters up here would only compete with it.
	connected, available := 0, 0
	for _, p := range m.rows() {
		if p.connected() {
			connected++
		} else {
			available++
		}
	}
	summary := fmt.Sprintf("%d connected · %d available to connect", connected, available)
	if n := m.filters.active(m.filterSections()); n > 0 {
		summary += fmt.Sprintf(" · %d filters on", n)
	}
	var b strings.Builder
	switch {
	case m.busy != "":
		b.WriteString(" " + stWarn.Render("● "+m.busy) + "\n")
	case m.signin.connected != "":
		b.WriteString(" " + m.connectedLine() + "\n")
	default:
		b.WriteString(" " + stSubtle.Render(summary) + stFaint.Render("  ·  connected providers offer their models to routing sets") + "\n")
	}
	b.WriteString("\n")
	if m.signin.active != nil {
		b.WriteString(m.signin.panel(m.width) + "\n")
	}
	localY := strings.Count(b.String(), "\n")
	m.list.height = max(m.height-localY, 5)
	m.list.setOrigin(m.app.bodyX, m.app.bodyY+localY)
	b.WriteString(m.list.render())
	return tea.NewView(b.String())
}

// connectedLine is the durable success line after a sign-in, kept until the
// account is routing (or dismissed) rather than a toast that fades.
func (m *providersModel) connectedLine() string {
	r := m.rowFor(m.signin.connected)
	name := m.signin.connected
	if r != nil {
		name = r.dir.Name
	}
	line := stGood.Render("● " + name + " connected")
	switch {
	case r != nil && r.enrolled():
		line += stSubtle.Render(" · its account models were added to routing")
	case r != nil && r.login != nil && r.login.RoutesTo != "":
		line += stSubtle.Render(" · finishing model discovery; reload in a moment if it is not routing yet")
	default:
		line += stSubtle.Render(" · no routing adapter can spend this account yet; it remains safely stored")
	}
	return line + stFaint.Render("  x dismiss")
}

func (m *providersModel) actions() []action {
	if m.signin.active != nil {
		return m.signin.actions()
	}
	acts := make([]action, 0, 8)
	if r := m.list.selected(); r != nil {
		if r.header {
			acts = append(acts, action{Key: "enter", Label: "Collapse / expand", Primary: true})
		} else if p, ok := r.key.(providerRow); ok {
			switch {
			case !p.connected():
				acts = append(acts,
					action{Key: "c", Label: "Connect", Primary: true},
					action{Key: "enter", Label: "Details"},
					action{Key: "o", Label: "Signup page"},
				)
			default:
				acts = append(acts, action{Key: "enter", Label: "Manage", Primary: true})
				switch {
				case p.accountKind() && m.accountNeedsSignIn(p):
					acts = append(acts, action{Key: "c", Label: "Sign in again"})
				case p.accountKind():
					acts = append(acts, action{Key: "c", Label: "Add API key"})
				default:
					acts = append(acts, action{Key: "c", Label: "Add key"})
				}
				pause := "Pause"
				if (p.signedIn() && !p.enrolled()) || (!p.signedIn() && keyStatus(p.keys, m.data.activity) == "paused") {
					pause = "Resume"
				}
				acts = append(acts,
					action{Key: "space", Label: pause},
					action{Key: "d", Label: "Disconnect", Dangerous: true},
				)
			}
		}
	}
	acts = append(acts,
		action{Key: "f", Label: "Filter"},
		action{Key: "/", Label: "Search"},
		action{Key: "a", Label: "Custom endpoint"},
	)
	if m.signin.connected != "" {
		acts = append(acts, action{Key: "x", Label: "Dismiss"})
	}
	return acts
}

func (m *providersModel) searching() bool { return m.list.isSearching() }
