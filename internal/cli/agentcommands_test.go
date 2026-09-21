package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/capability"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/skills"
)

// The command tree is the only source of truth for what prowl can run, so
// every command Prowl teaches an agent to run must resolve against it. Drift is
// silent otherwise: a renamed command or dropped flag ships while the capability
// manifests, the installed skills, and the injected project map keep advertising
// an invocation that now exits non-zero, and the agent pays for it.
func TestAgentFacingCommandsResolveAgainstTheCommandTree(t *testing.T) {
	root := &cobra.Command{Use: "prowl"}
	Register(root, "test", "")
	commands := commandPaths(root)
	if len(commands) == 0 {
		t.Fatal("no commands registered")
	}

	for _, source := range agentFacingCommandSources(t) {
		for _, invocation := range extractInvocations(source.text) {
			path, remainder, ok := resolveInvocation(commands, invocation)
			if !ok {
				t.Errorf("%s advertises %q, but %q is not a prowl command",
					source.name, invocation, strings.Fields(invocation)[0])
				continue
			}
			command := commands[path]
			for _, flag := range citedFlags(remainder) {
				name := strings.TrimPrefix(flag, "--")
				if command.Flags().Lookup(name) == nil && command.InheritedFlags().Lookup(name) == nil &&
					root.PersistentFlags().Lookup(name) == nil {
					t.Errorf("%s advertises %q, but %q has no %s flag", source.name, invocation, path, flag)
				}
			}
		}
	}
}

func TestAgentFacingCommandsIncludeNativeReviewOnly(t *testing.T) {
	root := &cobra.Command{Use: "prowl"}
	Register(root, "test", "")
	commands := commandPaths(root)
	for path, flags := range map[string][]string{
		"review plan":  {"base", "head", "commit", "structured"},
		"review unit":  {"budget-tokens", "budget-bytes"},
		"review check": {"review", "report"},
	} {
		command := commands[path]
		if command == nil {
			t.Fatalf("agent-facing command %q is not registered", path)
		}
		for _, flag := range flags {
			if command.Flags().Lookup(flag) == nil {
				t.Errorf("agent-facing command %q has no --%s flag", path, flag)
			}
		}
	}
	if _, ok := commands["serve review"]; ok {
		t.Fatal("review must remain a native CLI workflow, not an MCP command")
	}
}

// TestAgentFacingCommandTreeIncludesSkills proves the installer surface is part of
// the one command tree agents resolve against: `skills` is public (it appears in
// help), while `_search-advisory` -- a Claude hook helper, never invoked by a
// human -- is registered but hidden.
func TestAgentFacingCommandTreeIncludesSkills(t *testing.T) {
	root := &cobra.Command{Use: "prowl"}
	Register(root, "test", "")
	commands := commandPaths(root)

	skillsCmd, ok := commands["skills"]
	if !ok {
		t.Fatal("skills command is not registered")
	}
	if skillsCmd.Hidden {
		t.Error("skills command must be visible in help")
	}

	advisory, ok := commands["_search-advisory"]
	if !ok {
		t.Fatal("_search-advisory command is not registered")
	}
	if !advisory.Hidden {
		t.Error("_search-advisory command must stay hidden from help")
	}
}

// commandPaths indexes every registered command, hidden ones included, by its
// space-joined path. Hidden commands matter: `serve` and `lsp` are documented but
// absent from --help, so a check that scraped help output would flag them
// falsely.
func commandPaths(root *cobra.Command) map[string]*cobra.Command {
	commands := map[string]*cobra.Command{}
	var walk func(prefix string, cmd *cobra.Command)
	walk = func(prefix string, cmd *cobra.Command) {
		for _, child := range cmd.Commands() {
			path := strings.TrimSpace(prefix + " " + child.Name())
			commands[path] = child
			walk(path, child)
		}
	}
	walk("", root)
	return commands
}

type agentFacingSource struct {
	name string
	text string
}

// agentFacingCommandSources returns every shipped surface that tells an agent
// which prowl command to run.
func agentFacingCommandSources(t *testing.T) []agentFacingSource {
	t.Helper()
	catalog, err := capability.BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var sources []agentFacingSource
	for _, manifest := range catalog.All() {
		// Manifest recipes are already bare commands; wrap each in the code span
		// the extractor expects instead of teaching it a second input shape.
		sources = append(sources, agentFacingSource{
			name: "capability " + manifest.Name,
			text: "`" + strings.Join(manifest.Commands, "`\n`") + "`",
		})
	}
	for _, skill := range skills.All() {
		sources = append(sources, agentFacingSource{name: "skill " + skill.Name, text: skill.Content})
	}
	sources = append(sources, agentFacingSource{
		name: "project map block",
		text: projectMapBlock(query.Overview{}),
	})
	return sources
}

var (
	codeSpan     = regexp.MustCompile("`([^`\n]+)`")
	fencedBlock  = regexp.MustCompile("(?s)```[a-zA-Z]*\n(.*?)```")
	commandToken = regexp.MustCompile(`^[a-z][a-z0-9-]*(\|[a-z][a-z0-9-]*)*$`)
	flagToken    = regexp.MustCompile(`^--[a-z][a-z0-9-]*`)
)

// extractInvocations pulls `prowl ...` invocations out of code spans and fenced
// blocks only. Prose is deliberately ignored: a sentence like "reach for Prowl
// before grepping" names no command, and treating it as one would bury the real
// signal in noise.
func extractInvocations(text string) []string {
	var candidates []string
	for _, match := range fencedBlock.FindAllStringSubmatch(text, -1) {
		candidates = append(candidates, strings.Split(match[1], "\n")...)
	}
	for _, match := range codeSpan.FindAllStringSubmatch(text, -1) {
		candidates = append(candidates, match[1])
	}
	var invocations []string
	for _, candidate := range candidates {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "$ ")
		rest, found := strings.CutPrefix(strings.TrimSpace(candidate), "prowl ")
		if !found {
			continue
		}
		if rest = strings.TrimSpace(rest); rest != "" {
			invocations = append(invocations, rest)
		}
	}
	return invocations
}

// resolveInvocation greedily matches the longest command path an invocation
// names and returns that path with the tokens after it. A leading alternation
// (find|def|outline) must resolve in every branch.
func resolveInvocation(commands map[string]*cobra.Command, invocation string) (string, []string, bool) {
	tokens := strings.Fields(invocation)
	if len(tokens) == 0 {
		return "", nil, false
	}
	if alternatives := strings.Split(tokens[0], "|"); len(alternatives) > 1 {
		for _, alternative := range alternatives {
			if _, ok := commands[alternative]; !ok {
				return "", nil, false
			}
		}
		return alternatives[0], tokens[1:], true
	}
	best, consumed, path := "", 0, ""
	for index, token := range tokens {
		if !commandToken.MatchString(token) {
			break
		}
		if path == "" {
			path = token
		} else {
			path += " " + token
		}
		if _, ok := commands[path]; ok {
			best, consumed = path, index+1
		}
	}
	if best == "" {
		return "", nil, false
	}
	return best, tokens[consumed:], true
}

func citedFlags(tokens []string) []string {
	var flags []string
	for _, token := range tokens {
		if flagToken.MatchString(token) {
			flags = append(flags, strings.SplitN(flagToken.FindString(token), "=", 2)[0])
		}
	}
	return flags
}
