package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// manageOverlay is a provider's control panel: what it is, how it is
// connected, every credential with its health and traffic, the account's
// allowance, and the actions that operate on the selected credential. It
// reads the page's live state on every render, so a completed action shows
// its result without the panel being rebuilt.
type manageOverlay struct {
	page          *providersModel
	platform      string
	width, height int
	cursor        int
	offset        int
	closeHit      hitRegion
}

// credential is one selectable line in the panel: the stored login or one
// pasted key.
type credential struct {
	login bool
	key   KeyView
}

func (m *providersModel) openManage(p providerRow) tea.Cmd {
	overlay := &manageOverlay{page: m, platform: p.platform()}
	m.app.overlay = overlay
	cmds := []tea.Cmd{overlay.Init()}
	if _, cached := m.probes[p.platform()]; !cached && p.connected() {
		cmds = append(cmds, m.probe(p))
	}
	return tea.Batch(cmds...)
}

// probe asks the gateway what quota the provider published. It never mutates
// gateway state.
func (m *providersModel) probe(p providerRow) tea.Cmd {
	c := m.app.Client
	id, platform := p.dir.ID, p.platform()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var pr probeReading
		err := c.Get(ctx, "/api/providers/"+id+"/probe", &pr)
		return providerDetailMsg{platform: platform, probe: pr, err: err}
	}
}

// refreshManageOverlay keeps an open panel's cursor valid after the page's
// data changed under it (a removed key, a forgotten login).
func (m *providersModel) refreshManageOverlay() {
	if o, ok := m.app.overlay.(*manageOverlay); ok {
		o.clamp()
	}
}

func (o *manageOverlay) Init() tea.Cmd { return nil }

func (o *manageOverlay) row() *providerRow { return o.page.rowFor(o.platform) }

func (o *manageOverlay) credentials() []credential {
	r := o.row()
	if r == nil {
		return nil
	}
	out := make([]credential, 0, len(r.keys)+1)
	if r.signedIn() {
		out = append(out, credential{login: true})
	}
	for _, k := range r.pastedKeys() {
		out = append(out, credential{key: k})
	}
	return out
}

func (o *manageOverlay) clamp() {
	n := len(o.credentials())
	if n == 0 {
		o.cursor = 0
		return
	}
	o.cursor = min(max(o.cursor, 0), n-1)
}

func (o *manageOverlay) selected() (credential, bool) {
	creds := o.credentials()
	if o.cursor < 0 || o.cursor >= len(creds) {
		return credential{}, false
	}
	return creds[o.cursor], true
}

func (o *manageOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		o.width, o.height = msg.Width, msg.Height
	case tea.MouseClickMsg:
		if o.closeHit.contains(msg.X, msg.Y) {
			return nil, nil
		}
	case tea.KeyPressMsg:
		r := o.row()
		if r == nil {
			return nil, nil
		}
		m := o.page
		switch msg.String() {
		case "esc", "q":
			return nil, nil
		case "up", "k":
			o.cursor--
			o.clamp()
			return o, nil
		case "down", "j":
			o.cursor++
			o.clamp()
			return o, nil
		case "o":
			return o, m.openSignup(r.dir)
		}
		if m.busy != "" {
			return o, nil
		}
		// handoff yields whatever the page put on screen in place of this
		// panel - a confirm, a form, nothing at all for a browser flow - so
		// the root model installs that instead of clearing it.
		handoff := func(cmd tea.Cmd) (tea.Model, tea.Cmd) {
			if m.app.overlay != o {
				return m.app.overlay, cmd
			}
			return o, cmd
		}
		switch msg.String() {
		case "c":
			if !r.connected() {
				return handoff(m.connect(*r))
			}
		case "a":
			if r.dir.Adapter {
				return handoff(m.openAddKey(*r))
			}
			return o, nil
		case "s":
			if r.accountKind() && (m.accountNeedsSignIn(*r) || r.signedIn()) {
				return handoff(m.beginSignIn(*r.login))
			}
			return o, nil
		}
		cred, ok := o.selected()
		if !ok {
			return o, nil
		}
		if cred.login {
			switch msg.String() {
			case "space":
				return o, m.pauseResume(*r)
			case "d":
				return handoff(m.confirmForgetLogin(*r))
			}
			return o, nil
		}
		k := cred.key
		switch msg.String() {
		case "enter":
			return o, m.checkKey(k)
		case "space":
			return o, m.setKeysEnabled(r.dir.Name, []KeyView{k}, !k.Enabled)
		case "r":
			return o, m.revealKey(k)
		case "x":
			return o, m.clearCooldowns(k)
		case "d":
			return handoff(m.confirmRemoveKey(r.dir.Name, k))
		}
	}
	return o, nil
}

func (o *manageOverlay) View() tea.View {
	r := o.row()
	if r == nil {
		return tea.NewView(stModal.Render(stFaint.Render("This provider is no longer listed.")))
	}
	m := o.page
	boxWidth := min(max(o.width-12, 50), 92)
	contentWidth := max(boxWidth-6, 20)
	const kw = 11

	var body strings.Builder
	title := brandText(r.dir.Name, 0)
	summary := accessWord(accessOf(r.dir)) + stFaint.Render(" · ") + stSubtle.Render(fmt.Sprintf("%d models", r.dir.ModelCount))
	if !r.connected() && r.dir.FreeModels > 0 {
		summary = accessWord(accessOf(r.dir)) + stFaint.Render(" · ") + stSubtle.Render(fmt.Sprintf("%d free models advertised", r.dir.FreeModels))
	}
	body.WriteString(joinEdges(title, summary, contentWidth) + "\n")
	body.WriteString(stFaint.Render(strings.Repeat("─", contentWidth)) + "\n")

	line := func(k, v string) { body.WriteString(truncate(keyRow(k, v, kw), contentWidth) + "\n") }
	connection, status := m.connectionCells(*r)
	line("Connection", stSubtle.Render(connection)+"  "+statusStyle(status))
	if !r.dir.Adapter {
		line("Adapter", stBad.Render("none - browse only, cannot route"))
	}
	setup := frictionOf(r.dir)
	if r.dir.Keyless {
		setup = "key optional"
	}
	line("Setup", stSubtle.Render(setup))
	if r.dir.APIKeyURL != "" {
		line("Signup", stSubtle.Render(r.dir.APIKeyURL))
	}
	if r.dir.DocsURL != "" {
		line("Docs", stSubtle.Render(r.dir.DocsURL))
	}
	if r.connected() {
		if probe, ok := m.probes[o.platform]; ok {
			line("Quota", probeText(probe.probe, probe.err))
		} else {
			line("Quota", stFaint.Render("reading…"))
		}
	}
	if strings.TrimSpace(r.dir.Note) != "" {
		line("Note", stSubtle.Render(r.dir.Note))
	}

	creds := o.credentials()
	if len(creds) > 0 {
		body.WriteString("\n" + section("Credentials") + "\n")
		for i, cred := range creds {
			cursor := "  "
			if i == o.cursor {
				cursor = brandText("◆", i) + " "
			}
			body.WriteString(truncate(cursor+o.credentialLine(*r, cred), contentWidth) + "\n")
		}
		if m.reveal.value != "" && strings.EqualFold(m.reveal.platform, o.platform) && time.Now().Before(m.reveal.until) {
			body.WriteString("  " + stWarn.Render(m.reveal.value) + stFaint.Render("  hides in 30s") + "\n")
		}
	}

	if r.signedIn() {
		body.WriteString("\n" + section("Allowance") + "\n")
		body.WriteString(o.allowanceBlock(*r, contentWidth) + "\n")
	}

	body.WriteString("\n")
	body.WriteString(toolbarWrap(o.chips(*r), contentWidth))

	box := stModal.Width(boxWidth).Render(strings.TrimRight(body.String(), "\n"))
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(o.width, o.height, boxW, boxH)
	o.closeHit = hitRegion{x: boxX + boxW - 12, y: boxY + boxH - 3, w: 10, h: 1}
	return tea.NewView(box)
}

func (o *manageOverlay) credentialLine(r providerRow, cred credential) string {
	m := o.page
	if cred.login {
		who := "browser login"
		if account := maskAccount(r.login.Account); account != "" {
			who = account
		}
		state := statusStyle(m.accountStatus(r))
		return padRight(stHead.Render(who), 26) + " " + state
	}
	k := cred.key
	name := k.MaskedKey
	if k.Label != "" {
		name = k.Label + " " + stFaint.Render(k.MaskedKey)
	}
	parts := []string{padRight(stHead.Render(name), 26), pill(statusWord(k.Status)), pill(enabledWord(k.Enabled))}
	if act, ok := m.data.activity[k.ID]; ok {
		if act.Served+act.Failed > 0 {
			parts = append(parts, stSubtle.Render(fmt.Sprintf("%d✓ %d✗", act.Served, act.Failed)))
		}
		if act.CoolingUntil != nil && *act.CoolingUntil > time.Now().Unix() {
			left := time.Until(time.Unix(*act.CoolingUntil, 0)).Round(time.Second)
			parts = append(parts, stWarn.Render("cooling "+left.String()))
		}
	}
	if k.LastCheckedAt != nil && *k.LastCheckedAt != "" {
		parts = append(parts, stFaint.Render("checked "+relTime(*k.LastCheckedAt)+" ago"))
	}
	return strings.Join(parts, "  ")
}

func (o *manageOverlay) allowanceBlock(r providerRow, width int) string {
	m := o.page
	lid := strings.ToLower(r.login.ID)
	u, ok := m.usage[lid]
	switch {
	case !m.usageLoaded:
		return stSubtle.Render("Reading allowance…")
	case !ok:
		if m.usageErr != nil && publishesAllowance(lid) {
			return stWarn.Render("Allowance report offline; routing may still work.") + "\n" +
				stFaint.Render(truncate(m.usageErr.Error(), width))
		}
		return stFaint.Render("This account does not publish an allowance report.")
	case u.NeedsSignIn:
		return stWarn.Render("Sign in again to refresh this account.") + "\n" + stFaint.Render(truncate(u.Error, width))
	case u.Error != "":
		return stWarn.Render("Allowance report offline; routing may still work.") + "\n" + stFaint.Render(truncate(u.Error, width))
	default:
		return allowanceDetail(u)
	}
}

func (o *manageOverlay) chips(r providerRow) []string {
	m := o.page
	var chips []string
	if !r.connected() {
		chips = append(chips, actionChip("c", "Connect", true, false, false))
	} else if cred, ok := o.selected(); ok {
		if cred.login {
			pause := "Pause routing"
			if !r.enrolled() {
				pause = "Resume routing"
			}
			if m.accountNeedsSignIn(r) {
				chips = append(chips, actionChip("s", "Sign in again", true, false, false))
			}
			chips = append(chips,
				actionChip("space", pause, !m.accountNeedsSignIn(r), false, false),
				actionChip("d", "Forget login", false, true, false),
			)
		} else {
			toggle := "Disable"
			if !cred.key.Enabled {
				toggle = "Enable"
			}
			chips = append(chips,
				actionChip("enter", "Check health", true, false, false),
				actionChip("space", toggle, false, false, false),
				actionChip("r", "Reveal", false, false, false),
				actionChip("x", "Clear cooldowns", false, false, false),
				actionChip("d", "Remove key", false, true, false),
			)
		}
	}
	if r.dir.Adapter && r.connected() {
		chips = append(chips, actionChip("a", "Add key", false, false, false))
	}
	if r.dir.APIKeyURL != "" || r.dir.DocsURL != "" {
		chips = append(chips, actionChip("o", "Open site", false, false, false))
	}
	return append(chips, actionChip("esc", "Close", false, false, false))
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
		return head
	}
	if pr.Message != "" {
		return stSubtle.Render(pr.Message)
	}
	return stFaint.Render("no published quota")
}

// ── credential actions ──────────────────────────────────────────────────────

func (m *providersModel) checkKey(k KeyView) tea.Cmd {
	c := m.app.Client
	m.busy = "checking " + k.Platform + "…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var out struct {
			KeyID  int64  `json:"keyId"`
			Status string `json:"status"`
		}
		if err := c.Post(ctx, fmt.Sprintf("/api/health/check/%d", k.ID), map[string]any{}, &out); err != nil {
			return errMsg{Screen: "providers", Err: err}
		}
		return doneMsg{Tab: TabProviders, Text: k.Platform + " health: " + out.Status}
	}
}

func (m *providersModel) revealKey(k KeyView) tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out struct {
			Key string `json:"key"`
		}
		if err := c.Post(ctx, fmt.Sprintf("/api/keys/%d/reveal", k.ID), map[string]any{}, &out); err != nil {
			return errMsg{Screen: "providers", Err: err}
		}
		return revealMsg{platform: k.Platform, value: out.Key, until: time.Now().Add(30 * time.Second)}
	}
}

func (m *providersModel) clearCooldowns(k KeyView) tea.Cmd {
	c := m.app.Client
	m.busy = "clearing cooldowns…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out map[string]any
		if err := c.Delete(ctx, fmt.Sprintf("/api/keys/%d/cooldowns", k.ID), &out); err != nil {
			return errMsg{Screen: "providers", Err: err}
		}
		return doneMsg{Tab: TabProviders, Text: "cooldowns cleared for " + k.Platform}
	}
}

func (m *providersModel) confirmRemoveKey(name string, k KeyView) tea.Cmd {
	c := m.app.Client
	label := k.Label
	if label == "" {
		label = k.MaskedKey
	}
	m.app.overlay = &confirmOverlay{
		verb:     "Remove key",
		question: fmt.Sprintf("Remove the %s key %q? Routing stops using it immediately.", name, label),
		yes: func() tea.Msg {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var out map[string]any
				if err := c.Delete(ctx, fmt.Sprintf("/api/keys/%d", k.ID), &out); err != nil {
					return errMsg{Screen: "providers", Err: err}
				}
				return doneMsg{Tab: TabProviders, Text: name + " key removed"}
			}
		},
	}
	return m.app.overlay.Init()
}

func (m *providersModel) confirmForgetLogin(p providerRow) tea.Cmd {
	c := m.app.Client
	id, name := p.login.ID, p.dir.Name
	m.app.overlay = &confirmOverlay{
		verb:     "Forget login",
		question: fmt.Sprintf("Forget the %s login? Its models leave routing. Your actual account is untouched; only this gateway's stored token is deleted.", name),
		yes: func() tea.Msg {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var out map[string]any
				if err := c.Delete(ctx, fmt.Sprintf("/api/logins/%s/forget", id), &out); err != nil {
					return errMsg{Screen: "providers", Err: err}
				}
				return doneMsg{Tab: TabProviders, Text: name + " login forgotten"}
			}
		},
	}
	return m.app.overlay.Init()
}
