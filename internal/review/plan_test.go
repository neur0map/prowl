package review

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/query"
)

type mapSourceResolver map[string][]byte

func (m mapSourceResolver) Read(_ context.Context, side ReviewSide, path string, _ int64) (SourceEntry, error) {
	b := append([]byte(nil), m[string(side)+":"+path]...)
	return SourceEntry{Path: path, Present: true, Kind: "regular", Bytes: b, TextClass: TextClassText}, nil
}

func testScope() Scope {
	return Scope{Kind: ScopeRange, ObjectFormat: "sha1", Base: SideIdentity{Kind: SideGitOID, Value: bytes.Repeat([]byte{1}, 20)}, Head: SideIdentity{Kind: SideGitOID, Value: bytes.Repeat([]byte{2}, 20)}, Digest: Digest{9}}
}

func additionHunk(ordinal uint64, start, lines int) RawHunk {
	var payload strings.Builder
	for i := 0; i < lines; i++ {
		payload.WriteString("+x\n")
	}
	return RawHunk{Ordinal: ordinal, OldStart: uint64(start), NewStart: uint64(start), NewLines: uint64(lines), Payload: []byte(payload.String())}
}

func basicGraph(paths ...string) fakeGraphQueries {
	relations := make(map[string]query.Relations, len(paths))
	for _, path := range paths {
		relations[path] = query.Relations{File: path, Exists: true}
	}
	return fakeGraphQueries{relations: relations}
}

func captureFor(records ...RawPathRecord) Capture {
	var adds, dels int
	for _, record := range records {
		adds += int(record.Additions)
		dels += int(record.Deletions)
	}
	return Capture{Scope: testScope(), Paths: records, RawAdditions: adds, RawDeletions: dels, RawChurn: adds + dels, ChangedPaths: len(records), CanonicalPatch: Digest{7}}
}

func TestUnitChangedLineBoundaryAndOversizedAtomic(t *testing.T) {
	for _, tc := range []struct {
		lines    int
		wantKind string
	}{{400, UnitKindNormal}, {401, UnitKindOversizedAtomic}} {
		record := RawPathRecord{Kind: "tracked", Status: "A", NewPath: "x.go", TextClass: string(TextClassText), Additions: uint64(tc.lines), Hunks: []RawHunk{additionHunk(0, 1, tc.lines)}}
		packed, large, err := PackReviewPartition(testScope(), "p_"+strings.Repeat("0", 32), "c_"+strings.Repeat("0", 32), "l_"+strings.Repeat("0", 32), []PackHunk{{Path: record, Hunk: record.Hunks[0], HunkID: StableID{Public: "h_" + strings.Repeat("0", 32)}}}, MaxUnitChangedLinesV1, MaxUnitMandatoryJSONBytesV1)
		if err != nil || len(large) != 0 || len(packed) != 1 || packed[0].Kind != tc.wantKind {
			t.Fatalf("lines=%d packed=%#v large=%v err=%v", tc.lines, packed, large, err)
		}
	}
}

func TestUnitMandatoryJSONExactCapMinusAtPlusOne(t *testing.T) {
	unit := Unit{Schema: UnitSchemaV1, ReviewID: "rvw_" + strings.Repeat("0", 40), UnitID: "u_" + strings.Repeat("0", 32), CohortID: "c_" + strings.Repeat("0", 32), LayerID: "l_" + strings.Repeat("0", 32), ScopeKind: ScopeRange, ObjectFormat: "sha1", Base: testScope().Base, Head: testScope().Head, Hunks: []UnitHunk{{PathID: "p", NewPath: "x", Status: "M", PatchBase64: ""}}}
	base, err := unit.CanonicalMandatoryJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-1, 0, 1} {
		u := unit
		u.Hunks = append([]UnitHunk(nil), unit.Hunks...)
		u.Hunks[0].PatchBase64 = strings.Repeat("A", MaxUnitMandatoryJSONBytesV1-len(base)+delta)
		fits, size, err := MandatoryUnitFits(u, MaxUnitMandatoryJSONBytesV1)
		if err != nil {
			t.Fatal(err)
		}
		if size != MaxUnitMandatoryJSONBytesV1+delta || fits != (delta <= 0) {
			t.Fatalf("delta=%d size=%d fits=%v", delta, size, fits)
		}
	}
}

func TestPlanDirectAndStructuredUseSamePartitionsAndIDs(t *testing.T) {
	records := []RawPathRecord{
		{Kind: "tracked", Status: "M", OldPath: "a.go", NewPath: "a.go", TextClass: string(TextClassText), Additions: 1, Deletions: 1, Hunks: []RawHunk{{Ordinal: 0, OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Payload: []byte("-func a() {}\n+func a() { println(1) }\n")}}},
		{Kind: "tracked", Status: "M", OldPath: "b.go", NewPath: "b.go", TextClass: string(TextClassText), Additions: 1, Deletions: 1, Hunks: []RawHunk{{Ordinal: 0, OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Payload: []byte("-func b() {}\n+func b() { println(2) }\n")}}},
	}
	sources := mapSourceResolver{"base:a.go": []byte("package p\n\nfunc a() {}\n"), "head:a.go": []byte("package p\n\nfunc a() { println(1) }\n"), "base:b.go": []byte("package p\n\nfunc b() {}\n"), "head:b.go": []byte("package p\n\nfunc b() { println(2) }\n")}
	view := &HeadView{Sources: sources}
	graph := basicGraph("a.go", "b.go")
	direct, err := (&Planner{Graph: graph}).Build(context.Background(), captureFor(records...), view, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	structured, err := (&Planner{Graph: graph, ForceStructured: true}).Build(context.Background(), captureFor(records...), view, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	if direct.Mode != ModeDirect || len(direct.Cohorts) != 0 || len(direct.RequiredAudits) != 0 {
		t.Fatalf("direct collections = %#v", direct)
	}
	if structured.Mode != ModeStructured || len(structured.Cohorts) != 2 || len(structured.RequiredAudits) != 4 {
		t.Fatalf("structured collections = %#v", structured)
	}
	if len(direct.PrimaryUnits) != 2 || len(structured.PrimaryUnits) != 2 {
		t.Fatalf("units direct=%d structured=%d", len(direct.PrimaryUnits), len(structured.PrimaryUnits))
	}
	for i := range direct.PrimaryUnits {
		d, s := direct.PrimaryUnits[i], structured.PrimaryUnits[i]
		if d.UnitID != s.UnitID || d.CohortID != s.CohortID || d.LayerID != s.LayerID {
			t.Fatalf("partition identity differs: %#v %#v", d, s)
		}
	}
}

func TestAuditTargetsExactAndPrimaryOwnershipDeterministic(t *testing.T) {
	impl := RawPathRecord{Kind: "tracked", Status: "M", OldPath: "impl.go", NewPath: "impl.go", TextClass: string(TextClassText), Additions: 1, Deletions: 1, Hunks: []RawHunk{{Ordinal: 0, OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Payload: []byte("-func f(a int) {}\n+func f(a string) {}\n")}}}
	testFile := RawPathRecord{Kind: "tracked", Status: "A", NewPath: "impl_test.go", TextClass: string(TextClassText), Additions: 1, Hunks: []RawHunk{additionHunk(0, 1, 1)}}
	binary := RawPathRecord{Kind: "tracked", Status: "M", OldPath: "asset.bin", NewPath: "asset.bin", TextClass: string(TextClassBinary)}
	capture := captureFor(impl, testFile, binary)
	capture.RawChurn = StructuredThresholdV1 + 1
	capture.RawAdditions = capture.RawChurn
	graph := basicGraph("impl.go", "impl_test.go", "asset.bin")
	graph.tests = map[string]query.TestsResult{"impl.go": {Tests: []string{"impl_test.go"}}}
	view := &HeadView{Sources: mapSourceResolver{"base:impl.go": []byte("package p\n\nfunc f(a int) {}\n"), "head:impl.go": []byte("package p\n\nfunc f(a string) {}\n"), "head:impl_test.go": []byte("x\n")}}
	planner := &Planner{Graph: graph}
	first, err := planner.Build(context.Background(), capture, view, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := planner.Build(context.Background(), capture, view, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeat build was nondeterministic")
	}
	owners := map[string]int{}
	for _, unit := range first.PrimaryUnits {
		for _, h := range unit.Hunks {
			owners[h.PathID+":"+string(rune(h.Ordinal))]++
		}
	}
	if len(owners) != 2 {
		t.Fatalf("owners = %#v", owners)
	}
	for key, n := range owners {
		if n != 1 {
			t.Fatalf("owner %s count %d", key, n)
		}
	}
	audits := map[string][]string{}
	for _, audit := range first.RequiredAudits {
		audits[audit.AuditID] = audit.TargetIDs
	}
	if len(audits[AuditRemovedBehaviorV1]) == 0 || len(audits[AuditContractMigrationV1]) == 0 || len(audits[AuditTestMatrixV1]) != 1 || len(audits[AuditIntegrationGapV1]) == 0 {
		t.Fatalf("audit targets = %#v", audits)
	}
}

func TestUnitLargeTextLeavesSmallHunkReviewable(t *testing.T) {
	largeBytes := bytes.Repeat([]byte("x"), (MaxUnitMandatoryJSONBytesV1*3/4)+1024)
	largePayload := []byte("+")
	largePayload = append(largePayload, largeBytes...)
	largePayload = append(largePayload, '\n')
	record := RawPathRecord{Kind: "tracked", Status: "A", NewPath: "large.txt", TextClass: string(TextClassText), Additions: 2, Hunks: []RawHunk{
		{Ordinal: 0, NewStart: 1, NewLines: 1, Payload: largePayload},
		{Ordinal: 1, NewStart: 3, NewLines: 1, Payload: []byte("+small\n")},
	}}
	packed, large, err := PackReviewPartition(testScope(), "p_"+strings.Repeat("0", 32), "c_"+strings.Repeat("0", 32), "l_"+strings.Repeat("0", 32), []PackHunk{{Path: record, Hunk: record.Hunks[0]}, {Path: record, Hunk: record.Hunks[1]}}, MaxUnitChangedLinesV1, MaxUnitMandatoryJSONBytesV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(large) != 1 || large[0].Ordinal != 0 || len(packed) != 1 || packed[0].Hunks[0].Ordinal != 1 {
		t.Fatalf("packed=%#v large=%#v", packed, large)
	}
	if _, err := base64.StdEncoding.DecodeString(packed[0].Unit.Hunks[0].PatchBase64); err != nil {
		t.Fatalf("non-padded base64: %v", err)
	}
}

func TestPlanGraphFailureKeepsExactPrimaryOwnershipAndStableOmissions(t *testing.T) {
	record := RawPathRecord{Kind: "tracked", Status: "M", OldPath: "x.go", NewPath: "x.go", TextClass: string(TextClassText), Additions: 1, Deletions: 1, Hunks: []RawHunk{{
		Ordinal: 0, OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Payload: []byte("-func x() {}\n+func x() { println(1) }\n"),
	}}}
	boom := errors.New("unstable provider message")
	graph := fakeGraphQueries{fail: map[string]error{
		"Clusters:":           boom,
		"FileRelations:x.go":  boom,
		"BlastSummarize:x.go": boom,
		"EntrypointsFor:x.go": boom,
		"TestsFor:x.go":       boom,
	}}
	view := &HeadView{Sources: mapSourceResolver{
		"base:x.go": []byte("package p\n\nfunc x() {}\n"),
		"head:x.go": []byte("package p\n\nfunc x() { println(1) }\n"),
	}}
	plan, err := (&Planner{Graph: graph, ForceStructured: true}).Build(context.Background(), captureFor(record), view, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PrimaryUnits) != 1 || len(plan.PrimaryUnits[0].Hunks) != 1 {
		t.Fatalf("primary ownership lost: %#v", plan.PrimaryUnits)
	}
	omissions := 0
	for _, signal := range plan.AttentionSignals {
		if signal.Kind == "graph_omission" {
			omissions++
			if strings.Contains(signal.Fact, "unstable provider message") {
				t.Fatalf("provider detail leaked into stable omission: %#v", signal)
			}
		}
	}
	if omissions != 5 {
		t.Fatalf("graph omissions = %d, signals = %#v", omissions, plan.AttentionSignals)
	}
}

func TestHunkDuplicateCoordinatesAcrossFilesHaveDistinctOwners(t *testing.T) {
	payload := []byte("-func same() {}\n+func same() { println(1) }\n")
	records := []RawPathRecord{
		{Kind: "tracked", Status: "M", OldPath: "a.go", NewPath: "a.go", TextClass: string(TextClassText), Additions: 1, Deletions: 1, Hunks: []RawHunk{{Ordinal: 0, OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Payload: payload}}},
		{Kind: "tracked", Status: "M", OldPath: "b.go", NewPath: "b.go", TextClass: string(TextClassText), Additions: 1, Deletions: 1, Hunks: []RawHunk{{Ordinal: 0, OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Payload: payload}}},
	}
	view := &HeadView{Sources: mapSourceResolver{
		"base:a.go": []byte("package p\n\nfunc same() {}\n"), "head:a.go": []byte("package p\n\nfunc same() { println(1) }\n"),
		"base:b.go": []byte("package p\n\nfunc same() {}\n"), "head:b.go": []byte("package p\n\nfunc same() { println(1) }\n"),
	}}
	plan, err := (&Planner{Graph: basicGraph("a.go", "b.go"), ForceStructured: true}).Build(context.Background(), captureFor(records...), view, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PrimaryUnits) != 2 {
		t.Fatalf("units = %#v", plan.PrimaryUnits)
	}
	if plan.PrimaryUnits[0].UnitID == plan.PrimaryUnits[1].UnitID || plan.PrimaryUnits[0].Hunks[0].PathID == plan.PrimaryUnits[1].Hunks[0].PathID {
		t.Fatalf("duplicate coordinates collided: %#v", plan.PrimaryUnits)
	}
}

func TestUnitPlaceholderIDSubstitutionPreservesMandatoryLength(t *testing.T) {
	record := RawPathRecord{Kind: "tracked", Status: "A", NewPath: "x.go", TextClass: string(TextClassText), Additions: 1, Hunks: []RawHunk{additionHunk(0, 1, 1)}}
	packed, _, err := PackReviewPartition(testScope(), "p_"+strings.Repeat("0", 32), "", "", []PackHunk{{Path: record, Hunk: record.Hunks[0]}}, MaxUnitChangedLinesV1, MaxUnitMandatoryJSONBytesV1)
	if err != nil {
		t.Fatal(err)
	}
	before, err := packed[0].Unit.CanonicalMandatoryJSON()
	if err != nil {
		t.Fatal(err)
	}
	unit := packed[0].Unit
	unit.ReviewID = "rvw_" + strings.Repeat("a", 40)
	unit.UnitID = "u_" + strings.Repeat("b", 32)
	unit.CohortID = "c_" + strings.Repeat("c", 32)
	unit.LayerID = "l_" + strings.Repeat("d", 32)
	after, err := unit.CanonicalMandatoryJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("placeholder substitution changed length: %d -> %d", len(before), len(after))
	}
}

func TestUnitPartialPathRetainsSmallReviewableHunk(t *testing.T) {
	largePayload := []byte("+" + strings.Repeat("x", 2000) + "\n")
	record := RawPathRecord{Kind: "tracked", Status: "A", NewPath: "large.txt", TextClass: string(TextClassText), Additions: 2, Hunks: []RawHunk{
		{Ordinal: 0, NewStart: 1, NewLines: 1, Payload: largePayload},
		{Ordinal: 1, NewStart: 3, NewLines: 1, Payload: []byte("+small\n")},
	}}
	view := &HeadView{Sources: mapSourceResolver{"head:large.txt": []byte("large\nsmall\n")}}
	plan, err := (&Planner{Graph: basicGraph("large.txt"), ForceStructured: true, MaxMandatoryBytes: 1200}).Build(context.Background(), captureFor(record), view, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ChangedPaths) != 1 || plan.ChangedPaths[0].Coverage != string(PathCoveragePartial) {
		t.Fatalf("coverage = %#v", plan.ChangedPaths)
	}
	if len(plan.PrimaryUnits) != 1 || len(plan.PrimaryUnits[0].Hunks) != 1 || plan.PrimaryUnits[0].Hunks[0].Ordinal != 1 {
		t.Fatalf("small hunk ownership = %#v", plan.PrimaryUnits)
	}
	var integration []string
	for _, audit := range plan.RequiredAudits {
		if audit.AuditID == AuditIntegrationGapV1 {
			integration = audit.TargetIDs
		}
	}
	if len(integration) < 2 {
		t.Fatalf("integration targets omit cohort or large hunk: %v", integration)
	}
}

func TestUnitCoalescedSymbolGroupSplitsAtWholeHunkLineBoundaries(t *testing.T) {
	record := RawPathRecord{Kind: "tracked", Status: "A", NewPath: "x.go", TextClass: string(TextClassText), Additions: 500, Hunks: []RawHunk{
		additionHunk(0, 1, 250),
		additionHunk(1, 300, 250),
	}}
	group := "same-symbols"
	packed, large, err := PackReviewPartition(testScope(), "p_"+strings.Repeat("0", 32), "", "", []PackHunk{
		{Path: record, Hunk: record.Hunks[0], GroupKey: group},
		{Path: record, Hunk: record.Hunks[1], GroupKey: group},
	}, MaxUnitChangedLinesV1, MaxUnitMandatoryJSONBytesV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(large) != 0 || len(packed) != 2 {
		t.Fatalf("coalesced line split packed=%#v large=%#v", packed, large)
	}
	for _, unit := range packed {
		if unit.Kind != UnitKindNormal || len(unit.Hunks) != 1 {
			t.Fatalf("whole-hunk line split produced %#v", unit)
		}
	}
}

func TestUnitCoalescedSymbolGroupSplitsAtWholeHunkJSONBoundaries(t *testing.T) {
	payload := []byte("+" + strings.Repeat("x", 600) + "\n")
	record := RawPathRecord{Kind: "tracked", Status: "A", NewPath: "x.go", TextClass: string(TextClassText), Additions: 2, Hunks: []RawHunk{
		{Ordinal: 0, NewStart: 1, NewLines: 1, Payload: payload},
		{Ordinal: 1, NewStart: 3, NewLines: 1, Payload: payload},
	}}
	pathID := "p_" + strings.Repeat("0", 32)
	one, _, err := PackReviewPartition(testScope(), pathID, "", "", []PackHunk{{Path: record, Hunk: record.Hunks[0]}}, MaxUnitChangedLinesV1, MaxUnitMandatoryJSONBytesV1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := one[0].Unit.CanonicalMandatoryJSON()
	if err != nil {
		t.Fatal(err)
	}
	group := "same-symbols"
	packed, large, err := PackReviewPartition(testScope(), pathID, "", "", []PackHunk{
		{Path: record, Hunk: record.Hunks[0], GroupKey: group},
		{Path: record, Hunk: record.Hunks[1], GroupKey: group},
	}, MaxUnitChangedLinesV1, len(encoded)+8)
	if err != nil {
		t.Fatal(err)
	}
	if len(large) != 0 || len(packed) != 2 {
		t.Fatalf("coalesced JSON split packed=%#v large=%#v", packed, large)
	}
}

func TestAuditRoleExclusionsPrecedeEligibleRolesPerPath(t *testing.T) {
	excludedTest := StableID{Public: "p_test", Full: Digest{1}}
	excludedMechanical := StableID{Public: "p_mechanical", Full: Digest{2}}
	eligible := StableID{Public: "p_impl", Full: Digest{3}}
	excludedUnit := StableID{Public: "u_excluded", Full: Digest{4}}
	mechanicalUnit := StableID{Public: "u_mechanical", Full: Digest{5}}
	mixedUnit := StableID{Public: "u_mixed", Full: Digest{6}}
	cohortID := StableID{Public: "c_test", Full: Digest{7}}
	planned := map[string]*plannedPath{
		"test.go": {pathID: excludedTest, roles: []string{RoleTest, RoleImplementation}, hunkIDs: map[uint64]StableID{}, reviewability: map[uint64]HunkReviewability{}},
		"lock":    {pathID: excludedMechanical, roles: []string{RoleMechanical, RoleOperations}, hunkIDs: map[uint64]StableID{}, reviewability: map[uint64]HunkReviewability{}},
		"impl.go": {pathID: eligible, roles: []string{RoleImplementation}, hunkIDs: map[uint64]StableID{}, reviewability: map[uint64]HunkReviewability{}},
	}
	units := []PackedUnit{
		{UnitStable: excludedUnit, PathIDs: []string{excludedTest.Public}},
		{UnitStable: mechanicalUnit, PathIDs: []string{excludedMechanical.Public}},
		{UnitStable: mixedUnit, PathIDs: []string{excludedTest.Public, eligible.Public}},
	}
	audits := buildAudits(ModeStructured, []FileCohort{{Key: "test"}}, []StableID{cohortID}, planned, units)
	var targets []string
	for _, audit := range audits {
		if audit.AuditID == AuditTestMatrixV1 {
			targets = audit.TargetIDs
		}
	}
	if !reflect.DeepEqual(targets, []string{mixedUnit.Public}) {
		t.Fatalf("test-matrix targets = %v", targets)
	}
}

func TestPlanCheckedIdentityCollisionPropagatesTypedError(t *testing.T) {
	records := []RawPathRecord{
		{Kind: "tracked", Status: "A", NewPath: "a.go", TextClass: string(TextClassText), Additions: 1, Hunks: []RawHunk{additionHunk(0, 1, 1)}},
		{Kind: "tracked", Status: "A", NewPath: "b.go", TextClass: string(TextClassText), Additions: 1, Hunks: []RawHunk{additionHunk(0, 1, 1)}},
	}
	view := &HeadView{Sources: mapSourceResolver{"head:a.go": []byte("package p\n"), "head:b.go": []byte("package p\n")}}
	planner := &Planner{
		Graph: basicGraph("a.go", "b.go"),
		IDCollisionProbe: func(id StableID) StableID {
			if strings.HasPrefix(id.Public, PathIDPrefixV1) {
				id.Public = "p_" + strings.Repeat("f", 32)
			}
			return id
		},
	}
	_, err := planner.Build(context.Background(), captureFor(records...), view, Evidence{})
	var collision *IDCollisionError
	if !errors.Is(err, ErrIDCollision) || !errors.As(err, &collision) {
		t.Fatalf("error = %T %v, want typed ErrIDCollision", err, err)
	}
	if collision.PublicID != "p_"+strings.Repeat("f", 32) {
		t.Fatalf("collision = %#v", collision)
	}
}

func TestPlanCheckedIdentitySetAllowsExactDuplicateAndRejectsDigestMismatch(t *testing.T) {
	ids := newCheckedIDSet(nil)
	first := StableID{Public: "u_same", Full: Digest{1}}
	if err := ids.Insert(first); err != nil {
		t.Fatal(err)
	}
	if err := ids.Insert(first); err != nil {
		t.Fatalf("exact duplicate rejected: %v", err)
	}
	err := ids.Insert(StableID{Public: first.Public, Full: Digest{2}})
	var collision *IDCollisionError
	if !errors.Is(err, ErrIDCollision) || !errors.As(err, &collision) {
		t.Fatalf("error = %T %v, want typed ErrIDCollision", err, err)
	}
}
