package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/capability"
)

func TestCapabilitiesSearchHasHumanAndJSONContracts(t *testing.T) {
	catalog, err := capability.BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	human := bindFormatFlags(newCapabilitiesSearchCmd(catalog))
	var output bytes.Buffer
	human.SetOut(&output)
	human.SetArgs([]string{"context"})
	if err := human.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "retrieve-context") || strings.HasPrefix(strings.TrimSpace(output.String()), "[") {
		t.Fatalf("human capability output = %q", output.String())
	}
	machine := bindFormatFlags(newCapabilitiesSearchCmd(catalog))
	output.Reset()
	machine.SetOut(&output)
	machine.SetArgs([]string{"context", "--json"})
	if err := machine.Execute(); err != nil {
		t.Fatal(err)
	}
	var summaries []capability.Summary
	if err := json.Unmarshal(output.Bytes(), &summaries); err != nil || len(summaries) == 0 {
		t.Fatalf("JSON capability output = %q err=%v", output.String(), err)
	}
}
