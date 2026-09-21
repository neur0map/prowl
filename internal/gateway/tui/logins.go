package tui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Accounts shows browser-authenticated subscriptions (Claude, ChatGPT,
// Copilot, Hyper), their allowance, and whether their models are active in
// routing. A successful sign-in activates compatible accounts automatically;
// Space remains an explicit pause/resume control whose preference survives a
// restart.

type loginsLoadedMsg struct {
	platforms []SignInPlatform
	logins    []LoginRow
	err       error
}

// LoginUsage is one connected account's allowance as the gateway reports it.
// A rate-windowed account (Claude) fills Windows; a running-credit account
// (Hyper) fills Balance/Unit. An error degrades only that account instead of
// failing the screen.
type LoginUsage struct {
	Provider    string             `json:"provider"`
	Name        string             `json:"name"`
	Windows     []LoginUsageWindow `json:"windows"`
	Balance     *float64           `json:"balance"`
	Unit        string             `json:"unit"`
	Cadence     string             `json:"cadence"`
	Note        string             `json:"note"`
	NeedsSignIn bool               `json:"needsSignIn"`
	Error       string             `json:"error"`
}

// LoginUsageWindow is one rate-limit bucket: Utilization is a 0-100 percentage
// and ResetsAt the wall clock it refills, when the provider publishes one.
type LoginUsageWindow struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resetsAt"`
}

// connectedLogin is the just-signed-in provider whose success panel persists.
type connectedLogin struct {
	provider string // lowercase id, resolved to name/adapter at render time
}

type loginsUsageLoadedMsg struct {
	accounts []LoginUsage
	err      error
}

type loginsModel struct {
	app    *App
	width  int
	height int
	list   list
	data   loginsLoadedMsg
	loaded bool
	busy   string

	// usage holds the last allowance read per lowercase provider id;
	// usageLoaded marks the first read done so the Allowance column shows a
	// pending marker until then.
	usage       map[string]LoginUsage
	usageLoaded bool
	usageErr    error

	// connected persists a just-completed sign-in as a durable success panel
	// while model discovery finishes, rather than a toast that fades.
	connected *connectedLogin

	// active tracks a sign-in flow in progress: the id, its verdict poll.
	active      *SignInSession
	activeSince time.Time

	// signInStarting gates a second sign-in start while the POST /api/signin
	// that opens the flow is still in flight (before m.active is set). It is
	// cleared ONLY by that start's own result (signInStartedMsg or
	// signInStartFailedMsg), so an unrelated loginsLoadedMsg clearing m.busy
	// cannot reopen the gate and let a second concurrent flow begin.
	signInStarting bool
}

func (m *loginsModel) Init() tea.Cmd { return nil }

func (m *loginsModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.list.width = w
}

func (m *loginsModel) load() tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var out loginsLoadedMsg
		var platforms struct {
			Platforms []SignInPlatform `json:"platforms"`
		}
		if err := c.Get(ctx, "/api/logins/platforms", &platforms); err != nil {
			out.err = err
			return out
		}
		out.platforms = platforms.Platforms
		var env struct {
			Logins []LoginRow `json:"logins"`
		}
		if err := c.Get(ctx, "/api/logins", &env); err != nil {
			out.err = err
			return out
		}
		out.logins = env.Logins
		return out
	}
}

func (m *loginsModel) loadUsage() tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var env struct {
			Accounts []LoginUsage `json:"accounts"`
		}
		if err := c.Get(ctx, "/api/logins/usage", &env); err != nil {
			return loginsUsageLoadedMsg{err: err}
		}
		return loginsUsageLoadedMsg{accounts: env.Accounts}
	}
}

func (m *loginsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case loginsLoadedMsg:
		m.data = msg
		m.loaded = true
		m.busy = ""
		m.buildRows()
		return m, m.loadUsage()

	case loginsUsageLoadedMsg:
		m.usageLoaded = true
		m.usageErr = msg.err
		if msg.err == nil {
			m.usage = map[string]LoginUsage{}
			for _, a := range msg.accounts {
				m.usage[strings.ToLower(a.Provider)] = a
			}
		}
		m.buildRows()
		return m, nil

	case signInStartedMsg:
		m.active = &msg.session
		m.activeSince = time.Now()
		m.signInStarting = false
		m.busy = ""
		return m, tea.Batch(m.pollSignIn(msg.session.ID), openSignInURL(msg.session.URL))

	case signInStartFailedMsg:
		// The dedicated start-failure result: clear the start gate (which only
		// this message and signInStartedMsg may clear) and surface the error.
		// An unrelated loginsLoadedMsg/errMsg can clear m.busy but never this
		// gate, so a failed or slow start cannot be silently overtaken.
		m.signInStarting = false
		m.busy = ""
		return m, m.app.showToast("bad", msg.err.Error())

	case signInPolledMsg:
		// Ignore a poll result for a session the operator already cancelled or
		// replaced: a slow in-flight poll must not revive a finished flow nor
		// clobber the session of a newer sign-in.
		if m.active == nil || m.active.ID != msg.session.ID {
			return m, nil
		}
		m.active = &msg.session
		switch msg.session.State {
		case "complete":
			m.connected = &connectedLogin{provider: strings.ToLower(msg.session.Provider)}
			m.active = nil
			return m, tea.Batch(m.load(), func() tea.Msg {
				return doneMsg{Tab: TabLogins, Text: msg.session.Provider + " connected to routing"}
			})
		case "failed", "cancelled":
			m.active = nil
			text := msg.session.Error
			if text == "" {
				text = "sign-in " + msg.session.State
			}
			return m, func() tea.Msg { return errMsg{Screen: "logins", Err: fmt.Errorf("%s", text)} }
		}
		if time.Since(m.activeSince) > 15*time.Minute {
			m.active = nil
			return m, func() tea.Msg { return errMsg{Screen: "logins", Err: fmt.Errorf("sign-in timed out")} }
		}
		return m, tickSignIn(2*time.Second, m.active.ID)

	case signInPollErrMsg:
		// A transient poll failure (timeout, connection blip) must not abort the
		// flow. Ignore an error for a session already cancelled or replaced, then
		// keep the active session and retry until the deadline.
		if m.active == nil || m.active.ID != msg.id {
			return m, nil
		}
		if time.Since(m.activeSince) > 15*time.Minute {
			m.active = nil
			return m, func() tea.Msg { return errMsg{Screen: "logins", Err: fmt.Errorf("sign-in timed out")} }
		}
		return m, tickSignIn(2*time.Second, m.active.ID)

	case signInTickMsg:
		// A late tick from a superseded flow must not schedule a poll against the
		// session that replaced it.
		if m.active == nil || m.active.ID != msg.id {
			return m, nil
		}
		return m, m.pollSignIn(m.active.ID)
	case signInCancelledMsg:
		// Only the session whose cancel this confirms may be cleared; a duplicate
		// or late cancel must not wipe a newer sign-in.
		if m.active == nil || m.active.ID != msg.id {
			return m, nil
		}
		m.active = nil
		m.busy = ""
		return m, tea.Batch(m.load(), func() tea.Msg { return newToast("ok", "Sign-in cancelled") })

	case tea.KeyPressMsg:
		if m.active != nil {
			switch msg.String() {
			case "o":
				return m, openSignInURL(m.active.URL)
			case "c":
				value, label := m.active.UserCode, "Sign-in code copied"
				if value == "" {
					value, label = m.active.URL, "Sign-in URL copied"
				}
				return m, copyLoginValue(value, label)
			case "u":
				return m, copyLoginValue(m.active.URL, "Sign-in URL copied")
			case "esc":
				m.busy = "cancelling sign-in…"
				return m, m.cancelSignIn(m.active.ID)
			}
			// A sign-in is in progress: only its own controls are live. Block
			// the list navigation and account actions underneath so a stray
			// key cannot enrol, forget, or start a second flow mid sign-in.
			return m, nil
		}
		if m.list.typeFilter(msg) {
			m.buildRows()
			return m, nil
		}
		if m.connected != nil && msg.String() == "x" {
			m.connected = nil
			return m, nil
		}
		switch msg.String() {
		case "up", "k":
			m.list.move(-1)
		case "down", "j":
			m.list.move(1)
		case "/":
			m.list.startSearch()
			m.buildRows()
		}
		r := m.list.selected()
		if r == nil {
			return m, nil
		}
		row := r.key.(loginRowData)
		c := m.app.Client
		switch msg.String() {
		case "s":
			if !row.platform.SignedIn || m.accountNeedsSignIn(row.platform) {
				return m, m.beginSignIn(row.platform)
			}
		case "enter":
			if !row.platform.SignedIn || m.accountNeedsSignIn(row.platform) {
				return m, m.beginSignIn(row.platform)
			}
			m.app.overlay = m.accountDetail(row)
			return m, m.app.overlay.Init()
		case "space":
			if row.enrolled() {
				m.busy = "removing from routing…"
				return m, func() tea.Msg {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					var out map[string]any
					path := fmt.Sprintf("/api/logins/%s/enroll", row.platform.ID)
					if err := c.Delete(ctx, path, &out); err != nil {
						return errMsg{Screen: "logins", Err: err}
					}
					return doneMsg{Tab: TabLogins, Text: row.platform.Name + " removed from routing"}
				}
			}
			if row.platform.RoutesTo == "" || !row.platform.SignedIn {
				return m, func() tea.Msg {
					return errMsg{Screen: "logins", Err: fmt.Errorf(
						"%s cannot be used for routing: %s", row.platform.Name, enrolReason(row.platform))}
				}
			}
			m.busy = "adding models to routing…"
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				var out struct {
					Enrolled bool `json:"enrolled"`
					Models   int  `json:"models"`
				}
				path := fmt.Sprintf("/api/logins/%s/enroll", row.platform.ID)
				if err := c.Post(ctx, path, map[string]any{}, &out); err != nil {
					return errMsg{Screen: "logins", Err: err}
				}
				return doneMsg{Tab: TabLogins,
					Text: fmt.Sprintf("%s active with %d models", row.platform.Name, out.Models)}
			}
		case "d":
			if !row.platform.SignedIn {
				return m, nil
			}
			m.app.overlay = &confirmOverlay{
				question: fmt.Sprintf("Forget the %s login? If it is used for routing, its models are removed too. Your actual account is untouched; only this gateway's stored token is deleted.", row.platform.Name),
				yes: func() tea.Msg {
					c := m.app.Client
					// Return the DELETE as a command so runYes runs it off the
					// Update loop, never blocking on I/O inside Update.
					return func() tea.Msg {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						var out map[string]any
						path := fmt.Sprintf("/api/logins/%s/forget", row.platform.ID)
						if err := c.Delete(ctx, path, &out); err != nil {
							return errMsg{Screen: "logins", Err: err}
						}
						return doneMsg{Tab: TabLogins, Text: row.platform.Name + " login forgotten"}
					}
				},
			}
			return m, m.app.overlay.Init()
		}
	}
	return m, nil
}

func enrolReason(p SignInPlatform) string {
	if p.SignedIn {
		return "no compatible routing adapter is available yet"
	}
	return "connect the account first"
}

type loginRowData struct {
	platform SignInPlatform
	enrol    *LoginRow
}

func (l loginRowData) enrolled() bool { return l.enrol != nil && l.enrol.Enrolled }

func (m *loginsModel) accountNeedsSignIn(p SignInPlatform) bool {
	if p.Broken {
		return true
	}
	usage, ok := m.usage[strings.ToLower(p.ID)]
	return ok && usage.NeedsSignIn
}

func maskAccount(account string) string {
	account = strings.TrimSpace(account)
	if account == "" {
		return ""
	}
	if at := strings.LastIndex(account, "@"); at > 0 {
		local := []rune(account[:at])
		return string(local[0]) + "•••" + account[at:]
	}
	runes := []rune(account)
	if len(runes) > 14 {
		return string(runes[:4]) + "…" + string(runes[len(runes)-4:])
	}
	return account
}

func (m *loginsModel) buildRows() {
	enrolled := map[string]*LoginRow{}
	for i := range m.data.logins {
		l := m.data.logins[i]
		enrolled[strings.ToLower(l.ID)] = &m.data.logins[i]
	}
	if m.connected != nil {
		if l, ok := enrolled[strings.ToLower(m.connected.provider)]; ok && l.Enrolled {
			m.connected = nil
		}
	}
	rows := make([]row, 0, len(m.data.platforms)+len(m.data.logins))
	seen := map[string]bool{}
	for _, p := range m.data.platforms {
		seen[p.ID] = true
		needsSignIn := m.accountNeedsSignIn(p)
		state := "not connected"
		switch {
		case p.SignedIn && needsSignIn:
			state = "reconnect"
		case p.SignedIn:
			state = "connected"
		}
		pool := "-"
		if needsSignIn {
			pool = "paused · sign in again"
		} else if l, ok := enrolled[p.ID]; ok && l.Enrolled {
			pool = fmt.Sprintf("active · %d models", l.Models)
		} else if p.RoutesTo == "" {
			pool = "not supported"
		} else if p.SignedIn {
			pool = "ready to add"
		}
		allowance := m.allowanceCell(p.ID, p.SignedIn)
		rows = append(rows, row{
			cells: []string{p.Name, state, pool, allowance, maskAccount(p.Account)},
			styles: []func(string) string{
				nil,
				func(s string) string { return pill(s) },
				func(s string) string {
					if strings.HasPrefix(s, "active") {
						return stGood.Render(s)
					}
					if strings.HasPrefix(s, "paused") {
						return stWarn.Render(s)
					}
					return stFaint.Render(s)
				},
				allowanceStyle,
				func(s string) string { return stFaint.Render(truncate(s, 24)) },
			},
			key: loginRowData{platform: p, enrol: enrolled[p.ID]},
		})
	}
	// Any routed login the platform list does not cover (a hand-copied link row
	// from an older install) still gets a line - hiding usable capacity is worse
	// than a row with less detail.
	for id := range enrolled {
		if seen[id] {
			continue
		}
		l := enrolled[id]
		rows = append(rows, row{
			cells: []string{l.Name, "linked", fmt.Sprintf("active · %d models", l.Models), "not published", ""},
			key:   loginRowData{platform: SignInPlatform{ID: l.ID, Name: l.Name, SignedIn: true, RoutesTo: l.ID}, enrol: l},
		})
	}
	m.list.setRows(rows)
}

// beginSignIn dispatches a sign-in start only when none is already starting or
// active. s/enter set signInStarting and dispatch the async start; until its
// result lands (signInStartedMsg sets m.active, which then gates all input;
// signInStartFailedMsg clears the gate), m.active is still nil, so a second
// rapid press would otherwise open a second concurrent flow - two browser
// windows, two server sessions, the first orphaned. The gate is signInStarting,
// not busy: an unrelated loginsLoadedMsg clears busy but must not reopen it.
func (m *loginsModel) beginSignIn(p SignInPlatform) tea.Cmd {
	if m.signInStarting || m.active != nil {
		return nil
	}
	m.signInStarting = true
	m.busy = "starting the sign-in flow…"
	return m.startSignIn(p)
}

func (m *loginsModel) startSignIn(p SignInPlatform) tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var session SignInSession
		body := map[string]string{"provider": p.ID}
		if err := c.Post(ctx, "/api/signin", body, &session); err != nil {
			// Dedicated start-failure result: only its own handler clears the
			// start gate, so a generic logins errMsg cannot leave it stuck.
			return signInStartFailedMsg{err: err}
		}
		return signInStartedMsg{session: session}
	}
}

// allowanceCell is the compact Allowance column: a pending marker until the
// first read, the balance or window percentages once known, or a dash for an
// account with no allowance surface.
func (m *loginsModel) allowanceCell(id string, signedIn bool) string {
	if !signedIn {
		return "-"
	}
	lid := strings.ToLower(id)
	publishesAllowance := lid == "anthropic" || lid == "hyper"
	if !m.usageLoaded {
		if publishesAllowance {
			return "…"
		}
		return "not published"
	}
	u, ok := m.usage[lid]
	if !ok {
		// The allowance read failed entirely: for accounts that DO publish an
		// allowance, surface the failure instead of implying none exists.
		if m.usageErr != nil && publishesAllowance {
			return "report offline"
		}
		return "not published"
	}
	if u.NeedsSignIn {
		return "sign in again"
	}
	if u.Error != "" {
		return "report offline"
	}
	if u.Balance != nil {
		unit := u.Unit
		if unit == "" {
			unit = "credits"
		}
		return trimNum(*u.Balance) + " " + unit
	}
	// The compact column carries the account-wide windows; per-model scoped
	// rows are shown only in the account detail so the column stays legible.
	parts := make([]string, 0, 2)
	for _, w := range u.Windows {
		if w.Key == "five_hour" || w.Key == "seven_day" {
			parts = append(parts, fmt.Sprintf("%s %.0f%% used", windowShort(w.Key), w.Utilization))
		}
	}
	if len(parts) == 0 {
		if len(u.Windows) == 0 {
			return "-"
		}
		w := u.Windows[0]
		parts = append(parts, fmt.Sprintf("%s %.0f%% used", windowShort(w.Key), w.Utilization))
	}
	return strings.Join(parts, " · ")
}

func windowShort(key string) string {
	switch key {
	case "five_hour":
		return "5h"
	case "seven_day":
		return "7d"
	case "weekly_scoped":
		return "7d·s"
	}
	return key
}

func allowanceStyle(s string) string {
	switch s {
	case "-", "…", "", "not published":
		return stFaint.Render(s)
	case "sign in again":
		return stWarn.Render(s)
	case "report offline":
		return stBad.Render(s)
	default:
		return stSubtle.Render(s)
	}
}

// accountDetail is the read-only drill-down for a connected account: routing
// readiness and full allowance, with routing/forget actions returned to the
// owning screen via actionChosenMsg.
func (m *loginsModel) accountDetail(rowData loginRowData) tea.Model {
	p := rowData.platform
	u, ok := m.usage[strings.ToLower(p.ID)]
	needsSignIn := p.Broken || (ok && u.NeedsSignIn)
	var body strings.Builder
	if strings.TrimSpace(p.Account) != "" {
		body.WriteString(stFaint.Render("Account   ") + maskAccount(p.Account) + "\n")
	}
	switch {
	case needsSignIn:
		body.WriteString(stWarn.Render("Routing paused · sign in again") + "\n")
	case rowData.enrolled():
		body.WriteString(stGood.Render("Active in routing") +
			stFaint.Render(fmt.Sprintf("  ·  %d models", rowData.enrol.Models)) + "\n")
	case p.RoutesTo != "":
		body.WriteString(stSubtle.Render("Connected, but not currently used for routing") + "\n")
	default:
		body.WriteString(stFaint.Render("No routing adapter can spend this account yet") + "\n")
	}
	body.WriteString("\n" + stHead.Render("Allowance") + "\n")
	switch {
	case !m.usageLoaded:
		body.WriteString(stSubtle.Render("Reading allowance…"))
	case !ok:
		lid := strings.ToLower(p.ID)
		if m.usageErr != nil && (lid == "anthropic" || lid == "hyper") {
			body.WriteString(stWarn.Render("Allowance report offline; routing may still work.") + "\n" +
				stFaint.Render(m.usageErr.Error()))
		} else {
			body.WriteString(stFaint.Render("This account does not publish an allowance report."))
		}
	case u.NeedsSignIn:
		body.WriteString(stWarn.Render("Sign in again to refresh this account.") + "\n" +
			stFaint.Render(u.Error))
	case u.Error != "":
		body.WriteString(stWarn.Render("Allowance report offline; routing may still work.") + "\n" +
			stFaint.Render(u.Error))
	default:
		body.WriteString(usageDetailBody(u))
	}
	actions := make([]action, 0, 3)
	if needsSignIn {
		actions = append(actions, action{Key: "s", Label: "Sign in again", Primary: true})
	}
	switch {
	case rowData.enrolled():
		actions = append(actions,
			action{Key: "space", Label: "Remove from routing"},
			action{Key: "d", Label: "Forget", Dangerous: true})
	case p.RoutesTo != "":
		actions = append(actions,
			action{Key: "space", Label: "Add to routing", Primary: len(actions) == 0},
			action{Key: "d", Label: "Forget", Dangerous: true})
	}
	return newDetailOverlay(p.Name, "Connected account", body.String(), actions)
}

func usageDetailBody(u LoginUsage) string {
	var b strings.Builder
	if u.Balance != nil {
		unit := u.Unit
		if unit == "" {
			unit = "credits"
		}
		b.WriteString(stKey.Render(trimNum(*u.Balance)) + " " + unit + "\n")
		if u.Cadence != "" {
			b.WriteString(stFaint.Render("Cadence   "+u.Cadence) + "\n")
		}
		if u.Note != "" {
			b.WriteString(stFaint.Render(u.Note) + "\n")
		}
	}
	for _, w := range u.Windows {
		line := padRight(w.Label, 16) + usagePercent(w.Utilization)
		if strings.TrimSpace(w.ResetsAt) != "" {
			line += "   " + stFaint.Render("resets "+humanReset(w.ResetsAt))
		}
		b.WriteString(line + "\n")
	}
	if u.Balance == nil && len(u.Windows) == 0 {
		b.WriteString(stFaint.Render("No allowance windows reported."))
	}
	return strings.TrimRight(b.String(), "\n")
}

func usagePercent(pct float64) string {
	label := fmt.Sprintf("%.0f%% used", pct)
	switch {
	case pct >= 90:
		return stBad.Render(label)
	case pct >= 70:
		return stWarn.Render(label)
	default:
		return stGood.Render(label)
	}
}

func trimNum(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func humanReset(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04Z07:00", iso)
	}
	if err != nil {
		return iso
	}
	d := time.Until(t)
	if d <= 0 {
		return "now"
	}
	d = d.Round(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("in %dd", int(d.Hours())/24)
	}
}

func (m *loginsModel) connectedPanel() string {
	prov := m.connected.provider
	name := prov
	routesTo := ""
	enrolled := false
	for _, p := range m.data.platforms {
		if strings.EqualFold(p.ID, prov) {
			name = p.Name
			routesTo = p.RoutesTo
		}
	}
	for _, l := range m.data.logins {
		if strings.EqualFold(l.ID, prov) && l.Enrolled {
			enrolled = true
		}
	}
	var body strings.Builder
	body.WriteString(stGood.Render("● Connected") + "  " + stTitle.Render(name) + "\n")
	switch {
	case enrolled:
		body.WriteString(stSubtle.Render("Ready for routing now. Its account models were added automatically."))
	case routesTo != "":
		body.WriteString(stSubtle.Render("Finishing model discovery. Reload if the account is not active in a moment."))
	default:
		body.WriteString(stFaint.Render("No routing adapter can spend this account yet; it remains safely stored."))
	}
	body.WriteString("\n" + stFaint.Render("x dismiss"))
	return roundedPanel("Account connected", body.String(), m.width)
}

// sign-in polling ------------------------------------------------------------

type signInStartedMsg struct{ session SignInSession }
type signInStartFailedMsg struct{ err error }
type signInPolledMsg struct{ session SignInSession }
type signInTickMsg struct{ id string }
type signInCancelledMsg struct{ id string }
type signInPollErrMsg struct {
	id  string
	err error
}

func (m *loginsModel) pollSignIn(id string) tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		var session SignInSession
		if err := c.Get(ctx, "/api/signin/"+id, &session); err != nil {
			return signInPollErrMsg{id: id, err: err}
		}
		return signInPolledMsg{session: session}
	}
}
func (m *loginsModel) cancelSignIn(id string) tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		var out map[string]any
		if err := c.Delete(ctx, "/api/signin/"+id, &out); err != nil {
			return errMsg{Screen: "logins", Err: err}
		}
		return signInCancelledMsg{id: id}
	}
}

var openURL = func(rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return fmt.Errorf("sign-in provider returned no browser URL")
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", rawURL)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		command = exec.Command("xdg-open", rawURL)
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func openSignInURL(rawURL string) tea.Cmd {
	return func() tea.Msg {
		if err := openURL(rawURL); err != nil {
			return newToast("bad", "Could not open the browser: "+err.Error()+". Copy the URL with u.")
		}
		return newToast("ok", "Browser opened. Complete sign-in there; this screen will update automatically.")
	}
}

func copyLoginValue(value, label string) tea.Cmd {
	if value == "" {
		return func() tea.Msg { return newToast("bad", "The provider did not return a value to copy.") }
	}
	return tea.Batch(
		tea.SetClipboard(value),
		tea.SetPrimaryClipboard(value),
		func() tea.Msg { return newToast("ok", label) },
	)
}

func tickSignIn(d time.Duration, id string) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return signInTickMsg{id: id} })
}

func (m *loginsModel) View() tea.View {
	if !m.loaded && m.active == nil {
		return tea.NewView(roundedPanel(
			"Subscriptions",
			m.app.spinner.View()+" "+stSubtle.Render("Reading connected accounts"),
			m.width,
		))
	}
	if m.data.err != nil {
		return tea.NewView(roundedPanel(
			"Could not read accounts",
			stBad.Render("● "+m.data.err.Error()),
			m.width,
		))
	}
	signedIn, enrolled := 0, 0
	routed := map[string]bool{}
	for _, login := range m.data.logins {
		routed[strings.ToLower(login.ID)] = login.Enrolled
	}
	for _, platform := range m.data.platforms {
		if platform.SignedIn {
			signedIn++
		}
		if routed[strings.ToLower(platform.ID)] && !m.accountNeedsSignIn(platform) {
			enrolled++
		}
	}

	var b strings.Builder
	b.WriteString(metricStrip([]metric{
		{"Services", fmt.Sprintf("%d", len(m.data.platforms)), "supported accounts"},
		{"Connected", fmt.Sprintf("%d", signedIn), "stored securely"},
		{"Routing", fmt.Sprintf("%d", enrolled), "accounts active"},
	}, m.width))
	b.WriteString("\n")
	if m.busy != "" {
		b.WriteString(roundedPanel("Working", stWarn.Render("● "+m.busy), m.width) + "\n")
	}
	if m.active != nil {
		detail := stSubtle.Render("Waiting for "+m.active.Provider+" to finish sign-in.") + "\n"
		if m.active.UserCode == "" {
			detail += stKey.Render(truncate(m.active.URL, max(m.width-6, 20)))
		} else {
			detail += stFaint.Render("Code  ") + stKey.Render(m.active.UserCode) + "\n" +
				stKey.Render(truncate(m.active.URL, max(m.width-6, 20)))
		}
		detail += "\n" + stFaint.Render("o reopen browser  ·  c copy code  ·  u copy URL  ·  esc cancel")
		b.WriteString(roundedPanel("Continue in your browser", detail, m.width) + "\n")
	}
	if m.active == nil && m.connected != nil {
		b.WriteString(m.connectedPanel() + "\n")
	}
	localY := strings.Count(b.String(), "\n")
	m.list.height = max(m.height-localY, 5)
	m.list.setOrigin(m.app.bodyX, m.app.bodyY+localY)
	b.WriteString(m.list.render())
	return tea.NewView(b.String())
}

func (m *loginsModel) actions() []action {
	if m.active != nil {
		actions := []action{{Key: "o", Label: "Open browser", Primary: true}}
		if m.active.UserCode != "" {
			actions = append(actions, action{Key: "c", Label: "Copy code"})
		}
		return append(actions, action{Key: "u", Label: "Copy URL"}, action{Key: "esc", Label: "Cancel", Dangerous: true})
	}
	primary := action{Key: "enter", Label: "Connect account", Primary: true}
	acts := []action{primary}
	if r := m.list.selected(); r != nil {
		if rowData, ok := r.key.(loginRowData); ok && rowData.platform.SignedIn {
			if m.accountNeedsSignIn(rowData.platform) {
				acts[0] = action{Key: "s", Label: "Sign in again", Primary: true}
			} else {
				acts[0] = action{Key: "enter", Label: "Account details", Primary: true}
			}
			if rowData.platform.RoutesTo != "" {
				label := "Add to routing"
				if rowData.enrolled() {
					label = "Remove from routing"
				}
				acts = append(acts, action{Key: "space", Label: label})
			}
			acts = append(acts, action{Key: "d", Label: "Forget", Dangerous: true})
		}
	}
	if m.connected != nil {
		acts = append(acts, action{Key: "x", Label: "Dismiss panel"})
	}
	return acts
}

func (m *loginsModel) searching() bool { return m.list.isSearching() }
