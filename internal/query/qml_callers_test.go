package query

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/store"
)

// callers/callees must follow QML coupling: component instantiation (Button {})
// and singleton member use (Config.spacing), not just imports. Before these
// edge kinds were traversed, callers on a shared singleton returned empty.
func TestCallersFollowQMLCoupling(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Config.qml", "pragma Singleton\nimport QtQuick\nQtObject {\n  property int spacing: 8\n}\n")
	write("Button.qml", "import QtQuick\nRectangle {\n  id: root\n}\n")
	write("Panel.qml", "import QtQuick\nItem {\n  width: Config.spacing\n  Button { }\n}\n")

	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := index.Index(s, dir, nil); err != nil {
		t.Fatal(err)
	}
	q := New(s)

	has := func(rows []EdgeView, file string) bool {
		for _, r := range rows {
			if r.File == file {
				return true
			}
		}
		return false
	}

	callers, err := q.FindCallers("Config.qml")
	if err != nil {
		t.Fatal(err)
	}
	if !has(callers, "Panel.qml") {
		t.Fatalf("callers Config.qml = %+v, want Panel.qml (singleton use)", callers)
	}
	callers, err = q.FindCallers("Button.qml")
	if err != nil {
		t.Fatal(err)
	}
	if !has(callers, "Panel.qml") {
		t.Fatalf("callers Button.qml = %+v, want Panel.qml (instantiation)", callers)
	}

	callees, err := q.FindCallees("Panel.qml")
	if err != nil {
		t.Fatal(err)
	}
	hasEdge := func(rows []EdgeView, kind, raw string) bool {
		for _, r := range rows {
			if r.Kind == kind && r.Raw == raw && r.Resolved {
				return true
			}
		}
		return false
	}
	if !hasEdge(callees, "uses", "Config") || !hasEdge(callees, "instantiates", "Button") {
		t.Fatalf("callees Panel.qml = %+v, want resolved uses Config and instantiates Button", callees)
	}
}
