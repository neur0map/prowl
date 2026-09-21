package cli

import (
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

func TestFormatValueTOONTabular(t *testing.T) {
	hits := []store.SymbolHit{
		{ID: 1, Name: "battery", Kind: "widget", Signature: "", File: "bar/battery.lua", Line: 12, EndLine: 20},
		{ID: 2, Name: "battery_low", Kind: "function", Signature: "sig", File: "bar/battery.lua", Line: 40, EndLine: 45},
	}
	got, err := formatValue(hits, formatTOON)
	if err != nil {
		t.Fatalf("formatValue toon: %v", err)
	}
	// A uniform array of structs collapses to one header + CSV-style rows, fields
	// alphabetized via the json round-trip and named once in the header. SymbolHit
	// emits every column (no omitempty) so a mixed result set stays tabular.
	if !strings.Contains(got, "{end_line,file,id,kind,line,name,signature}") {
		t.Errorf("expected tabular header naming all columns, got:\n%s", got)
	}
	if !strings.Contains(got, "45,bar/battery.lua,2,function,40,battery_low,sig") {
		t.Errorf("expected a CSV-style data row in header order, got:\n%s", got)
	}
	if strings.Count(got, "name") != 1 {
		t.Errorf("field name should appear once (header only), got:\n%s", got)
	}
}

func TestFormatValueOmitemptyDropped(t *testing.T) {
	// The formatter honors json omitempty (relied on by EdgeRow/ResourceRow):
	// an empty omitempty field is dropped while the array stays tabular.
	rows := []struct {
		A string `json:"a"`
		B string `json:"b,omitempty"`
	}{
		{A: "x"},
		{A: "y"},
	}
	got, err := formatValue(rows, formatTOON)
	if err != nil {
		t.Fatalf("formatValue toon: %v", err)
	}
	if strings.Contains(got, "b") {
		t.Errorf("omitempty field b should be dropped when empty, got:\n%s", got)
	}
}

func TestFormatValueJSONMatchesTags(t *testing.T) {
	hits := []store.SymbolHit{{ID: 1, Name: "x", Kind: "k", File: "f", Line: 1, EndLine: 2}}
	got, err := formatValue(hits, formatJSON)
	if err != nil {
		t.Fatalf("formatValue json: %v", err)
	}
	if !strings.Contains(got, `"end_line"`) || !strings.Contains(got, `"line"`) {
		t.Errorf("json should use json tags (end_line, line), got: %s", got)
	}
}

func TestFormatValueTOONIsLeanerThanJSON(t *testing.T) {
	hits := make([]store.SymbolHit, 0, 20)
	for i := 0; i < 20; i++ {
		hits = append(hits, store.SymbolHit{ID: int64(i), Name: "sym", Kind: "function", File: "some/path/file.lua", Line: i})
	}
	js, _ := formatValue(hits, formatJSON)
	tn, _ := formatValue(hits, formatTOON)
	if len(tn) >= len(js) {
		t.Errorf("TOON (%d bytes) should be leaner than JSON (%d bytes) for uniform arrays", len(tn), len(js))
	}
}

func TestParseOutputFormat(t *testing.T) {
	tests := []struct {
		name string
		want outputFormat
	}{
		{"human", formatHuman},
		{"toon", formatTOON},
		{"json", formatJSON},
		{"markdown", formatMarkdown},
		{"md", formatMarkdown},
	}
	for _, tt := range tests {
		got, err := parseOutputFormat(tt.name)
		if err != nil {
			t.Fatalf("parseOutputFormat(%q): %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("parseOutputFormat(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
	if _, err := parseOutputFormat("xml"); err == nil {
		t.Fatal("parseOutputFormat(xml) should fail")
	}
}

func TestDefaultOutputFormat(t *testing.T) {
	if got := defaultOutputFormat(true); got != formatHuman {
		t.Fatalf("TTY default = %v, want human", got)
	}
	if got := defaultOutputFormat(false); got != formatTOON {
		t.Fatalf("non-TTY default = %v, want toon", got)
	}
}

func TestFormatOverviewHumanSuppressesEmptySerialization(t *testing.T) {
	overview := query.Overview{Counts: store.Counts{Files: 2, Symbols: 3}, Docs: []string{"README.md"}}
	got, err := formatValue(overview, formatHuman)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Project overview", "2 files", "3 symbols", "README.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("human overview missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"null", "clusters[0]", "[]", "{}"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("human overview contains serialization noise %q:\n%s", unwanted, got)
		}
	}
}

func TestFormatOverviewMarkdown(t *testing.T) {
	overview := query.Overview{Counts: store.Counts{Files: 1}, Entrypoints: []string{"cmd/app/main.go"}}
	got, err := formatValue(overview, formatMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Project overview") || !strings.Contains(got, "`cmd/app/main.go`") {
		t.Fatalf("unexpected markdown overview:\n%s", got)
	}
}

// TestFormatValueEmptySliceIsBracket pins the programmatic-consumption contract:
// a nil result slice must serialize as `[]`, never `null`, so an agent parsing
// --format json in a REPL can iterate the result without a nil guard.
func TestFormatValueEmptySliceIsBracket(t *testing.T) {
	var empty []store.SymbolHit // nil slice, as an empty find returns
	got, err := formatValue(empty, formatJSON)
	if err != nil {
		t.Fatalf("formatValue json: %v", err)
	}
	if got != "[]" {
		t.Errorf("empty slice JSON = %q, want %q", got, "[]")
	}
	// A populated slice is unaffected.
	got, err = formatValue([]store.SymbolHit{{ID: 1, Name: "x"}}, formatJSON)
	if err != nil {
		t.Fatalf("formatValue json: %v", err)
	}
	if !strings.HasPrefix(got, "[{") {
		t.Errorf("populated slice JSON = %q, want a JSON array of objects", got)
	}
}
