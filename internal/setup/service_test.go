package setup

import (
	"context"
	"reflect"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupDetectAndPlanDoNotWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"other":{"command":"secret-token"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeState(t, root)
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	detect, err := service.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !contains(detect.Integrations, IntegrationGeneric) || strings.Contains(detect.ProjectConfigVersion, root) {
		t.Fatalf("unsafe detect result: %+v", detect)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationGeneric})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Hash == "" || plan.Actions[0].Path != ".mcp.json" || strings.Contains(plan.Hash, root) {
		t.Fatalf("unsafe plan: %+v", plan)
	}
	if after := treeState(t, root); after != before {
		t.Fatalf("detect/plan wrote files:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestSetupApplyRequiresApprovalAndFreshPlanWithoutWrites(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	before := treeState(t, root)
	if _, err := service.Apply(context.Background(), ApplyRequest{Integrations: plan.Integrations, PlanHash: plan.Hash, ExpectedProjectConfigVersion: plan.ProjectConfigVersion, IdempotencyKey: "approval-denied"}); err == nil {
		t.Fatal("unapproved apply succeeded")
	}
	if after := treeState(t, root); after != before {
		t.Fatalf("denied apply wrote files: %s", after)
	}
	if err := os.MkdirAll(filepath.Join(root, ".prowl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".prowl", "config.toml"), []byte("changed = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before = treeState(t, root)
	if _, err := service.Apply(context.Background(), ApplyRequest{Integrations: plan.Integrations, PlanHash: plan.Hash, ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "stale-version"}); err == nil {
		t.Fatal("stale apply succeeded")
	}
	if after := treeState(t, root); after != before {
		t.Fatalf("stale apply wrote files: %s", after)
	}
}

func TestSetupApplyIdempotencyConflictAndAuditRedaction(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationGeneric})
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{Integrations: plan.Integrations, PlanHash: plan.Hash, ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "stable-key"}
	first, err := service.Apply(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Apply(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || !first.Verified || len(first.RollbackManifest) != 1 {
		t.Fatalf("replay outcome = %+v, want durable verified original", second)
	}
	if _, err := service.Apply(context.Background(), ApplyRequest{Integrations: []string{IntegrationAgents}, PlanHash: plan.Hash, ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: request.IdempotencyKey}); err == nil {
		t.Fatal("mismatched replay succeeded")
	}
	for _, value := range append([]string{root, "secret-token"}, flattenOutcome(first)...) {
		if strings.Contains(strings.Join(flattenOutcome(first), "\n"), root) || strings.Contains(strings.Join(flattenOutcome(first), "\n"), "secret-token") {
			t.Fatalf("unsafe outcome value %q: %+v", value, first)
		}
	}
}

func TestSetupRollbackPreservesFilesAfterMutationFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("{ invalid json")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationAgents, IntegrationCursor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Apply(context.Background(), ApplyRequest{Integrations: plan.Integrations, PlanHash: plan.Hash, ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "rollback"})
	if err == nil {
		t.Fatal("invalid configuration apply succeeded")
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != string(original) {
		t.Fatalf("config after rollback = %q, %v; want %q", data, readErr, original)
	}
	if _, statErr := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(statErr) {
		t.Fatalf("created file survived failed apply: %v", statErr)
	}
}

func TestSetupVerifyRejectsMissingMarker(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Verify(context.Background(), plan); err == nil {
		t.Fatal("verify accepted missing marker")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func treeState(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, filepath.ToSlash(rel)+":"+string(data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return strings.Join(entries, "\n")
}

func flattenOutcome(outcome ApplyOutcome) []string {
	values := []string{outcome.PlanHash, outcome.ProjectConfigVersion, outcome.IdempotencyKey}
	for _, item := range outcome.RollbackManifest {
		values = append(values, item.Path)
	}
	return values
}
