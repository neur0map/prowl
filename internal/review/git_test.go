//go:build unix

package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---- helpers -------------------------------------------------------------

func gitBin(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
}

func rawGit(t *testing.T, root string, args ...string) {
	t.Helper()
	if out, err := rawGitEnv(root, nil, args...); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func rawGitEnv(root string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd.CombinedOutput()
}

func initRepo(t *testing.T) (root, markers string) {
	t.Helper()
	gitBin(t)
	root = t.TempDir()
	markers = t.TempDir()
	rawGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("l1\nl2\nl3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, root, "add", "payload.txt")
	rawGit(t, root, "commit", "-qm", "seed")
	return root, markers
}

func markerPresent(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func clearMarkers(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

func markerNames(dir string) []string {
	entries, _ := os.ReadDir(dir)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeGitProc writes a POSIX shell stand-in that spawns a SIGTERM-ignoring
// background child (recording its PID synchronously via $!), performs the
// requested stdout/stderr/hang misbehaviour, then lingers. The filter/driver
// enumeration probe (--get-regexp) exits with the no-match status so pre-flight
// discovery is a benign no-op.
func fakeGitProc(t *testing.T, mode, childPIDPath string) string {
	t.Helper()
	var action string
	switch mode {
	case "stdout":
		action = "i=0\nwhile [ $i -lt 4000 ]; do printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\\n'; i=$((i+1)); done\n"
	case "stderr":
		action = "i=0\nwhile [ $i -lt 4000 ]; do printf 'EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE\\n' 1>&2; i=$((i+1)); done\n"
	case "hang":
		action = ""
	default:
		t.Fatalf("unknown fake mode %q", mode)
	}
	body := "#!/bin/sh\n" +
		"for a in \"$@\"; do case \"$a\" in --get-regexp) exit 1 ;; esac; done\n" +
		"sh -c 'trap \"\" TERM; sleep 30' &\n" +
		"echo $! > \"" + childPIDPath + "\"\n" +
		action +
		"sleep 30\n"
	path := filepath.Join(t.TempDir(), "fakegit.sh")
	writeExec(t, path, body)
	return path
}

// fakeGitConfig makes the filter/driver enumeration probe misbehave so discovery
// failure paths can be exercised. Any non-enumeration call exits 0.
func fakeGitConfig(t *testing.T, mode string) string {
	t.Helper()
	var probe string
	switch mode {
	case "overflow":
		probe = "dd if=/dev/zero bs=1024 count=1200 2>/dev/null | tr '\\0' 'A'\nexit 0\n"
	case "hang":
		probe = "sleep 30\n"
	case "error":
		probe = "echo boom 1>&2\nexit 2\n"
	case "malformed":
		// Exit 0 with output that is not valid `git config -z` framing
		// (no NUL terminator, no key/value newline).
		probe = "printf 'filter.evil.clean garbage-without-newline'\nexit 0\n"
	default:
		t.Fatalf("unknown config mode %q", mode)
	}
	body := "#!/bin/sh\n" +
		"for a in \"$@\"; do case \"$a\" in --get-regexp)\n" + probe + ";; esac; done\n" +
		"exit 0\n"
	path := filepath.Join(t.TempDir(), "fakegit_config.sh")
	writeExec(t, path, body)
	return path
}

func fakeGitExit(t *testing.T, code int) string {
	t.Helper()
	body := "#!/bin/sh\n" +
		"for a in \"$@\"; do case \"$a\" in --get-regexp) exit 1 ;; esac; done\n" +
		"exit " + strconv.Itoa(code) + "\n"
	path := filepath.Join(t.TempDir(), "fakegit_exit.sh")
	writeExec(t, path, body)
	return path
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, perr := strconv.Atoi(string(bytes.TrimSpace(data))); perr == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child PID file %s never populated", path)
	return 0
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func requireProcessDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d in the tree was not killed", pid)
}

type fakeRunner struct {
	out []byte
	err error
}

func (f fakeRunner) Output(_ context.Context, _ string, _ int64, _ ...string) ([]byte, error) {
	return f.out, f.err
}

func (f fakeRunner) Pipe(_ context.Context, _ string, _ int64, _ io.Reader, _ io.Writer, _ ...string) error {
	return f.err
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		m[name] = value
	}
	return m
}

// ---- finding 3: environment allowlist ------------------------------------

func TestExecGitSanitizesInheritedEnvironment(t *testing.T) {
	hostile := []string{
		"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_PARAMETERS",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_ASKPASS", "SSH_ASKPASS", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_PROXY_COMMAND",
		"GIT_ALLOW_PROTOCOL", "GIT_PROTOCOL_FROM_USER", "GIT_EXTERNAL_DIFF",
		"GIT_TRACE", "GIT_TRACE2", "GIT_TRACE_PACKET", "GIT_TRACE2_EVENT",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"PROWL_TEST_UNKNOWN",
	}
	for _, k := range hostile {
		t.Setenv(k, "hostile-"+k)
	}
	// Positive controls: allowlisted names must survive.
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("HOME", "/home/reviewer")
	t.Setenv("LC_ALL", "C")

	seen := envMap(scrubGitEnv(os.Environ()))

	for _, k := range hostile {
		if _, ok := seen[k]; ok {
			t.Errorf("hostile/unknown variable %s survived the allowlist", k)
		}
	}
	if seen["HOME"] != "/home/reviewer" {
		t.Errorf("allowlisted HOME dropped: %q", seen["HOME"])
	}
	if seen["PATH"] == "" {
		t.Errorf("allowlisted PATH dropped")
	}
	if seen["LC_ALL"] != "C" {
		t.Errorf("allowlisted LC_ALL dropped: %q", seen["LC_ALL"])
	}
	want := map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_PAGER":           "cat",
		"PAGER":               "cat",
		"GIT_OPTIONAL_LOCKS":  "0",
		"GIT_NO_LAZY_FETCH":   "1",
	}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("pinned env %s=%q, want %q", k, seen[k], v)
		}
	}
}

// ---- finding 9: per-surface hostile neutralization -----------------------

func TestExecGitNeutralizesHostileSurfaces(t *testing.T) {
	ctx := context.Background()
	g := func(t *testing.T) ExecGit {
		return ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	}
	m := func(markers, name string) string { return filepath.Join(markers, name) }

	t.Run("cleanFilter", func(t *testing.T) {
		root, markers := initRepo(t)
		rawGit(t, root, "config", "filter.evil.clean", "touch '"+m(markers, "clean")+"'; cat")
		rawGit(t, root, "config", "filter.evil.required", "true")
		if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("payload.txt filter=evil\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("l1\nCHANGED\nl3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		clearMarkers(t, markers)
		_, _ = rawGitEnv(root, nil, "diff")
		if !markerPresent(markers, "clean") {
			t.Fatal("control: hostile clean filter did not fire; assertion would be vacuous")
		}
		clearMarkers(t, markers)
		if _, err := g(t).RawStatus(ctx, root, 1<<20, "HEAD"); err != nil {
			t.Fatalf("RawStatus: %v", err)
		}
		if markerPresent(markers, "clean") {
			t.Fatal("sanitized RawStatus ran the clean filter")
		}
	})

	t.Run("externalDiff", func(t *testing.T) {
		root, markers := initRepo(t)
		rawGit(t, root, "config", "diff.external", "touch '"+m(markers, "external")+"'")
		if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("l1\nCHANGED\nl3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		clearMarkers(t, markers)
		_, _ = rawGitEnv(root, nil, "diff", "HEAD")
		if !markerPresent(markers, "external") {
			t.Fatal("control: global external diff did not fire; assertion would be vacuous")
		}
		clearMarkers(t, markers)
		if _, err := g(t).Diff(ctx, root, 1<<20, "HEAD", "--", "payload.txt"); err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if markerPresent(markers, "external") {
			t.Fatal("sanitized Diff ran the external diff command")
		}
	})

	t.Run("diffDriverCommand", func(t *testing.T) {
		root, markers := initRepo(t)
		rawGit(t, root, "config", "diff.evil.command", "touch '"+m(markers, "extdrv")+"'")
		if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("payload.txt diff=evil\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("l1\nCHANGED\nl3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		clearMarkers(t, markers)
		_, _ = rawGitEnv(root, nil, "diff", "HEAD")
		if !markerPresent(markers, "extdrv") {
			t.Fatal("control: per-driver external diff did not fire; assertion would be vacuous")
		}
		clearMarkers(t, markers)
		if _, err := g(t).Diff(ctx, root, 1<<20, "HEAD", "--", "payload.txt"); err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if markerPresent(markers, "extdrv") {
			t.Fatal("sanitized Diff ran the per-driver external diff")
		}
	})

	t.Run("textconv", func(t *testing.T) {
		root, markers := initRepo(t)
		rawGit(t, root, "config", "diff.evil.textconv", "touch '"+m(markers, "textconv")+"'; cat")
		if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("bin.dat diff=evil\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte("bin\x00A\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rawGit(t, root, "add", "bin.dat", ".gitattributes")
		rawGit(t, root, "commit", "-qm", "bin1")
		if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte("bin\x00B\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rawGit(t, root, "add", "bin.dat")
		rawGit(t, root, "commit", "-qm", "bin2")
		clearMarkers(t, markers)
		_, _ = rawGitEnv(root, nil, "show", "HEAD")
		if !markerPresent(markers, "textconv") {
			t.Fatal("control: textconv did not fire; assertion would be vacuous")
		}
		clearMarkers(t, markers)
		if _, err := g(t).Diff(ctx, root, 1<<20, "HEAD~1", "HEAD", "--", "bin.dat"); err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if markerPresent(markers, "textconv") {
			t.Fatal("sanitized Diff ran textconv")
		}
	})

	t.Run("fsmonitor", func(t *testing.T) {
		root, markers := initRepo(t)
		rawGit(t, root, "config", "core.fsmonitor", "touch '"+m(markers, "fsmonitor")+"'; false")
		if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("l1\nCHANGED\nl3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		clearMarkers(t, markers)
		_, _ = rawGitEnv(root, nil, "status")
		if !markerPresent(markers, "fsmonitor") {
			t.Fatal("control: fsmonitor did not fire; assertion would be vacuous")
		}
		clearMarkers(t, markers)
		if _, err := g(t).Output(ctx, root, 1<<20, "status", "--porcelain"); err != nil {
			t.Fatalf("status: %v", err)
		}
		if markerPresent(markers, "fsmonitor") {
			t.Fatal("sanitized status ran the fsmonitor command")
		}
	})

	t.Run("hook", func(t *testing.T) {
		root, markers := initRepo(t)
		hooks := t.TempDir()
		writeExec(t, filepath.Join(hooks, "post-index-change"), "#!/bin/sh\ntouch '"+m(markers, "hook")+"'\nexit 0\n")
		rawGit(t, root, "config", "core.hooksPath", hooks)
		if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("l1\nCHANGED\nl3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		clearMarkers(t, markers)
		_, _ = rawGitEnv(root, nil, "status")
		if !markerPresent(markers, "hook") {
			t.Fatal("control: post-index-change hook did not fire; assertion would be vacuous")
		}
		clearMarkers(t, markers)
		if _, err := g(t).Output(ctx, root, 1<<20, "status", "--porcelain"); err != nil {
			t.Fatalf("status: %v", err)
		}
		if markerPresent(markers, "hook") {
			t.Fatal("sanitized status ran a repository hook")
		}
	})

	t.Run("credentialHelper", func(t *testing.T) {
		root, markers := initRepo(t)
		const desc = "protocol=https\nhost=example.com\n\n"
		hostile := "!touch '" + m(markers, "cred") + "'; true"
		// Control: a hostile credential helper fires when `git credential fill`
		// runs, hermetically (no network peer needed).
		clearMarkers(t, markers)
		ctrl := exec.Command("git", "-C", root, "-c", "credential.helper="+hostile, "credential", "fill")
		ctrl.Stdin = strings.NewReader(desc)
		ctrl.Env = os.Environ()
		_, _ = ctrl.CombinedOutput()
		if !markerPresent(markers, "cred") {
			t.Fatal("control: credential helper did not fire; assertion would be vacuous")
		}
		// Sanitized: the repository helper is emptied by our -c override, so it
		// is never invoked.
		rawGit(t, root, "config", "credential.helper", hostile)
		clearMarkers(t, markers)
		var out bytes.Buffer
		_ = g(t).Pipe(ctx, root, 1<<20, strings.NewReader(desc), &out, "credential", "fill")
		if markerPresent(markers, "cred") {
			t.Fatal("sanitized credential fill invoked the repository credential helper")
		}
	})

	t.Run("extTransportLazyFetchDenied", func(t *testing.T) {
		partial, markers, blob := setupPartialClone(t)
		// Control: allowing ext transport lets a lazy fetch invoke the helper.
		clearMarkers(t, markers)
		ctrl := exec.Command("git", "-C", partial, "-c", "protocol.ext.allow=always", "cat-file", "-p", blob)
		ctrl.Env = os.Environ()
		_, _ = ctrl.CombinedOutput()
		if !markerPresent(markers, "ext") {
			t.Fatal("control: lazy fetch via ext transport did not fire; assertion would be vacuous")
		}
		// Sanitized: GIT_NO_LAZY_FETCH + protocol denies means no fetch at all.
		clearMarkers(t, markers)
		_, _ = g(t).Output(ctx, partial, 1<<20, "cat-file", "-p", blob)
		if markerPresent(markers, "ext") {
			t.Fatalf("sanitized read triggered a promisor/ext lazy fetch (markers: %v)", markerNames(markers))
		}
	})
}

func setupPartialClone(t *testing.T) (partial, markers, blob string) {
	t.Helper()
	gitBin(t)
	base := t.TempDir()
	markers = t.TempDir()
	origin := filepath.Join(base, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	rawGit(t, origin, "init", "-q")
	rawGit(t, origin, "config", "uploadpack.allowFilter", "true")
	if err := os.WriteFile(filepath.Join(origin, "big.txt"), []byte(strings.Repeat("BIGCONTENT-line\n", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, origin, "add", "big.txt")
	rawGit(t, origin, "commit", "-qm", "seed")

	partial = filepath.Join(base, "partial")
	rawGit(t, base, "clone", "-q", "--no-local", "--filter=blob:none", "--no-checkout", "file://"+origin, partial)

	out, err := rawGitEnv(partial, []string{"GIT_NO_LAZY_FETCH=1"}, "rev-list", "--objects", "--all", "--missing=print")
	if err != nil {
		t.Fatalf("rev-list missing: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "?") {
			blob = strings.TrimSpace(line[1:])
			break
		}
	}
	if blob == "" {
		t.Skip("partial clone did not leave a missing blob (transport ignored the filter)")
	}
	helper := filepath.Join(t.TempDir(), "exthelper.sh")
	writeExec(t, helper, "#!/bin/sh\ntouch '"+filepath.Join(markers, "ext")+"'\nexit 1\n")
	rawGit(t, partial, "config", "remote.origin.url", "ext::"+helper)
	return partial, markers, blob
}

// ---- finding 4: canonical diff pinning -----------------------------------

func TestExecGitPinsDiffOptionsAgainstRepoConfig(t *testing.T) {
	gitBin(t)
	ctx := context.Background()
	hostile := t.TempDir()
	clean := t.TempDir()
	rawGit(t, hostile, "init", "-q")
	rawGit(t, hostile, "config", "diff.algorithm", "minimal")
	rawGit(t, hostile, "config", "diff.indentHeuristic", "true")
	rawGit(t, hostile, "config", "diff.interHunkContext", "99")
	rawGit(t, hostile, "config", "diff.renameLimit", "1")

	var base, head bytes.Buffer
	for i := 1; i <= 30; i++ {
		line := fmt.Sprintf("line-%02d\n", i)
		base.WriteString(line)
		switch i {
		case 3:
			head.WriteString("line-3-CHANGED\n")
		case 27:
			head.WriteString("line-27-CHANGED\n")
		default:
			head.WriteString(line)
		}
	}
	dir := t.TempDir()
	baseFile := filepath.Join(dir, "base")
	headFile := filepath.Join(dir, "head")
	if err := os.WriteFile(baseFile, base.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headFile, head.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	g := ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	fromHostile, err := g.DiffNoIndex(ctx, hostile, 1<<20, baseFile, headFile)
	if err != nil {
		t.Fatalf("hostile diff: %v", err)
	}
	fromClean, err := g.DiffNoIndex(ctx, clean, 1<<20, baseFile, headFile)
	if err != nil {
		t.Fatalf("clean diff: %v", err)
	}
	if !bytes.Equal(fromHostile, fromClean) {
		t.Fatalf("canonical diff differed by repo config:\n--hostile--\n%s\n--clean--\n%s", fromHostile, fromClean)
	}

	rawDiff := func(cwd string) []byte {
		cmd := exec.Command("git", "diff", "--no-index", "--text", baseFile, headFile)
		cmd.Dir = cwd
		cmd.Env = os.Environ()
		out, _ := cmd.Output()
		return out
	}
	if bytes.Equal(rawDiff(hostile), rawDiff(clean)) {
		t.Fatal("control: unpinned diff did not diverge; pin comparison would be vacuous")
	}
}

// ---- finding 2: driver discovery must not fail open ----------------------

func TestExecGitRefusesOnDriverDiscoveryFailure(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		mode    string
		timeout time.Duration
	}{
		{"overflow", "overflow", 30 * time.Second},
		{"timeout", "hang", 300 * time.Millisecond},
		{"error", "error", 30 * time.Second},
		{"malformed", "malformed", 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := ExecGit{Binary: fakeGitConfig(t, tc.mode), Timeout: tc.timeout, MaxStderr: 1 << 20}
			if _, err := g.Output(ctx, t.TempDir(), 1<<20, "cat-file", "-p", "HEAD"); !errors.Is(err, ErrDriverDiscovery) {
				t.Fatalf("Output err=%v, want ErrDriverDiscovery", err)
			}
		})
	}
}

// ---- finding 1 & 5: process-tree termination and bounds ------------------

func TestExecGitOutputOverflowKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGitProc(t, "stdout", childPID), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	if _, err := g.Output(context.Background(), t.TempDir(), 64, "spew"); !errors.Is(err, ErrGitOutputOverflow) {
		t.Fatalf("err=%v, want ErrGitOutputOverflow", err)
	}
	requireProcessDead(t, readPID(t, childPID))
}

func TestExecGitStderrOverflowKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGitProc(t, "stderr", childPID), Timeout: 30 * time.Second, MaxStderr: 64}
	if _, err := g.Output(context.Background(), t.TempDir(), 1<<20, "noisy"); !errors.Is(err, ErrGitStderrOverflow) {
		t.Fatalf("err=%v, want ErrGitStderrOverflow", err)
	}
	requireProcessDead(t, readPID(t, childPID))
}

func TestExecGitPipeOverflowKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGitProc(t, "stdout", childPID), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	if err := g.Pipe(context.Background(), t.TempDir(), 64, nil, io.Discard, "spew"); !errors.Is(err, ErrGitOutputOverflow) {
		t.Fatalf("err=%v, want ErrGitOutputOverflow", err)
	}
	requireProcessDead(t, readPID(t, childPID))
}

func TestExecGitTimeoutKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGitProc(t, "hang", childPID), Timeout: 300 * time.Millisecond, MaxStderr: 1 << 20}
	if _, err := g.Output(context.Background(), t.TempDir(), 1<<20, "hang"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded", err)
	}
	requireProcessDead(t, readPID(t, childPID))
}

func TestExecGitCancellationKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGitProc(t, "hang", childPID), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if _, err := os.Stat(childPID); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	if _, err := g.Output(ctx, t.TempDir(), 1<<20, "hang"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	requireProcessDead(t, readPID(t, childPID))
}

func TestExecGitRejectsNonPositiveLimits(t *testing.T) {
	g := ExecGit{HooksDir: t.TempDir()}
	ctx := context.Background()
	for _, limit := range []int64{0, -1} {
		if _, err := g.Output(ctx, t.TempDir(), limit, "status"); !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("Output limit=%d err=%v, want ErrInvalidLimit", limit, err)
		}
		if err := g.Pipe(ctx, t.TempDir(), limit, nil, io.Discard, "status"); !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("Pipe limit=%d err=%v, want ErrInvalidLimit", limit, err)
		}
	}
}

// ---- finding 3: generic runner rejects patch subcommands & unsafe opts ----

func TestExecGitOutputRejectsPatchSubcommands(t *testing.T) {
	g := ExecGit{HooksDir: t.TempDir()}
	ctx := context.Background()
	for _, args := range [][]string{
		{"diff", "HEAD"},
		{"show", "HEAD"},
		{"diff-tree", "-r", "HEAD"},
		{"format-patch", "-1"},
		{"log", "-p"},
		{"log", "--patch"},
	} {
		if _, err := g.Output(ctx, t.TempDir(), 1<<20, args...); !errors.Is(err, ErrPatchViaGenericRunner) {
			t.Errorf("Output %v err=%v, want ErrPatchViaGenericRunner", args, err)
		}
		if err := g.Pipe(ctx, t.TempDir(), 1<<20, nil, io.Discard, args...); !errors.Is(err, ErrPatchViaGenericRunner) {
			t.Errorf("Pipe %v err=%v, want ErrPatchViaGenericRunner", args, err)
		}
	}
	// A non-patch subcommand is accepted by the guard (reaches discovery).
	if err := rejectPatchSubcommand([]string{"status", "--porcelain"}); err != nil {
		t.Errorf("status rejected: %v", err)
	}
}

func TestExecGitDiffHelpersRejectOverridingOptions(t *testing.T) {
	g := ExecGit{HooksDir: t.TempDir()}
	ctx := context.Background()
	unsafe := [][]string{
		{"--textconv", "HEAD"},
		{"--ext-diff"},
		{"-U10", "HEAD"},
		{"--unified=9"},
		{"--inter-hunk-context=5"},
		{"-M10%"},
		{"-C"},
		{"--find-copies-harder"},
		{"--no-renames"},
		{"--diff-algorithm=minimal"},
		{"--src-prefix=x/"},
		{"--color"},
	}
	for _, args := range unsafe {
		if _, err := g.Diff(ctx, t.TempDir(), 1<<20, args...); !errors.Is(err, ErrUnsafeDiffOption) {
			t.Errorf("Diff %v err=%v, want ErrUnsafeDiffOption", args, err)
		}
		if _, err := g.RawStatus(ctx, t.TempDir(), 1<<20, args...); !errors.Is(err, ErrUnsafeDiffOption) {
			t.Errorf("RawStatus %v err=%v, want ErrUnsafeDiffOption", args, err)
		}
	}
	// Pathspecs after -- that look like flags are operands, not options.
	if err := rejectUnsafeDiffArgs([]string{"HEAD", "--", "--weird-name.txt"}); err != nil {
		t.Errorf("pathspec after -- rejected: %v", err)
	}
}

func TestExecGitRawStatusPinsRenamesNoCopies(t *testing.T) {
	gitBin(t)
	ctx := context.Background()
	root, _ := initRepo(t)
	// Hostile rename/copy config that the helper must override.
	rawGit(t, root, "config", "diff.renames", "copies")
	rawGit(t, root, "config", "diff.renameLimit", "1")
	// Create a rename (content preserved) and a copy (duplicate content).
	body := []byte("alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\n")
	if err := os.WriteFile(filepath.Join(root, "orig.txt"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, root, "add", "orig.txt")
	rawGit(t, root, "commit", "-qm", "orig")
	rawGit(t, root, "mv", "orig.txt", "renamed.txt")
	if err := os.WriteFile(filepath.Join(root, "copy.txt"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, root, "add", "copy.txt")

	g := ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	out, err := g.RawStatus(ctx, root, 1<<20, "HEAD")
	if err != nil {
		t.Fatalf("RawStatus: %v", err)
	}
	changes, err := ParseRawStatusZ(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, c := range changes {
		if strings.HasPrefix(c.Status, "C") {
			t.Fatalf("copy detection was not disabled: %+v", c)
		}
	}
	// Rename detection at 50% must still classify the moved content as a rename
	// (git may pair the deletion with either identical file); copy detection
	// stays off, so no record is a copy.
	sawRename := false
	for _, c := range changes {
		if strings.HasPrefix(c.Status, "R") {
			sawRename = true
		}
	}
	if !sawRename {
		t.Fatalf("expected a rename (renames pinned on); got %+v", changes)
	}
}

// ---- finding 4: bounded Pipe short writes --------------------------------

// shortWriter accepts at most max bytes total, short-writing the chunk that
// crosses the cap (returning n < len(p) with a nil error).
type shortWriter struct {
	max int
	n   int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.n >= w.max {
		return 0, nil
	}
	room := w.max - w.n
	if len(p) > room {
		w.n = w.max
		return room, nil
	}
	w.n += len(p)
	return len(p), nil
}

func TestExecGitPipeShortWriteKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGitProc(t, "stdout", childPID), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	sw := &shortWriter{max: 100}
	err := g.Pipe(context.Background(), t.TempDir(), 1<<20, nil, sw, "spew")
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err=%v, want io.ErrShortWrite", err)
	}
	if sw.n > sw.max {
		t.Fatalf("delivered %d bytes, exceeds cap %d (over-accounted)", sw.n, sw.max)
	}
	requireProcessDead(t, readPID(t, childPID))
}

// ---- no-index exit handling & streaming ----------------------------------

func TestExecGitDiffNoIndexTreatsExitOneAsDifference(t *testing.T) {
	gitBin(t)
	ctx := context.Background()
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	same := filepath.Join(dir, "same")
	for _, f := range []struct {
		path, content string
	}{{a, "one\ntwo\n"}, {b, "one\nTWO\n"}, {same, "one\ntwo\n"}} {
		if err := os.WriteFile(f.path, []byte(f.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	g := ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}

	out, err := g.DiffNoIndex(ctx, dir, 1<<20, a, b)
	if err != nil {
		t.Fatalf("differ: %v", err)
	}
	if !bytes.Contains(out, []byte("-two")) || !bytes.Contains(out, []byte("+TWO")) {
		t.Fatalf("differ: diff payload missing:\n%s", out)
	}
	out, err = g.DiffNoIndex(ctx, dir, 1<<20, a, same)
	if err != nil {
		t.Fatalf("identical: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("identical: want empty diff, got %q", out)
	}
	failing := ExecGit{Binary: fakeGitExit(t, 2), HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	if _, err := failing.DiffNoIndex(ctx, dir, 1<<20, a, b); err == nil {
		t.Fatal("exit 2 must surface as an error, not a difference")
	}
}

func TestExecGitPipeStreamsCatFileBatch(t *testing.T) {
	gitBin(t)
	ctx := context.Background()
	root, _ := initRepo(t)
	g := ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	var out bytes.Buffer
	if err := g.Pipe(ctx, root, 1<<20, strings.NewReader("HEAD:payload.txt\n"), &out, "cat-file", "--batch"); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	objs, err := ParseCatFileBatch(out.Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(objs) != 1 || objs[0].Missing || objs[0].Type != "blob" || !bytes.Equal(objs[0].Data, []byte("l1\nl2\nl3\n")) {
		t.Fatalf("unexpected cat-file record: %+v", objs)
	}
}

// ---- finding 8: parser framing & overflow --------------------------------

func TestExecGitParseRawStatus(t *testing.T) {
	valid := []byte(":100644 100644 aaaa bbbb M\x00f1.txt\x00" +
		":100644 100644 cccc dddd R100\x00old.txt\x00new.txt\x00")
	changes, err := ParseRawStatusZ(valid)
	if err != nil {
		t.Fatalf("valid parse: %v", err)
	}
	if len(changes) != 2 || changes[0].Path != "f1.txt" || changes[1].Status != "R100" ||
		changes[1].OldPath != "old.txt" || changes[1].Path != "new.txt" {
		t.Fatalf("unexpected records: %+v", changes)
	}

	// Missing terminal NUL is malformed framing.
	if got, err := ParseRawStatusZ([]byte(":100644 100644 a b M\x00f1.txt")); !errors.Is(err, ErrMalformedRawStatus) || got != nil {
		t.Fatalf("missing terminal NUL err=%v got=%v", err, got)
	}
	// Truncated record fed through a fake runner.
	r := fakeRunner{out: []byte(":100644 100644 a b M\x00")}
	out, _ := r.Output(context.Background(), "", 1<<20, "diff", "--raw", "-z")
	if got, err := ParseRawStatusZ(out); !errors.Is(err, ErrMalformedRawStatus) || got != nil {
		t.Fatalf("truncated err=%v got=%v", err, got)
	}
	// Missing leading colon.
	if _, err := ParseRawStatusZ([]byte("100644 100644 a b M\x00f1\x00")); !errors.Is(err, ErrMalformedRawStatus) {
		t.Fatalf("missing colon err=%v", err)
	}
}

func TestExecGitParseUnifiedDiff(t *testing.T) {
	valid := []byte("diff --git a/f b/f\nindex 1..2 100644\n--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n")
	ud, err := ParseUnifiedDiff(valid)
	if err != nil {
		t.Fatalf("valid parse: %v", err)
	}
	if ud.Additions != 1 || ud.Deletions != 1 || len(ud.Hunks) != 1 {
		t.Fatalf("unexpected result: +%d/-%d hunks=%d", ud.Additions, ud.Deletions, len(ud.Hunks))
	}

	// Bad hunk header.
	if _, err := ParseUnifiedDiff([]byte("@@ not a header @@\n+x\n")); !errors.Is(err, ErrMalformedUnifiedDiff) {
		t.Fatalf("bad header err=%v", err)
	}
	// Declared range not satisfied (says 3 old lines, provides 1).
	if _, err := ParseUnifiedDiff([]byte("@@ -1,3 +1,1 @@\n-only\n")); !errors.Is(err, ErrMalformedUnifiedDiff) {
		t.Fatalf("range mismatch err=%v", err)
	}
	// Empty unprefixed payload line inside a hunk.
	if _, err := ParseUnifiedDiff([]byte("@@ -1,2 +1,2 @@\n one\n\n two\n")); !errors.Is(err, ErrMalformedUnifiedDiff) {
		t.Fatalf("empty payload err=%v", err)
	}
}

func TestExecGitParseCatFileBatch(t *testing.T) {
	objs, err := ParseCatFileBatch([]byte("aaaa blob 6\nhello\n\nbbbb missing\n"))
	if err != nil {
		t.Fatalf("valid parse: %v", err)
	}
	if len(objs) != 2 || objs[0].Type != "blob" || objs[0].Size != 6 ||
		!bytes.Equal(objs[0].Data, []byte("hello\n")) || !objs[1].Missing || objs[1].OID != "bbbb" {
		t.Fatalf("unexpected records: %+v", objs)
	}

	// Non-numeric size.
	if got, err := ParseCatFileBatch([]byte("aaaa blob notanumber\nhello\n\n")); !errors.Is(err, ErrMalformedCatFile) || got != nil {
		t.Fatalf("bad size err=%v got=%v", err, got)
	}
	// Overflowing declared size must be rejected before any size+1 arithmetic.
	if _, err := ParseCatFileBatch([]byte("aaaa blob 9223372036854775807\nhi\n")); !errors.Is(err, ErrMalformedCatFile) {
		t.Fatalf("overflow size err=%v", err)
	}
	// Size larger than int64 range.
	if _, err := ParseCatFileBatch([]byte("aaaa blob 99999999999999999999999999\nhi\n")); !errors.Is(err, ErrMalformedCatFile) {
		t.Fatalf("huge size err=%v", err)
	}
	// Truncated payload.
	if _, err := ParseCatFileBatch([]byte("aaaa blob 99\nhi\n")); !errors.Is(err, ErrMalformedCatFile) {
		t.Fatalf("truncated err=%v", err)
	}
}
