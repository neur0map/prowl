// Package cli: the user-facing agent-skill installer (`skills`) and the hidden
// Claude PreToolUse advisory hook (`_search-advisory`).
//
// `skills` is a thin, testable presenter over Task 4's ownership-safe user
// installer (internal/setup): it detects the harnesses actually installed on the
// machine, builds a deterministic plan, renders every action and conflict, and
// -- only in an interactive terminal, only on explicit y/yes -- applies it once.
// It reuses PlanUserSkills/ApplyUserSkills/VerifyUserSkills wholesale and never
// re-implements ownership checks or filesystem mutation.
//
// `_search-advisory` is the command Claude runs from the installed hooks.json on
// every Grep/Glob/Bash. It reads one PreToolUse JSON object, classifies only
// repository-wide searches, and either stays silent or emits Claude's
// additionalContext response nudging the agent toward Prowl's indexed search. It
// fails open: malformed, unknown, bounded, or prowl-agent input produces no
// output and exits 0. It never emits a permission decision and never echoes the
// caller's tool input.
package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/prowl-agent/prowl-agent/internal/setup"
)

// newSkillsCmd installs Prowl's agent-native skills under the user's own Claude,
// OMP, and Hermes configuration roots. It takes no positional arguments and has
// no subcommand. --clients selects which detected clients to target; --yes
// applies the reviewed plan without a prompt, which is the only way a
// non-interactive run (a pipe, a provisioning script) writes. version stamps the
// version-templated assets (only Claude's plugin manifest carries it).
func newSkillsCmd(version string) *cobra.Command {
	var assumeYes bool
	var clientsFlag string
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Install Prowl's agent-native skills into your Claude, OMP, and Hermes config",
		Long: "Install Prowl's release-matched agent skills into your own Claude, OMP,\n" +
			"and Hermes configuration roots (~/.claude/skills/prowl, ~/.omp/agent, and\n" +
			"~/.hermes/skills/prowl). The command shows a full preview of every file it\n" +
			"would write and every destination it refuses to touch, then asks once\n" +
			"before changing anything. Pass --yes to apply without prompting (the only\n" +
			"way a piped or non-interactive run writes) and --clients to target a\n" +
			"specific subset of the detected clients.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			clients := setup.DetectInstalledHarnesses()
			if clientsFlag != "" {
				clients = splitClients(clientsFlag)
			}
			opts := setup.UserInstallOptions{
				Home:    home,
				Version: version,
				Clients: clients,
			}
			in, out := cmd.InOrStdin(), cmd.OutOrStdout()
			return runSkills(opts, in, out, streamsInteractive(in, out), assumeYes)
		},
	}
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "apply the plan without prompting (also writes in a non-interactive run)")
	cmd.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated clients to target (claude,omp,hermes); default: detected")
	return cmd
}

// splitClients parses the --clients flag: a comma-separated list, trimmed of
// surrounding whitespace, with empty entries dropped. The setup planner
// normalizes the result (unknown names are ignored, duplicates removed), so this
// only has to tokenize.
func splitClients(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// userVerifier is the post-apply verification dependency. Production is
// setup.VerifyUserSkills; a test injects a failing one to prove the error is
// surfaced -- no shared mutable package state, no race.
type userVerifier func(setup.UserInstallOptions) (setup.UserHealth, error)

// runSkills is the presenter seam: injected input, output, interactivity, and
// assumeYes make the preview, TTY gate, prompt, single apply, and restart
// guidance observable in a unit test without a real terminal. It plans, renders,
// and -- when assumeYes is set, or interactively on an explicit approval --
// applies exactly once, verifying with the production verifier.
func runSkills(opts setup.UserInstallOptions, in io.Reader, out io.Writer, interactive, assumeYes bool) error {
	return runSkillsWithVerifier(opts, in, out, interactive, assumeYes, setup.VerifyUserSkills)
}

// runSkillsWithVerifier is the injectable core; runSkills wires the production
// verifier and tests wire a failing one.
func runSkillsWithVerifier(opts setup.UserInstallOptions, in io.Reader, out io.Writer, interactive, assumeYes bool, verify userVerifier) error {
	if len(opts.Clients) == 0 {
		fmt.Fprintln(out, "No supported agent detected (looked for Claude, OMP, and Hermes); nothing to install.")
		return nil
	}

	plan, err := setup.PlanUserSkills(opts)
	if err != nil {
		return err
	}
	renderSkillsPlan(out, plan)

	if !planHasWrites(plan) {
		fmt.Fprintln(out, "\nEverything is already up to date; nothing to apply.")
		return nil
	}
	// --yes applies without a prompt, in a pipe or a terminal alike. Without it,
	// a non-interactive run stays a preview and an interactive run asks once.
	if !assumeYes {
		if !interactive {
			fmt.Fprintln(out, "\nPreview only: no interactive terminal attached, so nothing was written. Re-run in a terminal or pass --yes to apply.")
			return nil
		}
		fmt.Fprint(out, "\nApply these changes? [y/N] ")
		if !confirmYes(in) {
			fmt.Fprintln(out, "No changes applied.")
			return nil
		}
	}

	result, err := setup.ApplyUserSkills(opts, plan, true)
	if err != nil {
		return err
	}
	// Verify before announcing success: an apply we cannot read back is an error,
	// not a done state, so restart guidance is never printed over a broken install.
	health, err := verify(opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nInstalled %d change(s) for version %s.\n", countWrites(result.Actions), result.Version)
	fmt.Fprintf(out, "Verified %d asset(s) as current.\n", countCurrent(health))
	renderRestart(out, opts.Clients)
	return nil
}

// renderSkillsPlan prints the reviewable plan in full: every planned action --
// install, update, remove, and unchanged alike -- and every destination Prowl
// refuses to touch, with the reason. It carries no file bodies and no absolute
// paths; the plan is already home-relative. Write filtering lives only in the
// apply/no-op/count decisions, never in what the user is shown.
func renderSkillsPlan(out io.Writer, plan setup.UserPlan) {
	fmt.Fprintf(out, "Prowl agent-skills install plan (version %s)\n", plan.Version)

	if len(plan.Actions) == 0 {
		fmt.Fprintln(out, "  (no actions)")
	} else {
		fmt.Fprintln(out, "\nActions:")
		for _, action := range plan.Actions {
			fmt.Fprintf(out, "  %-9s %-6s %s\n", action.Kind, action.Client, action.Destination)
		}
	}

	if len(plan.Conflicts) > 0 {
		fmt.Fprintln(out, "\nConflicts (left unchanged):")
		for _, conflict := range plan.Conflicts {
			fmt.Fprintf(out, "  %-6s %s -- %s\n", conflict.Client, conflict.Destination, conflict.Reason)
		}
	}
}

// renderRestart names the reload each detected client needs so the freshly
// installed skills take effect. The clients come straight from the plan's
// options, so only what was targeted is mentioned.
func renderRestart(out io.Writer, clients []string) {
	fmt.Fprintln(out, "\nNext steps:")
	for _, client := range clients {
		switch client {
		case setup.IntegrationClaude:
			fmt.Fprintln(out, "  - Restart Claude Code so it loads the prowl skills plugin.")
		case setup.IntegrationOMP:
			fmt.Fprintln(out, "  - Reload OMP so it picks up the prowl agent skills.")
		case setup.IntegrationHermes:
			fmt.Fprintln(out, "  - Reload Hermes so it picks up the prowl agent skills.")
		}
	}
}

// writeActions returns the plan's mutating actions (install/update/remove),
// dropping the unchanged entries that carry no news for a preview.
func writeActions(plan setup.UserPlan) []setup.UserAction {
	var out []setup.UserAction
	for _, action := range plan.Actions {
		if action.Kind != setup.UserActionUnchanged {
			out = append(out, action)
		}
	}
	return out
}

func planHasWrites(plan setup.UserPlan) bool {
	return len(writeActions(plan)) > 0
}

func countWrites(actions []setup.UserAction) int {
	n := 0
	for _, action := range actions {
		if action.Kind != setup.UserActionUnchanged {
			n++
		}
	}
	return n
}

func countCurrent(health setup.UserHealth) int {
	n := 0
	for _, asset := range health.Assets {
		if asset.State == setup.UserAssetCurrent {
			n++
		}
	}
	return n
}

// confirmYes reads one line and approves only an explicit y/yes (case- and
// whitespace-insensitive). Everything else -- an empty line, EOF, "n", or any
// other text -- keeps the safe default of No.
func confirmYes(in io.Reader) bool {
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// streamsInteractive reports whether both of the exact streams the command was
// handed are terminals. Judging the real streams -- not process-global stdio --
// means an embedded command driven by buffers or pipes is a preview even on a
// host that owns a TTY, while the default os.Stdin/os.Stdout still gate a live
// prompt. A write happens only when a human can review it on the same terminal
// that reads the answer.
func streamsInteractive(in io.Reader, out io.Writer) bool {
	return fileIsTerminal(in) && fileIsTerminal(out)
}

// fileIsTerminal reports whether a stream is a real terminal. A non-*os.File
// stream (a bytes.Buffer, a net pipe) is never a terminal, and an *os.File is
// tested with a real terminal predicate (an isatty ioctl), so a regular file, an
// anonymous pipe, or a character device that is not a tty (/dev/null) is false.
func fileIsTerminal(stream any) bool {
	f, ok := stream.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(f.Fd())
}

// searchAdvisoryContext is the constant guidance the hook injects for a
// repository-wide search. It is fixed text: the hook never reflects the caller's
// tool input, so it can neither leak nor echo attacker-controlled strings.
const searchAdvisoryContext = "Prowl indexes this repository for token-lean, cited code search. " +
	"Before a repository-wide scan, consider `prowl-agent search`, `prowl-agent find`, " +
	"`prowl-agent def`, `prowl-agent references`, or `prowl-agent outline`: they return " +
	"ranked, cited results without reading every file. This is advisory only."

// maxAdvisoryInput bounds the PreToolUse payload the hook will read so a runaway
// stdin can never exhaust memory. A real hook payload is a few kilobytes.
const maxAdvisoryInput = 1 << 20

// advisoryResponse is Claude's PreToolUse hook output shape for adding context.
// It intentionally has no permission-decision field: the hook informs, it never
// approves, asks, defers, or denies.
type advisoryResponse struct {
	HookSpecificOutput advisoryHook `json:"hookSpecificOutput"`
}

type advisoryHook struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// newSearchAdvisoryCmd is the hidden Claude hook: it reads one PreToolUse JSON
// object from stdin and, only for a repository-wide search, prints Claude's
// additionalContext advisory. It is not a human command and never appears in
// help.
func newSearchAdvisoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_search-advisory",
		Short:  "Claude PreToolUse hook that nudges broad searches toward Prowl (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSearchAdvisory(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// runSearchAdvisory reads one PreToolUse object, classifies it, and emits either
// nothing or the constant advisory. It always fails open: any read, parse, or
// classification miss produces no output and a zero exit, because the hook is an
// advisory and must never block or perturb the agent.
func runSearchAdvisory(in io.Reader, out io.Writer) error {
	payload, err := io.ReadAll(io.LimitReader(in, maxAdvisoryInput))
	if err != nil {
		return nil
	}
	if !advisorySuggests(payload) {
		return nil
	}
	data, err := json.Marshal(advisoryResponse{
		HookSpecificOutput: advisoryHook{
			HookEventName:     "PreToolUse",
			AdditionalContext: searchAdvisoryContext,
		},
	})
	if err != nil {
		return nil
	}
	fmt.Fprintln(out, string(data))
	return nil
}

// advisorySuggests is the whole classifier: it flags only a repository-wide
// Grep/Glob and a shell rg/grep/find, and nothing else. It is deliberately
// conservative -- a false negative just means no nudge, while a false positive
// would train the agent to ignore the hook.
func advisorySuggests(payload []byte) bool {
	var req struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return false
	}
	switch req.ToolName {
	case "Grep", "Glob":
		return grepGlobIsRepoWide(req.ToolInput)
	case "Bash":
		return bashRunsBroadSearch(req.ToolInput)
	default:
		return false
	}
}

// grepGlobIsRepoWide reports whether a native Grep/Glob has no narrowing path: an
// absent, empty, "." , "./", or "/" path scans the whole repository. Any other
// path is a bounded search and earns no advisory.
func grepGlobIsRepoWide(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return false
	}
	switch strings.TrimSpace(in.Path) {
	case "", ".", "./", "/":
		return true
	default:
		return false
	}
}

// bashRunsBroadSearch reports whether a Bash command runs a repository-wide
// rg/grep/find. It tokenizes the command with a small quote-aware scanner (so a
// separator or search word inside quotes is inert) and inspects each pipeline
// segment: a segment is flagged only when its command word is a search binary
// and its operands do not bound the search to a named file or subdirectory. A
// segment whose command word is prowl-agent is never flagged -- but only that
// segment, and only when prowl-agent is the command, never the search text.
func bashRunsBroadSearch(raw json.RawMessage) bool {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return false
	}
	for _, segment := range shellSegments(in.Command) {
		if segmentIsBroadSearch(segment) {
			return true
		}
	}
	return false
}

// segmentIsBroadSearch classifies one pipeline segment by its command word,
// skipping leading VAR=value assignments. Only rg/grep/find with unbounded
// operands is a broad search; every other command word -- prowl-agent included
// -- is not.
func segmentIsBroadSearch(tokens []string) bool {
	i := 0
	for i < len(tokens) && envAssignment.MatchString(tokens[i]) {
		i++
	}
	if i >= len(tokens) {
		return false
	}
	args := tokens[i+1:]
	switch path.Base(tokens[i]) {
	case "find":
		return findIsRepoWide(args)
	case "rg", "grep", "egrep", "fgrep":
		return grepIsRepoWide(args)
	default:
		return false
	}
}

// grepIsRepoWide reports whether a grep-family invocation is unbounded. Normally
// the first non-flag operand is the pattern and any following operand is a file
// or directory that bounds the search. When the pattern is attached to a flag
// (-eTODO, -fFILE, --regexp=, --file=) there is no positional pattern, so every
// operand is a path. A bare pattern (or a pipeline stage reading stdin) and a
// repository-root operand (".", "./", "/") are repo-wide.
func grepIsRepoWide(args []string) bool {
	paths := nonFlagOperands(args)
	if !patternAttachedToFlag(args) && len(paths) > 0 {
		// The first positional operand is the pattern; the rest are paths.
		paths = paths[1:]
	}
	if len(paths) == 0 {
		return true
	}
	return pathsRepoWide(paths)
}

// patternAttachedToFlag reports whether a grep-family pattern is supplied through
// a flag rather than a positional operand: --regexp=/--file= (long) or a short
// cluster whose -e/-f option carries its value in the same token (-eTODO,
// -neTODO, -fpatterns). A bare -e/-f (separate-form value) is not attached: its
// value sits as the first positional operand and the default parsing already
// treats it as the pattern.
func patternAttachedToFlag(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--regexp=") || strings.HasPrefix(arg, "--file=") {
			return true
		}
		if attachedShortPattern.MatchString(arg) {
			return true
		}
	}
	return false
}

// attachedShortPattern matches a short-option cluster whose -e or -f carries an
// attached value (at least one trailing char), e.g. -eTODO or -neTODO. A bare
// -e/-f (no trailing value) does not match.
var attachedShortPattern = regexp.MustCompile(`^-[A-Za-z]*[ef].+`)

// findIsRepoWide reports whether a find invocation scans the whole repository.
// find's grammar is `find [global-options] [start-paths] [expression]`, so the
// pre-path global options (-H/-L/-P, -D value, -O[level], --) are skipped first;
// the leading operands that follow, before the first expression token (`-...`,
// `(`, `!`), are the start paths. No start path (find defaults to the working
// directory) or a repository-root path is repo-wide; a named subdirectory bounds
// it.
func findIsRepoWide(args []string) bool {
	var paths []string
	for _, arg := range skipFindGlobals(args) {
		if strings.HasPrefix(arg, "-") || arg == "(" || arg == "!" {
			break
		}
		paths = append(paths, arg)
	}
	if len(paths) == 0 {
		return true
	}
	return pathsRepoWide(paths)
}

// skipFindGlobals returns args with find's recognized pre-path global options
// removed, conservatively consuming the values of -D and -O. `--` ends option
// parsing. An unrecognized leading token (a start path or an expression) stops
// the skip so classification proceeds on the remainder.
func skipFindGlobals(args []string) []string {
	i := 0
	for i < len(args) {
		switch arg := args[i]; {
		case arg == "-H", arg == "-L", arg == "-P":
			i++
		case arg == "--":
			i++
			return args[min(i, len(args)):]
		case arg == "-D", arg == "-O":
			i += 2 // separate-form value (debug flags / optimisation level)
		case strings.HasPrefix(arg, "-D"), strings.HasPrefix(arg, "-O"):
			i++ // attached value, e.g. -O3
		default:
			return args[i:]
		}
	}
	return args[min(i, len(args)):]
}

func nonFlagOperands(args []string) []string {
	var operands []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			operands = append(operands, arg)
		}
	}
	return operands
}

func pathsRepoWide(paths []string) bool {
	for _, p := range paths {
		switch strings.TrimSpace(p) {
		case ".", "./", "/":
			return true
		}
	}
	return false
}

// envAssignment matches a leading VAR=value prefix a segment may carry before its
// actual command word.
var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// shellSegments tokenizes a shell command into pipeline/sequence segments, each a
// slice of tokens, with a small quote-aware scanner. It honors single and double
// quotes and backslash escapes so a separator or command name inside quotes is
// inert, and it splits only on unquoted |, ||, &, &&, ;, and newline. It does
// not expand variables, command substitutions, or redirections. Unbalanced input
// -- an unmatched quote or a dangling backslash -- is unparseable, so it returns
// nil (no segments) and the classifier stays silent. Anything else it cannot
// resolve simply fails to look like a bare search command, which keeps the
// classifier erring toward staying silent (a false negative, never a false
// positive).
func shellSegments(command string) [][]string {
	var (
		segments [][]string
		tokens   []string
		buf      strings.Builder
		quote    rune
		started  bool
	)
	flushToken := func() {
		if started {
			tokens = append(tokens, buf.String())
			buf.Reset()
			started = false
		}
	}
	flushSegment := func() {
		flushToken()
		if len(tokens) > 0 {
			segments = append(segments, tokens)
			tokens = nil
		}
	}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				buf.WriteRune(c)
				started = true
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			started = true
		case ' ', '\t', '\r':
			flushToken()
		case ';', '\n':
			flushSegment()
		case '|':
			if i+1 < len(runes) && runes[i+1] == '|' {
				i++
			}
			flushSegment()
		case '&':
			if i+1 < len(runes) && runes[i+1] == '&' {
				i++
			}
			flushSegment()
		case '#':
			if started {
				buf.WriteRune(c) // an embedded '#' (foo#bar) is data, not a comment
				break
			}
			// A '#' at a word boundary begins a comment: skip through end of line.
			// The newline itself is left for the next iteration to flush a segment,
			// so parsing resumes after the comment.
			for i+1 < len(runes) && runes[i+1] != '\n' {
				i++
			}
		case '\\':
			if i+1 >= len(runes) {
				return nil // dangling escape: unparseable -> fail closed (silent)
			}
			i++
			buf.WriteRune(runes[i])
			started = true
		default:
			buf.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil // unmatched quote: unparseable -> fail closed (silent)
	}
	flushSegment()
	return segments
}
