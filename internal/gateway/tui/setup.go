package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/neur0map/prowl/internal/gateway/inject"
	"github.com/neur0map/prowl/internal/setup"
)

// Setup is the finish line: pick the coding harnesses this machine runs, and
// the console writes the gateway provider (canonical `auto` only, never the raw
// catalogue or override routes) plus Prowl's skills/rules into each one's own
// config, with a ledger so it is exactly reversible. The checklist on Overview
// flips as rows here go green.

type setupLoadedMsg struct {
	supported []string
	installed map[string]bool
	targets   map[string]inject.Target
	skills    map[string]setup.UserActionKind // client -> planned action
	// skillsErr records a failure to PLAN the skills install for detected
	// harnesses. It is distinct from err (a total setup-read failure) so a
	// skills planning fault is surfaced without blanking harness configuration.
	skillsErr error
	err       error
}

type setupModel struct {
	app    *App
	width  int
	height int
	list   list
	data   setupLoadedMsg
	loaded bool
	busy   string
	detail string
}

func (m *setupModel) Init() tea.Cmd { return nil }

func (m *setupModel) setSize(w, h int) {
	m.width, m.height = w, h
	m.list.width = w
	if m.list.place == "" {
		m.list.place = "Search harnesses"
	}
}

func (m *setupModel) load() tea.Cmd {
	home := m.app.Client.Home
	return func() tea.Msg {
		var out setupLoadedMsg
		out.supported = inject.Supported()
		out.installed = map[string]bool{}
		for _, h := range inject.Installed(home) {
			out.installed[h] = true
		}
		out.targets = map[string]inject.Target{}
		for _, t := range inject.Targets(home) {
			out.targets[t.Harness] = t
		}
		// Skills plan: reuse the same machinery `prowl skills` uses, so Setup
		// and the standalone command never disagree about what's current. The
		// plan lists one action per asset (sorted by destination), so a harness
		// is summarized by aggregating every action and conflict - not by the
		// last-sorted asset, which would let a trailing unchanged asset mask an
		// earlier install/update/conflict and show a false "current" state.
		out.skills = map[string]setup.UserActionKind{}
		clients := setup.DetectInstalledHarnesses()
		if len(clients) > 0 {
			plan, err := setup.PlanUserSkills(setup.UserInstallOptions{Home: home, Version: m.app.Version, Clients: clients})
			if err != nil {
				// A planning failure with harnesses present is a real fault, not
				// "nothing to do": carry it so the row surfaces it actionably
				// instead of masquerading as "no skill-capable harness".
				out.skillsErr = err
			} else {
				out.skills = aggregateSkillStatus(plan)
			}
		}
		return out
	}
}

func (m *setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case setupLoadedMsg:
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
		switch msg.String() {
		case "up", "k":
			m.list.move(-1)
			return m, nil
		case "down", "j":
			m.list.move(1)
			return m, nil
		case "/":
			m.list.startSearch()
			return m, nil
		}
		r := m.list.selected()
		if r == nil || m.busy != "" {
			return m, nil
		}
		harness := r.key.(string)
		key := msg.String()
		if harness == "__skills__" && (key == "enter" || key == "i") {
			key = "s"
		}
		c := m.app.Client
		switch key {
		case "enter", "i":
			m.busy = "injecting " + harness + "…"
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				key, err := c.unifiedKey(ctx)
				if err != nil {
					return errMsg{Screen: "setup", Err: fmt.Errorf("read the api key: %w", err)}
				}
				opts := inject.Options{
					Home:    c.Home,
					BaseURL: c.BaseURL + "/v1",
					Token:   key,
					Models:  inject.RoutingModels(),
				}
				t, err := inject.Apply(opts, harness)
				if err != nil {
					return errMsg{Screen: "setup", Err: err}
				}
				text := harness + " configured"
				if t.Note != "" {
					text += " - " + t.Note
				}
				return doneMsg{Tab: TabSetup, Text: text}
			}
		case "d":
			m.app.overlay = &confirmOverlay{
				question: fmt.Sprintf("Remove the gateway from %s's config? Only what injection wrote is reverted; anything you edited since stays.", harness),
				yes: func() tea.Msg {
					// Return the revert as a command so runYes runs it off the
					// Update loop instead of blocking on filesystem I/O inline.
					return func() tea.Msg {
						t, err := inject.Remove(c.Home, harness)
						if err != nil {
							return errMsg{Screen: "setup", Err: err}
						}
						text := harness + " reverted"
						if t.Note != "" {
							text += " - " + t.Note
						}
						return doneMsg{Tab: TabSetup, Text: text}
					}
				},
			}
			return m, m.app.overlay.Init()
		case "s":
			m.busy = "installing skills & rules…"
			return m, func() tea.Msg {
				clients := setup.DetectInstalledHarnesses()
				if len(clients) == 0 {
					return errMsg{Screen: "setup", Err: fmt.Errorf("no skill-capable harness detected (claude, omp, pi, hermes, openclaw, prowl-legacy)")}
				}
				opts := setup.UserInstallOptions{Home: c.Home, Version: m.app.Version, Clients: clients}
				plan, err := setup.PlanUserSkills(opts)
				if err != nil {
					return errMsg{Screen: "setup", Err: err}
				}
				res, err := setup.ApplyUserSkills(opts, plan, true)
				if err != nil {
					return errMsg{Screen: "setup", Err: err}
				}
				return setupSkillsResult(res, len(plan.Conflicts))
			}
		}
	}
	return m, nil
}

// setupSkillsResult turns an applied user-skills result into an honest console
// message. An apply plan lists one action per asset - unchanged ones included -
// so it counts only destinations Prowl actually wrote and never reports a false
// "N changes". A conflict-only outcome (nothing written, destinations Prowl
// refused to touch) is surfaced as an error, never a green success that touched
// no file; a mixed outcome notes the conflicts alongside the real writes.
func setupSkillsResult(res setup.UserApplyResult, conflicts int) tea.Msg {
	writes := 0
	touched := map[string]bool{}
	for _, a := range res.Actions {
		if a.Kind == setup.UserActionUnchanged {
			continue
		}
		writes++
		touched[a.Client] = true
	}
	switch {
	case writes == 0 && conflicts == 0:
		return doneMsg{Tab: TabSetup, Text: "skills & rules already up to date"}
	case writes == 0:
		return errMsg{Screen: "setup", Err: fmt.Errorf(
			"%d destination(s) left untouched - edited since Prowl installed them; nothing else to change", conflicts)}
	default:
		text := fmt.Sprintf("skills & rules applied: %d change(s) across %d harness(es)", writes, len(touched))
		if conflicts > 0 {
			text += fmt.Sprintf("; %d conflict(s) left untouched", conflicts)
		}
		return doneMsg{Tab: TabSetup, Text: text}
	}
}

func (m *setupModel) buildRows() {
	rows := make([]row, 0, len(m.data.supported)+1)
	for _, h := range m.data.supported {
		state := stFaint.Render("not installed")
		if m.data.installed[h] {
			state = pill("installed")
		}
		action := stFaint.Render("-")
		if t, ok := m.data.targets[h]; ok {
			action = stGood.Render("gateway injected")
			if len(t.Files) > 0 {
				action += stFaint.Render(" · " + filepath.Base(t.Files[0]))
			}
		}
		rows = append(rows, row{
			cells: []string{h, state, action},
			styles: []func(string) string{
				func(s string) string { return stHead.Render(s) }, nil, nil,
			},
			key: h,
			dim: !m.data.installed[h],
		})
	}
	skillsState := "not detected"
	n := 0
	for _, k := range m.data.skills {
		if k != setup.UserActionUnchanged {
			n++
		}
	}
	switch {
	case m.data.skillsErr != nil:
		// A real planning fault, surfaced with its reason. Pressing s re-plans
		// and shows the full error, so the row stays actionable.
		skillsState = stBad.Render("planning failed - " + m.data.skillsErr.Error())
	case len(m.data.skills) == 0:
		skillsState = stFaint.Render("no skill-capable harness")
	case n > 0:
		skillsState = stWarn.Render(fmt.Sprintf("%d harness(es) out of date", n))
	default:
		skillsState = pill("current")
	}
	rows = append(rows, row{
		cells:  []string{"skills & rules", skillsState, "press s"},
		styles: []func(string) string{func(s string) string { return stHead.Render(s) }, nil, nil},
		key:    "__skills__",
	})
	m.list.setRows(rows)
}

// aggregateSkillStatus folds every planned action and conflict for a harness
// into one status, so a harness is "unchanged" (current) only when every one of
// its assets is unchanged and it has no conflict. A conflict is a destination
// Prowl refuses to touch - it is never current - so it aggregates as needing
// attention (surfaced as an update). Precedence keeps the more actionable state:
// remove > update > install > unchanged, and any conflict beats a plain
// unchanged. Aggregating (rather than taking the last sorted asset) stops a
// trailing unchanged asset from masking an earlier install/update/conflict.
func aggregateSkillStatus(plan setup.UserPlan) map[string]setup.UserActionKind {
	status := map[string]setup.UserActionKind{}
	for _, a := range plan.Actions {
		status[a.Client] = mergeSkillStatus(status[a.Client], a.Kind)
	}
	for _, c := range plan.Conflicts {
		status[c.Client] = mergeSkillStatus(status[c.Client], setup.UserActionUpdate)
	}
	return status
}

// mergeSkillStatus returns the higher-precedence of two harness statuses.
func mergeSkillStatus(existing, incoming setup.UserActionKind) setup.UserActionKind {
	if skillStatusRank(incoming) > skillStatusRank(existing) {
		return incoming
	}
	return existing
}

func skillStatusRank(k setup.UserActionKind) int {
	switch k {
	case setup.UserActionUnchanged:
		return 1
	case setup.UserActionInstall:
		return 2
	case setup.UserActionUpdate:
		return 3
	case setup.UserActionRemove:
		return 4
	}
	return 0
}

func (m *setupModel) View() tea.View {
	if !m.loaded {
		return tea.NewView(roundedPanel(
			"Local integration",
			m.app.spinner.View()+" "+stSubtle.Render("Inspecting coding harnesses"),
			m.width,
		))
	}
	if m.data.err != nil {
		return tea.NewView(roundedPanel(
			"Harness setup unavailable",
			stBad.Render("● "+m.data.err.Error()),
			m.width,
		))
	}
	var b strings.Builder
	b.WriteString(inset(metricStrip([]metric{
		{"Supported", fmt.Sprintf("%d", len(m.data.supported)), "known harnesses"},
		{"Detected", fmt.Sprintf("%d", len(m.data.installed)), "installed locally"},
		{"Connected", fmt.Sprintf("%d", len(m.data.targets)), "routing through Prowl"},
	}, m.width-1), 1))
	b.WriteString("\n")
	if m.busy != "" {
		b.WriteString(roundedPanel("Applying setup", stWarn.Render("● "+m.busy), m.width) + "\n")
	}
	localY := strings.Count(b.String(), "\n")
	m.list.height = max(m.height-localY, 5)
	m.list.setOrigin(m.app.bodyX, m.app.bodyY+localY)
	b.WriteString(m.list.render())
	return tea.NewView(b.String())
}

func (m *setupModel) actions() []action {
	return []action{
		{Key: "enter", Label: "Configure", Primary: true},
		{Key: "s", Label: "Install skills"},
		{Key: "d", Label: "Remove", Dangerous: true},
	}
}

// injectTargets is the file-backed ledger read (no API involved: the ledger
// lives on disk beside the gateway state).
func injectTargets(home string) []inject.Target {
	if home == "" {
		return nil
	}
	return inject.Targets(home)
}

var _ = os.Getenv

func (m *setupModel) searching() bool { return m.list.isSearching() }
