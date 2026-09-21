package query

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/store"
)

func TestFindFuzzyRescuesTypos(t *testing.T) {
	dir := t.TempDir()
	src := "package widget\n\nfunc BatteryIndicator() int { return 1 }\nfunc unrelated() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "w.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module w\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := index.Index(s, dir, nil); err != nil {
		t.Fatal(err)
	}
	q := New(s)
	first := func(name string) string {
		h, err := q.FindSymbol(name)
		if err != nil {
			t.Fatalf("find %q: %v", name, err)
		}
		if len(h) == 0 {
			return ""
		}
		return h[0].Name
	}
	if got := first("BatteryIndicator"); got != "BatteryIndicator" {
		t.Fatalf("exact = %q", got)
	}
	if got := first("BatteryIndecator"); got != "BatteryIndicator" {
		t.Fatalf("wrong-letter typo not rescued: %q", got)
	}
	if got := first("Batteryindicatro"); got != "BatteryIndicator" {
		t.Fatalf("transposition typo not rescued: %q", got)
	}
	if got := first("Batteryndicator"); got != "BatteryIndicator" {
		t.Fatalf("missing-letter typo not rescued: %q", got)
	}
	// A far-off name must not fuzzy-match (no noise).
	if got := first("CompletelyUnrelatedThing"); got != "" {
		t.Fatalf("fuzzy over-matched an unrelated name: %q", got)
	}
	// osa sanity
	if d := osaDistance("ca", "abc"); d != 3 {
		t.Fatalf("osa(ca,abc)=%d want 3", d)
	}
	if d := osaDistance("ab", "ba"); d != 1 {
		t.Fatalf("osa transposition=%d want 1", d)
	}
}
