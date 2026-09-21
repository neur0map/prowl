package okfv01

import (
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/knowledge"
)

// TestBuildCandidateNestsProwlFields proves a structured candidate assembles
// into OKF with anchors under prowl, round-trips through Parse, and keeps the
// symbol so propose can resolve and hash it.
func TestBuildCandidateNestsProwlFields(t *testing.T) {
	data, err := BuildCandidate(CaptureInput{
		Type:    "Claim",
		Title:   "Foo guards input",
		Body:    "Foo returns early on empty input.",
		Anchors: []knowledge.Anchor{{Path: "pkg/foo.go", Symbol: "Foo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Codec{}.Parse("claims/foo.md", data)
	if err != nil {
		t.Fatalf("built candidate does not parse: %v", err)
	}
	if doc.Type != "Claim" || doc.Title != "Foo guards input" {
		t.Errorf("fields lost: type=%q title=%q", doc.Type, doc.Title)
	}
	if len(doc.Prowl.Anchors) != 1 || doc.Prowl.Anchors[0].Symbol != "Foo" || doc.Prowl.Anchors[0].Path != "pkg/foo.go" {
		t.Fatalf("anchor not nested under prowl: %+v", doc.Prowl.Anchors)
	}
	if !strings.Contains(string(doc.Body), "returns early") {
		t.Errorf("body lost: %q", doc.Body)
	}
}

func TestBuildCandidateRequiresTypeAndTitle(t *testing.T) {
	if _, err := BuildCandidate(CaptureInput{Title: "t"}); err == nil {
		t.Error("expected error when type is missing")
	}
	if _, err := BuildCandidate(CaptureInput{Type: "Claim"}); err == nil {
		t.Error("expected error when title is missing")
	}
}

// TestParseRejectsMisplacedProwlFields proves a candidate that puts a prowl
// field at the top level is rejected loudly instead of silently dropping it.
func TestParseRejectsMisplacedProwlFields(t *testing.T) {
	for _, field := range []string{"anchors", "status", "confidence", "related", "valid_from", "valid_to"} {
		doc := "---\ntype: Claim\ntitle: t\n" + field + ": x\n---\nbody\n"
		if _, err := (Codec{}).Parse("claims/x.md", []byte(doc)); err == nil {
			t.Errorf("top-level %q was accepted; want rejection", field)
		}
	}
	// A correctly nested field still parses.
	ok := "---\ntype: Claim\ntitle: t\nprowl:\n  status: proposed\n---\nbody\n"
	if _, err := (Codec{}).Parse("claims/x.md", []byte(ok)); err != nil {
		t.Errorf("nested prowl field wrongly rejected: %v", err)
	}
}
