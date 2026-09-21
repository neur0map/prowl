package graph

import (
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveQMLComponent(t *testing.T) {
	stem := map[string][]store.File{
		"NText":  {{ID: 1, RelPath: "shell/widgets/NText.qml"}},
		"Styled": {{ID: 2, RelPath: "a/Styled.qml"}, {ID: 3, RelPath: "b/Styled.qml"}},
	}
	// A repo-unique stem resolves from anywhere.
	if id, ok := resolveQMLComponent(stem, "shell/foo/Bar.qml", "NText"); !ok || id != 1 {
		t.Fatalf("unique stem: id=%d ok=%v, want 1", id, ok)
	}
	// Ambiguous stems prefer the same directory.
	if id, ok := resolveQMLComponent(stem, "b/Other.qml", "Styled"); !ok || id != 3 {
		t.Fatalf("ambiguous (b): id=%d ok=%v, want 3", id, ok)
	}
	if id, ok := resolveQMLComponent(stem, "a/Other.qml", "Styled"); !ok || id != 2 {
		t.Fatalf("ambiguous (a): id=%d ok=%v, want 2", id, ok)
	}
	// A built-in/external type has no matching .qml and does not resolve.
	if _, ok := resolveQMLComponent(stem, "x/Y.qml", "Rectangle"); ok {
		t.Fatal("built-in type should not resolve")
	}
}
