package mcp

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/neur0map/prowl/internal/capability"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

// Capability discovery is how an agent picks a tool, so every tool a manifest
// routes to has to exist on the surface that serves it. A renamed or removed
// tool is otherwise invisible until an agent calls it and the server answers
// "unknown tool".
func TestCapabilityManifestToolsAreRegistered(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	catalog, err := capability.BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	service := &contextpacket.Service{Store: database, Root: t.TempDir()}
	server := NewServerWithOptions(query.New(database), database, "test", nil, nil, nil,
		ServerOptions{Surface: SurfaceCore, Context: service, Capabilities: catalog})

	registered := map[string]bool{}
	for _, tool := range listToolDescriptors(t, server) {
		registered[tool.Name] = true
	}
	if len(registered) == 0 {
		t.Fatal("no tools registered to validate against")
	}
	available := make([]string, 0, len(registered))
	for name := range registered {
		available = append(available, name)
	}
	sort.Strings(available)

	for _, manifest := range catalog.All() {
		for _, tool := range manifest.Tools {
			if !registered[tool] {
				t.Errorf("capability %q routes to MCP tool %q, which the core surface does not register (have %v)",
					manifest.Name, tool, available)
			}
		}
	}
}
