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

// The browser/device sign-in flow for subscription accounts. It lives on the
// Providers page: an account is one way to connect a provider, so it is
// started from the provider's row and its progress panel sits above the
// same table. While a flow is pending only its own controls are live, so a
// stray key cannot start a second flow or forget the login mid sign-in.

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
// WindowSeconds is the window length (18000 for 5h, 604800 for 7d) and
// TokensUsed the input+output tokens this gateway routed to the account's
// platform inside it; both are 0 when the gateway cannot measure them.
type LoginUsageWindow struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	Utilization   float64 `json:"utilization"`
	ResetsAt      string  `json:"resetsAt"`
	WindowSeconds int64   `json:"windowSeconds"`
	TokensUsed    int64   `json:"tokensUsed"`
}

type signInStartedMsg struct{ session SignInSession }
type signInStartFailedMsg struct{ err error }
type signInPolledMsg struct{ session SignInSession }
type signInTickMsg struct{ id string }
type signInCancelledMsg struct{ id string }
type signInPollErrMsg struct {
	id  string
	err error
}

// signInFlow is the in-progress sign-in and the gate that stops a second one.
type signInFlow struct {
	// active is the session being polled, nil when none is running.
	active *SignInSession
	since  time.Time
	// starting gates a second start while the POST /api/signin that opens the
	// flow is still in flight (before active is set). It is cleared ONLY by
	// that start's own result (signInStartedMsg or signInStartFailedMsg), so an
	// unrelated load clearing busy cannot reopen the gate and let a second
	// concurrent flow begin.
	starting bool
	// connected is the just-signed-in provider whose success line persists
	// while model discovery finishes, rather than a toast that fades.
	connected string
}

const signInDeadline = 15 * time.Minute

// begin dispatches a sign-in start only when none is already starting or
// active; it returns nil (and the caller does nothing) otherwise.
func (f *signInFlow) begin(c *Client, providerID string) tea.Cmd {
	if f.starting || f.active != nil {
		return nil
	}
	f.starting = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var session SignInSession
		if err := c.Post(ctx, "/api/signin", map[string]string{"provider": providerID}, &session); err != nil {
			// Dedicated start-failure result: only its own handler clears the
			// start gate, so a generic errMsg cannot leave it stuck.
			return signInStartFailedMsg{err: err}
		}
		return signInStartedMsg{session: session}
	}
}

// update consumes the flow's own messages. handled reports whether msg was
// one of them; done carries the provider id of a flow that just completed.
func (f *signInFlow) update(c *Client, msg tea.Msg) (handled bool, cmd tea.Cmd, done string, err error) {
	switch msg := msg.(type) {
	case signInStartedMsg:
		f.active = &msg.session
		f.since = time.Now()
		f.starting = false
		return true, tea.Batch(pollSignIn(c, msg.session.ID), openSignInURL(msg.session.URL)), "", nil
	case signInStartFailedMsg:
		f.starting = false
		return true, nil, "", msg.err
	case signInPolledMsg:
		// Ignore a poll result for a session the operator already cancelled or
		// replaced: a slow in-flight poll must not revive a finished flow nor
		// clobber the session of a newer sign-in.
		if f.active == nil || f.active.ID != msg.session.ID {
			return true, nil, "", nil
		}
		f.active = &msg.session
		switch msg.session.State {
		case "complete":
			f.connected = strings.ToLower(msg.session.Provider)
			f.active = nil
			return true, nil, msg.session.Provider, nil
		case "failed", "cancelled":
			f.active = nil
			text := msg.session.Error
			if text == "" {
				text = "sign-in " + msg.session.State
			}
			return true, nil, "", fmt.Errorf("%s", text)
		}
		if time.Since(f.since) > signInDeadline {
			f.active = nil
			return true, nil, "", fmt.Errorf("sign-in timed out")
		}
		return true, tickSignIn(2*time.Second, f.active.ID), "", nil
	case signInPollErrMsg:
		// A transient poll failure (timeout, connection blip) must not abort the
		// flow. Ignore an error for a session already cancelled or replaced, then
		// keep the active session and retry until the deadline.
		if f.active == nil || f.active.ID != msg.id {
			return true, nil, "", nil
		}
		if time.Since(f.since) > signInDeadline {
			f.active = nil
			return true, nil, "", fmt.Errorf("sign-in timed out")
		}
		return true, tickSignIn(2*time.Second, f.active.ID), "", nil
	case signInTickMsg:
		// A late tick from a superseded flow must not schedule a poll against the
		// session that replaced it.
		if f.active == nil || f.active.ID != msg.id {
			return true, nil, "", nil
		}
		return true, pollSignIn(c, f.active.ID), "", nil
	case signInCancelledMsg:
		// Only the session whose cancel this confirms may be cleared; a duplicate
		// or late cancel must not wipe a newer sign-in.
		if f.active == nil || f.active.ID != msg.id {
			return true, nil, "", nil
		}
		f.active = nil
		return true, func() tea.Msg { return newToast("ok", "Sign-in cancelled") }, "", nil
	}
	return false, nil, "", nil
}

// key handles the pending flow's own controls. Every other key is swallowed
// while a flow is active.
func (f *signInFlow) key(c *Client, key string) tea.Cmd {
	if f.active == nil {
		return nil
	}
	switch key {
	case "o":
		return openSignInURL(f.active.URL)
	case "c":
		value, label := f.active.UserCode, "Sign-in code copied"
		if value == "" {
			value, label = f.active.URL, "Sign-in URL copied"
		}
		return copyLoginValue(value, label)
	case "u":
		return copyLoginValue(f.active.URL, "Sign-in URL copied")
	case "esc":
		return cancelSignIn(c, f.active.ID)
	}
	return nil
}

func (f *signInFlow) actions() []action {
	actions := []action{{Key: "o", Label: "Open browser", Primary: true}}
	if f.active.UserCode != "" {
		actions = append(actions, action{Key: "c", Label: "Copy code"})
	}
	return append(actions, action{Key: "u", Label: "Copy URL"}, action{Key: "esc", Label: "Cancel sign-in", Dangerous: true})
}

// panel is the progress panel shown above the table while a flow is pending.
func (f *signInFlow) panel(width int) string {
	detail := stSubtle.Render("Waiting for "+f.active.Provider+" to finish sign-in in your browser.") + "\n"
	if f.active.UserCode == "" {
		detail += stKey.Render(truncate(f.active.URL, max(width-6, 20)))
	} else {
		detail += stFaint.Render("Code  ") + stKey.Render(f.active.UserCode) + "\n" +
			stKey.Render(truncate(f.active.URL, max(width-6, 20)))
	}
	detail += "\n" + stFaint.Render("o reopen browser  ·  c copy code  ·  u copy URL  ·  esc cancel")
	return roundedPanel("Continue in your browser", detail, width)
}

func pollSignIn(c *Client, id string) tea.Cmd {
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

func cancelSignIn(c *Client, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		var out map[string]any
		if err := c.Delete(ctx, "/api/signin/"+id, &out); err != nil {
			return errMsg{Screen: "providers", Err: err}
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
		return newToast("ok", "Browser opened. Complete sign-in there; this screen updates automatically.")
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

// ── account presentation ────────────────────────────────────────────────────

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

// publishesAllowance reports whether an account has an allowance surface at
// all; the others are shown as "not published" rather than as a failure.
func publishesAllowance(id string) bool {
	lid := strings.ToLower(id)
	return lid == "anthropic" || lid == "hyper"
}

// allowanceSummary is the compact allowance text: a pending marker until the
// first read, the balance or window percentages once known, a dash for an
// account with no allowance surface, and a plain failure when the read broke
// for an account that does publish one.
func allowanceSummary(id string, usage *LoginUsage, loaded bool, loadErr error) string {
	if !loaded {
		if publishesAllowance(id) {
			return "…"
		}
		return ""
	}
	if usage == nil {
		if loadErr != nil && publishesAllowance(id) {
			return "allowance report offline"
		}
		return ""
	}
	if usage.NeedsSignIn {
		return "sign in again"
	}
	if usage.Error != "" {
		return "allowance report offline"
	}
	if usage.Balance != nil {
		unit := usage.Unit
		if unit == "" {
			unit = "credits"
		}
		return trimNum(*usage.Balance) + " " + unit
	}
	parts := make([]string, 0, 2)
	for _, w := range usage.Windows {
		if w.Key == "five_hour" || w.Key == "seven_day" {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", windowShort(w.Key), w.Utilization))
		}
	}
	if len(parts) == 0 && len(usage.Windows) > 0 {
		w := usage.Windows[0]
		parts = append(parts, fmt.Sprintf("%s %.0f%%", windowShort(w.Key), w.Utilization))
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

// allowanceDetail is the full allowance report for the manage panel.
func allowanceDetail(u LoginUsage) string {
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
