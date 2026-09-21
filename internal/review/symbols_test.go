package review

import (
	"errors"
	"testing"

	"github.com/neur0map/prowl/internal/parse/extract"
)

type failingSymbolExtractor struct{}

func (failingSymbolExtractor) Lang() string { return "go" }
func (failingSymbolExtractor) Extract([]byte) (extract.Result, error) {
	return extract.Result{}, errors.New("synthetic parser failure")
}

func TestSymbolMapsDeletionOnlyAndEveryTouchedSymbol(t *testing.T) {
	record := RawPathRecord{OldPath: "old.go", Status: "D", Hunks: []RawHunk{{
		Ordinal: 0, OldStart: 3, OldLines: 3, NewStart: 0, NewLines: 0,
		Payload: []byte("-type T struct{}\n-func (T) M() {}\n-func helper() {}\n"),
	}}}
	base := []byte("package p\n\ntype T struct{}\nfunc (T) M() {}\nfunc helper() {}\n")
	mapped, omissions := (SymbolMapper{}).Map(record, base, nil)
	if len(omissions) != 0 {
		t.Fatalf("unexpected omissions: %#v", omissions)
	}
	if len(mapped) != 1 || len(mapped[0].BaseSymbols) != 3 || len(mapped[0].HeadSymbols) != 0 {
		t.Fatalf("deletion mapping = %#v", mapped)
	}
}

func TestSymbolUnsupportedAndParserFailureKeepAtomicHunk(t *testing.T) {
	record := RawPathRecord{OldPath: "x.unknown", NewPath: "x.unknown", Status: "M", Hunks: []RawHunk{{
		Ordinal: 0, OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Payload: []byte("-old\n+new\n"),
	}}}
	mapped, omissions := (SymbolMapper{}).Map(record, []byte("old\n"), []byte("new\n"))
	if len(mapped) != 1 || len(omissions) != 2 || mapped[0].Kind != HunkMappingAtomicFallback {
		t.Fatalf("unsupported mapping = %#v, omissions = %#v", mapped, omissions)
	}

	mapper := SymbolMapper{
		Detect: func(string, []byte) string { return "go" },
		For:    func(string) (extract.Extractor, bool) { return failingSymbolExtractor{}, true },
	}
	mapped, omissions = mapper.Map(record, []byte("old\n"), []byte("new\n"))
	if len(mapped) != 1 || len(omissions) != 2 || mapped[0].Kind != HunkMappingAtomicFallback {
		t.Fatalf("parser failure mapping = %#v, omissions = %#v", mapped, omissions)
	}
}

func TestHunkGroupsAdjacentSameSymbolsWithoutSplitting(t *testing.T) {
	record := RawPathRecord{OldPath: "x.go", NewPath: "x.go", Status: "M", Hunks: []RawHunk{
		{Ordinal: 0, OldStart: 4, OldLines: 1, NewStart: 4, NewLines: 1, Payload: []byte("-\treturn 1\n+\treturn 2\n")},
		{Ordinal: 1, OldStart: 8, OldLines: 1, NewStart: 8, NewLines: 1, Payload: []byte("-\treturn 3\n+\treturn 4\n")},
	}}
	src := []byte("package p\n\nfunc f() int {\n\treturn 1\n\n\n\n\treturn 3\n}\n")
	mapped, _ := (SymbolMapper{}).Map(record, src, src)
	groups := GroupHunkMappings(mapped)
	if len(groups) != 1 || len(groups[0].Hunks) != 2 {
		t.Fatalf("groups = %#v", groups)
	}
	if groups[0].Hunks[0].Ordinal != 0 || groups[0].Hunks[1].Ordinal != 1 {
		t.Fatalf("hunks reordered: %#v", groups[0].Hunks)
	}
}

func TestSymbolSemanticTargetsUseCompleteDeclarationSets(t *testing.T) {
	record := RawPathRecord{OldPath: "x.go", NewPath: "x.go", Status: "M", Hunks: []RawHunk{{
		Ordinal: 0, OldStart: 2, OldLines: 3, NewStart: 2, NewLines: 2,
		Payload: []byte(" func f() {\n-\tprintln(\"removed body line\")\n }\n"),
	}}}
	base := []byte("package p\nfunc f() {\n\tprintln(\"removed body line\")\n}\n")
	head := []byte("package p\nfunc f() {\n}\n")
	mapped, omissions := (SymbolMapper{}).Map(record, base, head)
	if len(omissions) != 0 {
		t.Fatalf("unexpected omissions: %#v", omissions)
	}
	if targets := semanticTargets(StableID{Public: "p_test"}, mapped); len(targets) != 0 {
		t.Fatalf("retained function inferred as removed: %#v", targets)
	}

	baseField := SymbolRef{Name: "existing", Kind: "field", StartLine: 2, EndLine: 2}
	addedField := SymbolRef{Name: "enabled", Kind: "field", StartLine: 3, EndLine: 3}
	fieldMapping := HunkSymbolMapping{
		HeadSymbols:       []SymbolRef{addedField},
		BaseDeclarations:  []SymbolRef{baseField},
		HeadDeclarations:  []SymbolRef{baseField, addedField},
		SemanticAvailable: true,
	}
	targets := semanticTargets(StableID{Public: "p_test"}, []HunkSymbolMapping{fieldMapping})
	if len(targets) != 1 || targets[0].Kind != TargetAddedFieldOption || targets[0].Symbol.Name != "enabled" {
		t.Fatalf("added field targets = %#v", targets)
	}

	fallback, _ := (SymbolMapper{}).Map(
		RawPathRecord{OldPath: "x.unknown", NewPath: "x.unknown", Status: "M", Hunks: []RawHunk{{Ordinal: 0, OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Payload: []byte("-old\n+new\n")}}},
		[]byte("old\n"), []byte("new\n"),
	)
	if targets := semanticTargets(StableID{Public: "p_test"}, fallback); len(targets) != 0 {
		t.Fatalf("fallback emitted semantic targets: %#v", targets)
	}
}
