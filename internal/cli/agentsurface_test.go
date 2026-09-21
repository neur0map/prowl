package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/store"
)

// resolveWith runs resolveFormat inside a command that carries the persistent
// --format / --json flags, exactly as the real root registers them, and returns
// the resolved format (or the resolve error).
func resolveWith(t *testing.T, args ...string) (outputFormat, error) {
	t.Helper()
	root := &cobra.Command{Use: "root", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("format", "", "")
	root.PersistentFlags().Bool("json", false, "")

	var got outputFormat
	var resErr error
	sub := &cobra.Command{Use: "sub", RunE: func(c *cobra.Command, _ []string) error {
		got, resErr = resolveFormat(c, &bytes.Buffer{})
		return nil
	}}
	root.AddCommand(sub)
	root.SetArgs(append([]string{"sub"}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return got, resErr
}

// bindFormatFlags registers the persistent --format/--json flags locally on a
// command, so a test can drive it standalone the way the real root provides
// them as inherited persistent flags. resolveFormat reads local or inherited
// identically.
func bindFormatFlags(cmd *cobra.Command) *cobra.Command {
	cmd.Flags().String("format", "", "")
	cmd.Flags().Bool("json", false, "")
	return cmd
}

func TestResolveFormatUnifiesFlags(t *testing.T) {
	if f, err := resolveWith(t, "--json"); err != nil || f != formatJSON {
		t.Fatalf("--json => %v, %v; want json", f, err)
	}
	if f, err := resolveWith(t, "--format", "json"); err != nil || f != formatJSON {
		t.Fatalf("--format json => %v, %v; want json", f, err)
	}
	if f, err := resolveWith(t, "--format", "markdown"); err != nil || f != formatMarkdown {
		t.Fatalf("--format markdown => %v, %v; want markdown", f, err)
	}
	// --json and a conflicting --format is a usable error, not a silent pick.
	if _, err := resolveWith(t, "--json", "--format", "toon"); err == nil {
		t.Fatal("--json with --format toon should error")
	}
}

func TestEmitHintTeachesNextStep(t *testing.T) {
	var b bytes.Buffer
	emitHint(&b, "find", []store.SymbolHit{})
	if !strings.Contains(b.String(), "search") || !strings.Contains(b.String(), "capabilities") {
		t.Fatalf("empty find hint should route to search/capabilities: %q", b.String())
	}

	b.Reset()
	emitHint(&b, "find", []store.SymbolHit{{ID: 7, Name: "Foo"}, {ID: 8, Name: "Bar"}})
	if !strings.Contains(b.String(), "def 7") {
		t.Fatalf("ambiguous find hint should cite the top id: %q", b.String())
	}

	// A single unambiguous match is not noise; no hint.
	b.Reset()
	emitHint(&b, "find", []store.SymbolHit{{ID: 1, Name: "Only"}})
	if b.String() != "" {
		t.Fatalf("single match should not hint: %q", b.String())
	}
}

func TestSearchHintPointsAtPeek(t *testing.T) {
	var b bytes.Buffer
	searchHint(&b, nil, false)
	if !strings.Contains(b.String(), "no matches") {
		t.Fatalf("empty search hint: %q", b.String())
	}

	// Compact mode returns locations without snippets, so peek is the read step.
	b.Reset()
	searchHint(&b, []store.ChunkHit{{File: "a.go", StartLine: 5, EndLine: 9}}, true)
	if !strings.Contains(b.String(), "peek a.go:5-9") {
		t.Fatalf("compact search hint should cite peek: %q", b.String())
	}

	// Non-compact results already carry snippets; no peek nudge.
	b.Reset()
	searchHint(&b, []store.ChunkHit{{File: "a.go", StartLine: 1, EndLine: 3}}, false)
	if b.String() != "" {
		t.Fatalf("non-compact search should not hint: %q", b.String())
	}
}
