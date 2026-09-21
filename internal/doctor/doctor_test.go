package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/graph"
	"github.com/neur0map/prowl/internal/redact"
	"github.com/neur0map/prowl/internal/store"
)

func TestDoctor(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel, role string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "generic", Role: role, Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := mk("a.conf", "")
	b := mk("b.conf", "")
	theme := mk("themes/t.conf", "theme")
	kb := mk("hypr/hyprland.conf", "wm-config")
	mk("scripts/orphan.sh", "script") // referenced by nobody
	mk("scripts/s.sh", "script")      // referenced by theme (forbidden)

	// cyclic includes: a <-> b
	must(t, s.ReplaceFileGraph(a, nil, nil, []store.RawEdge{{Kind: "includes", Raw: "b.conf", Line: 1}}, nil))
	must(t, s.ReplaceFileGraph(b, nil, nil, []store.RawEdge{{Kind: "includes", Raw: "a.conf", Line: 1}}, nil))
	// theme execs a script (forbidden crossing themes -> scripts)
	must(t, s.ReplaceFileGraph(theme, nil, nil, []store.RawEdge{{Kind: "execs", Raw: "scripts/s.sh", Line: 1}}, nil))
	// duplicate keybinds + broken command + dangling include
	must(t, s.ReplaceFileGraph(kb,
		[]store.Symbol{
			{Name: "$mod Q", Kind: "keybind", StartLine: 1, EndLine: 1},
			{Name: "$mod, Q", Kind: "keybind", StartLine: 2, EndLine: 2},
		}, nil,
		[]store.RawEdge{
			{SrcName: "$mod Q", Kind: "binds", Raw: "prowl_absent_cmd_zzz --x", Line: 1},
			{Kind: "includes", Raw: "missing/x.conf", Line: 3},
		}, nil))

	if err := graph.Resolve(s); err != nil {
		t.Fatal(err)
	}

	rules := config.Rules{Forbid: []config.Forbid{{Name: "no-theme-scripts", From: "themes", To: "scripts"}}}
	rep, err := Run(s, rules, Options{Profile: ProfileRice})
	if err != nil {
		t.Fatal(err)
	}

	has := func(check string) bool {
		for _, f := range rep.Findings {
			if f.Check == check {
				return true
			}
		}
		return false
	}
	for _, c := range []string{
		"cyclic_include", "duplicate_keybind", "broken_command",
		"orphan_script", "dangling_reference", "forbidden_crossing",
	} {
		if !has(c) {
			t.Errorf("doctor missing check %q; findings=%+v", c, rep.Findings)
		}
	}
	if rep.Score >= 100 {
		t.Errorf("score = %d, want < 100 with errors present", rep.Score)
	}
	if rep.Summary["cyclic_include"] == 0 {
		t.Errorf("summary missing cyclic_include: %+v", rep.Summary)
	}

	general, err := Run(s, rules, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, riceOnly := range []string{"duplicate_keybind", "broken_command", "orphan_script", "hardcoded_color"} {
		for _, finding := range general.Findings {
			if finding.Check == riceOnly {
				t.Errorf("general doctor included rice-only check %q: %+v", riceOnly, general.Findings)
			}
		}
	}
	for _, required := range []string{"cyclic_include", "dangling_reference", "forbidden_crossing"} {
		found := false
		for _, finding := range general.Findings {
			found = found || finding.Check == required
		}
		if !found {
			t.Errorf("general doctor missing %q: %+v", required, general.Findings)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestCheckUnindexedLanguages proves doctor flags a language present on disk but
// missing from the index (the silent language-filter misconfiguration), and does
// not warn when that language is indexed.
func TestCheckUnindexedLanguages(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%d.go", i))
		if err := os.WriteFile(p, []byte("package p\nfunc F() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	unindexed, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer unindexed.Close()
	got, err := checkUnindexedLanguages(unindexed, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Check != "unindexed-language" || got[0].Severity != SevWarn {
		t.Fatalf("want one unindexed-language warning, got %+v", got)
	}

	indexed, err := store.Open(filepath.Join(t.TempDir(), "i2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer indexed.Close()
	for i := 0; i < 12; i++ {
		if _, err := indexed.UpsertFile(store.File{RelPath: fmt.Sprintf("f%d.go", i), Lang: "go", Hash: fmt.Sprint(i), Size: 1, MTime: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := checkUnindexedLanguages(indexed, Options{Root: root}); err != nil || len(got) != 0 {
		t.Fatalf("indexed go must not warn: got %+v err %v", got, err)
	}
}

// Masking protects the index; reporting is what lets a human fix the repository.
// The finding keys off the per-file redaction count the indexer persists
// (files.redacted) -- the durable record that a value was removed at index time
// -- so a doctor run over an already-built index still reports it, with the count
// and a warning severity.
func TestDoctorReportsHardcodedSecrets(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.UpsertFile(store.File{RelPath: "config.py", Lang: "python", Role: "source", Hash: "h", Size: 1, MTime: 1, Redacted: 2}); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(s, config.Rules{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == "hardcoded_secret" && rep.Findings[i].File == "config.py" {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("no hardcoded_secret finding in %+v", rep.Findings)
	}
	if found.Severity != SevWarn {
		t.Errorf("severity = %q, want %q", found.Severity, SevWarn)
	}
	if !strings.Contains(found.Detail, "2") {
		t.Errorf("detail %q should report the count 2", found.Detail)
	}
}

// A file whose source legitimately contains the mask string -- redact.go itself
// defines const Mask = "[redacted]" -- but had nothing masked (files.redacted 0)
// must not be reported. This is the defect R25 fixed: keying off chunk text made
// prowl flag the file that defines the marker.
func TestDoctorIgnoresMaskStringInSource(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fid, err := s.UpsertFile(store.File{RelPath: "redact.go", Lang: "go", Role: "source", Hash: "h", Size: 1, MTime: 1, Redacted: 0})
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.ReplaceFileGraph(fid, nil, nil, nil, []store.Chunk{
		{StartLine: 1, EndLine: 1, Text: `const Mask = "` + redact.Mask + `"`},
	}))

	rep, err := Run(s, config.Rules{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings {
		if f.Check == "hardcoded_secret" {
			t.Fatalf("mask string in source was falsely flagged: %+v", f)
		}
	}
}

// A credential masked in a file under an excluded lifecycle path (e.g. tests/)
// must stay reachable. filterExcluded drops non-actionable graph noise under
// those paths, but a leaked key is actionable wherever it lives -- test fixtures
// are one of the most common places real credentials get committed. So doctor
// rolls excluded secrets into a discoverable info summary by default, and
// --include-excluded surfaces the per-file finding. The credential must never
// vanish silently, which is exactly what plain path exclusion did.
func TestDoctorSurfacesSecretsUnderExcludedPaths(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.UpsertFile(store.File{RelPath: "tests/fixtures/creds.py", Lang: "python", Role: "source", Hash: "h", Size: 1, MTime: 1, Redacted: 3}); err != nil {
		t.Fatal(err)
	}

	// Default view: the per-file finding is filtered out of the live graph, but a
	// rollup info finding records that a masked credential lives under an excluded
	// path (info, so it does not trip --fail-on warn on a repo of safe fixtures).
	rep, err := Run(s, config.Rules{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings {
		if f.Check == "hardcoded_secret" && f.File == "tests/fixtures/creds.py" {
			t.Fatalf("excluded-path secret should not be a per-file finding by default: %+v", f)
		}
	}
	var rollup *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == "hardcoded_secret_excluded" {
			rollup = &rep.Findings[i]
		}
	}
	if rollup == nil {
		t.Fatalf("no rollup finding for excluded-path secret in %+v", rep.Findings)
	}
	if rollup.Severity != SevInfo {
		t.Errorf("rollup severity = %q, want %q (must not trip --fail-on warn)", rollup.Severity, SevInfo)
	}
	if !strings.Contains(rollup.Detail, "1") || !strings.Contains(rollup.Detail, "include-excluded") {
		t.Errorf("rollup detail %q should report the count and how to list the files", rollup.Detail)
	}

	// Unfiltered view: --include-excluded surfaces the per-file finding so the
	// full list of redacted files is reachable, and drops the now-redundant rollup.
	all, err := Run(s, config.Rules{}, Options{IncludeExcluded: true})
	if err != nil {
		t.Fatal(err)
	}
	var perFile *Finding
	for i := range all.Findings {
		if all.Findings[i].Check == "hardcoded_secret" && all.Findings[i].File == "tests/fixtures/creds.py" {
			perFile = &all.Findings[i]
		}
	}
	if perFile == nil {
		t.Fatalf("--include-excluded must surface the per-file secret; findings=%+v", all.Findings)
	}
	if perFile.Severity != SevWarn {
		t.Errorf("per-file severity = %q, want %q", perFile.Severity, SevWarn)
	}
	for _, f := range all.Findings {
		if f.Check == "hardcoded_secret_excluded" {
			t.Errorf("--include-excluded should not emit the rollup: %+v", f)
		}
	}
}
