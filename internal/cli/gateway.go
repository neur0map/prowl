package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"net/http"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/catalog"
	"github.com/neur0map/prowl/internal/gateway/inject"
	"github.com/neur0map/prowl/internal/gateway/service"
	"github.com/neur0map/prowl/internal/gateway/tui"
)

// newGatewayCmd is the provider/gateway surface: the interactive TUI is the
// default, with daemon control and scriptable subcommands around it. There is
// no browser dashboard; subscription OAuth may open the provider's login page.
func newGatewayCmd(version string, embedded bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Manage the smart-routing provider gateway from the terminal",
		Long: `Open the gateway console: add free-tier provider keys, sign in subscriptions,
build routing sets, watch usage, and inject the gateway into your coding
harnesses - all from the TUI. No web UI.

Running the console attaches to an already-running gateway (` + "`gateway up`" + `)
when one is listening, and otherwise starts one for the session.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			port, _ := cmd.Flags().GetInt("port")
			return tui.Run(cmd.Context(), port, version)
		},
	}
	cmd.Flags().Int("port", gateway.DefaultPort, "gateway port (loopback only)")

	up := &cobra.Command{
		Use:   "up",
		Short: "Start the gateway as a background daemon",
		Args:  cobra.NoArgs,
		RunE:  runGatewayUp,
	}
	down := &cobra.Command{
		Use:   "down",
		Short: "Stop the background gateway",
		Args:  cobra.NoArgs,
		RunE:  runGatewayDown,
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Report whether the gateway is running",
		Args:  cobra.NoArgs,
		RunE:  runGatewayStatus,
	}
	restart := &cobra.Command{
		Use:   "restart",
		Short: "Restart the background gateway",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runGatewayDown(cmd, args); err != nil {
				return err
			}
			return runGatewayUp(cmd, args)
		},
	}
	serve := &cobra.Command{
		Use:    "serve",
		Short:  "Run the gateway in the foreground (what `up` spawns)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE:   runGatewayServe,
	}
	providers := &cobra.Command{
		Use:   "providers",
		Short: "List the provider directory without opening the console",
		Args:  cobra.NoArgs,
		RunE:  runGatewayProviders,
	}
	providers.Flags().Bool("free-only", false, "only providers with a standing free tier")
	providers.Flags().Bool("routable-only", false, "only providers the gateway can proxy to today")

	injectCmd := &cobra.Command{
		Use:   "inject [harness...]",
		Short: "Write the gateway provider into coding harness configs",
		Long: `Point the harnesses you name at this gateway: OMP, Pi, Claude Code,
Codex, OpenCode, Hermes, OpenClaw, and Prowl Legacy. Only the routing aliases
(auto, auto:* and your named sets) are offered - the gateway picks the provider per
request. Every write is recorded so ` + "`gateway inject --remove`" + ` reverts
exactly what was added.

Skills and rules for repo navigation are a separate, existing step:
` + "`prowl skills`" + `.`,
		RunE: runGatewayInject,
	}
	injectCmd.Flags().Bool("remove", false, "revert a previous injection instead of applying")
	injectCmd.Flags().Bool("all", false, "every supported harness detected on this machine")
	injectCmd.Flags().Int("port", gateway.DefaultPort, "port the gateway listens on (harnesses are pointed here)")

	cmd.AddCommand(up, down, status, restart, serve, providers, injectCmd)
	for _, c := range []*cobra.Command{up, down, status, restart, serve} {
		c.Flags().Int("port", gateway.DefaultPort, "gateway port (loopback only)")
	}
	if embedded {
		// Embedded in a host program, the running executable is the host, not
		// Prowl. `up`/`restart` spawn it as a daemon (`<host> gateway serve`) and
		// `serve` runs the gateway in-process against the host's lifecycle - both
		// unsafe and usually broken. Refuse with a clear error; `down`/`status`
		// only read/stop a tracked pid, so they stay available.
		for _, c := range []*cobra.Command{up, restart, serve} {
			c.RunE = embeddedGatewayDaemonUnavailable
		}
		// The bare console (tui.Run) offers a "keep running" handoff that spawns
		// the daemon (`<host> gateway serve`); refuse it too. The non-spawning
		// subcommands (providers, inject, down, status) stay available.
		cmd.RunE = func(*cobra.Command, []string) error {
			return errors.New("the gateway console is unavailable while Prowl runs embedded in a host program; run the standalone prowl binary, or use the scriptable gateway subcommands (providers, inject, status, down)")
		}
	}
	return cmd
}

// embeddedGatewayDaemonUnavailable rejects a gateway daemon-lifecycle command
// that would own or spawn the running executable while Prowl is embedded in a
// host program.
func embeddedGatewayDaemonUnavailable(cmd *cobra.Command, _ []string) error {
	return fmt.Errorf("gateway %s is unavailable while Prowl runs embedded in a host program; run the standalone prowl binary to manage the gateway daemon", cmd.Name())
}

func runGatewayServe(cmd *cobra.Command, _ []string) error {
	port, _ := cmd.Flags().GetInt("port")
	_, restoreLogs, err := gateway.RouteLogsToFile("")
	if err != nil {
		return err
	}
	defer restoreLogs()

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The machine-local token authenticates the occupied-port probe below, so a
	// hostile local listener cannot impersonate a running gateway and trick this
	// process into silently backing off from a port it should own.
	token, err := gateway.EnsureToken(gateway.Dir())
	if err != nil {
		return err
	}

	ln, err := gateway.ListenLoopback(port)
	if err != nil {
		// A second gateway on this port is a solved problem, not an error: the
		// daemon exists precisely so clients attach. But only an authenticated
		// gateway counts - an occupied port that cannot prove it is ours
		// surfaces the bind failure instead of a false "already running".
		if url, ok := gateway.Running(ctx, port, token); ok {
			fmt.Fprintf(cmd.OutOrStdout(), "gateway already running: %s\n", url)
			return nil
		}
		return err
	}
	svc, err := service.Open(ctx, service.Options{Port: port})
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer func() { _ = svc.Close() }()
	if err := gateway.WritePIDFile(gateway.Dir(), port); err != nil {
		// Without a durable pid record `gateway down`/`status` cannot find or
		// verify this process, so an unrecorded listener is an unmanageable
		// daemon. Fail startup and release the port rather than serve one that
		// can never be cleanly stopped. The deferred svc.Close releases the
		// rest; closing the listener here leaves nothing bound.
		_ = ln.Close()
		return fmt.Errorf("record gateway pid: %w", err)
	}
	defer func() { _ = gateway.RemovePIDFileIfPID(gateway.Dir(), os.Getpid(), port) }()

	fmt.Fprintf(cmd.OutOrStdout(), "prowl gateway listening on %s\n", svc.Addr())
	return svc.Serve(ctx, ln)
}

func runGatewayUp(cmd *cobra.Command, _ []string) error {
	port, _ := cmd.Flags().GetInt("port")
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	result, err := gateway.StartDaemon(cmd.Context(), bin, port)
	if err != nil {
		return err
	}
	if result.AlreadyRunning {
		fmt.Fprintf(cmd.OutOrStdout(), "already running on 127.0.0.1:%d\n", port)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "gateway up on 127.0.0.1:%d (log: %s)\n", port, result.LogPath)
	return nil
}

func runGatewayDown(cmd *cobra.Command, _ []string) error {
	port, _ := cmd.Flags().GetInt("port")
	result, err := gateway.StopTrackedDaemon(cmd.Context(), port)
	if err != nil {
		return err
	}
	switch result.State {
	case gateway.DaemonNotTracked:
		fmt.Fprintln(cmd.OutOrStdout(), "no tracked gateway; nothing to stop")
	case gateway.DaemonStale:
		fmt.Fprintln(cmd.OutOrStdout(), "the tracked pid is gone; cleaned the pid file")
	case gateway.DaemonStopped:
		fmt.Fprintf(cmd.OutOrStdout(), "gateway stopped (pid %d)\n", result.PID)
	default:
		return fmt.Errorf("unknown gateway stop state %q", result.State)
	}
	return nil
}

func runGatewayStatus(cmd *cobra.Command, _ []string) error {
	port, _ := cmd.Flags().GetInt("port")
	token, err := gateway.EnsureToken(gateway.Dir())
	if err != nil {
		return err
	}
	url, ok := gateway.Running(cmd.Context(), port, token)
	out := cmd.OutOrStdout()
	if !ok {
		fmt.Fprintf(out, "stopped (port %d)\n", port)
		return nil
	}
	fmt.Fprintf(out, "running: %s\n", url)
	if pid, err := gateway.ReadPIDFile(gateway.Dir(), port); err == nil && pid > 0 {
		alive := "yes"
		if !gateway.TrackedDaemonRunning(port) {
			alive = "no (stale pid file)"
		}
		fmt.Fprintf(out, "pid: %d · alive: %s\n", pid, alive)
	}
	return nil
}

func runGatewayProviders(cmd *cobra.Command, _ []string) error {
	freeOnly, _ := cmd.Flags().GetBool("free-only")
	routableOnly, _ := cmd.Flags().GetBool("routable-only")

	all, err := catalog.Directory()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tCLASS\tFREE MODELS\tSIGNUP\tROUTABLE")
	n := 0
	for _, p := range all {
		if freeOnly && p.Class != catalog.ClassFree && p.Class != catalog.ClassCredits {
			continue
		}
		if routableOnly && !p.Routable {
			continue
		}
		n++
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%v\n", p.Name, p.Class, p.FreeModels, p.Friction, p.Routable)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\n%d providers shown\n", n)
	return nil
}

func runGatewayInject(cmd *cobra.Command, args []string) error {
	remove, _ := cmd.Flags().GetBool("remove")
	all, _ := cmd.Flags().GetBool("all")
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	if remove {
		if len(args) == 0 {
			return errors.New("name the harnesses to revert explicitly")
		}
		for _, h := range args {
			t, err := inject.Remove(home, h)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", h, orNone(t.Note))
		}
		return nil
	}

	targets := args
	if all || len(targets) == 0 {
		targets = inject.Installed(home)
		if len(targets) == 0 {
			return errors.New("no supported harness detected; name one explicitly: " +
				strings.Join(inject.Supported(), ", "))
		}
	}

	// The credential: prefer the live daemon's unified key (it is the
	// credential meant to be handed out). Falling back to the machine-local
	// token is deliberate - inject works before the first `gateway up` -
	// but the fallback must be announced: a harness config holding the
	// wrong credential is otherwise invisible until the first 401.
	port, _ := cmd.Flags().GetInt("port")
	token, baseURL, err := injectCredentials(cmd, home, port)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(token, "prowlag-") {
		fmt.Fprintln(cmd.OutOrStdout(), "note: the gateway is not running on this port, so the machine-local token was written instead of the unified api key. Run `prowl gateway up` and re-inject to hand out the routing key.")
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
	models := inject.RoutingModels()
	if sets := inject.DiscoverSets(ctx, baseURL, token); len(sets) > 0 {
		models = append(models, sets...)
	}

	failures := 0
	configured := 0
	for _, h := range targets {
		t, err := inject.Apply(inject.Options{Home: home, BaseURL: baseURL, Token: token, Models: models}, h)
		if err != nil {
			failures++
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", h, err)
			continue
		}
		configured++
		fmt.Fprintf(cmd.OutOrStdout(), "%s: wrote %s\n", h, strings.Join(relPaths(home, t.Files), " "))
		if t.Note != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", t.Note)
		}
	}
	if configured > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "\nstart the gateway (`prowl gateway up`) and select a prowl-gateway model in the harness.")
	}
	if failures > 0 {
		return fmt.Errorf("gateway injection failed for %d of %d requested harnesses", failures, len(targets))
	}
	return nil
}

func injectCredentials(cmd *cobra.Command, home string, port int) (token, baseURL string, err error) {
	tok, err := gateway.EnsureToken(gateway.Dir())
	if err != nil {
		return "", "", err
	}
	baseURL = fmt.Sprintf("http://127.0.0.1:%d/v1", port)
	if _, ok := gateway.Running(cmd.Context(), port, tok); ok {
		// Read the unified key over the API; the local token stays a
		// fallback, never what ships into a harness config.
		if key, err := fetchUnifiedKey(cmd.Context(), port, tok); err == nil && key != "" {
			return key, baseURL, nil
		}
	}
	return tok, baseURL, nil
}

func fetchUnifiedKey(ctx context.Context, port int, token string) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/settings/api-key", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return "", err
	}
	return body.APIKey, nil
}

func orNone(s string) string {
	if s == "" {
		return "reverted"
	}
	return s
}

func relPaths(home string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.HasPrefix(p, home+"/") {
			p = "~/" + strings.TrimPrefix(p, home+"/")
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
