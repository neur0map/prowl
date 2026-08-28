package review

import (
	"bytes"
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
	"syscall"
	"time"
)

// defaultMaxStderrBytes bounds captured stderr when ExecGit.MaxStderr is unset.
const defaultMaxStderrBytes = 1 << 20

var (
	// ErrGitOutputOverflow reports that a command's stdout exceeded its limit.
	ErrGitOutputOverflow = errors.New("review: git stdout exceeded its byte limit")
	// ErrGitStderrOverflow reports that a command's stderr exceeded its limit.
	ErrGitStderrOverflow = errors.New("review: git stderr exceeded its byte limit")
	// ErrMalformedRawStatus reports unparseable NUL-delimited raw diff status.
	ErrMalformedRawStatus = errors.New("review: malformed raw diff status")
	// ErrMalformedUnifiedDiff reports an unparseable unified diff.
	ErrMalformedUnifiedDiff = errors.New("review: malformed unified diff")
	// ErrMalformedCatFile reports unparseable cat-file --batch output.
	ErrMalformedCatFile = errors.New("review: malformed cat-file batch output")
)

// GitRunner runs sanitized, bounded Git subprocesses. Output captures a
// command's stdout up to a byte limit; Pipe streams stdin and stdout for
// object protocols such as cat-file --batch. Both enforce the sanitized
// execution policy, a fixed timeout, and process-group termination.
type GitRunner interface {
	Output(ctx context.Context, root string, limit int64, args ...string) ([]byte, error)
	Pipe(ctx context.Context, root string, stdin io.Reader, stdout io.Writer, args ...string) error
}

// ExecGit is the process-backed GitRunner. Every invocation runs with a
// scrubbed environment, an allowlisted -c configuration, no shell, an explicit
// working directory, bounded output, and its own process group so a hung or
// overflowing child (and any descendant it spawned) is killed and reaped.
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

// Output runs git with the given arguments and returns stdout, failing with
// ErrGitOutputOverflow if it exceeds limit and with an error carrying stderr if
// git exits non-zero.
func (g ExecGit) Output(ctx context.Context, root string, limit int64, args ...string) ([]byte, error) {
	stdout := &boundedBuffer{limit: limit}
	code, stderr, err := g.run(ctx, root, g.baseConfigFor(ctx, root), nil, stdout, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("review: git %s exited with status %d: %s", firstArg(args), code, strings.TrimSpace(string(stderr)))
	}
	return stdout.bytes(), nil
}

// Pipe runs git streaming stdin to the child and the child's stdout to stdout.
func (g ExecGit) Pipe(ctx context.Context, root string, stdin io.Reader, stdout io.Writer, args ...string) error {
	code, stderr, err := g.run(ctx, root, g.baseConfigFor(ctx, root), stdin, stdout, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("review: git %s exited with status %d: %s", firstArg(args), code, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// DiffNoIndex is the dedicated forced-text diff helper for two out-of-tree
// files. Its output-affecting options are pinned so repository configuration
// and attributes cannot perturb the canonical diff. git diff --no-index exits 0
// when the inputs are identical and 1 when they differ; only a status above 1
// is a genuine failure. This exit-1-as-success rule lives here alone.
func (g ExecGit) DiffNoIndex(ctx context.Context, root string, limit int64, base, head string) ([]byte, error) {
	config := append(g.baseConfigFor(ctx, root), diffPinConfig()...)
	stdout := &boundedBuffer{limit: limit}
	args := []string{
		"diff", "--no-index", "--text", "--no-color", "--no-ext-diff", "--no-textconv",
		"--no-renames", "--unified=3", "--src-prefix=a/", "--dst-prefix=b/", "--", base, head,
	}
	code, stderr, err := g.run(ctx, root, config, nil, stdout, args...)
	if err != nil {
		return nil, err
	}
	if code > 1 {
		return nil, fmt.Errorf("review: git diff --no-index exited with status %d: %s", code, strings.TrimSpace(string(stderr)))
	}
	return stdout.bytes(), nil
}

// run executes git once with full sanitization, bounded I/O, a private process
// group, and context/overflow-driven SIGKILL of that group. It returns the exit
// code, captured stderr, and a non-nil error only for overflow, cancellation,
// or a failure to launch or reap.
func (g ExecGit) run(ctx context.Context, root string, config []string, stdin io.Reader, stdout io.Writer, args ...string) (int, []byte, error) {
	if g.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.Timeout)
		defer cancel()
	}

	argv := make([]string, 0, len(config)+len(args))
	argv = append(argv, config...)
	argv = append(argv, args...)

	cmd := exec.Command(g.binary(), argv...)
	cmd.Dir = root
	cmd.Env = scrubGitEnv(os.Environ())
	// A private process group lets us signal the child and every descendant it
	// spawns as a unit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// After the process exits, bound how long Wait blocks on lingering I/O
	// copiers rather than hanging forever on a stuck stream.
	cmd.WaitDelay = 10 * time.Second

	stderr := &boundedBuffer{limit: g.maxStderr()}
	cmd.Stderr = stderr
	cmd.Stdout = stdout
	cmd.Stdin = stdin

	// SIGKILL cannot be caught or ignored, so a child trapping SIGTERM still
	// dies; the negative PID targets the whole group.
	kill := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	stderr.onOverflow = kill
	if bw, ok := stdout.(*boundedBuffer); ok {
		bw.onOverflow = kill
	}

	if err := cmd.Start(); err != nil {
		return 0, nil, err
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
		} else if !canceled.Load() && !stderr.overflowed() && !bufOverflowed(stdout) {
			return 0, stderr.bytes(), waitErr
		}
	}

	switch {
	case bufOverflowed(stdout):
		return exitCode, stderr.bytes(), ErrGitOutputOverflow
	case stderr.overflowed():
		return exitCode, stderr.bytes(), ErrGitStderrOverflow
	case canceled.Load():
		return exitCode, stderr.bytes(), ctx.Err()
	default:
		return exitCode, stderr.bytes(), nil
	}
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return "git"
	}
	return args[0]
}

// ---- environment and configuration sanitization --------------------------

// scrubGitEnv removes every inherited variable that could redirect Git's
// configuration, object store, transport, or helper resolution, then pins the
// safe values Prowl requires. Removing the pinned names first ensures our
// values win over any inherited copy.
func scrubGitEnv(inherited []string) []string {
	out := make([]string, 0, len(inherited)+8)
	for _, kv := range inherited {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || deniedGitEnvVar(name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_NO_LAZY_FETCH=1",
	)
}

func deniedGitEnvVar(name string) bool {
	switch name {
	case "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_GLOBAL",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE", "GIT_COMMON_DIR", "GIT_CEILING_DIRECTORIES",
		"GIT_ASKPASS", "SSH_ASKPASS", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT",
		"GIT_PROXY_COMMAND", "GIT_ALLOW_PROTOCOL", "GIT_PROTOCOL_FROM_USER", "GIT_PROTOCOL",
		"GIT_EXTERNAL_DIFF", "GIT_TEXTCONV",
		"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER", "PAGER",
		"GIT_TERMINAL_PROMPT", "GIT_OPTIONAL_LOCKS", "GIT_NO_LAZY_FETCH":
		return true
	}
	return strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// baseConfigFor enumerates the repository's filter-driver names without running
// them and returns the allowlisted -c overrides that neutralize every hostile
// surface, including those filters.
func (g ExecGit) baseConfigFor(ctx context.Context, root string) []string {
	return g.baseConfig(g.enumerateFilters(ctx, root))
}

func (g ExecGit) baseConfig(filters []string) []string {
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
		"-c", "diff.noprefix=false",
		"-c", "diff.mnemonicPrefix=false",
		"-c", "diff.colorMoved=no",
		"-c", "diff.wsErrorHighlight=none",
	}
}

// enumerateFilters lists local filter-driver names by reading configuration
// only; it never starts a filter. It intentionally omits filter overrides from
// its own configuration to avoid recursion, and ignores the exit status because
// git config --get-regexp exits non-zero when nothing matches.
func (g ExecGit) enumerateFilters(ctx context.Context, root string) []string {
	stdout := &boundedBuffer{limit: 4 << 20}
	_, _, _ = g.run(ctx, root, g.baseConfig(nil), nil, stdout, "config", "-z", "--get-regexp", `^filter\.`)
	return parseFilterNames(stdout.bytes())
}

func parseFilterNames(out []byte) []string {
	const prefix = "filter."
	seen := map[string]bool{}
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		key := entry
		if nl := bytes.IndexByte(entry, '\n'); nl >= 0 {
			key = entry[:nl]
		}
		s := string(key)
		if !strings.HasPrefix(s, prefix) {
			continue
		}
		rest := s[len(prefix):]
		dot := strings.LastIndexByte(rest, '.')
		if dot <= 0 {
			continue
		}
		seen[rest[:dot]] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ---- bounded output buffer -----------------------------------------------

// boundedBuffer accumulates up to limit bytes and fires onOverflow exactly once
// when a write would exceed it, then silently discards the rest so the source
// never blocks. A non-positive limit is unbounded.
type boundedBuffer struct {
	limit      int64
	mu         sync.Mutex
	buf        bytes.Buffer
	n          int64
	over       bool
	onOverflow func()
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

func bufOverflowed(w io.Writer) bool {
	if bw, ok := w.(*boundedBuffer); ok {
		return bw.overflowed()
	}
	return false
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

// ParseRawStatusZ parses NUL-delimited raw diff status. It returns a typed
// error and no records when any record is truncated or malformed.
func ParseRawStatusZ(data []byte) ([]RawChange, error) {
	tokens := strings.Split(string(data), "\x00")
	if len(tokens) > 0 && tokens[len(tokens)-1] == "" {
		tokens = tokens[:len(tokens)-1]
	}
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
		if change.Path == "" {
			return nil, fmt.Errorf("%w: record %q has empty path", ErrMalformedRawStatus, meta)
		}
		changes = append(changes, change)
		i += 1 + wantPaths
	}
	return changes, nil
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
// excluding the +++/--- file headers. It returns a typed error and no result
// when a hunk header or hunk line is malformed.
func ParseUnifiedDiff(data []byte) (UnifiedDiff, error) {
	var ud UnifiedDiff
	var cur *DiffHunk
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			m := hunkHeaderRE.FindStringSubmatch(line)
			if m == nil {
				return UnifiedDiff{}, fmt.Errorf("%w: bad hunk header %q", ErrMalformedUnifiedDiff, line)
			}
			ud.Hunks = append(ud.Hunks, DiffHunk{
				OldStart: atoiOr(m[1], 0),
				OldLines: atoiOr(m[2], 1),
				NewStart: atoiOr(m[3], 0),
				NewLines: atoiOr(m[4], 1),
			})
			cur = &ud.Hunks[len(ud.Hunks)-1]
			continue
		}
		if cur == nil {
			if isDiffPreambleLine(line) {
				continue
			}
			return UnifiedDiff{}, fmt.Errorf("%w: unexpected line %q outside any hunk", ErrMalformedUnifiedDiff, line)
		}
		if line == "" {
			continue
		}
		switch line[0] {
		case '+':
			cur.Additions++
			ud.Additions++
		case '-':
			cur.Deletions++
			ud.Deletions++
		case ' ', '\\':
			// context line or "\ No newline at end of file"
		default:
			return UnifiedDiff{}, fmt.Errorf("%w: unexpected hunk line %q", ErrMalformedUnifiedDiff, line)
		}
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

// ParseCatFileBatch parses git cat-file --batch output. It returns a typed
// error and no records when a header, size, or object payload is malformed.
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
			if int64(len(data)) < size+1 {
				return nil, fmt.Errorf("%w: object %s truncated, want %d payload bytes", ErrMalformedCatFile, fields[0], size)
			}
			if data[size] != '\n' {
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

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
