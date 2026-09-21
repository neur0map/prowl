package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/assist"
	"github.com/neur0map/prowl/internal/workspace"
)

// ollamaKeepAlive is how long a warmed model stays resident between queries:
// long enough to cover a coding session without pinning VRAM indefinitely.
const ollamaKeepAlive = "30m"

// ollamaEnv abstracts the side-effecting calls the lifecycle makes, so the
// decision logic is unit-tested with fakes instead of spawning ollama/systemctl.
type ollamaEnv struct {
	reachable      func(ctx context.Context) bool // ollama daemon answers
	hasUnit        func() (user bool, ok bool)    // an ollama.service exists (system or user)
	systemdUser    func() bool                    // a user systemd manager is usable
	startUnit      func(user bool) error          // systemctl [--user] start ollama.service
	writeAndEnable func() error                   // install + enable a user ollama.service
	spawnDetached  func() error                   // setsid ollama serve in the background
}

// ensureOllamaRunning brings the Ollama daemon up, preferring the least invasive
// option that works: reuse a running daemon, then an existing ollama.service,
// then a systemd user unit (survives reboot), then a detached background
// process. It returns whether the daemon ended up reachable. Best-effort:
// semantic search degrades to structural-only when it cannot be started.
func ensureOllamaRunning(ctx context.Context, e ollamaEnv) bool {
	if e.reachable(ctx) {
		return true
	}
	if user, ok := e.hasUnit(); ok {
		if e.startUnit(user) == nil && e.reachable(ctx) {
			return true
		}
	}
	if e.systemdUser() {
		if e.writeAndEnable() == nil && e.reachable(ctx) {
			return true
		}
	}
	_ = e.spawnDetached()
	return e.reachable(ctx)
}

// ensureOllama is the daemon-management hook setupAI calls; a package variable so
// tests can stub it out instead of touching systemd or spawning ollama.
var ensureOllama = func(ctx context.Context, oll *assist.Ollama, root string) bool {
	return ensureOllamaRunning(ctx, realOllamaEnv(oll, root))
}

// realOllamaEnv wires the lifecycle to the local system: systemctl for service
// management and a detached `ollama serve` fallback. root locates the per-project
// log directory used by the fallback.
func realOllamaEnv(oll *assist.Ollama, root string) ollamaEnv {
	return ollamaEnv{
		reachable: oll.Available,
		hasUnit: func() (bool, bool) {
			if !haveSystemctl() {
				return false, false
			}
			// Prefer the user unit: starting it needs no polkit authorization.
			if systemctlOK("--user", "cat", "ollama.service") {
				return true, true
			}
			if systemctlOK("cat", "ollama.service") {
				return false, true
			}
			return false, false
		},
		systemdUser: func() bool {
			return haveSystemctl() && systemctlOK("--user", "show-environment")
		},
		startUnit: func(user bool) error {
			uiLog.Info("starting the Ollama service")
			args := []string{"--no-ask-password", "start", "ollama.service"}
			if user {
				args = append([]string{"--user"}, args...)
			}
			if err := systemctlRun(args...); err != nil {
				return err
			}
			if !waitReachable(oll, 8*time.Second) {
				return fmt.Errorf("ollama service started but is not answering")
			}
			return nil
		},
		writeAndEnable: func() error {
			uiLog.Info("installing an Ollama user service so it stays up across reboots")
			if err := writeOllamaUserUnit(); err != nil {
				return err
			}
			_ = systemctlRun("--user", "daemon-reload")
			if err := systemctlRun("--user", "--no-ask-password", "enable", "--now", "ollama.service"); err != nil {
				return err
			}
			if !waitReachable(oll, 8*time.Second) {
				return fmt.Errorf("ollama user service enabled but is not answering")
			}
			return nil
		},
		spawnDetached: func() error {
			uiLog.Info("starting Ollama in the background for this session")
			if err := spawnOllama(root); err != nil {
				uiLog.Warnf("could not start Ollama: %v", err)
				return err
			}
			if !waitReachable(oll, 8*time.Second) {
				uiLog.Warn("Ollama is starting but did not answer yet; semantic search activates once it is up")
			}
			return nil
		},
	}
}

func haveSystemctl() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

const systemctlTimeout = 10 * time.Second

// systemctlOK reports whether a systemctl command exits zero within the timeout.
func systemctlOK(args ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", args...).Run() == nil
}

// systemctlRun runs a systemctl command bounded by the timeout, so a hung daemon
// or a blocked polkit authorization cannot stall init indefinitely.
func systemctlRun(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", args...).Run()
}

// ollamaPath resolves the ollama binary, falling back to the bare name.
func ollamaPath() string {
	if p, err := exec.LookPath("ollama"); err == nil {
		return p
	}
	return "ollama"
}

// waitReachable polls the daemon until it answers or the deadline passes.
func waitReachable(oll *assist.Ollama, d time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for {
		if oll.Available(ctx) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// userUnitDir returns the systemd user unit directory, honoring XDG_CONFIG_HOME.
func userUnitDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user"), nil
}

// writeOllamaUserUnit installs a user-level ollama.service that restarts on
// failure, so Ollama comes back after a crash or a reboot.
func writeOllamaUserUnit() error {
	// systemd requires an absolute ExecStart; refuse to write a unit we cannot
	// run rather than leave a broken one on disk.
	bin, err := exec.LookPath("ollama")
	if err != nil || !filepath.IsAbs(bin) {
		return fmt.Errorf("ollama binary not found on PATH; skipping user service")
	}
	dir, err := userUnitDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var envBlock string
	for _, l := range ollamaEnvLines(os.Environ()) {
		envBlock += l + "\n"
	}
	unit := "[Unit]\n" +
		"Description=Ollama (managed by Prowl)\n\n" +
		"[Service]\n" +
		"ExecStart=" + bin + " serve\n" +
		envBlock +
		"Restart=always\n" +
		"RestartSec=2\n\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
	return os.WriteFile(filepath.Join(dir, "ollama.service"), []byte(unit), 0o644)
}

// ollamaEnvLines turns OLLAMA_* entries from environ into systemd Environment=
// directives, so the managed service uses the same models dir and host as the
// user's own ollama (for example a custom OLLAMA_MODELS) rather than the empty
// default store, which would otherwise look like the models are missing.
func ollamaEnvLines(environ []string) []string {
	var lines []string
	for _, e := range environ {
		if strings.HasPrefix(e, "OLLAMA_") {
			lines = append(lines, "Environment=\""+e+"\"")
		}
	}
	return lines
}

// spawnOllama starts `ollama serve` in its own session (so it outlives init),
// logging to the project's .prowl/logs/ollama.log.
func spawnOllama(root string) error {
	logDir := filepath.Join(root, workspace.Dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(logDir, "ollama.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(ollamaPath(), "serve")
	cmd.Stdout, cmd.Stderr = logf, logf
	configureDetachedProcess(cmd)
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return err
	}
	// The child holds its own copy of the log fd; release ours.
	_ = logf.Close()
	return nil
}
