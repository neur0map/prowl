package review

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultMaxStderrBytes bounds captured stderr when ExecGit.MaxStderr is unset.
	defaultMaxStderrBytes = 1 << 20
	// maxDriverListBytes bounds the filter/diff-driver discovery output.
	maxDriverListBytes = 1 << 20
)

var (
	// ErrGitOutputOverflow reports that a command's stdout exceeded its limit.
	ErrGitOutputOverflow = errors.New("review: git stdout exceeded its byte limit")
	// ErrGitStderrOverflow reports that a command's stderr exceeded its limit.
	ErrGitStderrOverflow = errors.New("review: git stderr exceeded its byte limit")
	// ErrInvalidLimit reports a non-positive output limit.
	ErrInvalidLimit = errors.New("review: output byte limit must be positive")
	// ErrPatchViaGenericRunner reports that a patch-producing subcommand was
	// passed to the generic Output/Pipe runners instead of a dedicated,
	// pinned diff helper.
	ErrPatchViaGenericRunner = errors.New("review: patch-producing subcommand requires a dedicated diff helper")
	// ErrUnsafeDiffOption reports that a caller supplied a diff option that would
	// override a pinned canonical/safety control.
	ErrUnsafeDiffOption = errors.New("review: caller diff option overrides a pinned control")
	// ErrDriverDiscovery reports that filter/diff-driver enumeration failed, so
	// the runner refuses the main command rather than run with an unknown or
	// partial set of hostile drivers.
	ErrDriverDiscovery = errors.New("review: git filter/diff-driver discovery failed")
	// ErrMalformedRawStatus reports unparseable NUL-delimited raw diff status.
	ErrMalformedRawStatus = errors.New("review: malformed raw diff status")
	// ErrMalformedUnifiedDiff reports an unparseable unified diff.
	ErrMalformedUnifiedDiff = errors.New("review: malformed unified diff")
	// ErrMalformedCatFile reports unparseable cat-file --batch output.
	ErrMalformedCatFile = errors.New("review: malformed cat-file batch output")
)

// GitExitError reports that a Git subprocess ran to completion but exited with
// a non-zero status. It carries the exit code so callers can distinguish a
// meaningful status - such as merge-base's exit 1 for "no merge base" - from
// any other failure, while still comparing with errors.As against the concrete
// type. Overflow, cancellation, and launch failures never produce this error;
// they surface as their own typed errors from run.
type GitExitError struct {
	Subcommand string
	Code       int
	Stderr     string
}

func (e *GitExitError) Error() string {
	return fmt.Sprintf("review: git %s exited with status %d: %s", e.Subcommand, e.Code, e.Stderr)
}

// newGitExitError builds a GitExitError from a completed run's arguments, exit
// code, and captured stderr. It is the single source of the non-zero-exit error
// shared by Output, Pipe, and diffCapture.
func newGitExitError(args []string, code int, stderr []byte) *GitExitError {
	return &GitExitError{Subcommand: firstArg(args), Code: code, Stderr: strings.TrimSpace(string(stderr))}
}

// GitRunner runs sanitized, bounded Git subprocesses. Output captures stdout up
// to a positive byte limit; Pipe streams stdin and stdout for object protocols
// such as cat-file --batch, bounding stdout to a positive limit. Both enforce
// the sanitized execution policy, a fixed timeout, and process-tree termination.
type GitRunner interface {
	Output(ctx context.Context, root string, limit int64, args ...string) ([]byte, error)
	Pipe(ctx context.Context, root string, limit int64, stdin io.Reader, stdout io.Writer, args ...string) error
}

// ExecGit is the process-backed GitRunner. Every invocation runs with an
// allowlisted environment, an allowlisted -c configuration, no shell, an
// explicit working directory, bounded output, and platform process-tree
// termination so a hung or overflowing child (and any descendant it spawned) is
// killed and reaped.
type ExecGit struct {
	// Binary is the git executable; empty means "git" on PATH.
	Binary string
	// Timeout bounds each invocation; zero disables the deadline.
	Timeout time.Duration
	// HooksDir is the empty directory pinned as core.hooksPath; empty defaults
	// to os.DevNull so no repository hook can run.
	HooksDir string
	// MaxStderr bounds captured stderr; zero uses defaultMaxStderrBytes.
	MaxStderr int64
}

var _ GitRunner = ExecGit{}

// processController abstracts platform process-tree setup and termination.
type processController interface {
	prepare(cmd *exec.Cmd) error
	started(cmd *exec.Cmd) error
	kill()
	release()
}

func (g ExecGit) binary() string {
	if g.Binary != "" {
		return g.Binary
	}
	return "git"
}

func (g ExecGit) maxStderr() int64 {
	if g.MaxStderr > 0 {
		return g.MaxStderr
	}
	return defaultMaxStderrBytes
}

// Output runs git and returns stdout. It rejects patch-producing subcommands
// (which must use a dedicated diff helper), a non-positive limit, and surfaces
// ErrGitOutputOverflow, ErrDriverDiscovery, or a stderr-bearing exit error.
func (g ExecGit) Output(ctx context.Context, root string, limit int64, args ...string) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrInvalidLimit
	}
	if err := rejectPatchSubcommand(args); err != nil {
		return nil, err
	}
	config, err := g.safeConfig(ctx, root)
	if err != nil {
		return nil, err
	}
	stdout := &boundedBuffer{limit: limit}
	code, stderr, err := g.run(ctx, root, config, nil, stdout, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, newGitExitError(args, code, stderr)
	}
	return stdout.bytes(), nil
}

// Pipe runs git streaming stdin to the child and the child's stdout to stdout,
// bounding stdout to a positive limit.
func (g ExecGit) Pipe(ctx context.Context, root string, limit int64, stdin io.Reader, stdout io.Writer, args ...string) error {
	if limit <= 0 {
		return ErrInvalidLimit
	}
	if err := rejectPatchSubcommand(args); err != nil {
		return err
	}
	config, err := g.safeConfig(ctx, root)
	if err != nil {
		return err
	}
	sink := &boundedWriter{dest: stdout, limit: limit}
	code, stderr, err := g.run(ctx, root, config, stdin, sink, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return newGitExitError(args, code, stderr)
	}
	return nil
}

// RawStatus captures NUL-delimited raw diff status with rename detection pinned
// to 50% (no copies, unlimited rename limit), full-width object ids, and
// textconv/external diff neutralized centrally; callers supply only revisions
// and pathspecs. --no-abbrev forces every raw old/new object id (including
// gitlink submodule commits) to full object-format width so an identity is never
// lost to abbreviation; --full-index pins full index-line ids for the same
// reason. Both are pinned here and, like every other pinned control, rejected
// when a caller tries to supply them.
func (g ExecGit) RawStatus(ctx context.Context, root string, limit int64, args ...string) ([]byte, error) {
	if err := rejectUnsafeDiffArgs(args); err != nil {
		return nil, err
	}
	fixed := []string{"diff", "--raw", "-z", "--no-textconv", "--no-ext-diff", "--find-renames=50%", "--no-color", "--full-index", "--no-abbrev"}
	return g.diffCapture(ctx, root, limit, false, append(fixed, args...)...)
}

// Diff captures a canonical worktree/tree patch. All diff-driver, textconv,
// rename/copy, prefix, context, and algorithm controls are injected here, so
// callers pass only revisions and pathspecs; options that would override a
// pinned control are rejected.
func (g ExecGit) Diff(ctx context.Context, root string, limit int64, args ...string) ([]byte, error) {
	if err := rejectUnsafeDiffArgs(args); err != nil {
		return nil, err
	}
	fixed := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--find-renames=50%", "--unified=3", "--src-prefix=a/", "--dst-prefix=b/"}
	return g.diffCapture(ctx, root, limit, false, append(fixed, args...)...)
}

// DiffNoIndex is the dedicated forced-text diff helper for two out-of-tree
// files. git diff --no-index exits 0 when the inputs are identical and 1 when
// they differ; only a status above 1 is a genuine failure. This exit-1-as-
// success rule lives here alone.
func (g ExecGit) DiffNoIndex(ctx context.Context, root string, limit int64, base, head string) ([]byte, error) {
	return g.diffCapture(ctx, root, limit, true,
		"diff", "--no-index", "--text", "--no-color", "--no-ext-diff", "--no-textconv",
		"--no-renames", "--unified=3", "--src-prefix=a/", "--dst-prefix=b/", "--", base, head)
}

// diffCapture centralizes the sanitized diff path: allowlisted base config plus
// pinned diff options, bounded capture, and exit handling. allowExit1 accepts
// git diff --no-index's exit 1 (a difference).
func (g ExecGit) diffCapture(ctx context.Context, root string, limit int64, allowExit1 bool, args ...string) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrInvalidLimit
	}
	config, err := g.safeConfig(ctx, root)
	if err != nil {
		return nil, err
	}
	config = append(config, diffPinConfig()...)
	stdout := &boundedBuffer{limit: limit}
	code, stderr, err := g.run(ctx, root, config, nil, stdout, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 && !(allowExit1 && code == 1) {
		return nil, newGitExitError(args, code, stderr)
	}
	return stdout.bytes(), nil
}

// run executes git once with full sanitization, bounded I/O, platform process-
// tree control, and context/overflow-driven termination of that tree. It
// returns the exit code, captured stderr, and a non-nil error only for a bad
// limit, overflow, cancellation, discovery failure, or a failure to launch,
// control, or reap.
func (g ExecGit) run(ctx context.Context, root string, config []string, stdin io.Reader, stdout overflowSink, args ...string) (int, []byte, error) {
	if g.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.Timeout)
		defer cancel()
	}

	cmd := g.sanitizedCommand(root, config, args)

	stderr := &boundedBuffer{limit: g.maxStderr()}
	cmd.Stderr = stderr
	cmd.Stdout = stdout
	cmd.Stdin = stdin

	pc := newProcessController()
	if err := pc.prepare(cmd); err != nil {
		return 0, nil, err
	}
	kill := func() { pc.kill() }
	stderr.setOnOverflow(kill)
	stdout.setOnOverflow(kill)

	if err := cmd.Start(); err != nil {
		pc.release() // free resources prepared before a failed Start
		return 0, nil, err
	}
	if err := pc.started(cmd); err != nil {
		// Process control could not be established; do not let the child run
		// unmanaged. Best-effort kill the single process and reap it.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		pc.release()
		return 0, nil, err
	}
	defer pc.release()
	// An overflow or cancellation may have fired during startup, before the
	// controller was fully armed; ensure the tree is torn down in that case.
	if stdout.overflowed() || stderr.overflowed() || ctx.Err() != nil {
		kill()
	}

	done := make(chan struct{})
	var canceled atomic.Bool
	go func() {
		select {
		case <-ctx.Done():
			canceled.Store(true)
			kill()
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	close(done)

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if !canceled.Load() && !stderr.overflowed() && !stdout.overflowed() && stdout.failure() == nil {
			return 0, stderr.bytes(), waitErr
		}
	}

	switch {
	case stdout.failure() != nil:
		return exitCode, stderr.bytes(), stdout.failure()
	case stdout.overflowed():
		return exitCode, stderr.bytes(), ErrGitOutputOverflow
	case stderr.overflowed():
		return exitCode, stderr.bytes(), ErrGitStderrOverflow
	case canceled.Load():
		return exitCode, stderr.bytes(), ctx.Err()
	default:
		return exitCode, stderr.bytes(), nil
	}
}

// sanitizedCommand builds the *exec.Cmd shared by run and by tests that must
// supply their own stdout (e.g. a TTY for the pager control): explicit working
// directory, allowlist-scrubbed environment, and the allowlisted -c config
// followed by the command arguments. It is the single source of the
// environment/config policy so tests never reassemble it by hand.
func (g ExecGit) sanitizedCommand(root string, config, args []string) *exec.Cmd {
	argv := make([]string, 0, len(config)+len(args))
	argv = append(argv, config...)
	argv = append(argv, args...)
	cmd := exec.Command(g.binary(), argv...)
	cmd.Dir = root
	cmd.Env = scrubGitEnv(os.Environ())
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return "git"
	}
	return args[0]
}

// patchSubcommands produce diffs/patches and must go through a dedicated,
// pinned diff helper rather than the generic Output/Pipe runners.
var patchSubcommands = map[string]bool{
	"diff": true, "diff-tree": true, "diff-index": true, "diff-files": true,
	"show": true, "format-patch": true, "range-diff": true, "whatchanged": true,
}

// globalOptsWithValue are git global options that consume the following
// argument as their value, so the subcommand resolver must skip both.
var globalOptsWithValue = map[string]bool{"-C": true, "-c": true}

// resolveSubcommand returns the actual git subcommand and the arguments that
// follow it, skipping any leading global options (e.g. -c k=v, -C path,
// --paginate, --no-optional-locks) and a global `--` so a caller cannot hide a
// patch-producing subcommand (or a patch flag on it) behind them. ok is false
// when no subcommand is present.
func resolveSubcommand(args []string) (sub string, rest []string, ok bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1], args[i+2:], true
			}
			return "", nil, false
		}
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:], true // first non-option token is the subcommand
		}
		if globalOptsWithValue[a] {
			i++ // also skip this option's separate value
		}
	}
	return "", nil, false
}

// rejectPatchSubcommand refuses a patch-producing subcommand on the generic
// runners, resolving the real subcommand past global options (including a global
// `--`), and a `log` invoked with a patch flag. The patch-flag scan runs on the
// arguments after the subcommand so a global `--` cannot terminate it early.
func rejectPatchSubcommand(args []string) error {
	sub, rest, ok := resolveSubcommand(args)
	if !ok {
		return nil
	}
	if patchSubcommands[sub] {
		return fmt.Errorf("%w: %q", ErrPatchViaGenericRunner, sub)
	}
	if sub == "log" {
		for _, a := range rest {
			if a == "--" {
				break // pathspec separator: operands follow, not options
			}
			if a == "-p" || a == "-u" || a == "--patch" || a == "--full-diff" ||
				strings.HasPrefix(a, "-U") || strings.HasPrefix(a, "--unified") {
				return fmt.Errorf("%w: log %s", ErrPatchViaGenericRunner, a)
			}
		}
	}
	return nil
}

// rejectUnsafeDiffArgs enforces that callers of the dedicated diff helpers pass
// only safe operands: revisions and pathspecs. Every token before the `--`
// separator must be a non-flag operand, so no caller option can override a
// pinned output or canonicalization control (whitespace, word/name/stat/summary,
// raw/patch/binary/full-index/abbrev/relative/output/context/algorithm/rename/
// copy, or any future flag). Tokens after `--` are pathspecs and are never
// interpreted as options by git.
func rejectUnsafeDiffArgs(args []string) error {
	for _, a := range args {
		if a == "--" {
			break // pathspec separator: everything after is an operand
		}
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("%w: %s", ErrUnsafeDiffOption, a)
		}
	}
	return nil
}

// ---- environment and configuration sanitization --------------------------

// gitEnvPins are the environment values Prowl forces after the allowlist filter.
var gitEnvPins = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_TERMINAL_PROMPT=0",
	"GIT_PAGER=cat",
	"PAGER=cat",
	"GIT_OPTIONAL_LOCKS=0",
	"GIT_NO_LAZY_FETCH=1",
}

// baseEnvAllowlist is the minimal cross-platform set of inherited variables Git
// may keep. Everything else (all GIT_*, askpass, SSH, proxy, and trace/trace2
// families) is dropped; global config is neutralized by the pins regardless of
// HOME.
var baseEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "LOGNAME": true, "USER": true, "SHELL": true,
	"TMPDIR": true, "TERM": true, "TZ": true, "LANG": true, "LANGUAGE": true,
}

func envAllowed(name string) bool {
	if baseEnvAllowlist[name] {
		return true
	}
	if strings.HasPrefix(name, "LC_") {
		return true
	}
	return platformEnvAllowed(name)
}

// scrubGitEnv keeps only allowlisted inherited variables, then appends the
// mandated pins. An allowlist (rather than a denylist) guarantees that unknown
// GIT_*, transport, proxy, or trace variables cannot survive.
func scrubGitEnv(inherited []string) []string {
	out := make([]string, 0, len(baseEnvAllowlist)+len(gitEnvPins))
	for _, kv := range inherited {
		name, _, ok := strings.Cut(kv, "=")
		if ok && envAllowed(name) {
			out = append(out, kv)
		}
	}
	return append(out, gitEnvPins...)
}

// safeConfig enumerates the repository's filter and diff-driver names without
// running them and returns the allowlisted -c overrides that neutralize every
// hostile surface, including those drivers. A discovery failure is fatal.
func (g ExecGit) safeConfig(ctx context.Context, root string) ([]string, error) {
	filters, diffs, err := g.enumerateDrivers(ctx, root)
	if err != nil {
		return nil, err
	}
	return g.baseConfig(filters, diffs), nil
}

func (g ExecGit) baseConfig(filters, diffs []string) []string {
	hooks := g.HooksDir
	if hooks == "" {
		hooks = os.DevNull
	}
	cfg := []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "credential.helper=",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + hooks,
		"-c", "core.askPass=",
		"-c", "core.sshCommand=",
		"-c", "core.pager=cat",
		"-c", "core.editor=false",
		"-c", "diff.external=",
		"-c", "color.ui=false",
		"-c", "color.diff=false",
		"-c", "pager.diff=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
		"-c", "uploadpack.packObjectsHook=",
	}
	for _, name := range filters {
		cfg = append(cfg,
			"-c", "filter."+name+".clean=",
			"-c", "filter."+name+".smudge=",
			"-c", "filter."+name+".process=",
			"-c", "filter."+name+".required=false",
		)
	}
	for _, name := range diffs {
		cfg = append(cfg,
			"-c", "diff."+name+".command=",
			"-c", "diff."+name+".textconv=",
			"-c", "diff."+name+".cachetextconv=false",
			"-c", "diff."+name+".binary=false",
		)
	}
	return cfg
}

// diffPinConfig pins every output-affecting diff option so repository
// configuration cannot change canonical diff rendering.
func diffPinConfig() []string {
	return []string{
		"-c", "diff.algorithm=histogram",
		"-c", "diff.indentHeuristic=false",
		"-c", "diff.interHunkContext=0",
		"-c", "diff.context=3",
		"-c", "diff.renameLimit=0",
		"-c", "diff.renames=true",
		"-c", "diff.noprefix=false",
		"-c", "diff.mnemonicPrefix=false",
		"-c", "diff.colorMoved=no",
		"-c", "diff.wsErrorHighlight=none",
	}
}

// enumerateDrivers lists local filter and diff-driver names by reading
// configuration only; it never starts a driver. It accepts only exit 0 with
// parsed names or the explicit no-match exit (1 with empty output); overflow,
// timeout, or any other outcome is a fatal discovery error so the caller
// refuses to run with an unknown driver set. Its own config omits driver
// overrides to avoid recursion.
func (g ExecGit) enumerateDrivers(ctx context.Context, root string) (filters, diffs []string, err error) {
	stdout := &boundedBuffer{limit: maxDriverListBytes}
	code, stderr, rerr := g.run(ctx, root, g.baseConfig(nil, nil), nil, stdout, "config", "-z", "--get-regexp", `^(filter|diff)\.`)
	if rerr != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrDriverDiscovery, rerr)
	}
	switch code {
	case 0:
		return parseDriverNames(stdout.bytes())
	case 1:
		if len(bytes.TrimSpace(stdout.bytes())) != 0 {
			return nil, nil, fmt.Errorf("%w: exit 1 with unexpected output", ErrDriverDiscovery)
		}
		return nil, nil, nil
	default:
		return nil, nil, fmt.Errorf("%w: config exited with status %d: %s", ErrDriverDiscovery, code, strings.TrimSpace(string(stderr)))
	}
}

// filterProps and diffDriverProps are the only configuration properties a
// discovered filter or diff driver may carry; anything else under a successful
// (exit 0) discovery is refused rather than run.
var (
	filterProps     = map[string]bool{"clean": true, "smudge": true, "process": true, "required": true}
	diffDriverProps = map[string]bool{"command": true, "textconv": true, "cachetextconv": true, "binary": true}
)

// splitDriverKey splits a `<name>.<prop>` driver subkey on its final dot,
// reporting ok only when both the driver name and the property segment are
// non-empty. Any empty segment (e.g. `.clean`, `foo.`, `.`) is malformed so a
// discovery that carries one is refused rather than silently accepted.
func splitDriverKey(rest string) (name, prop string, ok bool) {
	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 {
		return "", "", false
	}
	name, prop = rest[:dot], rest[dot+1:]
	if name == "" || prop == "" {
		return "", "", false
	}
	return name, prop, true
}

// parseDriverNames extracts filter and diff-driver names from `git config -z
// --get-regexp` output, validating NUL/newline framing and key shape. Any
// unexpected key, missing key/value separator, incomplete filter key, or bad
// NUL framing is an error so a malformed but exit-0 discovery is refused.
func parseDriverNames(out []byte) (filters, diffs []string, err error) {
	fset := map[string]bool{}
	dset := map[string]bool{}
	if len(out) == 0 {
		return nil, nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, nil, fmt.Errorf("%w: output not NUL-terminated", ErrDriverDiscovery)
	}
	records := bytes.Split(out[:len(out)-1], []byte{0})
	for _, entry := range records {
		if len(entry) == 0 {
			return nil, nil, fmt.Errorf("%w: empty config record", ErrDriverDiscovery)
		}
		nl := bytes.IndexByte(entry, '\n')
		if nl < 0 {
			return nil, nil, fmt.Errorf("%w: record %q missing key/value separator", ErrDriverDiscovery, entry)
		}
		key := string(entry[:nl])
		switch {
		case strings.HasPrefix(key, "filter."):
			// filter keys are always <name>.<prop>; an empty name or property
			// segment is malformed and refused.
			name, prop, ok := splitDriverKey(key[len("filter."):])
			if !ok {
				return nil, nil, fmt.Errorf("%w: malformed filter key %q", ErrDriverDiscovery, key)
			}
			if !filterProps[prop] {
				return nil, nil, fmt.Errorf("%w: unexpected filter property %q", ErrDriverDiscovery, key)
			}
			fset[name] = true
		case strings.HasPrefix(key, "diff."):
			rest := key[len("diff."):]
			if strings.IndexByte(rest, '.') < 0 {
				// diff.<setting> (two segments) is a legitimate non-driver key;
				// an empty setting (diff.) is malformed.
				if rest == "" {
					return nil, nil, fmt.Errorf("%w: empty diff key %q", ErrDriverDiscovery, key)
				}
				continue
			}
			// A three-or-more-segment diff.<name>.<prop> is a driver key; an
			// empty name (diff..binary) or property (diff.foo.) is refused.
			name, prop, ok := splitDriverKey(rest)
			if !ok {
				return nil, nil, fmt.Errorf("%w: malformed diff driver key %q", ErrDriverDiscovery, key)
			}
			if !diffDriverProps[prop] {
				return nil, nil, fmt.Errorf("%w: unexpected diff driver property %q", ErrDriverDiscovery, key)
			}
			dset[name] = true
		default:
			return nil, nil, fmt.Errorf("%w: unexpected key %q", ErrDriverDiscovery, key)
		}
	}
	return sortedKeys(fset), sortedKeys(dset), nil
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- bounded output sinks -------------------------------------------------

// overflowSink is a writer that fires a one-shot kill when its byte budget is
// exceeded and reports whether it overflowed.
type overflowSink interface {
	io.Writer
	overflowed() bool
	setOnOverflow(func())
	// failure reports a terminal write error (short write or destination error)
	// that should fail the whole command.
	failure() error
}

// boundedBuffer captures up to limit bytes, firing onOverflow once when a write
// would exceed it, then silently discarding the rest so the source never blocks.
type boundedBuffer struct {
	limit      int64
	mu         sync.Mutex
	buf        bytes.Buffer
	n          int64
	over       bool
	onOverflow func()
}

func (b *boundedBuffer) setOnOverflow(f func()) {
	b.mu.Lock()
	b.onOverflow = f
	b.mu.Unlock()
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	fire := false
	if !b.over {
		if b.limit > 0 && b.n+int64(len(p)) > b.limit {
			if room := b.limit - b.n; room > 0 {
				b.buf.Write(p[:room])
				b.n += room
			}
			b.over = true
			fire = true
		} else {
			b.buf.Write(p)
			b.n += int64(len(p))
		}
	}
	cb := b.onOverflow
	b.mu.Unlock()
	if fire && cb != nil {
		cb()
	}
	return len(p), nil
}

func (b *boundedBuffer) overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.over
}

func (b *boundedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Bytes()
}

func (b *boundedBuffer) failure() error { return nil }

// boundedWriter streams to an underlying writer up to limit bytes, firing
// onOverflow once when exceeded and then discarding so the source never blocks.
type boundedWriter struct {
	dest       io.Writer
	limit      int64
	mu         sync.Mutex
	n          int64
	over       bool
	failErr    error
	onOverflow func()
}

func (b *boundedWriter) setOnOverflow(f func()) {
	b.mu.Lock()
	b.onOverflow = f
	b.mu.Unlock()
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.over || b.failErr != nil {
		b.mu.Unlock()
		return len(p), nil
	}
	overflow := false
	write := p
	if b.limit > 0 && b.n+int64(len(p)) > b.limit {
		room := b.limit - b.n
		if room < 0 {
			room = 0
		}
		write = p[:room]
		overflow = true
	}
	cb := b.onOverflow
	b.mu.Unlock()

	delivered := 0
	var werr error
	if len(write) > 0 {
		dn, err := b.dest.Write(write)
		delivered = dn
		switch {
		case err != nil:
			werr = err
		case dn < len(write):
			werr = io.ErrShortWrite
		}
	}

	b.mu.Lock()
	b.n += int64(delivered) // account only delivered bytes
	if overflow {
		b.over = true
	}
	if werr != nil && b.failErr == nil {
		b.failErr = werr
	}
	b.mu.Unlock()

	if (overflow || werr != nil) && cb != nil {
		cb() // kill the process tree on overflow or a destination failure
	}
	if werr != nil {
		// Honor io.Writer's contract: n < len(p) implies a non-nil error.
		return delivered, werr
	}
	// On overflow the excess is intentionally discarded; report full
	// consumption so io.Copy does not treat it as a short write.
	return len(p), nil
}

func (b *boundedWriter) overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.over
}

func (b *boundedWriter) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failErr
}

// ---- parsers --------------------------------------------------------------

// RawChange is one record of git diff --raw -z output. Score-bearing statuses
// such as R100 populate OldPath.
type RawChange struct {
	OldMode string
	NewMode string
	OldOID  string
	NewOID  string
	Status  string
	OldPath string
	Path    string
}

// ParseRawStatusZ parses NUL-delimited raw diff status. Well-formed input
// terminates every path with NUL and carries full object-format-width old/new
// object ids (RawStatus pins --no-abbrev); a missing terminal NUL, a truncated
// record, a malformed field, or an abbreviated/non-hex nonzero object id yields
// a typed error and no records.
func ParseRawStatusZ(data []byte) ([]RawChange, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("%w: input not NUL-terminated", ErrMalformedRawStatus)
	}
	tokens := strings.Split(string(data[:len(data)-1]), "\x00")
	var changes []RawChange
	for i := 0; i < len(tokens); {
		meta := tokens[i]
		if !strings.HasPrefix(meta, ":") {
			return nil, fmt.Errorf("%w: metadata %q missing leading colon", ErrMalformedRawStatus, meta)
		}
		fields := strings.Fields(meta[1:])
		if len(fields) != 5 {
			return nil, fmt.Errorf("%w: metadata %q has %d fields, want 5", ErrMalformedRawStatus, meta, len(fields))
		}
		status := fields[4]
		if !rawStatusOIDValid(fields[2]) || !rawStatusOIDValid(fields[3]) {
			return nil, fmt.Errorf("%w: metadata %q has an abbreviated or malformed object id", ErrMalformedRawStatus, meta)
		}
		wantPaths := 1
		if status != "" && (status[0] == 'R' || status[0] == 'C') {
			wantPaths = 2
		}
		if i+wantPaths >= len(tokens) {
			return nil, fmt.Errorf("%w: record %q missing %d path token(s)", ErrMalformedRawStatus, meta, wantPaths)
		}
		change := RawChange{OldMode: fields[0], NewMode: fields[1], OldOID: fields[2], NewOID: fields[3], Status: status}
		if wantPaths == 2 {
			change.OldPath = tokens[i+1]
			change.Path = tokens[i+2]
		} else {
			change.Path = tokens[i+1]
		}
		if change.Path == "" || (wantPaths == 2 && change.OldPath == "") {
			return nil, fmt.Errorf("%w: record %q has an empty path", ErrMalformedRawStatus, meta)
		}
		changes = append(changes, change)
		i += 1 + wantPaths
	}
	return changes, nil
}

// rawStatusOIDValid reports whether a raw diff-status object id field is
// acceptable: the all-zero unresolved-side placeholder, or a full-width hex id
// of a recognized object format (40 hex for SHA-1, 64 for SHA-256). RawStatus
// pins --no-abbrev, so real output is always full width; an abbreviated or
// non-hex nonzero id is rejected rather than accepted as a truncated identity.
func rawStatusOIDValid(oid string) bool {
	if isZeroOID(oid) {
		return true
	}
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// DiffHunk is one unified-diff hunk with its ranges and payload line counts.
type DiffHunk struct {
	OldStart  int
	OldLines  int
	NewStart  int
	NewLines  int
	Additions int
	Deletions int
}

// UnifiedDiff is a parsed unified diff with aggregate payload line counts.
type UnifiedDiff struct {
	Hunks     []DiffHunk
	Additions int
	Deletions int
}

var hunkHeaderRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParseUnifiedDiff parses a unified diff, counting +/- payload lines while
// excluding the +++/--- file headers. Each hunk's payload must exactly account
// for the declared old/new line ranges, and every payload line must carry a
// ' ', '+', '-', or '\' prefix. Any mismatch or unprefixed/empty payload line
// yields a typed error and no result.
func ParseUnifiedDiff(data []byte) (UnifiedDiff, error) {
	var ud UnifiedDiff
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	oldRemaining, newRemaining := 0, 0
	inHunk := false
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			if inHunk && (oldRemaining != 0 || newRemaining != 0) {
				return UnifiedDiff{}, fmt.Errorf("%w: hunk ended with %d old / %d new lines unaccounted", ErrMalformedUnifiedDiff, oldRemaining, newRemaining)
			}
			m := hunkHeaderRE.FindStringSubmatch(line)
			if m == nil {
				return UnifiedDiff{}, fmt.Errorf("%w: bad hunk header %q", ErrMalformedUnifiedDiff, line)
			}
			oldStart, err1 := parseHunkNum(m[1], 0)
			oldLines, err2 := parseHunkNum(m[2], 1)
			newStart, err3 := parseHunkNum(m[3], 0)
			newLines, err4 := parseHunkNum(m[4], 1)
			if err := cmp.Or(err1, err2, err3, err4); err != nil {
				return UnifiedDiff{}, fmt.Errorf("%w: hunk header %q: %v", ErrMalformedUnifiedDiff, line, err)
			}
			// A start of 0 is only valid for an empty side (count 0); a nonzero
			// count must begin at line 1 or later.
			if (oldLines > 0 && oldStart < 1) || (newLines > 0 && newStart < 1) {
				return UnifiedDiff{}, fmt.Errorf("%w: hunk header %q has a nonzero count starting at line 0", ErrMalformedUnifiedDiff, line)
			}
			hunk := DiffHunk{OldStart: oldStart, OldLines: oldLines, NewStart: newStart, NewLines: newLines}
			ud.Hunks = append(ud.Hunks, hunk)
			oldRemaining, newRemaining = hunk.OldLines, hunk.NewLines
			inHunk = true
			continue
		}
		if !inHunk {
			if isDiffPreambleLine(line) {
				continue
			}
			return UnifiedDiff{}, fmt.Errorf("%w: unexpected line %q outside any hunk", ErrMalformedUnifiedDiff, line)
		}
		if oldRemaining == 0 && newRemaining == 0 {
			// The declared ranges are satisfied; anything else is a new section.
			if isDiffPreambleLine(line) {
				inHunk = false
				continue
			}
			return UnifiedDiff{}, fmt.Errorf("%w: payload line %q past declared hunk range", ErrMalformedUnifiedDiff, line)
		}
		if line == "" {
			return UnifiedDiff{}, fmt.Errorf("%w: empty payload line inside hunk", ErrMalformedUnifiedDiff)
		}
		hunk := &ud.Hunks[len(ud.Hunks)-1]
		switch line[0] {
		case '+':
			if newRemaining == 0 {
				return UnifiedDiff{}, fmt.Errorf("%w: extra added line %q", ErrMalformedUnifiedDiff, line)
			}
			hunk.Additions++
			ud.Additions++
			newRemaining--
		case '-':
			if oldRemaining == 0 {
				return UnifiedDiff{}, fmt.Errorf("%w: extra deleted line %q", ErrMalformedUnifiedDiff, line)
			}
			hunk.Deletions++
			ud.Deletions++
			oldRemaining--
		case ' ':
			if oldRemaining == 0 || newRemaining == 0 {
				return UnifiedDiff{}, fmt.Errorf("%w: extra context line %q", ErrMalformedUnifiedDiff, line)
			}
			oldRemaining--
			newRemaining--
		case '\\':
			// "\ No newline at end of file": not a payload line.
		default:
			return UnifiedDiff{}, fmt.Errorf("%w: unprefixed hunk line %q", ErrMalformedUnifiedDiff, line)
		}
	}
	if inHunk && (oldRemaining != 0 || newRemaining != 0) {
		return UnifiedDiff{}, fmt.Errorf("%w: final hunk ended with %d old / %d new lines unaccounted", ErrMalformedUnifiedDiff, oldRemaining, newRemaining)
	}
	return ud, nil
}

func isDiffPreambleLine(line string) bool {
	switch {
	case line == "",
		strings.HasPrefix(line, "diff "),
		strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "--- "),
		strings.HasPrefix(line, "+++ "),
		strings.HasPrefix(line, "old mode "),
		strings.HasPrefix(line, "new mode "),
		strings.HasPrefix(line, "new file mode "),
		strings.HasPrefix(line, "deleted file mode "),
		strings.HasPrefix(line, "similarity index "),
		strings.HasPrefix(line, "dissimilarity index "),
		strings.HasPrefix(line, "rename "),
		strings.HasPrefix(line, "copy "),
		strings.HasPrefix(line, "Binary files "),
		strings.HasPrefix(line, "GIT binary patch"):
		return true
	}
	return false
}

// CatFileObject is one record of git cat-file --batch output.
type CatFileObject struct {
	OID     string
	Type    string
	Size    int64
	Data    []byte
	Missing bool
}

// ParseCatFileBatch parses git cat-file --batch output. A header, size (rejected
// before any size+1 arithmetic to avoid overflow), or object payload that is
// malformed or truncated yields a typed error and no records.
func ParseCatFileBatch(data []byte) ([]CatFileObject, error) {
	var objs []CatFileObject
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			return nil, fmt.Errorf("%w: header without terminating newline", ErrMalformedCatFile)
		}
		header := string(data[:nl])
		data = data[nl+1:]
		fields := strings.Fields(header)
		switch {
		case len(fields) == 2 && fields[1] == "missing":
			objs = append(objs, CatFileObject{OID: fields[0], Missing: true})
		case len(fields) == 3:
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || size < 0 {
				return nil, fmt.Errorf("%w: bad object size %q", ErrMalformedCatFile, fields[2])
			}
			// Compare before any size+1 so an attacker-supplied huge size cannot
			// overflow or index out of range.
			if size > int64(len(data)) {
				return nil, fmt.Errorf("%w: object %s truncated, want %d payload bytes", ErrMalformedCatFile, fields[0], size)
			}
			if int64(len(data)) < size+1 || data[size] != '\n' {
				return nil, fmt.Errorf("%w: object %s missing trailing newline", ErrMalformedCatFile, fields[0])
			}
			objs = append(objs, CatFileObject{
				OID:  fields[0],
				Type: fields[1],
				Size: size,
				Data: append([]byte(nil), data[:size]...),
			})
			data = data[size+1:]
		default:
			return nil, fmt.Errorf("%w: bad header %q", ErrMalformedCatFile, header)
		}
	}
	return objs, nil
}

// parseHunkNum parses a unified-diff hunk-range number. An omitted optional
// count (empty string) uses def; a present value must be a valid non-negative
// int, so an overflowing or otherwise invalid explicit number is an error.
func parseHunkNum(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("negative count %d", n)
	}
	return n, nil
}
