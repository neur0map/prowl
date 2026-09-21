package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/workspace"
)

func TestKnowledgeCommandLifecycleAndDatabaseIndependence(t *testing.T) {
	root := t.TempDir()
	if _, err := workspace.Create(root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	initOutput := runKnowledgeCommand(t, "init", "--json")
	if !strings.Contains(initOutput, `"initialized":true`) {
		t.Fatalf("init output = %s", initOutput)
	}
	candidate := filepath.Join(root, "candidate.md")
	if err := os.WriteFile(candidate, []byte("---\ntype: Decision\ntitle: CLI knowledge\nprowl:\n  id: cli-decision\n  status: accepted\n---\nReviewable from the CLI.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proposalOutput := runKnowledgeCommand(t, "propose", "--file", candidate, "--target", "decisions/cli.md", "--author", "fixture", "--json")
	var proposed struct {
		Proposal struct {
			ID string `json:"id"`
		} `json:"proposal"`
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal([]byte(proposalOutput), &proposed); err != nil || proposed.Proposal.ID == "" || !strings.Contains(proposed.Diff, "+Reviewable from the CLI.") {
		t.Fatalf("proposal output = %s, err=%v", proposalOutput, err)
	}
	acceptOutput := runKnowledgeCommand(t, "accept", proposed.Proposal.ID, "--json")
	var accepted struct {
		Proposal knowledge.Proposal `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(acceptOutput), &accepted); err != nil || accepted.Proposal.Status != "accepted" || accepted.Proposal.Decision == nil || accepted.Proposal.Decision.PrincipalID != knowledge.LocalPrincipalID {
		t.Fatalf("accept output = %s, proposal=%+v, err=%v", acceptOutput, accepted.Proposal, err)
	}
	listOutput := runKnowledgeCommand(t, "list", "--json")
	if !strings.Contains(listOutput, `"id":"cli-decision"`) || !strings.Contains(listOutput, `"path":"decisions/cli.md"`) {
		t.Fatalf("list output = %s", listOutput)
	}
	showOutput := runKnowledgeCommand(t, "show", "cli-decision", "--json")
	if !strings.Contains(showOutput, "Reviewable from the CLI.") {
		t.Fatalf("show output = %s", showOutput)
	}
	// Canonical list/show do not depend on the derived database.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(filepath.Join(root, ".prowl", "index.db") + suffix)
	}
	if output := runKnowledgeCommand(t, "list", "--json"); !strings.Contains(output, `"id":"cli-decision"`) {
		t.Fatalf("list after database deletion = %s", output)
	}
	export := filepath.Join(root, "exported")
	runKnowledgeCommand(t, "export", export, "--json")
	if _, err := os.Stat(filepath.Join(export, "decisions", "cli.md")); err != nil {
		t.Fatalf("export missing accepted document: %v", err)
	}
	lintOutput := runKnowledgeCommand(t, "lint", "--json")
	if !strings.Contains(lintOutput, "knowledge.missing_evidence") {
		t.Fatalf("lint output = %s", lintOutput)
	}
}

func runKnowledgeCommand(t *testing.T, args ...string) string {
	t.Helper()
	var output bytes.Buffer
	command := newKnowledgeCmd()
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		t.Fatalf("knowledge %v: %v\n%s", args, err, output.String())
	}
	return output.String()
}
