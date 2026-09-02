package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prowl-agent/prowl-agent/skills"
)

// wantAsset is an independent test oracle for one installed user asset. Its
// checksum is derived here with crypto/sha256 -- never through the production
// digest helper -- so a bug in the installer's checksum path cannot hide.
type wantAsset struct {
	AssetID  string
	Dest     string
	Content  string
	Checksum string
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// wantUserAssets rebuilds the expected destination set independently from the
// embedded skill/native bundles, so the tests do not merely echo the installer.
func wantUserAssets(version string, clients []string) []wantAsset {
	roots := map[string]string{"claude": ".claude/skills/prowl", "omp": ".omp/agent"}
	var out []wantAsset
	for _, client := range clients {
		root := roots[client]
		for _, asset := range skills.Native(client) {
			content := strings.ReplaceAll(asset.Content, "{{VERSION}}", version)
			out = append(out, wantAsset{
				AssetID:  client + ":" + asset.Path,
				Dest:     root + "/" + asset.Path,
				Content:  content,
				Checksum: sha256Hex(content),
			})
		}
		for _, skill := range skills.All() {
			out = append(out, wantAsset{
				AssetID:  client + ":skills/" + skill.Name + "/SKILL.md",
				Dest:     root + "/skills/" + skill.Name + "/SKILL.md",
				Content:  skill.Content,
				Checksum: sha256Hex(skill.Content),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dest < out[j].Dest })
	return out
}

func newUserOpts(t *testing.T) UserInstallOptions {
	t.Helper()
	return UserInstallOptions{
		Home:     t.TempDir(),
		StateDir: t.TempDir(),
		Version:  "9.9.9",
		Clients:  []string{"claude", "omp"},
	}
}

// manifestPath is the persisted state file location: StateDir is the XDG state
// base, so the manifest lives under <StateDir>/prowl-agent/.
func manifestPath(opts UserInstallOptions) string {
	return filepath.Join(opts.StateDir, "prowl-agent", "agent-assets.json")
}

func actionByAssetID(plan UserPlan, id string) (UserAction, bool) {
	for _, action := range plan.Actions {
		if action.AssetID == id {
			return action, true
		}
	}
	return UserAction{}, false
}

func conflictByAssetID(plan UserPlan, id string) (UserConflict, bool) {
	for _, conflict := range plan.Conflicts {
		if conflict.AssetID == id {
			return conflict, true
		}
	}
	return UserConflict{}, false
}

func mustPlan(t *testing.T, opts UserInstallOptions) UserPlan {
	t.Helper()
	plan, err := PlanUserSkills(opts)
	if err != nil {
		t.Fatalf("PlanUserSkills: %v", err)
	}
	return plan
}

func mustApply(t *testing.T, opts UserInstallOptions) {
	t.Helper()
	plan := mustPlan(t, opts)
	if _, err := ApplyUserSkills(opts, plan, true); err != nil {
		t.Fatalf("ApplyUserSkills: %v", err)
	}
}

func planDests(plan UserPlan) []string {
	var out []string
	for _, action := range plan.Actions {
		out = append(out, action.Destination)
	}
	return out
}

// TestUserSkillPlanClaudeDestinations proves the Claude package lands under
// ~/.claude/skills/prowl/: the native plugin tree plus every canonical portable
// skill copied into the plugin's own skills/ directory.
func TestUserSkillPlanClaudeDestinations(t *testing.T) {
	opts := newUserOpts(t)
	opts.Clients = []string{"claude"}
	plan := mustPlan(t, opts)

	want := map[string]bool{
		".claude/skills/prowl/.claude-plugin/plugin.json": false,
		".claude/skills/prowl/agents/code-scout.md":       false,
		".claude/skills/prowl/commands/search.md":         false,
		".claude/skills/prowl/hooks/hooks.json":           false,
	}
	for _, skill := range skills.All() {
		want[".claude/skills/prowl/skills/"+skill.Name+"/SKILL.md"] = false
	}
	for _, action := range plan.Actions {
		if _, ok := want[action.Destination]; ok {
			want[action.Destination] = true
		}
		if action.Client != "claude" {
			t.Errorf("unexpected client %q for %s", action.Client, action.Destination)
		}
	}
	for dest, seen := range want {
		if !seen {
			t.Errorf("plan missing Claude destination %s", dest)
		}
	}
}

// TestUserSkillPlanOMPDestinations proves OMP lands under ~/.omp/agent/: skills/,
// the native agent, and the extension.
func TestUserSkillPlanOMPDestinations(t *testing.T) {
	opts := newUserOpts(t)
	opts.Clients = []string{"omp"}
	plan := mustPlan(t, opts)

	want := map[string]bool{
		".omp/agent/agents/code-scout.md":        false,
		".omp/agent/extensions/prowl-routing.ts": false,
		".omp/agent/skills/code-search/SKILL.md": false,
	}
	for _, action := range plan.Actions {
		if _, ok := want[action.Destination]; ok {
			want[action.Destination] = true
		}
		if action.Client != "omp" {
			t.Errorf("unexpected client %q for %s", action.Client, action.Destination)
		}
	}
	for dest, seen := range want {
		if !seen {
			t.Errorf("plan missing OMP destination %s", dest)
		}
	}
}

// TestUserSkillPlanPreviewOrderingIsStable proves the previewed destinations are
// deterministically ordered and carry no absolute paths or file bodies.
func TestUserSkillPlanPreviewOrderingIsStable(t *testing.T) {
	opts := newUserOpts(t)
	plan := mustPlan(t, opts)

	var got []string
	for _, action := range plan.Actions {
		if action.Kind != UserActionInstall {
			t.Fatalf("fresh plan action %s is %q, want install", action.Destination, action.Kind)
		}
		got = append(got, action.Destination)
		if filepath.IsAbs(action.Destination) || strings.HasPrefix(action.Destination, "/") {
			t.Errorf("preview leaks an absolute path: %s", action.Destination)
		}
		if strings.Contains(action.Destination, opts.Home) {
			t.Errorf("preview leaks the user home: %s", action.Destination)
		}
	}
	var want []string
	for _, asset := range wantUserAssets(opts.Version, opts.Clients) {
		want = append(want, asset.Dest)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("preview ordering mismatch\n got=%v\nwant=%v", got, want)
	}
	// Stable across repeated planning.
	if again := strings.Join(planDests(mustPlan(t, opts)), "\n"); again != strings.Join(got, "\n") {
		t.Fatalf("preview ordering not stable across runs")
	}
}

// TestUserSkillPlanFirstInstallChecksums proves a first install produces install
// actions whose checksums equal the exact bytes that would be written.
func TestUserSkillPlanFirstInstallChecksums(t *testing.T) {
	opts := newUserOpts(t)
	plan := mustPlan(t, opts)
	if len(plan.Conflicts) != 0 {
		t.Fatalf("fresh install reported conflicts: %+v", plan.Conflicts)
	}
	for _, asset := range wantUserAssets(opts.Version, opts.Clients) {
		action, ok := actionByAssetID(plan, asset.AssetID)
		if !ok {
			t.Fatalf("no action for %s", asset.AssetID)
		}
		if action.Kind != UserActionInstall {
			t.Errorf("%s: kind %q, want install", asset.AssetID, action.Kind)
		}
		if action.Checksum != asset.Checksum {
			t.Errorf("%s: checksum %q, want %q", asset.AssetID, action.Checksum, asset.Checksum)
		}
	}
}

// TestUserSkillPlanAllCurrentIsNoOp proves a re-plan after a successful install
// reports every asset as unchanged.
func TestUserSkillPlanAllCurrentIsNoOp(t *testing.T) {
	opts := newUserOpts(t)
	mustApply(t, opts)
	plan := mustPlan(t, opts)
	if len(plan.Conflicts) != 0 {
		t.Fatalf("re-plan reported conflicts: %+v", plan.Conflicts)
	}
	for _, action := range plan.Actions {
		if action.Kind != UserActionUnchanged {
			t.Errorf("%s: kind %q, want unchanged", action.Destination, action.Kind)
		}
	}
}

// TestUserSkillPlanUpdatesRecordedBytes proves that a destination whose bytes
// still match the recorded checksum is planned as an update when the shipped
// bytes change (here via the package version bump in the Claude manifest).
func TestUserSkillPlanUpdatesRecordedBytes(t *testing.T) {
	opts := newUserOpts(t)
	opts.Version = "1.0.0"
	mustApply(t, opts)

	bumped := opts
	bumped.Version = "2.0.0"
	plan := mustPlan(t, bumped)

	manifestID := "claude:.claude-plugin/plugin.json"
	action, ok := actionByAssetID(plan, manifestID)
	if !ok {
		t.Fatalf("no action for %s", manifestID)
	}
	if action.Kind != UserActionUpdate {
		t.Fatalf("%s: kind %q, want update", manifestID, action.Kind)
	}
	if _, ok := conflictByAssetID(plan, manifestID); ok {
		t.Fatalf("%s reported as conflict, want update", manifestID)
	}
}

// TestUserSkillPlanConflictsOnLocalModification proves a Prowl-owned file the
// user edited becomes a conflict, never an overwrite.
func TestUserSkillPlanConflictsOnLocalModification(t *testing.T) {
	opts := newUserOpts(t)
	mustApply(t, opts)

	dest := filepath.Join(opts.Home, ".omp", "agent", "skills", "code-search", "SKILL.md")
	if err := os.WriteFile(dest, []byte("user edited this skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustPlan(t, opts)
	id := "omp:skills/code-search/SKILL.md"
	if _, ok := conflictByAssetID(plan, id); !ok {
		t.Fatalf("locally modified %s not reported as a conflict", id)
	}
	if action, ok := actionByAssetID(plan, id); ok {
		t.Fatalf("locally modified %s planned as %q instead of a conflict", id, action.Kind)
	}
}

// TestUserSkillPlanConflictsOnUnownedFile proves a pre-existing file with no
// ownership record is a conflict, not a silent overwrite.
func TestUserSkillPlanConflictsOnUnownedFile(t *testing.T) {
	opts := newUserOpts(t)
	dest := filepath.Join(opts.Home, ".claude", "skills", "prowl", "commands", "search.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("someone else's command\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustPlan(t, opts)
	id := "claude:commands/search.md"
	if _, ok := conflictByAssetID(plan, id); !ok {
		t.Fatalf("unowned pre-existing %s not reported as a conflict", id)
	}
	if _, ok := actionByAssetID(plan, id); ok {
		t.Fatalf("unowned pre-existing %s planned as a write", id)
	}
	// Other destinations still install normally.
	if _, ok := actionByAssetID(plan, "claude:hooks/hooks.json"); !ok {
		t.Fatalf("unrelated destination lost its install action")
	}
}

// TestUserSkillPlanRemovesExactLegacyAndPreservesModified proves migration off
// the retired skill removes an exact embedded copy but never a user-edited one.
func TestUserSkillPlanRemovesExactLegacyAndPreservesModified(t *testing.T) {
	legacy, ok := skills.Legacy("prowl-repo-exploration")
	if !ok {
		t.Fatal("legacy skill fixture missing")
	}
	opts := newUserOpts(t)

	exact := filepath.Join(opts.Home, ".claude", "skills", "prowl", "skills", "prowl-repo-exploration", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(exact), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exact, []byte(legacy.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	modified := filepath.Join(opts.Home, ".omp", "agent", "skills", "prowl-repo-exploration", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(modified), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modified, []byte(legacy.Content+"\nlocal tweak\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := mustPlan(t, opts)
	removeID := "claude:skills/prowl-repo-exploration/SKILL.md"
	action, ok := actionByAssetID(plan, removeID)
	if !ok || action.Kind != UserActionRemove {
		t.Fatalf("exact legacy copy not planned for removal: %+v", action)
	}
	preserveID := "omp:skills/prowl-repo-exploration/SKILL.md"
	if _, ok := conflictByAssetID(plan, preserveID); !ok {
		t.Fatalf("modified legacy copy not preserved as a conflict")
	}
	if _, ok := actionByAssetID(plan, preserveID); ok {
		t.Fatalf("modified legacy copy planned for removal")
	}

	// Applying actually deletes the exact copy and leaves the edited one.
	if _, err := ApplyUserSkills(opts, plan, true); err != nil {
		t.Fatalf("apply legacy removal: %v", err)
	}
	if _, err := os.Lstat(exact); !os.IsNotExist(err) {
		t.Fatalf("exact legacy copy survived removal: %v", err)
	}
	if _, err := os.Stat(modified); err != nil {
		t.Fatalf("edited legacy copy was destroyed: %v", err)
	}
}

// TestUserSkillPlanRejectsSymlinkAndIrregularDestinations proves Prowl refuses a
// destination that is a symlink or otherwise not a regular file.
func TestUserSkillPlanRejectsSymlinkAndIrregularDestinations(t *testing.T) {
	opts := newUserOpts(t)

	symDest := filepath.Join(opts.Home, ".claude", "skills", "prowl", "hooks", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(symDest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(opts.Home, "elsewhere"), symDest); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// A directory sitting where a regular file is expected is also irregular.
	dirDest := filepath.Join(opts.Home, ".omp", "agent", "extensions", "prowl-routing.ts")
	if err := os.MkdirAll(dirDest, 0o755); err != nil {
		t.Fatal(err)
	}

	plan := mustPlan(t, opts)
	if _, ok := conflictByAssetID(plan, "claude:hooks/hooks.json"); !ok {
		t.Fatalf("symlink destination not rejected as a conflict")
	}
	if _, ok := actionByAssetID(plan, "claude:hooks/hooks.json"); ok {
		t.Fatalf("symlink destination planned for a write")
	}
	if _, ok := conflictByAssetID(plan, "omp:extensions/prowl-routing.ts"); !ok {
		t.Fatalf("non-regular destination not rejected as a conflict")
	}
}

// TestUserSkillPlanSubstitutesPackageVersion proves the Claude manifest is
// stamped with the package version -- both in the checksum and in the bytes
// apply writes -- while the raw template token never reaches disk.
func TestUserSkillPlanSubstitutesPackageVersion(t *testing.T) {
	opts := newUserOpts(t)
	opts.Version = "3.4.5"
	plan := mustPlan(t, opts)

	manifestID := "claude:.claude-plugin/plugin.json"
	action, ok := actionByAssetID(plan, manifestID)
	if !ok {
		t.Fatalf("no action for %s", manifestID)
	}
	var stamped string
	for _, asset := range wantUserAssets(opts.Version, opts.Clients) {
		if asset.AssetID == manifestID {
			stamped = asset.Content
			if action.Checksum != asset.Checksum {
				t.Fatalf("manifest checksum %q, want stamped %q", action.Checksum, asset.Checksum)
			}
		}
	}
	if !strings.Contains(stamped, "3.4.5") || strings.Contains(stamped, "{{VERSION}}") {
		t.Fatalf("oracle content not stamped: %q", stamped)
	}

	if _, err := ApplyUserSkills(opts, plan, true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(opts.Home, ".claude", "skills", "prowl", ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "3.4.5") || strings.Contains(string(got), "{{VERSION}}") {
		t.Fatalf("written manifest not version-stamped: %s", got)
	}
}

// TestUserIntegrationApplyRequiresApprovalWritesNothing proves an unapproved
// apply is a strict no-op: no assets, no manifest.
func TestUserIntegrationApplyRequiresApprovalWritesNothing(t *testing.T) {
	opts := newUserOpts(t)
	plan := mustPlan(t, opts)
	before := treeState(t, opts.Home)

	if _, err := ApplyUserSkills(opts, plan, false); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("unapproved apply error = %v, want ErrApprovalRequired", err)
	}
	if after := treeState(t, opts.Home); after != before {
		t.Fatalf("unapproved apply wrote assets:\n%s", after)
	}
	if _, err := os.Stat(manifestPath(opts)); !os.IsNotExist(err) {
		t.Fatalf("unapproved apply wrote a manifest: %v", err)
	}
}

// TestUserIntegrationApplyIsIdempotent proves re-applying an unchanged plan
// changes nothing on disk.
func TestUserIntegrationApplyIsIdempotent(t *testing.T) {
	opts := newUserOpts(t)
	mustApply(t, opts)

	before := treeState(t, opts.Home)
	stateBefore, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	plan := mustPlan(t, opts)
	if _, err := ApplyUserSkills(opts, plan, true); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if after := treeState(t, opts.Home); after != before {
		t.Fatalf("idempotent apply changed assets")
	}
	stateAfter, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	if string(stateAfter) != string(stateBefore) {
		t.Fatalf("idempotent apply changed the manifest")
	}
}

// TestUserIntegrationApplyRejectsStalePlan proves a plan is refused when a
// destination changed between planning and applying, and writes nothing.
func TestUserIntegrationApplyRejectsStalePlan(t *testing.T) {
	opts := newUserOpts(t)
	mustApply(t, opts)

	plan := mustPlan(t, opts) // all-unchanged plan

	dest := filepath.Join(opts.Home, ".claude", "skills", "prowl", "commands", "search.md")
	if err := os.WriteFile(dest, []byte("changed after planning\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeState(t, opts.Home)
	if _, err := ApplyUserSkills(opts, plan, true); !errors.Is(err, ErrUserPlanStale) {
		t.Fatalf("stale apply error = %v, want ErrUserPlanStale", err)
	}
	if after := treeState(t, opts.Home); after != before {
		t.Fatalf("stale apply mutated the tree")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "changed after planning\n" {
		t.Fatalf("stale apply overwrote the changed destination")
	}
}

// TestUserIntegrationRollbackRemovesEarlierWritesAndDefersManifest proves a
// mid-apply write failure restores every earlier file and never leaves a
// manifest behind -- the manifest is written strictly last. The write seam is a
// private argument to the unexported apply, never a production interface.
func TestUserIntegrationRollbackRemovesEarlierWritesAndDefersManifest(t *testing.T) {
	opts := newUserOpts(t)
	plan := mustPlan(t, opts)

	asset := 0
	fail := func(root *os.Root, rel string, data []byte, mode os.FileMode) error {
		if strings.HasSuffix(rel, "agent-assets.json") {
			t.Errorf("manifest written before all assets succeeded")
			return nil
		}
		asset++
		if asset == 2 {
			return errors.New("injected asset write failure")
		}
		return writeAtomicInRoot(root, rel, data, mode)
	}

	if _, err := applyUserSkills(opts, plan, true, fail); err == nil {
		t.Fatal("apply succeeded despite injected failure")
	}
	if state := treeState(t, opts.Home); state != "" {
		t.Fatalf("rollback left files behind:\n%s", state)
	}
	if _, err := os.Stat(manifestPath(opts)); !os.IsNotExist(err) {
		t.Fatalf("failed apply left a manifest: %v", err)
	}

	// A clean retry still succeeds, proving no corruption.
	mustApply(t, opts)
	if _, err := os.Stat(filepath.Join(opts.Home, ".claude", "skills", "prowl", ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("clean retry did not install: %v", err)
	}
}

// TestUserIntegrationRollbackRestoresPriorFilesAndManifest proves a failure on
// the final manifest write restores every asset changed in that run and the
// prior manifest bytes.
func TestUserIntegrationRollbackRestoresPriorFilesAndManifest(t *testing.T) {
	opts := newUserOpts(t)
	opts.Version = "1.0.0"
	mustApply(t, opts)

	priorState, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(opts.Home, ".claude", "skills", "prowl", ".claude-plugin", "plugin.json")
	priorPlugin, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal(err)
	}
	// Delete a skill so the next apply has two writes before the manifest.
	deleted := filepath.Join(opts.Home, ".omp", "agent", "skills", "code-search", "SKILL.md")
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}

	bumped := opts
	bumped.Version = "2.0.0"
	plan := mustPlan(t, bumped)

	fail := func(root *os.Root, rel string, data []byte, mode os.FileMode) error {
		if strings.HasSuffix(rel, "agent-assets.json") {
			return errors.New("injected manifest write failure")
		}
		return writeAtomicInRoot(root, rel, data, mode)
	}

	if _, err := applyUserSkills(bumped, plan, true, fail); err == nil {
		t.Fatal("apply succeeded despite injected manifest failure")
	}

	gotPlugin, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotPlugin) != string(priorPlugin) {
		t.Fatalf("plugin.json not restored to prior (v1) bytes after rollback")
	}
	if _, err := os.Stat(deleted); !os.IsNotExist(err) {
		t.Fatalf("reinstalled skill not rolled back to absent: %v", err)
	}
	gotState, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotState) != string(priorState) {
		t.Fatalf("prior manifest not restored after rollback")
	}
}

// TestUserIntegrationManifestRecordsOwnership proves the persisted manifest
// records schema, package version, asset id, destination, and installed-byte
// checksum for each owned asset.
func TestUserIntegrationManifestRecordsOwnership(t *testing.T) {
	opts := newUserOpts(t)
	mustApply(t, opts)

	data, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schema         int    `json:"schema"`
		PackageVersion string `json:"package_version"`
		Assets         []struct {
			AssetID     string `json:"asset_id"`
			Destination string `json:"destination"`
			Checksum    string `json:"checksum"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if doc.Schema == 0 {
		t.Errorf("manifest schema not recorded")
	}
	if doc.PackageVersion != opts.Version {
		t.Errorf("manifest package version = %q, want %q", doc.PackageVersion, opts.Version)
	}
	byID := map[string]string{}
	for _, entry := range doc.Assets {
		if entry.AssetID == "" || entry.Destination == "" || entry.Checksum == "" {
			t.Errorf("incomplete manifest entry: %+v", entry)
		}
		byID[entry.AssetID] = entry.Checksum
	}
	for _, asset := range wantUserAssets(opts.Version, opts.Clients) {
		if byID[asset.AssetID] != asset.Checksum {
			t.Errorf("%s: manifest checksum %q, want %q", asset.AssetID, byID[asset.AssetID], asset.Checksum)
		}
	}
}

// TestUserIntegrationStateHasNoBodiesOrForeignPaths proves the manifest never
// stores file contents or absolute paths outside the user's integration roots.
func TestUserIntegrationStateHasNoBodiesOrForeignPaths(t *testing.T) {
	opts := newUserOpts(t)
	mustApply(t, opts)

	data, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, opts.Home) || strings.Contains(text, opts.StateDir) {
		t.Fatalf("state leaks an absolute filesystem path")
	}
	// A distinctive slice of the Claude manifest body must not appear.
	if strings.Contains(text, "Route repository discovery through the read-only") {
		t.Fatalf("state stores installed file contents")
	}
	var doc struct {
		Assets []struct {
			Destination string `json:"destination"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, entry := range doc.Assets {
		if filepath.IsAbs(entry.Destination) || strings.HasPrefix(entry.Destination, "/") {
			t.Errorf("manifest destination is absolute: %s", entry.Destination)
		}
		if !strings.HasPrefix(entry.Destination, ".claude/") && !strings.HasPrefix(entry.Destination, ".omp/") {
			t.Errorf("manifest destination escapes the integration roots: %s", entry.Destination)
		}
	}
}

// TestUserSkillVerifyReportsHealthStates proves VerifyUserSkills exposes a
// single-model health view Task 6 can reuse: missing, current, stale, conflict.
func TestUserSkillVerifyReportsHealthStates(t *testing.T) {
	opts := newUserOpts(t)
	opts.Version = "1.0.0"

	fresh, err := VerifyUserSkills(opts)
	if err != nil {
		t.Fatalf("verify (fresh): %v", err)
	}
	for _, asset := range fresh.Assets {
		if asset.State != UserAssetMissing {
			t.Errorf("fresh %s state %q, want missing", asset.Destination, asset.State)
		}
	}

	mustApply(t, opts)
	current, err := VerifyUserSkills(opts)
	if err != nil {
		t.Fatalf("verify (installed): %v", err)
	}
	if !allState(current, UserAssetCurrent) {
		t.Fatalf("installed assets not all current: %+v", current.Assets)
	}

	// Local edit -> conflict for that asset.
	edited := filepath.Join(opts.Home, ".claude", "skills", "prowl", "commands", "search.md")
	if err := os.WriteFile(edited, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Version bump -> the manifest asset goes stale.
	bumped := opts
	bumped.Version = "2.0.0"
	health, err := VerifyUserSkills(bumped)
	if err != nil {
		t.Fatalf("verify (mixed): %v", err)
	}
	states := map[string]UserAssetState{}
	for _, asset := range health.Assets {
		states[asset.AssetID] = asset.State
	}
	if states["claude:commands/search.md"] != UserAssetConflict {
		t.Errorf("edited asset state %q, want conflict", states["claude:commands/search.md"])
	}
	if states["claude:.claude-plugin/plugin.json"] != UserAssetStale {
		t.Errorf("version-bumped manifest state %q, want stale", states["claude:.claude-plugin/plugin.json"])
	}
}

func allState(health UserHealth, want UserAssetState) bool {
	for _, asset := range health.Assets {
		if asset.State != want {
			return false
		}
	}
	return len(health.Assets) > 0
}

// writeTamperedManifest overwrites the on-disk ownership manifest with m, for
// tests that corrupt or falsify an ownership record.
func writeTamperedManifest(t *testing.T, opts UserInstallOptions, m userManifest) {
	t.Helper()
	data, err := marshalUserManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath(opts)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath(opts), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestUserIntegrationApplyRejectsTamperedPlan proves apply refuses a plan whose
// digest does not match its own canonical content -- Actions, Conflicts,
// Version, or Digest tampered -- and writes nothing. (Finding 1)
func TestUserIntegrationApplyRejectsTamperedPlan(t *testing.T) {
	cases := []struct {
		name string
		mut  func(UserPlan) UserPlan
	}{
		{"actions", func(p UserPlan) UserPlan { p.Actions[0].Checksum = "deadbeef"; return p }},
		{"conflicts", func(p UserPlan) UserPlan {
			p.Conflicts = append(p.Conflicts, UserConflict{Client: "claude", AssetID: "x", Destination: ".claude/x", Reason: "injected"})
			return p
		}},
		{"version", func(p UserPlan) UserPlan { p.Version += "-tampered"; return p }},
		{"digest", func(p UserPlan) UserPlan { p.Digest = "0000"; return p }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := newUserOpts(t)
			plan := tc.mut(mustPlan(t, opts))
			before := treeState(t, opts.Home)
			if _, err := ApplyUserSkills(opts, plan, true); err == nil {
				t.Errorf("tampered plan applied")
			}
			if after := treeState(t, opts.Home); after != before {
				t.Errorf("tampered plan wrote assets")
			}
			if _, err := os.Stat(manifestPath(opts)); !os.IsNotExist(err) {
				t.Errorf("tampered plan wrote a manifest")
			}
		})
	}
}

// TestUserSkillPlanConflictsOnUnrecordedIdenticalFile proves a pre-existing file
// byte-identical to the desired asset but with no ownership record is a
// conflict, never adopted as unchanged. (Finding 2)
func TestUserSkillPlanConflictsOnUnrecordedIdenticalFile(t *testing.T) {
	opts := newUserOpts(t)
	id := "omp:skills/code-search/SKILL.md"
	var content string
	for _, asset := range wantUserAssets(opts.Version, opts.Clients) {
		if asset.AssetID == id {
			content = asset.Content
		}
	}
	dest := filepath.Join(opts.Home, ".omp", "agent", "skills", "code-search", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustPlan(t, opts)
	if _, ok := conflictByAssetID(plan, id); !ok {
		t.Fatalf("byte-identical unrecorded file not reported as a conflict")
	}
	if action, ok := actionByAssetID(plan, id); ok {
		t.Fatalf("byte-identical unrecorded file planned as %q; want conflict", action.Kind)
	}
}

// TestUserSkillPlanRejectsMismatchedOwnershipRecord proves ownership is
// authorized only when AssetID, Client, and Destination all match the recorded
// entry: a record with the right AssetID but wrong client or destination does
// not authorize a write. (Finding 3)
func TestUserSkillPlanRejectsMismatchedOwnershipRecord(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(*userManifestEntry)
	}{
		{"wrong-client", func(e *userManifestEntry) { e.Client = "omp" }},
		{"wrong-destination", func(e *userManifestEntry) { e.Destination = ".claude/skills/prowl/elsewhere.json" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := newUserOpts(t)
			opts.Version = "1.0.0"
			mustApply(t, opts)

			m, err := loadUserManifest(opts)
			if err != nil {
				t.Fatal(err)
			}
			for i := range m.Assets {
				if m.Assets[i].AssetID == "claude:.claude-plugin/plugin.json" {
					tc.tamper(&m.Assets[i])
				}
			}
			writeTamperedManifest(t, opts, m)

			plan := mustPlan(t, opts)
			id := "claude:.claude-plugin/plugin.json"
			if _, ok := conflictByAssetID(plan, id); !ok {
				t.Fatalf("mismatched ownership record not treated as a conflict")
			}
			if _, ok := actionByAssetID(plan, id); ok {
				t.Fatalf("mismatched ownership record authorized a write")
			}
		})
	}
}

// TestUserIntegrationRechecksActionPreconditionAtMutation proves that when a
// destination changes after the fresh plan is computed but before its action
// mutates, apply aborts and rolls back instead of overwriting. The write seam
// plants a foreign file at a not-yet-applied target to simulate the race.
// (Finding 4)
func TestUserIntegrationRechecksActionPreconditionAtMutation(t *testing.T) {
	opts := newUserOpts(t)
	plan := mustPlan(t, opts)
	first := filepath.Join(opts.Home, filepath.FromSlash(plan.Actions[0].Destination))
	planted := filepath.Join(opts.Home, filepath.FromSlash(plan.Actions[1].Destination))

	calls := 0
	seam := func(root *os.Root, rel string, data []byte, mode os.FileMode) error {
		if strings.HasSuffix(rel, "agent-assets.json") {
			return writeAtomicInRoot(root, rel, data, mode)
		}
		calls++
		if calls == 1 {
			if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(planted, []byte("planted after planning\n"), 0o644); err != nil {
				return err
			}
		}
		return writeAtomicInRoot(root, rel, data, mode)
	}

	if _, err := applyUserSkills(opts, plan, true, seam); err == nil {
		t.Fatal("apply overwrote a destination that changed after planning")
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("first write not rolled back: %v", err)
	}
	if got, _ := os.ReadFile(planted); string(got) != "planted after planning\n" {
		t.Fatalf("recheck abort destroyed foreign content at %s", plan.Actions[1].Destination)
	}
	if _, err := os.Stat(manifestPath(opts)); !os.IsNotExist(err) {
		t.Fatalf("manifest written despite recheck abort: %v", err)
	}
}

// TestUserIntegrationRejectsSymlinkedStateDir proves plan and apply refuse a
// generated state directory that is a symlink, and write nothing through it.
// (Finding 5)
func TestUserIntegrationRejectsSymlinkedStateDir(t *testing.T) {
	opts := newUserOpts(t)
	plan := mustPlan(t, opts)

	target := t.TempDir()
	link := filepath.Join(opts.StateDir, "prowl-agent")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := ApplyUserSkills(opts, plan, true); err == nil {
		t.Fatal("apply wrote through a symlinked state directory")
	}
	if state := treeState(t, opts.Home); state != "" {
		t.Fatalf("apply wrote assets through a symlinked state dir:\n%s", state)
	}
	if _, err := os.Stat(filepath.Join(target, "agent-assets.json")); !os.IsNotExist(err) {
		t.Fatalf("apply wrote the manifest through the symlink: %v", err)
	}
	if _, err := PlanUserSkills(opts); err == nil {
		t.Fatal("plan followed a symlinked state directory")
	}
}

// TestUserIntegrationSurfacesRollbackFailure proves a rollback failure is
// combined with the initiating error so the caller sees recovery is needed. The
// seam writes the first asset, then replaces its path with a non-empty directory
// and fails the second write, so rollback cannot remove the first file. (Finding 6)
func TestUserIntegrationSurfacesRollbackFailure(t *testing.T) {
	opts := newUserOpts(t)
	plan := mustPlan(t, opts)
	first := filepath.Join(opts.Home, filepath.FromSlash(plan.Actions[0].Destination))

	calls := 0
	seam := func(root *os.Root, rel string, data []byte, mode os.FileMode) error {
		if strings.HasSuffix(rel, "agent-assets.json") {
			return writeAtomicInRoot(root, rel, data, mode)
		}
		calls++
		if calls == 1 {
			return writeAtomicInRoot(root, rel, data, mode)
		}
		// Sabotage the first write's path so its rollback removal fails.
		_ = os.Remove(first)
		if err := os.MkdirAll(filepath.Join(first, "child"), 0o755); err != nil {
			return err
		}
		return errors.New("injected asset write failure")
	}

	_, err := applyUserSkills(opts, plan, true, seam)
	if err == nil {
		t.Fatal("apply succeeded despite injected failure")
	}
	if !strings.Contains(err.Error(), "injected asset write failure") {
		t.Errorf("initiating error not surfaced: %v", err)
	}
	if !strings.Contains(err.Error(), "rollback") {
		t.Errorf("rollback failure not surfaced: %v", err)
	}
}

// TestUserIntegrationRollbackRemovesNewlyCreatedStateLeaf proves the
// transaction-owned prowl-agent leaf is rolled back on failure, while the
// persistent state base holding the lock inode is preserved. (Finding 7 +
// round-2 lock lifecycle)
func TestUserIntegrationRollbackRemovesNewlyCreatedStateLeaf(t *testing.T) {
	opts := newUserOpts(t)
	// A nested, not-yet-existent state base the apply must create for the lock.
	opts.StateDir = filepath.Join(t.TempDir(), "xdg-state")
	plan := mustPlan(t, opts)

	calls := 0
	seam := func(root *os.Root, rel string, data []byte, mode os.FileMode) error {
		if strings.HasSuffix(rel, "agent-assets.json") {
			return writeAtomicInRoot(root, rel, data, mode)
		}
		calls++
		if calls == 2 {
			return errors.New("injected asset write failure")
		}
		return writeAtomicInRoot(root, rel, data, mode)
	}

	if _, err := applyUserSkills(opts, plan, true, seam); err == nil {
		t.Fatal("apply succeeded despite injected failure")
	}
	// The transaction-owned prowl-agent leaf is removed.
	if _, err := os.Stat(filepath.Join(opts.StateDir, "prowl-agent")); !os.IsNotExist(err) {
		t.Fatalf("rolled-back apply left the prowl-agent state dir")
	}
	// The base persists because it holds the persistent lock inode.
	if _, err := os.Stat(opts.StateDir); err != nil {
		t.Fatalf("rollback removed the persistent lock base: %v", err)
	}
	if _, err := os.Stat(filepath.Join(opts.StateDir, "prowl-agent.lock")); err != nil {
		t.Fatalf("persistent lock file missing after rollback: %v", err)
	}
}

// TestUserIntegrationConcurrentApplyRejectedWhileLocked proves apply takes the
// shared user-state lock with deterministic, bounded real contention: while the
// lock is held, a synchronous apply given a short lock timeout fails fast with
// the concrete already-in-progress error and writes nothing; once released the
// same apply succeeds. No goroutine, no sleep. If apply did not take the lock,
// it would install immediately and the error assertion would fail. (Finding 3)
func TestUserIntegrationConcurrentApplyRejectedWhileLocked(t *testing.T) {
	opts := newUserOpts(t)
	plan := mustPlan(t, opts)

	unlock, err := acquireUserStateLock(opts, setupLockTimeout)
	if err != nil {
		t.Fatalf("acquire state lock: %v", err)
	}

	before := treeState(t, opts.Home)
	_, lockedErr := applyUserSkillsLocked(opts, plan, true, writeAtomicInRoot, 50*time.Millisecond)
	after := treeState(t, opts.Home)
	unlock()

	if lockedErr == nil {
		t.Fatal("apply proceeded while the state lock was held")
	}
	if !strings.Contains(lockedErr.Error(), "already in progress") {
		t.Fatalf("expected the already-in-progress lock error, got: %v", lockedErr)
	}
	if after != before {
		t.Fatalf("locked-out apply wrote assets")
	}

	// After release the same apply succeeds and installs.
	if _, err := applyUserSkillsLocked(opts, plan, true, writeAtomicInRoot, setupLockTimeout); err != nil {
		t.Fatalf("apply after lock release failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(opts.Home, ".claude", "skills", "prowl", ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("apply after release did not install: %v", err)
	}
}

// TestUserIntegrationDisjointClientInstallsRetainOwnership proves the merge is
// based on the on-disk manifest read at apply time: installing a second,
// disjoint client retains the first client's ownership records instead of
// dropping them. (Findings 1 + 3, ownership-merge)
func TestUserIntegrationDisjointClientInstallsRetainOwnership(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	claudeOpts := UserInstallOptions{Home: home, StateDir: stateDir, Version: "1.0.0", Clients: []string{"claude"}}
	ompOpts := UserInstallOptions{Home: home, StateDir: stateDir, Version: "1.0.0", Clients: []string{"omp"}}

	mustApply(t, claudeOpts)
	mustApply(t, ompOpts)

	m, err := loadUserManifest(claudeOpts)
	if err != nil {
		t.Fatal(err)
	}
	var haveClaude, haveOMP bool
	for _, entry := range m.Assets {
		switch entry.Client {
		case "claude":
			haveClaude = true
		case "omp":
			haveOMP = true
		}
	}
	if !haveClaude || !haveOMP {
		t.Fatalf("second install dropped the first client's ownership: claude=%v omp=%v", haveClaude, haveOMP)
	}
}

// TestUserIntegrationRechecksUnchangedBeforeCommit proves an unchanged asset
// deleted after the fresh plan but before the manifest commit aborts the apply
// and rolls back, rather than committing a manifest that claims ownership of a
// now-absent file. (Finding 2)
func TestUserIntegrationRechecksUnchangedBeforeCommit(t *testing.T) {
	opts := newUserOpts(t)
	mustApply(t, opts)

	priorState, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	// Delete the first asset so it re-installs (a write that triggers the seam),
	// which sorts before the omp skill we will delete mid-apply.
	pluginPath := filepath.Join(opts.Home, ".claude", "skills", "prowl", ".claude-plugin", "plugin.json")
	if err := os.Remove(pluginPath); err != nil {
		t.Fatal(err)
	}
	plan := mustPlan(t, opts) // plugin.json => install; everything else unchanged

	victim := filepath.Join(opts.Home, ".omp", "agent", "skills", "code-search", "SKILL.md")
	once := false
	seam := func(root *os.Root, rel string, data []byte, mode os.FileMode) error {
		if !strings.HasSuffix(rel, "agent-assets.json") && !once {
			once = true
			// External deletion of a file the plan marked unchanged.
			if err := os.Remove(victim); err != nil {
				return err
			}
		}
		return writeAtomicInRoot(root, rel, data, mode)
	}

	if _, err := applyUserSkills(opts, plan, true, seam); err == nil {
		t.Fatal("apply committed despite an unchanged asset deleted mid-apply")
	}
	// Manifest not committed: still the prior bytes.
	gotState, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotState) != string(priorState) {
		t.Fatalf("manifest committed despite unchanged-asset recheck abort")
	}
	// The reinstalled plugin.json was rolled back to absent.
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Fatalf("reinstalled asset not rolled back after unchanged-recheck abort")
	}
}

// TestUserIntegrationRollbackReportsDirRemovalFailure proves rollback collects a
// non-ENOENT directory-removal failure (foreign content left a created dir
// non-empty) into its returned error while preserving that foreign content.
// (Finding 4 / round-2 point 4)
func TestUserIntegrationRollbackReportsDirRemovalFailure(t *testing.T) {
	opts := newUserOpts(t)
	plan := mustPlan(t, opts)
	firstDir := filepath.Dir(filepath.Join(opts.Home, filepath.FromSlash(plan.Actions[0].Destination)))
	foreign := filepath.Join(firstDir, "user-notes.txt")

	calls := 0
	seam := func(root *os.Root, rel string, data []byte, mode os.FileMode) error {
		if strings.HasSuffix(rel, "agent-assets.json") {
			return writeAtomicInRoot(root, rel, data, mode)
		}
		calls++
		if calls == 1 {
			return writeAtomicInRoot(root, rel, data, mode) // first asset written; its dir created
		}
		// Plant foreign content in the created dir, then fail so rollback runs.
		if err := os.WriteFile(foreign, []byte("keep me\n"), 0o644); err != nil {
			return err
		}
		return errors.New("injected asset write failure")
	}

	_, err := applyUserSkills(opts, plan, true, seam)
	if err == nil {
		t.Fatal("apply succeeded despite injected failure")
	}
	if !strings.Contains(err.Error(), "rollback") {
		t.Errorf("incomplete directory rollback not surfaced: %v", err)
	}
	if got, _ := os.ReadFile(foreign); string(got) != "keep me\n" {
		t.Errorf("rollback destroyed foreign content instead of preserving it")
	}
}

// TestUserIntegrationAbortsOnManifestChangedBeforeCommit proves that if the
// ownership manifest changes after the single locked read but before the
// commit -- as an external or older installer might do -- apply aborts and
// rolls back its asset mutations while leaving the external manifest change
// untouched (the snapshot is enlisted only immediately before the manifest
// write). (Round-4 findings 1 + 2)
func TestUserIntegrationAbortsOnManifestChangedBeforeCommit(t *testing.T) {
	opts := newUserOpts(t)
	mustApply(t, opts)
	// Delete plugin.json so the re-apply has one install write (triggering the seam).
	pluginPath := filepath.Join(opts.Home, ".claude", "skills", "prowl", ".claude-plugin", "plugin.json")
	if err := os.Remove(pluginPath); err != nil {
		t.Fatal(err)
	}
	plan := mustPlan(t, opts)

	const foreign = `{"schema":1,"package_version":"x","assets":[]}` + "\n"
	once := false
	seam := func(root *os.Root, rel string, data []byte, mode os.FileMode) error {
		if !strings.HasSuffix(rel, "agent-assets.json") && !once {
			once = true
			// An external/older installer rewrites the ownership manifest mid-apply.
			if err := os.WriteFile(manifestPath(opts), []byte(foreign), 0o644); err != nil {
				return err
			}
		}
		return writeAtomicInRoot(root, rel, data, mode)
	}

	if _, err := applyUserSkills(opts, plan, true, seam); err == nil {
		t.Fatal("apply committed despite the manifest changing since the snapshot")
	}
	// The external manifest change survives: rollback does not restore the
	// snapshot because it was never enlisted before the pre-commit abort.
	got, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != foreign {
		t.Fatalf("apply overwrote/rolled back the external manifest change; got:\n%s", got)
	}
	if _, err := os.Stat(pluginPath); !os.IsNotExist(err) {
		t.Fatalf("reinstalled asset not rolled back after manifest-change abort")
	}
}

// TestUserIntegrationFinalUnchangedPassCatchesEarlyDrift proves unchanged checks
// are finalized in a single pass immediately before commit, not in action
// order: an unchanged asset that sorts BEFORE the sole mutable action, deleted
// during that action's write, is still caught and aborts the commit. An
// action-order recheck would have passed it before the deletion. (Round-3
// finding 2)
func TestUserIntegrationFinalUnchangedPassCatchesEarlyDrift(t *testing.T) {
	opts := newUserOpts(t)
	mustApply(t, opts)
	priorState, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	// The mutable action sorts late (an omp skill); the unchanged victim sorts
	// first (the claude plugin manifest).
	late := filepath.Join(opts.Home, ".omp", "agent", "skills", "prowl-durable-knowledge", "SKILL.md")
	if err := os.Remove(late); err != nil {
		t.Fatal(err)
	}
	early := filepath.Join(opts.Home, ".claude", "skills", "prowl", ".claude-plugin", "plugin.json")
	plan := mustPlan(t, opts)

	once := false
	seam := func(root *os.Root, rel string, data []byte, mode os.FileMode) error {
		if !strings.HasSuffix(rel, "agent-assets.json") && !once {
			once = true
			// Delete an early-sorting unchanged asset during the late install.
			if err := os.Remove(early); err != nil {
				return err
			}
		}
		return writeAtomicInRoot(root, rel, data, mode)
	}

	if _, err := applyUserSkills(opts, plan, true, seam); err == nil {
		t.Fatal("apply committed despite an early unchanged asset drifting before commit")
	}
	got, err := os.ReadFile(manifestPath(opts))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(priorState) {
		t.Fatalf("manifest committed despite the final unchanged-pass abort")
	}
}

func TestUserIntegrationHealthStates(t *testing.T) {
	probe := func(version string, found bool) userCLIProbe {
		return func() (string, bool) { return version, found }
	}

	absentOpts := newUserOpts(t)
	absentOpts.Clients = nil
	absent, err := verifyUserIntegrations(absentOpts, probe(absentOpts.Version, true))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Clients[0].Status != UserIntegrationAbsent || absent.Clients[1].Status != UserIntegrationAbsent {
		t.Fatalf("absent clients: %+v", absent.Clients)
	}

	opts := newUserOpts(t)
	missing, err := verifyUserIntegrations(opts, probe(opts.Version, true))
	if err != nil {
		t.Fatal(err)
	}
	if missing.Clients[0].Status != UserIntegrationMissing || missing.CLI.Status != UserCLIStatusCurrent {
		t.Fatalf("missing health: %+v", missing)
	}
	if _, err := os.Stat(filepath.Join(opts.StateDir, "prowl-agent")); !os.IsNotExist(err) {
		t.Fatalf("health created state: %v", err)
	}

	mustApply(t, opts)
	current, err := verifyUserIntegrations(opts, probe(opts.Version, true))
	if err != nil {
		t.Fatal(err)
	}
	if current.Clients[0].Status != UserIntegrationCurrent || current.Clients[0].Activation != UserActivationRestartRequired {
		t.Fatalf("claude current health: %+v", current.Clients[0])
	}
	if current.Clients[1].Status != UserIntegrationCurrent || current.Clients[1].Activation != UserActivationReloadRequired {
		t.Fatalf("omp current health: %+v", current.Clients[1])
	}

	bumped := opts
	bumped.Version = "v10.0.0"
	stale, err := verifyUserIntegrations(bumped, probe(bumped.Version, true))
	if err != nil {
		t.Fatal(err)
	}
	if stale.Clients[0].Status != UserIntegrationStale || stale.Clients[1].Status != UserIntegrationStale {
		t.Fatalf("stale package: %+v", stale.Clients)
	}

	edited := filepath.Join(opts.Home, ".claude", "skills", "prowl", "commands", "search.md")
	if err := os.WriteFile(edited, []byte("locally edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conflicted, err := verifyUserIntegrations(opts, probe(opts.Version, true))
	if err != nil {
		t.Fatal(err)
	}
	if conflicted.Clients[0].Status != UserIntegrationConflict {
		t.Fatalf("conflict health: %+v", conflicted.Clients[0])
	}

	missingCLI, err := verifyUserIntegrations(opts, probe("", false))
	if err != nil {
		t.Fatal(err)
	}
	if missingCLI.CLI.Status != UserCLIStatusMissing {
		t.Fatalf("missing CLI: %+v", missingCLI.CLI)
	}
	mismatch, err := verifyUserIntegrations(opts, probe("v0.0.1", true))
	if err != nil {
		t.Fatal(err)
	}
	if mismatch.CLI.Status != UserCLIStatusMismatch {
		t.Fatalf("mismatched CLI: %+v", mismatch.CLI)
	}
}

func TestUserIntegrationHealthProductionCLIProbe(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	program := `package main
import ("fmt"; "os"; "strings"; "time")
func main() {
	switch os.Getenv("PROWL_TEST_PROBE") {
	case "valid": fmt.Println("prowl-agent version v9.9.9")
	case "malformed": fmt.Println("prowl-agent v9.9.9")
	case "overflow": fmt.Print(strings.Repeat("x", 5000))
	case "nonzero": os.Exit(3)
	case "slow": time.Sleep(3*time.Second); fmt.Println("prowl-agent version v9.9.9")
	}
}`
	if err := os.WriteFile(source, []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "prowl-agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	build := exec.Command(goBin, "build", "-o", binary, source)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake prowl-agent: %v\n%s", err, output)
	}
	t.Setenv("PATH", dir)

	tests := []struct {
		mode    string
		version string
		found   bool
	}{
		{"valid", "v9.9.9", true},
		{"malformed", "", false},
		{"overflow", "", false},
		{"nonzero", "", false},
		{"slow", "", false},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			t.Setenv("PROWL_TEST_PROBE", test.mode)
			start := time.Now()
			version, found := probeUserCLI()
			if version != test.version || found != test.found {
				t.Fatalf("probe = (%q, %v), want (%q, %v)", version, found, test.version, test.found)
			}
			if test.mode == "slow" && time.Since(start) >= 3*time.Second {
				t.Fatalf("slow probe was not bounded: %s", time.Since(start))
			}
		})
	}
}

// TestNormalizeUserClientsIncludesHermes proves hermes is a supported user
// client: normalization keeps it alongside claude and omp, drops unknown names,
// deduplicates, and sorts, so planning is deterministic regardless of detection
// order.
func TestNormalizeUserClientsIncludesHermes(t *testing.T) {
	got := normalizeUserClients([]string{"omp", "hermes", "claude", "hermes", "bogus", ""})
	want := []string{"claude", "hermes", "omp"}
	if len(got) != len(want) {
		t.Fatalf("normalizeUserClients = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeUserClients = %v, want %v", got, want)
		}
	}
	if root := userClientRoot(IntegrationHermes); root != ".hermes/skills/prowl" {
		t.Fatalf("userClientRoot(hermes) = %q, want .hermes/skills/prowl", root)
	}
	if src := nativeAssetClient(IntegrationHermes); src != IntegrationClaude {
		t.Fatalf("nativeAssetClient(hermes) = %q, want claude", src)
	}
}

// TestUserSkillHermesMirrorsClaude proves the hermes client installs Claude's
// exact asset bodies under .hermes/skills/prowl: every destination sits under
// that root, and each native/skill asset's content matches Claude's byte for
// byte, only the destination root and the ownership assetID differ.
func TestUserSkillHermesMirrorsClaude(t *testing.T) {
	opts := UserInstallOptions{
		Home:     t.TempDir(),
		StateDir: t.TempDir(),
		Version:  "9.9.9",
		Clients:  []string{"hermes"},
	}
	plan := mustPlan(t, opts)
	if len(plan.Actions) == 0 {
		t.Fatal("hermes plan produced no actions")
	}
	// Build Claude's expected asset set, then re-root it under hermes: the two
	// clients must ship identical bodies at parallel paths.
	claude := wantUserAssets("9.9.9", []string{"claude"})
	wantByDest := map[string]wantAsset{}
	for _, a := range claude {
		dest := strings.Replace(a.Dest, ".claude/skills/prowl", ".hermes/skills/prowl", 1)
		wantByDest[dest] = a
	}
	for _, action := range plan.Actions {
		if !strings.HasPrefix(action.Destination, ".hermes/skills/prowl/") {
			t.Errorf("hermes action escaped its root: %q", action.Destination)
		}
		want, ok := wantByDest[action.Destination]
		if !ok {
			t.Errorf("hermes action has no Claude counterpart: %q", action.Destination)
			continue
		}
		if action.Checksum != want.Checksum {
			t.Errorf("hermes %q checksum %q, want Claude's %q", action.Destination, action.Checksum, want.Checksum)
		}
	}
	if len(plan.Actions) != len(wantByDest) {
		t.Errorf("hermes installed %d assets, Claude ships %d", len(plan.Actions), len(wantByDest))
	}
	// Applying with --yes-equivalent approval lands the files under the hermes root.
	mustApply(t, opts)
	probe := filepath.Join(opts.Home, ".hermes", "skills", "prowl", ".claude-plugin", "plugin.json")
	if _, err := os.Stat(probe); err != nil {
		t.Errorf("hermes apply did not install %s: %v", probe, err)
	}
}
