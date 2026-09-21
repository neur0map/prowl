package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Keys is the vault view: every credential the pool holds - pasted API keys,
// keyless providers, linked subscription logins - with its health, its bench
// state, and the actions an operator reaches for: re-check, reveal, toggle,
// delete, clear cooldowns. `a` also registers a custom OpenAI-compatible
// endpoint (the web UI's other half of key management).

type keysLoadedMsg struct {
	keys     []KeyView
	health   HealthResponse
	activity map[int64]keyActivityRow
	err      error
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

type keysModel struct {
	app    *App
	width  int
	height int
	list   list
	data   keysLoadedMsg
	loaded bool
	busy   string
	reveal revealMsg
	// revealGen tags each shown secret so an earlier reveal's hide timer cannot
	// clear a newer one: only the tick carrying the current generation hides.
	revealGen int
}

func (m *keysModel) Init() tea.Cmd { return nil }

func (m *keysModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.list.width = w
}

func (m *keysModel) load() tea.Cmd {
	c := m.app.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var out keysLoadedMsg
		if err := c.Get(ctx, "/api/keys", &out.keys); err != nil {
			out.err = err
			return out
		}
		var health HealthResponse
		if err := c.Get(ctx, "/api/health", &health); err == nil {
			out.health = health
		}
		activity := map[int64]keyActivityRow{}
		var env struct {
			Activity []keyActivityRow `json:"activity"`
		}
		if err := c.Get(ctx, "/api/keys/activity", &env); err == nil {
			for _, a := range env.Activity {
				activity[a.KeyID] = a
			}
		}
		out.activity = activity
		return out
	}
}

func (m *keysModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case keysLoadedMsg:
		m.data = msg
		m.loaded = true
		m.busy = ""
		m.buildRows()
		return m, nil

	case tea.KeyPressMsg:
		if m.list.typeFilter(msg) {
			m.buildRows()
			return m, nil
		}
		if m.busy != "" {
			return m, nil
		}
		switch msg.String() {
		case "up", "k":
			m.list.move(-1)
		case "down", "j":
			m.list.move(1)
		case "/":
			m.list.startSearch()
		case "a":
			return m, m.openCustomForm()
		}
		r := m.list.selected()
		if r == nil {
			return m, nil
		}
		k := r.key.(KeyView)
		c := m.app.Client
		switch msg.String() {
		case "enter", "c":
			m.busy = "checking " + k.Platform + "…"
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				var out struct {
					KeyID  int64  `json:"keyId"`
					Status string `json:"status"`
				}
				if err := c.Post(ctx, fmt.Sprintf("/api/health/check/%d", k.ID), map[string]any{}, &out); err != nil {
					return errMsg{Screen: "keys", Err: err}
				}
				return doneMsg{Tab: TabKeys, Text: k.Platform + " health: " + out.Status}
			}
		case "space", "e":
			newState := !k.Enabled
			m.busy = "updating…"
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var out map[string]any
				if err := c.Patch(ctx, fmt.Sprintf("/api/keys/%d", k.ID), map[string]any{"enabled": newState}, &out); err != nil {
					return errMsg{Screen: "keys", Err: err}
				}
				return doneMsg{Tab: TabKeys, Text: fmt.Sprintf("%s key %s", k.Platform, enabledWord(newState))}
			}
		case "x":
			m.busy = "clearing cooldowns…"
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var out map[string]any
				if err := c.Delete(ctx, fmt.Sprintf("/api/keys/%d/cooldowns", k.ID), &out); err != nil {
					return errMsg{Screen: "keys", Err: err}
				}
				return doneMsg{Tab: TabKeys, Text: "cooldowns cleared for " + k.Platform}
			}
		case "r":
			if k.Platform == "" {
				return m, nil
			}
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var out struct {
					Key string `json:"key"`
				}
				if err := c.Post(ctx, fmt.Sprintf("/api/keys/%d/reveal", k.ID), map[string]any{}, &out); err != nil {
					return errMsg{Screen: "keys", Err: err}
				}
				return revealMsg{platform: k.Platform, value: out.Key, until: time.Now().Add(30 * time.Second)}
			}
		case "d":
			return m, m.confirmDelete(k)
		case "C":
			m.busy = "checking all keys…"
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()
				var out map[string]any
				if err := c.Post(ctx, "/api/health/check-all", map[string]any{}, &out); err != nil {
					return errMsg{Screen: "keys", Err: err}
				}
				return doneMsg{Tab: TabKeys, Text: "health pass finished"}
			}
		}
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
		}
		return m, nil
	}
	return m, nil
}

type revealMsg struct {
	platform, value string
	until           time.Time
}

// revealExpiredMsg is a reveal's hide deadline, tagged with the generation it
// was armed for so a superseded reveal's timer is ignored (see revealGen).
type revealExpiredMsg struct{ gen int }

func (m *keysModel) confirmDelete(k KeyView) tea.Cmd {
	m.app.overlay = &confirmOverlay{
		question: fmt.Sprintf("Delete the %s key %q? Routing stops using it immediately.", k.Platform, k.Label),
		yes: func() tea.Msg {
			c := m.app.Client
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var out map[string]any
				if err := c.Delete(ctx, fmt.Sprintf("/api/keys/%d", k.ID), &out); err != nil {
					return errMsg{Screen: "keys", Err: err}
				}
				return doneMsg{Tab: TabKeys, Text: "key removed"}
			}
		},
	}
	return m.app.overlay.Init()
}

func (m *keysModel) openCustomForm() tea.Cmd {
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
			return errMsg{Screen: "keys", Err: err}
		}
		return doneMsg{Tab: TabKeys, Text: fmt.Sprintf("endpoint registered with %d models", out.Models)}
	}
	m.app.overlay = f
	return f.Init()
}

func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

func (m *keysModel) buildRows() {
	rows := make([]row, 0, len(m.data.keys))
	for _, k := range m.data.keys {
		linked := strings.HasPrefix(k.MaskedKey, "link:")
		name := k.Platform
		if k.Label != "" {
			name = k.Label
		}
		detail := k.MaskedKey
		if linked {
			detail = "linked login"
		}
		act := m.data.activity[k.ID]
		hops := ""
		if act.Served+act.Failed > 0 {
			hops = fmt.Sprintf("%d✓ %d✗", act.Served, act.Failed)
		}
		cooling := ""
		if act.CoolingUntil != nil && *act.CoolingUntil > time.Now().Unix() {
			left := time.Until(time.Unix(*act.CoolingUntil, 0)).Round(time.Second)
			cooling = pill("cooling") + " " + left.String()
		}
		enabled := pill("enabled")
		if !k.Enabled {
			enabled = pill("disabled")
		}
		rows = append(rows, row{
			cells: []string{name, statusWord(k.Status), enabled, detail, hops, cooling},
			styles: []func(string) string{
				nil,
				func(s string) string { return pill(s) },
				func(s string) string { return pill(s) },
				nil, nil,
				func(s string) string {
					if s == "" {
						return s
					}
					return stWarn.Render(s)
				},
			},
			key: k,
		})
	}
	m.list.setRows(rows)
}

// healthSummary lines count platforms with a working credential - that is
// what "the pool can route" actually means.
func (m *keysModel) healthSummary() string {
	var parts []string
	for _, p := range m.data.health.Platforms {
		parts = append(parts, fmt.Sprintf("%s %d/%d", p.Platform, p.HealthyKeys, p.TotalKeys))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "  ")
}

func (m *keysModel) View() tea.View {
	if !m.loaded {
		return tea.NewView(roundedPanel(
			"Credential vault",
			m.app.spinner.View()+" "+stSubtle.Render("Opening encrypted credentials"),
			m.width,
		))
	}
	if m.data.err != nil {
		return tea.NewView(roundedPanel(
			"Credential vault unavailable",
			stBad.Render("● "+m.data.err.Error()),
			m.width,
		))
	}

	health := m.healthSummary()
	healthValue := "waiting"
	if health != "" {
		healthValue = "live"
	}
	var b strings.Builder
	b.WriteString(metricStrip([]metric{
		{"Credentials", fmt.Sprintf("%d", len(m.data.keys)), "stored securely"},
		{"Enabled", fmt.Sprintf("%d", m.countEnabled()), "available to routing"},
		{"Health", healthValue, truncate(health, max(m.width/3-4, 8))},
	}, m.width))
	b.WriteString("\n")
	if m.busy != "" {
		b.WriteString(roundedPanel("Working", stWarn.Render("● "+m.busy), m.width) + "\n")
	}
	if m.reveal.value != "" && time.Now().Before(m.reveal.until) {
		b.WriteString(roundedPanel(
			"Temporary reveal",
			stWarn.Render(m.reveal.platform+"  "+m.reveal.value)+"\n"+
				stFaint.Render("This value hides again after 30 seconds."),
			m.width,
		) + "\n")
	}
	localY := strings.Count(b.String(), "\n")
	m.list.height = max(m.height-localY, 5)
	m.list.setOrigin(m.app.bodyX, m.app.bodyY+localY)
	b.WriteString(m.list.render())
	return tea.NewView(b.String())
}

func (m *keysModel) actions() []action {
	return []action{
		{Key: "enter", Label: "Check", Primary: true},
		{Key: "space", Label: "Enable / disable"},
		{Key: "a", Label: "Add endpoint"},
		{Key: "r", Label: "Reveal"},
		{Key: "d", Label: "Remove", Dangerous: true},
	}
}

func (m *keysModel) countEnabled() int {
	n := 0
	for _, k := range m.data.keys {
		if k.Enabled {
			n++
		}
	}
	return n
}

// confirmOverlay is the pointer-safe gate every destructive action passes
// through. Cancel owns focus by default.
type confirmOverlay struct {
	question      string
	yes           func() tea.Msg
	chosen        int
	width, height int
	running       bool
	// op is the id of the operation this confirm launched, stamped on the
	// result so only this confirm - not an unrelated background completion -
	// is dismissed when it finishes.
	op                uint64
	cancelHit, yesHit hitRegion
}

func (c *confirmOverlay) Init() tea.Cmd { return nil }

func (c *confirmOverlay) runYes() (tea.Model, tea.Cmd) {
	c.running = true
	c.op = nextOverlayOp()
	op := c.op
	inner := c.yes()
	return c, func() tea.Msg {
		msg := inner
		if fn, ok := inner.(func() tea.Msg); ok {
			msg = fn()
		}
		return tagOverlayOp(msg, op)
	}
}

func (c *confirmOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
	case tea.MouseClickMsg:
		if c.running {
			return c, nil
		}
		switch {
		case c.cancelHit.contains(msg.X, msg.Y):
			return nil, nil
		case c.yesHit.contains(msg.X, msg.Y):
			c.chosen = 1
			return c.runYes()
		}
	case tea.KeyPressMsg:
		if c.running {
			return c, nil
		}
		switch msg.String() {
		case "esc", "n":
			return nil, nil
		case "left", "h":
			c.chosen = 0
		case "right", "l", "y":
			c.chosen = 1
		case "enter", "space":
			if c.chosen == 1 {
				return c.runYes()
			}
			return nil, nil
		default:
			return c, nil
		}
	}
	return c, nil
}

func (c *confirmOverlay) View() tea.View {
	cancel := actionChip("esc", "Keep it", c.chosen == 0, false, false)
	confirm := actionChip("enter", "Remove", c.chosen == 1, true, false)
	status := stFaint.Render("Nothing changes until you confirm.")
	if c.running {
		status = stWarn.Render("● Applying change…")
	}
	body := brandText("Confirm removal", 0) + "\n" +
		stSubtle.Render("This action changes gateway state immediately.") + "\n\n" +
		c.question + "\n\n" + status + "\n\n" + cancel + " " + confirm
	box := stModal.Width(min(max(c.width-16, 44), 72)).Render(body)
	boxW, boxH := lipgloss.Width(box), lipgloss.Height(box)
	boxX, boxY := overlayOrigin(c.width, c.height, boxW, boxH)
	buttonY := boxY + boxH - 3
	c.cancelHit = hitRegion{x: boxX + 3, y: buttonY, w: lipgloss.Width(cancel), h: 1}
	c.yesHit = hitRegion{x: boxX + 4 + lipgloss.Width(cancel), y: buttonY, w: lipgloss.Width(confirm), h: 1}
	return tea.NewView(box)
}

func (m *keysModel) searching() bool { return m.list.isSearching() }
