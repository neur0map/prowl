package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/prowl-agent/prowl-agent/skills"
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

func TestSetupApplyReplaysCompletedRequestAfterConfigChanges(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "replay-after-config-change",
	}
	first, err := service.Apply(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".prowl", "config.toml"), []byte("changed = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := service.Apply(context.Background(), request)
	if err != nil {
		t.Fatalf("completed replay was rejected after config change: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("completed replay changed: first=%+v second=%+v", first, second)
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

// A symlinked destination must never be written through -- that is how a setup
// write escapes the project root. It must also not sink the whole setup: repos
// that serve several agent harnesses routinely point AGENTS.md at CLAUDE.md, so
// the destination is reported as blocked and the remaining integrations apply.
func TestSetupBlocksSymlinkedDestinationWithoutWrites(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.md")
	original := []byte("do not modify\n")
	if err := os.WriteFile(victim, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationAgents, IntegrationGeneric})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blocked) != 1 || plan.Blocked[0].Path != "AGENTS.md" || plan.Blocked[0].Integration != IntegrationAgents {
		t.Fatalf("plan.Blocked = %+v, want AGENTS.md blocked", plan.Blocked)
	}
	if !strings.Contains(plan.Blocked[0].Reason, "symbolic link") {
		t.Errorf("blocked reason = %q, want it to name the symlink", plan.Blocked[0].Reason)
	}
	for _, action := range plan.Actions {
		if action.Path == "AGENTS.md" {
			t.Fatalf("blocked destination still planned: %+v", plan.Actions)
		}
	}
	if _, err = service.Apply(context.Background(), ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "symlink",
	}); err != nil {
		t.Fatalf("apply failed instead of skipping the blocked destination: %v", err)
	}
	if got, readErr := os.ReadFile(victim); readErr != nil || string(got) != string(original) {
		t.Fatalf("symlink target changed: %q, %v", got, readErr)
	}
	// The writable integration in the same plan still landed.
	if _, statErr := os.Stat(filepath.Join(root, ".mcp.json")); statErr != nil {
		t.Errorf("writable integration was skipped too: %v", statErr)
	}
}

// An unreadable destination must still be reported in terms the caller can act
// on rather than collapsed into one opaque failure string.
func TestSetupErrorsNameTheirCause(t *testing.T) {
	if errors.Is(safeError(errors.New("some os detail: /abs/path")), errSymlinkDestination) {
		t.Fatal("unknown error must not be classified as a symlink failure")
	}
	if got := safeError(errors.New("some os detail: /abs/path")).Error(); got != "setup operation failed" {
		t.Errorf("unknown error = %q, want the opaque fallback", got)
	}
	if got := safeError(fmt.Errorf("open x: %w", fs.ErrPermission)).Error(); !strings.Contains(got, "permission denied") {
		t.Errorf("permission error = %q, want it to name permission denial", got)
	}
	if got := safeError(fmt.Errorf("AGENTS.md %w", errSymlinkDestination)).Error(); !strings.Contains(got, "AGENTS.md") || !strings.Contains(got, "symbolic link") {
		t.Errorf("symlink error = %q, want the project-relative reason", got)
	}
}

func TestSetupRollbackRemovesTransactionCreatedDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte("{ invalid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationCursor, IntegrationGeneric})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Apply(context.Background(), ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "directory-rollback",
	})
	if err == nil {
		t.Fatal("invalid configuration apply succeeded")
	}
	if _, statErr := os.Stat(filepath.Join(root, ".cursor")); !os.IsNotExist(statErr) {
		t.Fatalf("transaction-created integration directory survived rollback: %v", statErr)
	}
}

func TestSetupApplyRecoversInterruptedTransaction(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "interrupted",
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agentsBlock+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".prowl"), 0o755); err != nil {
		t.Fatal(err)
	}
	transaction := map[string]any{
		"schema_version": 1,
		"request":        request,
		"snapshots": []map[string]any{{
			"path": "AGENTS.md", "data": "", "mode": 0, "existed": false,
		}},
		"directories": []map[string]any{{
			"path": ".prowl", "created": true,
		}},
	}
	data, err := json.Marshal(transaction)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".prowl-setup-transaction.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Apply(context.Background(), request); err == nil {
		t.Fatal("interrupted transaction was applied instead of recovered")
	}
	if _, statErr := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(statErr) {
		t.Fatalf("interrupted integration was not restored: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".prowl-setup-transaction.json")); !os.IsNotExist(statErr) {
		t.Fatalf("recovered transaction journal remained: %v", statErr)
	}
}

func TestSetupApplyRecoversLegacyInterruptedTransaction(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "legacy-interrupted",
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agentsBlock+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cursor", "mcp.json"), []byte(`{"mcpServers":{"prowl-agent":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".prowl"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"schema_version": 1,
		"request":        request,
		"snapshots": []map[string]any{
			{"path": "AGENTS.md", "data": "", "mode": 0, "existed": false},
			{"path": ".cursor/mcp.json", "data": "", "mode": 0, "existed": false},
		},
		"directories": []map[string]any{
			{"path": ".prowl", "existed": false},
			{"path": ".cursor", "existed": false},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, ".prowl", "setup-transaction.json")
	if err := os.WriteFile(legacyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(context.Background(), request); err == nil {
		t.Fatal("legacy interrupted transaction was applied instead of recovered")
	}
	if _, statErr := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(statErr) {
		t.Fatalf("legacy interrupted integration was not restored: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".cursor")); !os.IsNotExist(statErr) {
		t.Fatalf("legacy transaction-created directory remained: %v", statErr)
	}
	if _, statErr := os.Stat(legacyPath); !os.IsNotExist(statErr) {
		t.Fatalf("legacy transaction journal remained: %v", statErr)
	}
}

func TestSetupApplyDoesNotRecoverLiveLockedTransaction(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "live-transaction",
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agentsBlock+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	transaction := map[string]any{
		"schema_version": 1,
		"request":        request,
		"snapshots": []map[string]any{{
			"path": "AGENTS.md", "data": "", "mode": 0, "existed": false,
		}},
	}
	data, err := json.Marshal(transaction)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".prowl-setup-transaction.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(root, ".prowl-setup.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := service.Apply(ctx, request); err == nil {
		t.Fatal("live transaction was processed while its lock was held")
	}
	if _, statErr := os.Stat(filepath.Join(root, "AGENTS.md")); statErr != nil {
		t.Fatalf("live transaction was rolled back while locked: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".prowl-setup-transaction.json")); statErr != nil {
		t.Fatalf("live transaction journal was removed while locked: %v", statErr)
	}
}

func TestSetupRollbackRetainsJournalUntilDirectoryCleanupCompletes(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cursor", "user-owned"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal := map[string]any{
		"schema_version": transactionSchema,
		"snapshots": []map[string]any{{
			"path": "absent", "data": "", "mode": 0, "existed": false,
		}},
		"directories": []map[string]any{{
			"path": ".cursor", "created": true,
		}},
	}
	data, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, transactionPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, transactionPath), data, 0o600); err != nil {
		t.Fatal(err)
	}
	pending, found, err := service.loadTransaction()
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("transaction journal was not loaded")
	}
	if err := service.rollbackTransaction(pending); err == nil {
		t.Fatal("rollback unexpectedly removed a non-empty directory")
	}
	if _, statErr := os.Stat(filepath.Join(root, transactionPath)); statErr != nil {
		t.Fatalf("rollback journal disappeared before cleanup completed: %v", statErr)
	}
}

func TestSetupRollbackPreservesUnownedDirectory(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	pending := transaction{Directories: []transactionDirectory{{Path: ".cursor"}}}
	if err := service.rollbackTransaction(pending); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".cursor")); statErr != nil {
		t.Fatalf("unowned directory was removed: %v", statErr)
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

func TestSetupApplyInstallsAndRemovesEmbeddedSkills(t *testing.T) {
	root := t.TempDir()
	// A present harness config dir makes it a detected, installable target.
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if detected := DetectIntegrations(root); !contains(detected, IntegrationClaude) {
		t.Fatalf("config dir did not surface the integration: %v", detected)
	}
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationOMP, IntegrationClaude})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.Apply(context.Background(), ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "skills",
	})
	if err != nil {
		t.Fatalf("apply installing skills failed: %v", err)
	}
	if !outcome.Verified {
		t.Fatal("apply installing skills was not verified")
	}
	const skill = "code-search"
	want, ok := skillContent(skill)
	if !ok || want == "" {
		t.Fatalf("embedded skill %q missing", skill)
	}
	dirs := []string{".omp/skills", ".claude/skills"}
	for _, dir := range dirs {
		path := filepath.Join(root, filepath.FromSlash(dir), skill, "SKILL.md")
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("installed skill %s: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("installed skill %s content = %q, want embedded body", path, got)
		}
	}
	// The omp integration also installs the sticky RULES.md rule (re-injected each
	// turn), marker-bounded so it merges with any user RULES.md and reverts clean.
	rulesPath := filepath.Join(root, ".omp", "RULES.md")
	rules, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read .omp/RULES.md: %v", err)
	}
	if got, want := string(rules), rulesBlock+"\n"; got != want {
		t.Fatalf(".omp/RULES.md content = %q, want generated routing block %q", got, want)
	}
	// Removing the integrations takes their installed skill files back out.
	if err := service.removeIntegrations([]string{IntegrationOMP, IntegrationClaude}); err != nil {
		t.Fatalf("remove integrations: %v", err)
	}
	for _, dir := range dirs {
		path := filepath.Join(root, filepath.FromSlash(dir), skill, "SKILL.md")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("skill %s survived removal: %v", path, err)
		}
	}
	if _, err := os.Stat(rulesPath); !os.IsNotExist(err) {
		t.Fatalf(".omp/RULES.md survived removal: %v", err)
	}
}

func TestReviewSkillInstallsForEveryProjectHost(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	integrations := []string{IntegrationOMP, IntegrationClaude, IntegrationAgentSkills}
	plan, err := service.Plan(context.Background(), integrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(context.Background(), ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "review-skill-hosts",
	}); err != nil {
		t.Fatalf("apply review skill integrations: %v", err)
	}

	const skill = "prowl-pr-review"
	want, ok := skillContent(skill)
	if !ok || want == "" {
		t.Fatalf("embedded skill %q missing", skill)
	}
	for _, rel := range []string{
		".agents/skills/prowl-pr-review/SKILL.md",
		".claude/skills/prowl-pr-review/SKILL.md",
		".omp/skills/prowl-pr-review/SKILL.md",
	} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read installed review skill %s: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("installed review skill %s differs from embedded portable skill", rel)
		}
	}
}

// Migration must move a repository off the retired prowl-repo-exploration skill
// without ever destroying user work: an exact Prowl-owned copy is removed, a
// locally edited copy is preserved and reported as a conflict, and the new
// code-search skill installs alongside whatever is left. An absent copy is a
// no-op.
func TestSetupMigratesLegacyExplorationSkill(t *testing.T) {
	legacy, ok := skills.Legacy("prowl-repo-exploration")
	if !ok || legacy.Content == "" {
		t.Fatal("legacy prowl-repo-exploration bytes are not embedded")
	}
	const legacyRel = ".omp/skills/prowl-repo-exploration/SKILL.md"
	const codeRel = ".omp/skills/code-search/SKILL.md"
	cases := []struct {
		name          string
		seed          string // pre-existing legacy file content ("" = none)
		wantBlocked   bool
		wantPreserved bool // legacy file survives apply with its seed content
	}{
		{"exact prowl-owned copy is removed", legacy.Content, false, false},
		{"locally edited copy is preserved and blocked", legacy.Content + "\n\n<!-- local edit -->\n", true, true},
		{"absent copy needs no migration", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".omp"), 0o755); err != nil {
				t.Fatal(err)
			}
			legacyPath := filepath.Join(root, filepath.FromSlash(legacyRel))
			if tc.seed != "" {
				if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(legacyPath, []byte(tc.seed), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			service, err := NewService(root)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := service.Plan(context.Background(), []string{IntegrationOMP})
			if err != nil {
				t.Fatal(err)
			}
			if got := planBlocks(plan, legacyRel); got != tc.wantBlocked {
				t.Errorf("plan blocks %s = %v, want %v (blocked=%+v)", legacyRel, got, tc.wantBlocked, plan.Blocked)
			}
			// An exact owned copy is scheduled for removal; a preserved or absent
			// copy is not.
			removalPlanned := planHasAction(plan, integrationLegacySkill, legacyRel)
			wantRemoval := tc.seed == legacy.Content
			if removalPlanned != wantRemoval {
				t.Errorf("plan schedules legacy removal = %v, want %v", removalPlanned, wantRemoval)
			}
			if _, err := service.Apply(context.Background(), ApplyRequest{
				Integrations: plan.Integrations, PlanHash: plan.Hash,
				ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "migrate",
			}); err != nil {
				t.Fatalf("apply failed: %v", err)
			}
			if tc.wantPreserved {
				got, err := os.ReadFile(legacyPath)
				if err != nil {
					t.Fatalf("preserved legacy copy was removed: %v", err)
				}
				if string(got) != tc.seed {
					t.Errorf("preserved legacy copy was modified: %q", got)
				}
			} else if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
				t.Fatalf("legacy copy survived migration: %v", err)
			}
			// The canonical skill installs regardless of the conflict.
			want, _ := skillContent("code-search")
			got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(codeRel)))
			if err != nil {
				t.Fatalf("code-search was not installed: %v", err)
			}
			if string(got) != want {
				t.Errorf("installed code-search content mismatch")
			}
		})
	}
}

func TestReviewRoutingContractIsCompactAndMandatory(t *testing.T) {
	const contract = "Before reviewing a workspace, commit, or branch range, run `prowl-agent review plan`. If raw additions plus deletions exceed 300 (`raw_additions + raw_deletions > 300`), review every returned unit and all four required audits, then run `prowl-agent review check`; never approve an incomplete, stale, or invalid report."
	for name, body := range map[string]string{
		"AGENTS.md block":     agentsBlock,
		".omp/RULES.md block": rulesBlock,
	} {
		if count := strings.Count(body, contract); count != 1 {
			t.Errorf("%s contains the mandatory review routing contract %d times, want once", name, count)
		}
		for _, portableDetail := range []string{
			"prowl-agent review unit",
			"audit_removed_behavior_v1",
			"acknowledged_primary_hunk_ids",
			"receipt",
		} {
			if strings.Contains(strings.ToLower(body), portableDetail) {
				t.Errorf("%s duplicates portable review skill detail %q", name, portableDetail)
			}
		}
	}
}

// Every generated routing surface -- the AGENTS.md block, the sticky .omp
// RULES.md, and the project scout override -- must lead with the read-only
// prowl-agent CLI and must not advertise MCP tools as the way to search. The
// scout additionally keeps its read-only, @smol contract while dropping MCP tool
// names for bash-run CLI commands.
func TestSetupGeneratedRoutingIsCLIFirst(t *testing.T) {
	surfaces := map[string]string{
		"AGENTS.md block":     agentsBlock,
		".omp/RULES.md block": rulesBlock,
		"omp scout override":  ompScoutAgent,
	}
	for name, body := range surfaces {
		core := strings.ReplaceAll(strings.ReplaceAll(body, agentsMarker, ""), agentsEndMarker, "")
		if !strings.Contains(core, "prowl-agent") {
			t.Errorf("%s names no prowl-agent CLI command outside its markers", name)
		}
		lower := strings.ToLower(core)
		for _, banned := range []string{"search_context", "read_symbol", "mcp__"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%s still advertises the MCP tool %q as a search transport", name, banned)
			}
		}
	}
	// The scout override drops MCP tool names entirely, runs the CLI via bash, and
	// keeps its read-only output contract and @smol model.
	if strings.Contains(strings.ToLower(ompScoutAgent), "mcp") {
		t.Error("scout override still references MCP")
	}
	if !strings.Contains(ompScoutAgent, "bash") {
		t.Error("scout override no longer runs prowl-agent through bash")
	}
	if !strings.Contains(ompScoutAgent, `model: "@smol"`) {
		t.Error("scout override lost its @smol model contract")
	}
	if !strings.Contains(ompScoutAgent, "read-only") {
		t.Error("scout override lost its read-only contract")
	}
}

// Verify must not pass a migration plan whose retired skill copy is still on
// disk: a legacy-removal action is satisfied only once the exact old file is
// gone. Otherwise Verify could green-light a pending or rematerialized plan
// while prowl-repo-exploration still shadows code-search.
func TestSetupVerifyRejectsPendingLegacyRemoval(t *testing.T) {
	legacy, ok := skills.Legacy("prowl-repo-exploration")
	if !ok || legacy.Content == "" {
		t.Fatal("legacy prowl-repo-exploration bytes are not embedded")
	}
	const legacyRel = ".omp/skills/prowl-repo-exploration/SKILL.md"
	root := t.TempDir()
	legacyPath := filepath.Join(root, filepath.FromSlash(legacyRel))
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacy.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationOMP})
	if err != nil {
		t.Fatal(err)
	}
	if !planHasAction(plan, integrationLegacySkill, legacyRel) {
		t.Fatalf("plan does not schedule the exact legacy removal: %+v", plan.Actions)
	}
	// Apply installs the CLI-first assets and removes the retired copy, so the
	// fully-migrated tree verifies clean against its own plan.
	if _, err := service.Apply(context.Background(), ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "verify-legacy",
	}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if _, statErr := os.Stat(legacyPath); !os.IsNotExist(statErr) {
		t.Fatalf("retired skill survived apply: %v", statErr)
	}
	if err := service.Verify(context.Background(), plan); err != nil {
		t.Fatalf("verify rejected the fully-migrated tree: %v", err)
	}
	// Rematerialize the retired copy (a pending or reappeared migration): every
	// install destination is still present, so only the legacy removal is
	// unsatisfied. Verify must reject rather than green-light the shadowed skill.
	if err := os.WriteFile(legacyPath, []byte(legacy.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := service.Verify(context.Background(), plan); err == nil {
		t.Fatal("verify accepted a plan whose retired skill copy still exists")
	}
}

// planBlocks reports whether the plan refuses to write path.
func planBlocks(plan Plan, path string) bool {
	for _, blocked := range plan.Blocked {
		if blocked.Path == path {
			return true
		}
	}
	return false
}

// planHasAction reports whether the plan schedules the given integration action.
func planHasAction(plan Plan, integration, path string) bool {
	for _, action := range plan.Actions {
		if action.Integration == integration && action.Path == path {
			return true
		}
	}
	return false
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

// TestSetupAgentSkillsInstallsToStandardLocation proves prowl emits its skills to
// `.agents/skills/`, the harness-agnostic Agent Skills standard location that
// Prime Agent and other standard-compliant harnesses discover, when `.agents/`
// is present (detected) as well as by explicit selection.
func TestSetupAgentSkillsInstallsToStandardLocation(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if detected := DetectIntegrations(root); !contains(detected, IntegrationAgentSkills) {
		t.Fatalf(".agents/ did not surface the agent-skills integration: %v", detected)
	}
	service, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationAgentSkills})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(context.Background(), ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: "agent-skills",
	}); err != nil {
		t.Fatalf("apply agent-skills failed: %v", err)
	}
	const skill = "code-search"
	want, _ := skillContent(skill)
	path := filepath.Join(root, ".agents", "skills", skill, "SKILL.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("skill not installed at standard location %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("installed skill content mismatch at %s", path)
	}
	// Reversible: removal takes the standard-location files back out.
	if err := service.removeIntegrations([]string{IntegrationAgentSkills}); err != nil {
		t.Fatalf("remove agent-skills: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("skill file still present after removal: %v", err)
	}
}
