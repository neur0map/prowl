package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/review"
)

func TestReviewCLIE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping built-binary review e2e in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	temp := t.TempDir()
	binary := filepath.Join(temp, "prowl-agent")
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", binary, "./cmd/prowl-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build prowl-agent: %v\n%s", err, output)
	}

	repository := filepath.Join(temp, "repository")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-c", "user.email=review@example.com", "-c", "user.name=Review E2E"}, args...)...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	runGit("init", "-q")
	writeReviewE2EBase(t, repository)

	t.Setenv("HOME", filepath.Join(temp, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(temp, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(temp, "config"))
	if stdout, stderr, err := runReviewBinary(binary, repository, nil, "init", "--no-input", "--integrations", "none", "--ai-provider", "agent", "--ai-command", "false", "--json"); err != nil {
		t.Fatalf("initialize prowl: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	runGit("add", "-A")
	runGit("commit", "-qm", "base")

	writeReviewE2EChange(t, repository, 297)
	planJSON, stderr, err := runReviewBinary(binary, repository, nil, "review", "plan", "--format", "json")
	if err != nil {
		t.Fatalf("review plan: %v\nstdout:\n%s\nstderr:\n%s", err, planJSON, stderr)
	}
	var plan review.Plan
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		t.Fatalf("decode plan: %v\n%s", err, planJSON)
	}
	if plan.Stats.RawChurn != 301 || plan.Mode != review.ModeStructured || !plan.StructuredRequired {
		t.Fatalf("301-line plan churn/mode/required = %d/%s/%v", plan.Stats.RawChurn, plan.Mode, plan.StructuredRequired)
	}
	assertReviewE2EPaths(t, plan)
	if len(plan.PrimaryUnits) == 0 {
		t.Fatal("structured plan has no primary units")
	}

	packets := make(map[string]review.UnitPacket, len(plan.PrimaryUnits))
	for _, plannedUnit := range plan.PrimaryUnits {
		unitJSON, unitStderr, err := runReviewBinary(binary, repository, nil, "review", "unit", plan.ReviewID+"/"+plannedUnit.UnitID, "--format", "json")
		if err != nil {
			t.Fatalf("review unit %s: %v\nstdout:\n%s\nstderr:\n%s", plannedUnit.UnitID, err, unitJSON, unitStderr)
		}
		var packet review.UnitPacket
		if err := json.Unmarshal(unitJSON, &packet); err != nil {
			t.Fatalf("decode unit %s: %v\n%s", plannedUnit.UnitID, err, unitJSON)
		}
		if packet.Mandatory.ReviewID != plan.ReviewID || packet.Mandatory.UnitID != plannedUnit.UnitID {
			t.Fatalf("unit identity = %s/%s", packet.Mandatory.ReviewID, packet.Mandatory.UnitID)
		}
		for _, hunk := range packet.Mandatory.Hunks {
			if _, err := base64.StdEncoding.DecodeString(hunk.PatchBase64); err != nil {
				t.Fatalf("unit %s patch is not base64: %v", plannedUnit.UnitID, err)
			}
		}
		packets[plannedUnit.UnitID] = packet
	}

	incomplete := review.Report{
		Schema: review.ReportSchemaV1, ReviewID: plan.ReviewID, PlanDigest: plan.PlanDigest,
		Base: plan.Scope.Base, Head: plan.Scope.Head, Recommendation: string(review.RecommendIncomplete),
		Findings: []review.Finding{},
	}
	incompleteJSON, err := json.Marshal(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	incompletePath := filepath.Join(temp, "incomplete-report.json")
	if err := os.WriteFile(incompletePath, incompleteJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	checkJSON, checkStderr, checkErr := runReviewBinary(binary, repository, nil, "review", "check", "--review", plan.ReviewID, "--report", incompletePath, "--format", "json")
	if checkErr == nil {
		t.Fatal("incomplete structured report exited successfully")
	}
	var incompleteResult review.CheckResult
	if err := json.Unmarshal(checkJSON, &incompleteResult); err != nil {
		t.Fatalf("decode incomplete check result: %v\nstdout:\n%s\nstderr:\n%s", err, checkJSON, checkStderr)
	}
	if incompleteResult.Status != review.CheckIncomplete || len(incompleteResult.Problems) == 0 {
		t.Fatalf("incomplete check result = %+v", incompleteResult)
	}
	if err := os.Remove(incompletePath); err != nil {
		t.Fatal(err)
	}

	hunkIDs := captureReviewE2EHunkIDs(t, repository)
	complete := completeReviewE2EReport(t, plan, packets, hunkIDs)
	completeJSON, err := json.Marshal(complete)
	if err != nil {
		t.Fatal(err)
	}
	acceptedJSON, acceptedStderr, acceptedErr := runReviewBinary(binary, repository, completeJSON, "review", "check", "--review", plan.ReviewID, "--report", "-", "--format", "json")
	if acceptedErr != nil {
		t.Fatalf("complete report rejected: %v\nstdout:\n%s\nstderr:\n%s", acceptedErr, acceptedJSON, acceptedStderr)
	}
	var accepted review.CheckResult
	if err := json.Unmarshal(acceptedJSON, &accepted); err != nil {
		t.Fatalf("decode accepted result: %v\n%s", err, acceptedJSON)
	}
	if !accepted.Complete() || accepted.Coverage != review.CoverageComplete {
		t.Fatalf("accepted check result = %+v", accepted)
	}

	file, err := os.OpenFile(filepath.Join(repository, "source.go"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("// stale mutation\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	staleJSON, staleStderr, staleErr := runReviewBinary(binary, repository, completeJSON, "review", "check", "--review", plan.ReviewID, "--report", "-", "--format", "json")
	if staleErr == nil {
		t.Fatal("stale workspace report exited successfully")
	}
	var stale review.CheckResult
	if err := json.Unmarshal(staleJSON, &stale); err != nil {
		t.Fatalf("decode stale result: %v\nstdout:\n%s\nstderr:\n%s", err, staleJSON, staleStderr)
	}
	if stale.Status != review.CheckStale {
		t.Fatalf("stale check result = %+v", stale)
	}

	// Reducing the source-only additions by one produces the exact direct-mode boundary.
	writeReviewE2EChange(t, repository, 296)
	boundaryJSON, boundaryStderr, boundaryErr := runReviewBinary(binary, repository, nil, "review", "plan", "--format", "json")
	if boundaryErr != nil {
		t.Fatalf("300-line boundary plan: %v\nstdout:\n%s\nstderr:\n%s", boundaryErr, boundaryJSON, boundaryStderr)
	}
	var boundary review.Plan
	if err := json.Unmarshal(boundaryJSON, &boundary); err != nil {
		t.Fatalf("decode boundary plan: %v\n%s", err, boundaryJSON)
	}
	if boundary.Stats.RawChurn != 300 || boundary.Mode != review.ModeDirect || boundary.StructuredRequired {
		t.Fatalf("300-line plan churn/mode/required = %d/%s/%v", boundary.Stats.RawChurn, boundary.Mode, boundary.StructuredRequired)
	}
}

func runReviewBinary(binary, directory string, stdin []byte, args ...string) ([]byte, []byte, error) {
	command := exec.Command(binary, args...)
	command.Dir = directory
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func writeReviewE2EBase(t *testing.T, root string) {
	t.Helper()
	files := map[string][]byte{
		"source.go":      []byte("package fixture\n\nfunc Value() int { return 1 }\n"),
		"source_test.go": []byte("package fixture\n\nfunc ExampleValue() {}\n"),
		"deleted.txt":    []byte("remove me\n"),
		"binary.bin":     {0, 1, 2, 0, 3},
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeReviewE2EChange(t *testing.T, root string, sourceAdditions int) {
	t.Helper()
	var source strings.Builder
	source.WriteString("package fixture\n\nfunc Value() int { return 1 }\n")
	for index := 0; index < sourceAdditions; index++ {
		source.WriteString("// source review line\n")
	}
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte(source.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source_test.go"), []byte("package fixture\n\nfunc ExampleValue() {}\n// test review one\n// test review two\n// test review three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0, 9, 8, 0, 7}, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertReviewE2EPaths(t *testing.T, plan review.Plan) {
	t.Helper()
	got := make(map[string]review.PlanPath, len(plan.ChangedPaths))
	for _, path := range plan.ChangedPaths {
		name := path.NewPath
		if name == "" {
			name = path.OldPath
		}
		got[name] = path
	}
	for _, name := range []string{"source.go", "source_test.go", "deleted.txt", "binary.bin"} {
		if _, ok := got[name]; !ok {
			t.Errorf("plan omits changed path %s: %+v", name, plan.ChangedPaths)
		}
	}
	if got["deleted.txt"].Status != "D" {
		t.Errorf("deleted path status = %q", got["deleted.txt"].Status)
	}
	if got["binary.bin"].ReviewClass != string(review.ReviewClassUnreviewable) {
		t.Errorf("binary path class = %q", got["binary.bin"].ReviewClass)
	}
}

func completeReviewE2EReport(t *testing.T, plan review.Plan, packets map[string]review.UnitPacket, hunkIDs map[string]string) review.Report {
	t.Helper()
	report := review.Report{
		Schema: review.ReportSchemaV1, ReviewID: plan.ReviewID, PlanDigest: plan.PlanDigest,
		Base: plan.Scope.Base, Head: plan.Scope.Head, Recommendation: string(review.RecommendApprove),
		Findings:        []review.Finding{},
		PrimaryReceipts: make([]review.PrimaryReceipt, 0, len(plan.PrimaryUnits)),
		AuditReceipts:   make([]review.AuditReceipt, 0, len(plan.RequiredAudits)),
	}
	for _, unit := range plan.PrimaryUnits {
		packet := packets[unit.UnitID]
		owned := make([]string, 0, len(packet.Mandatory.Hunks))
		for _, hunk := range packet.Mandatory.Hunks {
			id := hunkIDs[hunk.PathID+":"+fmt.Sprint(hunk.Ordinal)]
			if id == "" {
				t.Fatalf("no captured hunk ID for unit %s path %s ordinal %d", unit.UnitID, hunk.PathID, hunk.Ordinal)
			}
			owned = append(owned, id)
		}
		sort.Strings(owned)
		report.PrimaryReceipts = append(report.PrimaryReceipts, review.PrimaryReceipt{
			UnitID: unit.UnitID, AcknowledgedPrimaryHunkIDs: owned, Reviewer: "review-e2e",
		})
	}
	for _, audit := range plan.RequiredAudits {
		targets := append([]string(nil), audit.TargetIDs...)
		report.AuditReceipts = append(report.AuditReceipts, review.AuditReceipt{
			AuditID: audit.AuditID, AcknowledgedAuditTargetIDs: targets, Reviewer: "review-e2e",
		})
	}
	return report
}

func captureReviewE2EHunkIDs(t *testing.T, root string) map[string]string {
	t.Helper()
	scope, err := review.ResolveScope(context.Background(), review.ExecGit{}, root, review.PlanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	capture, err := (&review.Capturer{Root: root, Runner: review.ExecGit{}}).CaptureOnce(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]string)
	for _, record := range capture.Paths {
		pathID := review.PathID(capture.Scope.Digest, record)
		for _, hunk := range record.Hunks {
			hunkID := review.HunkID(capture.Scope.Digest, pathID.Full, hunk)
			ids[pathID.Public+":"+fmt.Sprint(hunk.Ordinal)] = hunkID.Public
		}
	}
	return ids
}
