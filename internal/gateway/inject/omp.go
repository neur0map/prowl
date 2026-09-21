package inject

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ── OMP ──────────────────────────────────────────────────────────────────────
//
// OMP reads custom providers from ~/.omp/agent/models.yml. Routing-only on
// purpose: a `discovery: {type: proxy}` entry would pull the gateway's whole
// free catalogue into the picker, which is the opposite of letting the gateway
// route. The file is YAML a human may have hand-edited, so this writer edits
// text surgically - it manages exactly the block that starts at the provider's
// key line and ends before the next line at the same or shallower indent.

func (o Options) ompPath() string { return filepath.Join(o.Home, ".omp", "agent", "models.yml") }

type ompWriter struct{}

func (ompWriter) apply(o Options) (Target, error) {
	path := o.ompPath()
	block := renderOMPBlock(o)

	created, err := writeBackup(o.tx, path)
	if err != nil {
		return Target{}, err
	}
	raw, err := os.ReadFile(path)
	yamlPrior := ""
	switch {
	case os.IsNotExist(err):
		blob := "providers:\n" + block
		if err := writeFile(path, []byte(blob), 0o600); err != nil {
			return Target{}, err
		}
	case err != nil:
		return Target{}, err
	default:
		text := string(raw)
		// Replace either provider key written before the product rename rather
		// than leaving duplicate Prowl entries in the model picker. Loop because
		// a machine can carry both generations after repeated old injections.
		for {
			start, end, ok := ompBlockRangeFor(text, legacyProviderID, legacyGatewayProviderID)
			if !ok {
				break
			}
			text = text[:start] + text[end:]
		}
		if start, end, ok := ompBlockRange(text); ok {
			yamlPrior = text[start:end]
			text = text[:start] + block + text[end:]
		} else {
			text, err = insertProviderBlock(text, block)
			if err != nil {
				return Target{}, err
			}
		}
		if err := writeFile(path, []byte(text), credentialMode(path)); err != nil {
			return Target{}, err
		}
	}
	t := Target{Harness: "omp", Files: []string{path},
		Note: "select a prowl model in omp; created " + boolWord(created)}
	if created {
		t.Ledger = []writtenEntry{{Path: path, CreatedFile: true}}
	} else {
		t.Ledger = []writtenEntry{{Path: path, YamlBlock: ProviderID,
			YamlAuthored: block, YamlPrior: yamlPrior}}
	}
	return t, nil
}

func (ompWriter) remove(home, harness string) (Target, error) {
	path := filepath.Join(home, ".omp", "agent", "models.yml")
	t := Target{Harness: harness, Files: []string{path}}
	loaded, err := loadRecord(home)
	if err != nil {
		return t, err
	}
	rec := loaded.Targets[harness]
	if len(rec.Ledger) == 0 {
		return t, fmt.Errorf("no injection record for %s - refusing to guess what to remove", harness)
	}
	e := rec.Ledger[0]
	// Refuse a symlink swapped in after apply before any read or write, exactly
	// as apply does - a dangling one would otherwise read as "already removed"
	// and let Remove forget the ledger. The error keeps the ledger for a retry.
	if err := refuseSymlink(path); err != nil {
		return t, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	// A file we created is deleted only while it still holds exactly what we
	// wrote; a diverged file falls through to block revision so later user
	// edits survive.
	if e.CreatedFile && createdFileUnchanged(raw, e.CreatedContent) {
		if err := os.Remove(path); err != nil {
			return t, err
		}
		t.Note = "removed the models.yml we created"
		return t, nil
	}
	newText, outcome := reviseYAMLBlock(string(raw), e, ProviderID, legacyProviderID, legacyGatewayProviderID)
	switch outcome {
	case blockAbsent:
		t.Note = "the gateway block is not present (already removed or renamed?)"
		return t, nil
	case blockConflict:
		t.Note = "kept your edits to the gateway provider block (conflict); left the file unchanged"
		return t, nil
	}
	// Propagate the write failure: Remove() only calls forgetRecord after a
	// nil error, so a failed revision must not drop our ownership while the
	// block is still in the file.
	if err := writeFile(path, []byte(newText), credentialMode(path)); err != nil {
		return t, err
	}
	t.Note = yamlRemovalNote(outcome, e.CreatedFile)
	return t, nil
}

func ompBlockRange(text string) (int, int, bool) {
	return ompBlockRangeFor(text, ProviderID)
}

// blockOutcome is what a removal did (or refused to do) to a managed YAML
// provider block.
type blockOutcome int

const (
	blockAbsent   blockOutcome = iota // no managed block is present
	blockConflict                     // the user edited our block; left intact
	blockRestored                     // a pre-existing block was restored
	blockExcised                      // a block we added was removed
)

// reviseYAMLBlock reverts a managed YAML provider block per the ledger entry.
// It restores the user's pre-existing block (YamlPrior) when our authored
// block is still on disk, excises a block we added, and preserves - reporting
// a conflict - a block the user has edited since apply. A block whose ledger
// predates the YamlAuthored guard (empty YamlAuthored) is reverted
// unconditionally, keeping the historical excise-on-removal behaviour.
func reviseYAMLBlock(text string, e writtenEntry, providerIDs ...string) (string, blockOutcome) {
	start, end, ok := ompBlockRangeFor(text, providerIDs...)
	if !ok {
		return text, blockAbsent
	}
	if !authoredBlockIntact(text[start:end], e.YamlAuthored) {
		return text, blockConflict
	}
	if e.YamlPrior != "" {
		return text[:start] + e.YamlPrior + text[end:], blockRestored
	}
	return text[:start] + text[end:], blockExcised
}

// yamlRemovalNote describes a completed block revision for the user.
func yamlRemovalNote(outcome blockOutcome, createdFile bool) string {
	switch {
	case outcome == blockRestored:
		return "restored your pre-existing provider block"
	case createdFile:
		return "kept your edits; removed the gateway provider block"
	default:
		return "removed the gateway provider block"
	}
}

// ompBlockRangeFor returns the byte range of the provider block whose key is
// one of providerIDs AND is a direct child of the top-level `providers:`
// mapping. A same-named key nested anywhere else - under another provider, or
// under an unrelated top-level section - is deliberately ignored: these
// writers manage exactly the one provider they wrote, never a coincidental
// `prowl:` the user happens to have elsewhere.
func ompBlockRangeFor(text string, providerIDs ...string) (int, int, bool) {
	lines := strings.SplitAfter(text, "\n")
	var off int
	inProviders := false
	childIndent := -1
	start := -1
	startIndent := 0
	for _, ln := range lines {
		trimmed := strings.TrimRight(ln, "\n")
		body := strings.TrimLeft(trimmed, " \t")
		indent := len(trimmed) - len(body)
		if start < 0 {
			switch {
			case body == "" || strings.HasPrefix(body, "#"):
				// Blank and comment lines neither open nor close the mapping.
			case indent == 0:
				// A top-level key: enter the providers mapping or leave it.
				inProviders = body == "providers:"
				childIndent = -1
			case inProviders:
				// The first non-blank child fixes the mapping's child indent;
				// only keys at exactly that depth are direct children.
				if childIndent < 0 {
					childIndent = indent
				}
				if indent == childIndent {
					for _, providerID := range providerIDs {
						if body == providerID+":" || strings.HasPrefix(body, providerID+": ") {
							start = off
							startIndent = indent
							break
						}
					}
				}
			}
		} else if body != "" && indent <= startIndent && !strings.HasPrefix(body, "#") {
			// End at the next non-blank, non-comment line no deeper than the
			// block's key line.
			return start, off, true
		}
		off += len(ln)
	}
	if start >= 0 {
		return start, len(text), true
	}
	return 0, 0, false
}

// insertProviderBlock inserts block (a two-space-indented provider entry
// ending in "\n") as a child of the top-level `providers:` mapping, creating
// that mapping when absent. It is the single structural rule both YAML writers
// use once they know the provider is not already present, and it never emits
// invalid YAML: it guarantees a newline boundary before the inserted child
// (fixing a file with no trailing newline, which used to splice the block onto
// the last line) and converts an empty non-block `providers:` (`providers:`,
// `providers: {}`, `providers: null`, `providers: []`) into a block mapping
// rather than appending a child that would not parse. A populated inline flow
// mapping is refused rather than corrupted.
func insertProviderBlock(text, block string) (string, error) {
	lineStart, afterKey, lineEnd, ok := providersHeader(text)
	if !ok {
		// No providers mapping at all: append a fresh one after a newline
		// boundary, with a blank separator only when there is prior content.
		out := text
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if strings.TrimSpace(out) != "" {
			out += "\n"
		}
		return out + "providers:\n" + block, nil
	}
	inline, comment := splitYAMLComment(strings.TrimRight(text[afterKey:lineEnd], "\n"))
	inline = strings.TrimSpace(inline)
	switch {
	case inline == "":
		// Block mapping (children may or may not already exist). Insert after
		// the last child, guaranteeing the text up to there ends in a newline.
		at := providersChildrenEnd(text, lineEnd)
		prefix := text[:at]
		if !strings.HasSuffix(prefix, "\n") {
			prefix += "\n"
		}
		return prefix + block + text[at:], nil
	case isEmptyInlineMapping(inline):
		// Drop the empty inline value so the key becomes a block mapping,
		// preserving any trailing comment, then add our child.
		header := "providers:"
		if comment != "" {
			header += " " + comment
		}
		return text[:lineStart] + header + "\n" + block + text[lineEnd:], nil
	default:
		return "", fmt.Errorf("providers: uses an inline value I can't edit safely; move it to a block mapping first")
	}
}

// providersHeader locates the top-level `providers:` line, returning the byte
// offset of the line start, the offset just past the `providers:` key, and the
// offset just past the line (including its newline, or end of file).
func providersHeader(text string) (lineStart, afterKey, lineEnd int, ok bool) {
	const key = "providers:"
	off := 0
	for _, ln := range strings.SplitAfter(text, "\n") {
		trimmedRight := strings.TrimRight(ln, "\n")
		body := strings.TrimLeft(trimmedRight, " \t")
		indent := len(trimmedRight) - len(body)
		if indent == 0 && strings.HasPrefix(body, key) {
			return off, off + len(key), off + len(ln), true
		}
		off += len(ln)
	}
	return 0, 0, 0, false
}

// providersChildrenEnd returns the offset after the last line belonging to the
// providers mapping, scanning from `from` (the start of the first line after
// the header). The mapping ends at the first non-blank, non-comment line at
// indent 0 (a new top-level key); trailing blank lines stay outside it so an
// inserted block sits directly beneath the last child.
func providersChildrenEnd(text string, from int) int {
	off := from
	last := from
	for _, ln := range strings.SplitAfter(text[from:], "\n") {
		trimmedRight := strings.TrimRight(ln, "\n")
		body := strings.TrimLeft(trimmedRight, " \t")
		indent := len(trimmedRight) - len(body)
		if body != "" && !strings.HasPrefix(body, "#") && indent == 0 {
			return last
		}
		if body != "" {
			last = off + len(ln)
		}
		off += len(ln)
	}
	return last
}

// splitYAMLComment splits an inline segment into its value and a trailing
// comment (a '#' at the segment start or preceded by whitespace).
func splitYAMLComment(seg string) (value, comment string) {
	for i := range len(seg) {
		if seg[i] != '#' {
			continue
		}
		if i == 0 || seg[i-1] == ' ' || seg[i-1] == '\t' {
			return seg[:i], strings.TrimSpace(seg[i:])
		}
	}
	return seg, ""
}

// isEmptyInlineMapping reports whether an inline `providers:` value is an empty
// mapping/sequence or null - a non-block form we can safely convert to a block
// mapping without losing user data.
func isEmptyInlineMapping(inline string) bool {
	switch inline {
	case "{}", "{ }", "null", "Null", "NULL", "~", "[]", "[ ]":
		return true
	}
	return false
}

func renderOMPBlock(o Options) string {
	var b strings.Builder
	b.WriteString("  " + ProviderID + ":\n")
	b.WriteString("    baseUrl: " + o.BaseURL + "\n")
	b.WriteString("    api: openai-completions\n")
	b.WriteString("    apiKey: " + quoteYAML(o.Token) + "\n")
	b.WriteString("    authHeader: true\n")
	b.WriteString("    disableStrictTools: true\n")
	b.WriteString("    models:\n")
	for _, m := range o.Models {
		b.WriteString(fmt.Sprintf("      - id: %s\n", m.ID))
		b.WriteString(fmt.Sprintf("        name: %s\n", quoteYAML(m.Name)))
		b.WriteString(fmt.Sprintf("        contextWindow: %d\n", m.Context))
		b.WriteString(fmt.Sprintf("        maxTokens: %d\n", m.MaxTokens))
	}
	return b.String()
}

func quoteYAML(s string) string {
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`,\n") || strings.TrimSpace(s) != s {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func boolWord(b bool) string {
	if b {
		return "file"
	}
	return "merged"
}
