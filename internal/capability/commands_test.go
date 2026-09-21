package capability

import (
	"strings"
	"testing"
)

// The CLI is the primary agent surface, so every builtin workflow must ship the
// prowl commands that run it, and the discovery Summary must carry them.
func TestBuiltinManifestsShipCLICommands(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	all := catalog.All()
	if len(all) == 0 {
		t.Fatal("empty catalog")
	}
	for _, manifest := range all {
		if len(manifest.Commands) == 0 {
			t.Errorf("capability %q ships no CLI commands", manifest.Name)
		}
		for _, recipe := range manifest.Commands {
			if !strings.HasPrefix(recipe, "prowl ") {
				t.Errorf("capability %q recipe %q is not a prowl command", manifest.Name, recipe)
			}
		}
		if len(manifest.Summary().Commands) != len(manifest.Commands) {
			t.Errorf("capability %q Summary drops CLI commands", manifest.Name)
		}
	}
}
