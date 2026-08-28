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

func gitBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available: %v", err)
	}
	return bin
}

func rawGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
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

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeGit writes a POSIX shell stand-in for git that spawns a SIGTERM-ignoring
// background child (recording its PID synchronously), performs the requested
// misbehaviour, then lingers. The child shares the process group so a correct
// group kill must terminate it too. The filter enumeration probe exits early so
// the harness's pre-flight config read is a no-op.
func fakeGit(t *testing.T, mode, childPIDPath string) string {
	t.Helper()
	var action string
	switch mode {
	case "stdout":
		action = "i=0\nwhile [ $i -lt 2000 ]; do printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\\n'; i=$((i+1)); done\n"
	case "stderr":
		action = "i=0\nwhile [ $i -lt 2000 ]; do printf 'EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE\\n' 1>&2; i=$((i+1)); done\n"
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
		if data, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(data)) > 0 {
			pid, perr := strconv.Atoi(string(bytes.TrimSpace(data)))
			if perr == nil {
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
	t.Fatalf("process %d in the group was not killed", pid)
}

type fakeRunner struct {
	out []byte
	err error
}

func (f fakeRunner) Output(_ context.Context, _ string, _ int64, _ ...string) ([]byte, error) {
	return f.out, f.err
}

func (f fakeRunner) Pipe(_ context.Context, _ string, _ io.Reader, _ io.Writer, _ ...string) error {
	return f.err
}

// ---- environment sanitization -------------------------------------------

func TestExecGitSanitizesInheritedEnvironment(t *testing.T) {
	hostile := map[string]string{
		"GIT_CONFIG_COUNT":                 "1",
		"GIT_CONFIG_KEY_0":                 "core.fsmonitor",
		"GIT_CONFIG_VALUE_0":               "/bin/evil",
		"GIT_CONFIG_PARAMETERS":            "'core.pager=evil'",
		"GIT_DIR":                          "/tmp/evil.git",
		"GIT_WORK_TREE":                    "/tmp/evil",
		"GIT_INDEX_FILE":                   "/tmp/evil.index",
		"GIT_OBJECT_DIRECTORY":             "/tmp/evilobj",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": "/tmp/alt",
		"GIT_ASKPASS":                      "/bin/evil",
		"SSH_ASKPASS":                      "/bin/evil",
		"GIT_SSH":                          "/bin/evil",
		"GIT_SSH_COMMAND":                  "/bin/evil",
		"GIT_PROXY_COMMAND":                "/bin/evil",
		"GIT_ALLOW_PROTOCOL":               "ext",
		"GIT_PROTOCOL_FROM_USER":           "1",
		"GIT_EXTERNAL_DIFF":                "/bin/evil",
	}
	for k, v := range hostile {
		t.Setenv(k, v)
	}
	env := scrubGitEnv(os.Environ())

	seen := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		seen[name] = value
	}
	for k := range hostile {
		if _, ok := seen[k]; ok {
			t.Errorf("hostile variable %s survived sanitization", k)
		}
	}
	for name, prefix := range map[string]string{"GIT_CONFIG_KEY_0": "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_0": "GIT_CONFIG_VALUE_"} {
		_ = name
		for k := range seen {
			if strings.HasPrefix(k, prefix) {
				t.Errorf("prefixed hostile variable %s survived", k)
			}
		}
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
			t.Errorf("sanitized env %s=%q, want %q", k, seen[k], v)
		}
	}
}

// ---- hostile helper/config neutralization --------------------------------

func setupHostileRepo(t *testing.T) (root, markers, hooks string) {
	t.Helper()
	gitBin(t)
	root = t.TempDir()
	markers = t.TempDir()
	hooks = t.TempDir()

	rawGit(t, root, "init", "-q")
	m := func(name string) string { return filepath.Join(markers, name) }

	// Commit a clean baseline BEFORE any hostile attribute or filter exists so
	// building the fixture never invokes the very machinery under test.
	if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, root, "add", "payload.txt")
	rawGit(t, root, "commit", "-qm", "seed")

	// Hostile repository-local config across every neutralized surface. A dumb
	// touch "process" filter with required=true makes an unsanitized clean
	// fatal, so our -c overrides must both empty it and force required=false.
	set := func(k, v string) { rawGit(t, root, "config", "--local", k, v) }
	set("filter.evil.clean", "touch '"+m("clean")+"'; cat")
	set("filter.evil.smudge", "touch '"+m("smudge")+"'; cat")
	set("filter.evil.process", "touch '"+m("process")+"'")
	set("filter.evil.required", "true")
	set("diff.external", "touch '"+m("external")+"'")
	set("diff.evil.command", "touch '"+m("extdrv")+"'")
	set("diff.evil.textconv", "touch '"+m("textconv")+"'; cat")
	set("core.fsmonitor", "touch '"+m("fsmonitor")+"'")
	set("core.pager", "touch '"+m("pager")+"'; cat")
	set("core.hooksPath", hooks)
	set("credential.helper", "!touch '"+m("credential")+"'; true")
	set("diff.algorithm", "minimal")
	set("diff.indentHeuristic", "true")
	set("diff.interHunkContext", "99")
	set("diff.renameLimit", "1")
	rawGit(t, root, "config", "--local", "remote.evil.url", "ext::sh -c touch% '"+m("ext")+"'")

	// Hostile hooks a read-only refresh might fire.
	for _, hook := range []string{"post-index-change", "reference-transaction", "fsmonitor-watchman"} {
		writeExec(t, filepath.Join(hooks, hook), "#!/bin/sh\ntouch '"+m("hook_"+hook)+"'\nexit 0\n")
	}

	// Route every path through the evil filter and diff driver. The attributes
	// file stays untracked so it is honored without re-cleaning tracked content.
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("* filter=evil diff=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Dirty the worktree so status/diff must clean the tracked file.
	if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("alpha\nBETA\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, markers, hooks
}

func TestExecGitNeutralizesHostileHelpers(t *testing.T) {
	root, markers, _ := setupHostileRepo(t)
	ctx := context.Background()

	// Control: an unsanitized git run must actually fire a hostile helper, or
	// the neutralization assertions below would be vacuous. A configured process
	// filter supersedes clean/smudge, so assert that *some* marker fired.
	clearMarkers(t, markers)
	ctrl := exec.Command("git", "-C", root, "diff")
	ctrl.Env = os.Environ()
	_, _ = ctrl.CombinedOutput()
	if entries, _ := os.ReadDir(markers); len(entries) == 0 {
		t.Fatalf("control: no hostile helper fired; neutralization test would be vacuous")
	}

	g := ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	assertClean := func(label string) {
		entries, _ := os.ReadDir(markers)
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("%s: hostile markers fired: %v", label, names)
		}
	}

	clearMarkers(t, markers)
	if _, err := g.Output(ctx, root, 1<<20, "diff", "--raw", "-z", "HEAD"); err != nil {
		t.Fatalf("raw status: %v", err)
	}
	assertClean("raw status")

	clearMarkers(t, markers)
	if _, err := g.Output(ctx, root, 1<<20, "cat-file", "-p", "HEAD:payload.txt"); err != nil {
		t.Fatalf("object read: %v", err)
	}
	assertClean("object read")

	clearMarkers(t, markers)
	if _, err := g.Output(ctx, root, 1<<20, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "HEAD", "--", "payload.txt"); err != nil {
		t.Fatalf("content diff: %v", err)
	}
	assertClean("content diff")
}

func TestExecGitPinsDiffOptionsAgainstRepoConfig(t *testing.T) {
	root, _, _ := setupHostileRepo(t)
	clean := t.TempDir()
	gitBin(t)
	ctx := context.Background()

	// Two changes far enough apart to be distinct hunks at 3 lines of context
	// but merged into one hunk if interHunkContext is inflated to 99.
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
	fromHostile, err := g.DiffNoIndex(ctx, root, 1<<20, baseFile, headFile)
	if err != nil {
		t.Fatalf("hostile diff: %v", err)
	}
	fromClean, err := g.DiffNoIndex(ctx, clean, 1<<20, baseFile, headFile)
	if err != nil {
		t.Fatalf("clean diff: %v", err)
	}
	if !bytes.Equal(fromHostile, fromClean) {
		t.Fatalf("canonical diff differed by repository config:\n--hostile--\n%s\n--clean--\n%s", fromHostile, fromClean)
	}

	// Control: without pinning, the hostile interHunkContext=99 collapses the
	// two changes into a single hunk, so raw git output diverges. This proves
	// the pin is load-bearing rather than incidental.
	rawDiff := func(cwd string) []byte {
		cmd := exec.Command("git", "diff", "--no-index", "--text", baseFile, headFile)
		cmd.Dir = cwd
		cmd.Env = os.Environ()
		out, _ := cmd.Output()
		return out
	}
	if bytes.Equal(rawDiff(root), rawDiff(clean)) {
		t.Fatalf("control: unpinned diff did not diverge; pin comparison would be vacuous")
	}
}

// ---- process-group termination -------------------------------------------

func TestExecGitOutputOverflowKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGit(t, "stdout", childPID), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	_, err := g.Output(context.Background(), t.TempDir(), 64, "spew")
	if !errors.Is(err, ErrGitOutputOverflow) {
		t.Fatalf("err=%v, want ErrGitOutputOverflow", err)
	}
	requireProcessDead(t, readPID(t, childPID))
}

func TestExecGitStderrOverflowKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGit(t, "stderr", childPID), Timeout: 30 * time.Second, MaxStderr: 64}
	_, err := g.Output(context.Background(), t.TempDir(), 1<<20, "noisy")
	if !errors.Is(err, ErrGitStderrOverflow) {
		t.Fatalf("err=%v, want ErrGitStderrOverflow", err)
	}
	requireProcessDead(t, readPID(t, childPID))
}

func TestExecGitTimeoutKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGit(t, "hang", childPID), Timeout: 300 * time.Millisecond, MaxStderr: 1 << 20}
	_, err := g.Output(context.Background(), t.TempDir(), 1<<20, "hang")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded", err)
	}
	requireProcessDead(t, readPID(t, childPID))
}

func TestExecGitCancellationKillsProcessGroup(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	g := ExecGit{Binary: fakeGit(t, "hang", childPID), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Give the child time to record its PID, then cancel.
		for {
			if _, err := os.Stat(childPID); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	_, err := g.Output(ctx, t.TempDir(), 1<<20, "hang")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	requireProcessDead(t, readPID(t, childPID))
}

// ---- no-index exit handling ----------------------------------------------

func TestExecGitDiffNoIndexTreatsExitOneAsDifference(t *testing.T) {
	gitBin(t)
	ctx := context.Background()
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	same := filepath.Join(dir, "same")
	if err := os.WriteFile(a, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("one\nTWO\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(same, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}

	out, err := g.DiffNoIndex(ctx, dir, 1<<20, a, b)
	if err != nil {
		t.Fatalf("differ: unexpected err %v", err)
	}
	if !bytes.Contains(out, []byte("-two")) || !bytes.Contains(out, []byte("+TWO")) {
		t.Fatalf("differ: diff payload missing:\n%s", out)
	}

	out, err = g.DiffNoIndex(ctx, dir, 1<<20, a, same)
	if err != nil {
		t.Fatalf("identical: unexpected err %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("identical: want empty diff, got %q", out)
	}

	failing := ExecGit{Binary: fakeGitExit(t, 2), HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	if _, err := failing.DiffNoIndex(ctx, dir, 1<<20, a, b); err == nil {
		t.Fatalf("exit 2 must surface as an error, not a difference")
	}
}

// ---- streaming pipe -------------------------------------------------------

func TestExecGitPipeStreamsCatFileBatch(t *testing.T) {
	gitBin(t)
	ctx := context.Background()
	root := t.TempDir()
	rawGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, root, "add", ".")
	rawGit(t, root, "commit", "-qm", "seed")

	g := ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	var out bytes.Buffer
	if err := g.Pipe(ctx, root, strings.NewReader("HEAD:f.txt\n"), &out, "cat-file", "--batch"); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	objs, err := ParseCatFileBatch(out.Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(objs) != 1 || objs[0].Missing || objs[0].Type != "blob" || !bytes.Equal(objs[0].Data, []byte("hello\n")) {
		t.Fatalf("unexpected cat-file record: %+v", objs)
	}
}

// ---- parser typed-error contracts ----------------------------------------

func TestExecGitParseRawStatus(t *testing.T) {
	valid := []byte(":100644 100644 aaaa bbbb M\x00f1.txt\x00" +
		":100644 100644 cccc dddd R100\x00old.txt\x00new.txt\x00")
	changes, err := ParseRawStatusZ(valid)
	if err != nil {
		t.Fatalf("valid parse: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("want 2 changes, got %d: %+v", len(changes), changes)
	}
	if changes[0].Status != "M" || changes[0].Path != "f1.txt" {
		t.Fatalf("record 0 wrong: %+v", changes[0])
	}
	if changes[1].Status != "R100" || changes[1].OldPath != "old.txt" || changes[1].Path != "new.txt" {
		t.Fatalf("record 1 wrong: %+v", changes[1])
	}

	// Fed through a fake runner: a truncated record (missing its path) must be a
	// typed error, never a partial slice.
	r := fakeRunner{out: []byte(":100644 100644 aaaa bbbb M\x00")}
	out, _ := r.Output(context.Background(), "", 1<<20, "diff", "--raw", "-z")
	got, err := ParseRawStatusZ(out)
	if !errors.Is(err, ErrMalformedRawStatus) {
		t.Fatalf("err=%v, want ErrMalformedRawStatus", err)
	}
	if got != nil {
		t.Fatalf("malformed parse returned partial records: %+v", got)
	}

	bad := fakeRunner{out: []byte("100644 100644 aaaa bbbb M\x00f1\x00")} // missing leading ':'
	out, _ = bad.Output(context.Background(), "", 1<<20)
	if _, err := ParseRawStatusZ(out); !errors.Is(err, ErrMalformedRawStatus) {
		t.Fatalf("missing colon err=%v, want ErrMalformedRawStatus", err)
	}
}

func TestExecGitParseUnifiedDiff(t *testing.T) {
	valid := []byte("diff --git a/f b/f\n" +
		"index 111..222 100644\n" +
		"--- a/f\n+++ b/f\n" +
		"@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n")
	ud, err := ParseUnifiedDiff(valid)
	if err != nil {
		t.Fatalf("valid parse: %v", err)
	}
	if ud.Additions != 1 || ud.Deletions != 1 {
		t.Fatalf("want +1/-1, got +%d/-%d", ud.Additions, ud.Deletions)
	}
	if len(ud.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(ud.Hunks))
	}

	r := fakeRunner{out: []byte("@@ this is not a hunk header @@\n+x\n")}
	out, _ := r.Output(context.Background(), "", 1<<20)
	if _, err := ParseUnifiedDiff(out); !errors.Is(err, ErrMalformedUnifiedDiff) {
		t.Fatalf("err=%v, want ErrMalformedUnifiedDiff", err)
	}
}

func TestExecGitParseCatFileBatch(t *testing.T) {
	valid := []byte("aaaa blob 6\nhello\n\n" + "bbbb missing\n")
	objs, err := ParseCatFileBatch(valid)
	if err != nil {
		t.Fatalf("valid parse: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 objects, got %d", len(objs))
	}
	if objs[0].Type != "blob" || objs[0].Size != 6 || !bytes.Equal(objs[0].Data, []byte("hello\n")) {
		t.Fatalf("object 0 wrong: %+v", objs[0])
	}
	if !objs[1].Missing || objs[1].OID != "bbbb" {
		t.Fatalf("object 1 wrong: %+v", objs[1])
	}

	// Non-numeric size.
	r := fakeRunner{out: []byte("aaaa blob notanumber\nhello\n\n")}
	out, _ := r.Output(context.Background(), "", 1<<20)
	if got, err := ParseCatFileBatch(out); !errors.Is(err, ErrMalformedCatFile) || got != nil {
		t.Fatalf("bad size err=%v got=%v, want ErrMalformedCatFile", err, got)
	}

	// Content shorter than the declared size.
	r = fakeRunner{out: []byte("aaaa blob 99\nhi\n")}
	out, _ = r.Output(context.Background(), "", 1<<20)
	if _, err := ParseCatFileBatch(out); !errors.Is(err, ErrMalformedCatFile) {
		t.Fatalf("truncated err=%v, want ErrMalformedCatFile", err)
	}
}
