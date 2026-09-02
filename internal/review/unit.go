package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	contextpacket "github.com/prowl-agent/prowl-agent/internal/context"
	"github.com/prowl-agent/prowl-agent/internal/query"
)

const (
	repositoryDataBegin = "<<<BEGIN UNTRUSTED REPOSITORY DATA>>>"
	repositoryDataEnd   = "<<<END UNTRUSTED REPOSITORY DATA>>>"
	maxEvidenceBytes    = int64(1 << 20)
	maxKnowledgeDocs    = 1024
)

// UnitRequest bounds only optional persisted context. Mandatory bytes are never
// repacked and do not consume this budget.
type UnitRequest struct {
	Mode         contextpacket.Mode `json:"mode,omitempty"`
	BudgetTokens int                `json:"budget_tokens,omitempty"`
	BudgetBytes  int                `json:"budget_bytes,omitempty"`
}

// UnitPacket is the complete persisted response used by renderers. Service.Unit
// returns its Mandatory member for the required domain API; callers needing the
// bounded optional context use UnitPacket without triggering any query or index
// refresh.
// UnitFileRoles is one owned changed path and its deterministic plan roles.
type UnitFileRoles struct {
	PathID  string   `json:"path_id"`
	OldPath string   `json:"old_path,omitempty"`
	NewPath string   `json:"new_path,omitempty"`
	Roles   []string `json:"roles"`
}

type UnitPacket struct {
	Mandatory        Unit                     `json:"mandatory"`
	MandatoryJSON    []byte                   `json:"mandatory_json"`
	FileRoles        []UnitFileRoles          `json:"file_roles"`
	AttentionSignals []AttentionSignal        `json:"attention_signals"`
	Context          contextpacket.Packet     `json:"context"`
	Questions        []string                 `json:"questions"`
	Citations        map[string]CitationProof `json:"citations"`
	NextCommands     []NextCommand            `json:"next_commands"`
}

// Unit serves the canonical mandatory object from persisted artifacts only.
func (s *Service) Unit(ctx context.Context, reviewID, unitID string, request UnitRequest) (Unit, error) {
	packet, err := s.UnitPacket(ctx, reviewID, unitID, request)
	return packet.Mandatory, err
}

// UnitPacket loads one immutable artifact transaction, checks workspace
// freshness, validates byte-for-byte mandatory stability, and packs only the
// candidates already persisted with that plan.
func (s *Service) UnitPacket(ctx context.Context, reviewID, unitID string, request UnitRequest) (packet UnitPacket, err error) {
	if s == nil {
		return UnitPacket{}, errors.New("review: nil service")
	}
	planStore, closeStore, err := s.acquireStore(ctx)
	if err != nil {
		return UnitPacket{}, err
	}
	defer func() { err = errors.Join(err, closeStore()) }()
	release, err := planStore.LockReview(ctx, reviewID)
	if err != nil {
		return UnitPacket{}, err
	}
	defer release()
	artifacts, err := planStore.Load(ctx, reviewID)
	if err != nil {
		return UnitPacket{}, err
	}
	if artifacts.Plan.Scope.Kind == ScopeWorkspace {
		capture, captureErr := s.captureOnce(ctx, artifacts.Plan.Scope)
		if captureErr != nil {
			if isCaptureMismatch(captureErr) {
				return UnitPacket{}, &StaleError{Reason: captureErr.Error()}
			}
			return UnitPacket{}, captureErr
		}
		if canonicalCaptureFingerprint(capture) != artifacts.WorkspaceCaptureFingerprint {
			return UnitPacket{}, &StaleError{Reason: "canonical workspace capture changed"}
		}
		if hex.EncodeToString(capture.Scope.Head.Value) != artifacts.WorkspaceFingerprint {
			return UnitPacket{}, &StaleError{Reason: "workspace tree fingerprint changed"}
		}
	}
	mandatory, ok := artifacts.MandatoryUnits[unitID]
	if !ok {
		return UnitPacket{}, fmt.Errorf("review: unit %s is not persisted in review %s", unitID, reviewID)
	}
	if len(mandatory) > MaxUnitMandatoryJSONBytesV1 {
		return UnitPacket{}, fmt.Errorf("%w: %d > %d", ErrMandatoryUnitTooLarge, len(mandatory), MaxUnitMandatoryJSONBytesV1)
	}
	var unit Unit
	if err := json.Unmarshal(mandatory, &unit); err != nil {
		return UnitPacket{}, fmt.Errorf("review: decode mandatory unit %s: %w", unitID, err)
	}
	canonical, err := unit.CanonicalMandatoryJSON()
	if err != nil {
		return UnitPacket{}, err
	}
	if !bytes.Equal(canonical, mandatory) {
		return UnitPacket{}, fmt.Errorf("review: persisted mandatory unit %s is not canonical", unitID)
	}
	if unit.ReviewID != reviewID || unit.UnitID != unitID {
		return UnitPacket{}, fmt.Errorf("review: persisted mandatory unit identity mismatch")
	}
	fileRoles, attentionSignals, err := unitPlanMetadata(artifacts.Plan, artifacts.PlanIdentity, unit)
	if err != nil {
		return UnitPacket{}, err
	}
	mode := request.Mode
	if mode == "" {
		mode = contextpacket.ModeCompact
	}
	candidates, persistedOmissions := optionalContextCandidates(artifacts.UnitCandidates[unitID])
	packed, err := contextpacket.Pack(contextpacket.Request{Mode: mode, BudgetTokens: request.BudgetTokens, BudgetBytes: request.BudgetBytes, IDs: []string{}, Filters: map[string]string{}}, candidates, unitEstimator(s))
	if err != nil {
		return UnitPacket{}, err
	}
	if packed.Omitted == nil {
		packed.Omitted = map[string]int{}
	}
	for reason, count := range persistedOmissions {
		packed.Omitted[reason] += count
	}
	questions := persistedQuestions(artifacts.UnitCandidates[unitID])
	return UnitPacket{
		Mandatory: unit, MandatoryJSON: append([]byte(nil), mandatory...),
		FileRoles: fileRoles, AttentionSignals: attentionSignals, Context: packed,
		Questions: questions, Citations: cloneCitationProofs(artifacts.Citations),
		NextCommands: unitNextCommands(artifacts.Plan, unitID),
	}, nil
}

func unitEstimator(s *Service) contextpacket.CostEstimator {
	if s != nil && s.context != nil && s.context.Estimator != nil {
		return s.context.Estimator
	}
	return contextpacket.ByteQuarterEstimator{}
}

func cloneCitationProofs(in map[string]CitationProof) map[string]CitationProof {
	out := make(map[string]CitationProof, len(in))
	for id, proof := range in {
		out[id] = proof
	}
	return out
}

func persistedQuestions(candidates []contextpacket.Candidate) []string {
	for _, candidate := range candidates {
		if candidate.Kind != "review_guidance" {
			continue
		}
		var questions []string
		if json.Unmarshal([]byte(candidate.FullContent), &questions) == nil {
			return questions
		}
	}
	return []string{}
}

func unitPlanMetadata(plan Plan, identity PlanIdentity, unit Unit) ([]UnitFileRoles, []AttentionSignal, error) {
	owned := make(map[string]bool, len(unit.Hunks))
	for _, hunk := range unit.Hunks {
		owned[hunk.PathID] = true
	}
	fileRoles := make([]UnitFileRoles, 0, len(owned))
	for _, changed := range plan.ChangedPaths {
		if !owned[changed.PathID] {
			continue
		}
		fileRoles = append(fileRoles, UnitFileRoles{
			PathID: changed.PathID, OldPath: changed.OldPath, NewPath: changed.NewPath,
			Roles: append([]string(nil), changed.Roles...),
		})
		delete(owned, changed.PathID)
	}
	if len(owned) != 0 {
		return nil, nil, fmt.Errorf("review: unit %s owns a path absent from the plan", unit.UnitID)
	}
	unitIndex := -1
	for i, primary := range plan.PrimaryUnits {
		if primary.UnitID == unit.UnitID {
			unitIndex = i
			break
		}
	}
	if unitIndex < 0 || unitIndex >= len(identity.Units) {
		return nil, nil, fmt.Errorf("review: unit %s is absent from plan identity", unit.UnitID)
	}
	wanted := make(map[string]bool, len(identity.Units[unitIndex].AttentionSignalIDs))
	for _, id := range identity.Units[unitIndex].AttentionSignalIDs {
		wanted[id] = true
	}
	signals := make([]AttentionSignal, 0, len(wanted))
	for _, signal := range plan.AttentionSignals {
		if wanted[signal.ID] {
			signals = append(signals, signal)
			delete(wanted, signal.ID)
		}
	}
	if len(wanted) != 0 {
		return nil, nil, fmt.Errorf("review: unit %s identity references an absent attention signal", unit.UnitID)
	}
	return fileRoles, signals, nil
}

func optionalContextCandidates(in []contextpacket.Candidate) ([]contextpacket.Candidate, map[string]int) {
	packable := make([]contextpacket.Candidate, 0, len(in))
	omitted := map[string]int{}
	for _, candidate := range in {
		if candidate.Kind == "review_omission" {
			omitted[candidate.Summary]++
			continue
		}
		packable = append(packable, candidate)
	}
	return packable, omitted
}
func questionsForSignals(signals []AttentionSignal) []string {
	present := map[string]bool{}
	for _, signal := range signals {
		present[signal.Kind] = true
	}
	type mapping struct {
		kinds    []string
		question string
	}
	ordered := []mapping{
		{[]string{"changed_signature"}, "For a signature change, which callers still rely on the prior contract?"},
		{[]string{"added_field_option"}, "For a new field or option, where is it populated and where is it consumed?"},
		{[]string{"removed_symbol", "deletion_heavy", "replacement_heavy"}, "For a removed guard/default/export, which invariant did it enforce and where is that invariant now established?"},
		{[]string{"high_fan_in"}, "For a high-fan-in change, do unchanged dependents remain compatible?"},
		{[]string{"no_mapped_test"}, "For a behavior unit without a mapped test, which observable branch or failure path lacks evidence?"},
	}
	out := make([]string, 0, len(ordered))
	for _, item := range ordered {
		for _, kind := range item.kinds {
			if present[kind] {
				out = append(out, item.question)
				break
			}
		}
	}
	return out
}

func unitNextCommands(plan Plan, unitID string) []NextCommand {
	current := -1
	positions := make(map[string]int, len(plan.PrimaryUnits))
	for i, unit := range plan.PrimaryUnits {
		positions[unit.UnitID] = i
		if unit.UnitID == unitID {
			current = i
		}
	}
	if current < 0 {
		return []NextCommand{}
	}
	out := make([]NextCommand, 0, len(plan.PrimaryUnits)-current+1)
	for _, command := range plan.NextCommands {
		if command.Label == "check review" {
			continue
		}
		if strings.HasPrefix(command.Label, "review unit ") {
			target := strings.TrimPrefix(command.Label, "review unit ")
			if position, ok := positions[target]; !ok || position <= current {
				continue
			}
		}
		out = append(out, command)
	}
	out = append(out, NextCommand{
		Label:   "check review",
		Command: "prowl-agent review check --review " + plan.ReviewID + " --report review-results.json",
	})
	return out
}

func repositoryData(label string, content []byte) string {
	return fmt.Sprintf("%s\nlabel: %s\nencoding: base64\nbyte_length: %d\npayload: %s\n%s", repositoryDataBegin, label, len(content), base64.StdEncoding.EncodeToString(content), repositoryDataEnd)
}

func typedContentCitation(kind string, side ReviewSide, sourcePath string, content []byte) contextpacket.Citation {
	uriPath := "/" + strings.TrimPrefix(sourcePath, "/")
	if side != "" {
		uriPath = "/" + string(side) + uriPath
	}
	uri := (&url.URL{Scheme: "review", Host: kind, Path: uriPath}).String()
	sum := sha256.Sum256(content)
	return contextpacket.Citation{
		URI:         uri,
		Path:        sourcePath,
		LineStart:   1,
		LineEnd:     lineCount(content),
		ContentHash: hex.EncodeToString(sum[:]),
	}
}

func evidenceDocumentCitation(document evidenceDocument) contextpacket.Citation {
	kind := "evidence"
	switch document.kind {
	case "untrusted_base_policy", "untrusted_proposed_policy":
		kind = "policy"
	case "trusted_local_knowledge":
		kind = "knowledge"
	}
	return typedContentCitation(kind, document.side, document.path, document.content)
}

func evidenceCandidate(id, kind, title string, content []byte, trusted bool, citation contextpacket.Citation) contextpacket.Candidate {
	text := string(content)
	if !trusted {
		text = repositoryData(strings.ReplaceAll(kind, "_", " "), content)
	}
	return contextpacket.Candidate{
		Item:           contextpacket.Item{ID: id, Kind: kind, Title: title, WhySelected: []string{"persisted review evidence"}, Freshness: "current", Confidence: 1, Audience: []string{"assistant", "user"}, Citations: []contextpacket.Citation{citation}, DetailResource: "review://candidate/" + id},
		CompactContent: text, StandardContent: text, FullContent: text, LexicalScore: 1,
		Knowledge: trusted, ChangedRelated: true,
	}
}

func outsideDiffCandidate(unitID, relation string, side ReviewSide, entry SourceEntry) contextpacket.Candidate {
	idSum := sha256.Sum256(Frame(
		Field{Name: "unit", Value: []byte(unitID)},
		Field{Name: "relation", Value: []byte(relation)},
		Field{Name: "side", Value: []byte(side)},
		Field{Name: "path", Value: []byte(entry.Path)},
		Field{Name: "content", Value: entry.Bytes},
	))
	id := "outside:" + hex.EncodeToString(idSum[:12])
	return evidenceCandidate(
		id,
		"outside_diff_"+relation,
		entry.Path,
		entry.Bytes,
		false,
		typedContentCitation("source", side, entry.Path, entry.Bytes),
	)
}

func citationProofForUnitHunk(id string, hunk UnitHunk, payload []byte) CitationProof {
	side, sourcePath, start, count := SideHead, hunk.NewPath, hunk.NewStart, hunk.NewCount
	if count == 0 {
		side, sourcePath, start, count = SideBase, hunk.OldPath, hunk.OldStart, hunk.OldCount
	}
	if count < 1 {
		count = 1
	}
	sum := sha256.Sum256(payload)
	return CitationProof{ID: id, Side: side, Path: sourcePath, ContentHash: hex.EncodeToString(sum[:]), Start: start, End: start + count - 1}
}

func citationProofForUnit(unit Unit) (CitationProof, error) {
	hunks := append([]UnitHunk(nil), unit.Hunks...)
	sort.Slice(hunks, func(i, j int) bool {
		a, b := hunks[i], hunks[j]
		if a.PathID != b.PathID {
			return a.PathID < b.PathID
		}
		if a.Ordinal != b.Ordinal {
			return a.Ordinal < b.Ordinal
		}
		if a.NewPath != b.NewPath {
			return a.NewPath < b.NewPath
		}
		return a.OldPath < b.OldPath
	})
	if len(hunks) == 0 {
		return CitationProof{}, fmt.Errorf("review: unit %s has no owned hunk citation", unit.UnitID)
	}
	payload, err := base64.StdEncoding.DecodeString(hunks[0].PatchBase64)
	if err != nil {
		return CitationProof{}, fmt.Errorf("review: decode unit %s citation patch: %w", unit.UnitID, err)
	}
	return citationProofForUnitHunk(unit.UnitID, hunks[0], payload), nil
}

// memoGraph ensures planning and candidate construction observe one ordered set
// of graph results without repeating a query.
type memoGraph struct {
	base        GraphQueries
	clusters    []query.Cluster
	clusterErr  error
	clusterSet  bool
	relations   map[string]query.Relations
	relationErr map[string]error
	blasts      map[string]query.BlastSummary
	blastErr    map[string]error
	entries     map[string]query.EntrypointSet
	entryErr    map[string]error
	tests       map[string]query.TestsResult
	testErr     map[string]error
}

func newMemoGraph(base GraphQueries) *memoGraph {
	return &memoGraph{base: base, relations: map[string]query.Relations{}, relationErr: map[string]error{}, blasts: map[string]query.BlastSummary{}, blastErr: map[string]error{}, entries: map[string]query.EntrypointSet{}, entryErr: map[string]error{}, tests: map[string]query.TestsResult{}, testErr: map[string]error{}}
}
func (m *memoGraph) Clusters() ([]query.Cluster, error) {
	if !m.clusterSet {
		m.clusterSet = true
		if m.base == nil {
			m.clusterErr = errors.New("unavailable")
		} else {
			m.clusters, m.clusterErr = m.base.Clusters()
		}
	}
	return append([]query.Cluster(nil), m.clusters...), m.clusterErr
}
func (m *memoGraph) FileRelations(p string) (query.Relations, error) {
	if v, ok := m.relations[p]; ok {
		return v, m.relationErr[p]
	}
	if err, ok := m.relationErr[p]; ok {
		return query.Relations{}, err
	}
	var v query.Relations
	var err error
	if m.base == nil {
		err = errors.New("unavailable")
	} else {
		v, err = m.base.FileRelations(p)
	}
	if err != nil {
		m.relationErr[p] = err
	} else {
		m.relations[p] = v
	}
	return v, err
}
func (m *memoGraph) BlastSummarize(p string) (query.BlastSummary, error) {
	if v, ok := m.blasts[p]; ok {
		return v, m.blastErr[p]
	}
	if err, ok := m.blastErr[p]; ok {
		return query.BlastSummary{}, err
	}
	var v query.BlastSummary
	var err error
	if m.base == nil {
		err = errors.New("unavailable")
	} else {
		v, err = m.base.BlastSummarize(p)
	}
	if err != nil {
		m.blastErr[p] = err
	} else {
		m.blasts[p] = v
	}
	return v, err
}
func (m *memoGraph) EntrypointsFor(p string) (query.EntrypointSet, error) {
	if v, ok := m.entries[p]; ok {
		return v, m.entryErr[p]
	}
	if err, ok := m.entryErr[p]; ok {
		return query.EntrypointSet{}, err
	}
	var v query.EntrypointSet
	var err error
	if m.base == nil {
		err = errors.New("unavailable")
	} else {
		v, err = m.base.EntrypointsFor(p)
	}
	if err != nil {
		m.entryErr[p] = err
	} else {
		m.entries[p] = v
	}
	return v, err
}
func (m *memoGraph) TestsFor(p string) (query.TestsResult, error) {
	if v, ok := m.tests[p]; ok {
		return v, m.testErr[p]
	}
	if err, ok := m.testErr[p]; ok {
		return query.TestsResult{}, err
	}
	var v query.TestsResult
	var err error
	if m.base == nil {
		err = errors.New("unavailable")
	} else {
		v, err = m.base.TestsFor(p)
	}
	if err != nil {
		m.testErr[p] = err
	} else {
		m.tests[p] = v
	}
	return v, err
}

// evidenceDocument is collected before plan identity finalization; candidates are
// rendered only after final review/unit IDs exist.
type evidenceDocument struct {
	kind, title, path string
	side              ReviewSide
	content           []byte
	trusted           bool
}

func (s *Service) buildArtifacts(ctx context.Context, capture Capture, view *HeadView, signature string, forceStructured bool) (PlanArtifacts, error) {
	if view == nil {
		return PlanArtifacts{}, errors.New("review: head view is unavailable")
	}
	graphBase := GraphQueries(s.query)
	if s.graph != nil {
		graphBase = s.graph
	} else if view.Query != nil {
		graphBase = view.Query
	}
	graph := newMemoGraph(graphBase)
	documents, err := s.collectEvidence(ctx, capture, view)
	if err != nil {
		return PlanArtifacts{}, err
	}
	trusted, untrusted := evidenceDigests(documents)
	version := "unknown"
	if view.Store != nil {
		if value, metaErr := view.Store.GetMetaContext(ctx, "index_version"); metaErr == nil && value != "" {
			version = value
		}
	}
	evidence := Evidence{IndexSchema: "index.v1", IndexVersion: version, HeadIndexSignature: []byte(signature), TrustedLocal: trusted, UntrustedPolicy: untrusted}
	planner := &Planner{Graph: graph, ForceStructured: forceStructured}
	plan, err := planner.Build(ctx, capture, view, evidence)
	if err != nil {
		return PlanArtifacts{}, err
	}
	artifacts, replayed, err := replayArtifacts(ctx, planner, capture, view, evidence, plan, graph)
	if err != nil {
		return PlanArtifacts{}, err
	}
	artifacts.PublishedIndexSignature = signature
	if capture.Scope.Kind == ScopeWorkspace {
		artifacts.WorkspaceFingerprint = hex.EncodeToString(capture.Scope.Head.Value)
		artifacts.WorkspaceCaptureFingerprint = canonicalCaptureFingerprint(capture)
	}
	artifacts.UnitCandidates = s.buildUnitCandidates(ctx, plan, capture, view, graph, replayed, documents, artifacts.Citations)
	return artifacts, nil
}

func (s *Service) collectEvidence(ctx context.Context, capture Capture, view *HeadView) ([]evidenceDocument, error) {
	var out []evidenceDocument
	if s.knowledge != nil {
		docs, err := s.knowledge.ListContext(ctx, maxKnowledgeDocs)
		if err != nil {
			return nil, fmt.Errorf("review: read accepted local knowledge: %w", err)
		}
		for _, doc := range docs {
			if !strings.EqualFold(doc.Prowl.Status, "accepted") {
				continue
			}
			content := append([]byte(doc.Title+"\n"+doc.Description+"\n"), doc.Body...)
			out = append(out, evidenceDocument{kind: "trusted_local_knowledge", title: doc.Title, path: doc.Path, content: content, trusted: true})
		}
	}
	policySet := map[string]bool{"AGENTS.md": true, "CONTRIBUTING.md": true, "REVIEW.md": true, ".github/CONTRIBUTING.md": true, ".github/PULL_REQUEST_TEMPLATE.md": true, ".github/copilot-instructions.md": true}
	for _, record := range capture.Paths {
		name := recordPath(record)
		for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
			policySet[path.Join(dir, "AGENTS.md")] = true
		}
		if isPolicyPath(name) {
			policySet[name] = true
		}
	}
	paths := make([]string, 0, len(policySet))
	for p := range policySet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		base, err := view.Sources.Read(ctx, SideBase, p, maxEvidenceBytes)
		if err == nil && base.Present && !base.Binary() {
			out = append(out, evidenceDocument{kind: "untrusted_base_policy", title: "Base policy: " + p, path: p, side: SideBase, content: append([]byte(nil), base.Bytes...)})
		}
		if !changedPath(capture.Paths, p) {
			continue
		}
		head, err := view.Sources.Read(ctx, SideHead, p, maxEvidenceBytes)
		if err == nil && head.Present && !head.Binary() {
			out = append(out, evidenceDocument{kind: "untrusted_proposed_policy", title: "Proposed changed policy: " + p, path: p, side: SideHead, content: append([]byte(nil), head.Bytes...)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].path < out[j].path
	})
	return out, nil
}

func isPolicyPath(p string) bool {
	base := strings.ToLower(path.Base(p))
	return base == "agents.md" || base == "contributing.md" || base == "review.md" || strings.Contains(base, "pull_request_template") || strings.Contains(base, "instructions")
}
func changedPath(records []RawPathRecord, p string) bool {
	for _, r := range records {
		if r.NewPath == p || r.OldPath == p {
			return true
		}
	}
	return false
}
func evidenceDigests(docs []evidenceDocument) (trusted, untrusted []Digest) {
	for _, doc := range docs {
		sum := Digest(sha256.Sum256(doc.content))
		if doc.trusted {
			trusted = append(trusted, sum)
		} else {
			untrusted = append(untrusted, sum)
		}
	}
	return trusted, untrusted
}

// replayPath retains only the state needed to reconstruct PlanIdentityV1 and
// build persisted optional candidates without issuing a second graph query.
type replayPath struct {
	record   RawPathRecord
	id       StableID
	mappings []HunkSymbolMapping
	semantic []SemanticTarget
	signals  []AttentionSignal
	facts    FileFacts
}

type replayState struct {
	paths     map[string]*replayPath
	stable    map[string]StableID
	canonical map[string][]byte
	hunkByKey map[string]StableID
	omissions []GraphOmission
}

func replayArtifacts(ctx context.Context, planner *Planner, capture Capture, view *HeadView, evidence Evidence, plan Plan, graph *memoGraph) (PlanArtifacts, replayState, error) {
	state := replayState{paths: map[string]*replayPath{}, stable: map[string]StableID{}, canonical: map[string][]byte{}, hunkByKey: map[string]StableID{}}
	records := sortedPathRecords(capture.Paths)
	paths := make([]string, 0, len(records))
	for _, r := range records {
		paths = append(paths, recordPath(r))
	}
	enrichment := EnrichGraph(ctx, paths, graph)
	state.omissions = append([]GraphOmission(nil), enrichment.Omissions...)
	planPaths := map[string]PlanPath{}
	for _, p := range plan.ChangedPaths {
		planPaths[p.PathID] = p
	}
	graphSignals := map[string][]AttentionSignal{}
	for _, omission := range enrichment.Omissions {
		signal := newAttentionSignal("graph_omission", omission.Path, omission.Error())
		owners := []string{omission.Path}
		if omission.Path == "" {
			owners = paths
		}
		for _, owner := range owners {
			graphSignals[owner] = append(graphSignals[owner], signal)
		}
	}
	owned := map[string]bool{}
	for _, unit := range plan.PrimaryUnits {
		for _, h := range unit.Hunks {
			if id, ok := rawHunkForUnit(capture, h); ok {
				owned[id.Public] = true
			}
		}
	}
	identity := PlanIdentity{PlannerVersion: planner.version(), ScopeDigest: capture.Scope.Digest, IndexSchema: evidence.IndexSchema, IndexVersion: evidence.IndexVersion, HeadIndexSignature: append([]byte(nil), evidence.HeadIndexSignature...), TrustedEvidence: append([]Digest(nil), evidence.TrustedLocal...), UntrustedEvidence: append([]Digest(nil), evidence.UntrustedPolicy...), Constants: PlanConstants{StructuredThreshold: StructuredThresholdV1, UnitLineCap: uint64(planner.lineCap()), MandatoryJSONCap: uint64(planner.mandatoryCap()), TextClassifierVersion: textClassifierVersionV1, ContextRankerVersion: contextRankerVersionV1}}
	for _, record := range records {
		pid := PathID(capture.Scope.Digest, record)
		addReplayID(&state, PathIDPrefixV1, pid, Frame(Field{Name: "scope", Value: capture.Scope.Digest[:]}, Field{Name: "record", Value: record.Frame()}))
		pp := planPaths[pid.Public]
		rp := &replayPath{record: record, id: pid, facts: enrichment.Facts[recordPath(record)]}
		var base, head []byte
		if record.TextClass == string(TextClassText) && len(record.Hunks) > 0 {
			var err error
			base, head, err = readPlanningSources(ctx, view, record)
			if err != nil {
				return PlanArtifacts{}, state, err
			}
		}
		var symbolOmissions []SymbolMappingOmission
		rp.mappings, symbolOmissions = (SymbolMapper{}).Map(record, base, head)
		rp.semantic = semanticTargets(pid, rp.mappings)
		fake := &plannedPath{record: record, path: recordPath(record), pathID: pid, roles: append([]string(nil), pp.Roles...), class: ReviewClass(pp.ReviewClass), coverage: PathCoverage(pp.Coverage), reason: pp.Reason, mappings: rp.mappings, semantic: rp.semantic}
		rp.signals = canonicalSignals(append(append([]AttentionSignal(nil), rp.facts.Signals...), graphSignals[recordPath(record)]...))
		rp.signals = append(rp.signals, contentSignals(fake)...)
		for _, omission := range symbolOmissions {
			rp.signals = append(rp.signals, newAttentionSignal("symbol_mapping_omission", omission.Path, omission.Class+":"+string(omission.Side)))
		}
		rp.signals = canonicalSignals(rp.signals)
		identity.Paths = append(identity.Paths, PlanPathEntry{PathID: pid, ReviewClass: pp.ReviewClass, Coverage: pp.Coverage, Reason: pp.Reason, RoleIDs: append([]string(nil), pp.Roles...)})
		for _, h := range record.Hunks {
			hid := HunkID(capture.Scope.Digest, pid.Full, h)
			canonical := Frame(Field{Name: "scope", Value: capture.Scope.Digest[:]}, Field{Name: "path", Value: pid.Full[:]}, Field{Name: "ordinal", Value: u64(h.Ordinal)}, Field{Name: "hunk", Value: h.Frame()})
			addReplayID(&state, HunkIDPrefixV1, hid, canonical)
			state.hunkByKey[pid.Public+":"+fmt.Sprint(h.Ordinal)] = hid
			reviewability, reason := string(HunkReviewable), ""
			if !owned[hid.Public] {
				reviewability, reason = string(HunkUnreviewableLargeText), string(HunkUnreviewableLargeText)
				rp.signals = canonicalSignals(append(rp.signals, newAttentionSignal("unreviewable_large_text", recordPath(record), fmt.Sprintf("hunk %d mandatory JSON exceeds %d bytes", h.Ordinal, planner.mandatoryCap()))))
			}
			identity.Hunks = append(identity.Hunks, PlanHunkEntry{HunkID: hid, Reviewability: reviewability, Reason: reason})
		}
		state.paths[recordPath(record)] = rp
		if plan.Mode == ModeStructured {
			for _, target := range rp.semantic {
				state.stable[target.ID.Public] = target.ID
				state.canonical[target.ID.Public] = semanticCanonical(target, pid.Full)
			}
		}
	}
	type unitReplay struct {
		unit    Unit
		stable  StableID
		hunks   []StableID
		paths   []string
		signals []string
	}
	units := make([]unitReplay, 0, len(plan.PrimaryUnits))
	for _, unit := range plan.PrimaryUnits {
		var hunks []StableID
		pathSet := map[string]bool{}
		for _, h := range unit.Hunks {
			hid := state.hunkByKey[h.PathID+":"+fmt.Sprint(h.Ordinal)]
			if hid.Public == "" {
				return PlanArtifacts{}, state, fmt.Errorf("review: unit %s references unknown hunk", unit.UnitID)
			}
			hunks = append(hunks, hid)
			pathSet[h.PathID] = true
		}
		kind := UnitKindNormal
		uid := UnitID(kind, hunks)
		if uid.Public != unit.UnitID {
			kind = UnitKindOversizedAtomic
			uid = UnitID(kind, hunks)
		}
		if uid.Public != unit.UnitID {
			return PlanArtifacts{}, state, fmt.Errorf("review: cannot reconstruct unit identity %s", unit.UnitID)
		}
		addReplayID(&state, UnitIDPrefixV1, uid, Frame(Field{Name: "kind", Value: []byte(kind)}, Field{Name: "hunks", Value: fullDigestList(hunks)}))
		var signalIDs, unitPaths []string
		for public := range pathSet {
			for _, rp := range state.paths {
				if rp.id.Public == public {
					unitPaths = append(unitPaths, recordPath(rp.record))
					for _, signal := range rp.signals {
						signalIDs = append(signalIDs, signal.ID)
					}
				}
			}
		}
		signalIDs = sortedUniqueStrings(signalIDs)
		sort.Strings(unitPaths)
		identity.Units = append(identity.Units, PlanUnitEntry{UnitID: uid, Kind: kind, HunkIDs: hunks, AttentionSignalIDs: signalIDs})
		units = append(units, unitReplay{unit: unit, stable: uid, hunks: hunks, paths: unitPaths, signals: signalIDs})
	}
	cohorts := BuildFileCohorts(paths, enrichment)
	for _, cohort := range cohorts {
		var members []StableID
		for _, u := range units {
			if anyPathIn(u.paths, cohort.Files) {
				members = append(members, u.stable)
			}
		}
		cid := CohortID(capture.Scope.Digest, cohort.Label, members)
		addReplayID(&state, CohortIDPrefixV1, cid, Frame(Field{Name: "scope", Value: capture.Scope.Digest[:]}, Field{Name: "label", Value: []byte(cohort.Label)}, Field{Name: "units", Value: fullDigestList(members)}))
		for _, layer := range cohort.Layers {
			var layerUnits []StableID
			for _, u := range units {
				if anyPathIn(u.paths, layer.Paths) {
					layerUnits = append(layerUnits, u.stable)
				}
			}
			lid := LayerID(cid, uint64(layer.Ordinal), layerUnits)
			addReplayID(&state, LayerIDPrefixV1, lid, Frame(Field{Name: "cohort", Value: cid.Full[:]}, Field{Name: "ordinal", Value: u64(uint64(layer.Ordinal))}, Field{Name: "units", Value: fullDigestList(layerUnits)}))
			identity.Cohorts = append(identity.Cohorts, PlanCohortEntry{CohortID: cid, LayerID: lid, UnitIDs: layerUnits})
		}
	}
	for _, audit := range plan.RequiredAudits {
		var ids []StableID
		for _, public := range audit.TargetIDs {
			id, ok := state.stable[public]
			if !ok {
				return PlanArtifacts{}, state, fmt.Errorf("review: audit references unknown target %s", public)
			}
			ids = append(ids, id)
		}
		identity.Audits = append(identity.Audits, PlanAuditEntry{AuditID: audit.AuditID, TargetIDs: ids})
	}
	identityBytes := ReviewPlanIdentityV1(identity)
	sum := sha256.Sum256(identityBytes)
	if hex.EncodeToString(sum[:]) != plan.PlanDigest || ReviewID(identity).Public != plan.ReviewID {
		return PlanArtifacts{}, state, errors.New("review: replayed planner identity does not match finalized plan")
	}
	artifacts := PlanArtifacts{Plan: plan, PlanIdentityBytes: identityBytes, PlanIdentity: identity, MandatoryUnits: map[string][]byte{}, Citations: map[string]CitationProof{}}
	for _, unit := range plan.PrimaryUnits {
		data, err := unit.CanonicalMandatoryJSON()
		if err != nil {
			return PlanArtifacts{}, state, err
		}
		if len(data) > planner.mandatoryCap() {
			return PlanArtifacts{}, state, fmt.Errorf("%w: %d", ErrMandatoryUnitTooLarge, len(data))
		}
		artifacts.MandatoryUnits[unit.UnitID] = data
	}
	for public, id := range state.stable {
		canonical, ok := state.canonical[public]
		if !ok {
			continue
		}
		artifacts.IDRecords = append(artifacts.IDRecords, IDRecord{Kind: strings.SplitN(public, "_", 2)[0] + "_", Public: public, Full: id.Full, Canonical: canonical})
	}
	sort.Slice(artifacts.IDRecords, func(i, j int) bool { return artifacts.IDRecords[i].Public < artifacts.IDRecords[j].Public })
	proof := defaultCitationProof(capture)
	for _, record := range artifacts.IDRecords {
		p := proof
		p.ID = record.Public
		artifacts.Citations[record.Public] = p
	}
	for _, unit := range plan.PrimaryUnits {
		proof, err := citationProofForUnit(unit)
		if err != nil {
			return PlanArtifacts{}, state, err
		}
		artifacts.Citations[unit.UnitID] = proof
	}
	return artifacts, state, nil
}

func addReplayID(state *replayState, kind string, id StableID, canonical []byte) {
	state.stable[id.Public] = id
	state.canonical[id.Public] = canonical
}
func rawHunkForUnit(capture Capture, unit UnitHunk) (StableID, bool) {
	for _, r := range capture.Paths {
		pid := PathID(capture.Scope.Digest, r)
		if pid.Public != unit.PathID {
			continue
		}
		for _, h := range r.Hunks {
			if int(h.Ordinal) == unit.Ordinal {
				return HunkID(capture.Scope.Digest, pid.Full, h), true
			}
		}
	}
	return StableID{}, false
}
func semanticCanonical(target SemanticTarget, pathID Digest) []byte {
	content := target.Symbol.Signature
	if content == "" {
		content = fmt.Sprintf("%s\x00%s\x00%s", target.Symbol.Kind, target.Symbol.Parent, target.Symbol.Name)
	}
	digest := Digest(sha256.Sum256([]byte(content)))
	return Frame(Field{Name: "kind", Value: []byte(target.Kind)}, Field{Name: "path", Value: pathID[:]}, Field{Name: "side", Value: []byte(target.Side)}, Field{Name: "symbol_kind", Value: []byte(target.Symbol.Kind)}, Field{Name: "symbol_name", Value: []byte(target.Symbol.Name)}, Field{Name: "start", Value: u64(uint64(target.Symbol.StartLine))}, Field{Name: "end", Value: u64(uint64(target.Symbol.EndLine))}, Field{Name: "signature", Value: digest[:]})
}
func anyPathIn(paths, members []string) bool {
	set := map[string]bool{}
	for _, p := range members {
		set[p] = true
	}
	for _, p := range paths {
		if set[p] {
			return true
		}
	}
	return false
}
func defaultCitationProof(capture Capture) CitationProof {
	for _, r := range sortedPathRecords(capture.Paths) {
		for _, h := range r.Hunks {
			sum := sha256.Sum256(h.Payload)
			side, p, start, count := SideHead, r.NewPath, int(h.NewStart), int(h.NewLines)
			if count == 0 {
				side, p, start, count = SideBase, r.OldPath, int(h.OldStart), int(h.OldLines)
			}
			if count < 1 {
				count = 1
			}
			return CitationProof{Side: side, Path: p, ContentHash: hex.EncodeToString(sum[:]), Start: start, End: start + count - 1}
		}
	}
	return CitationProof{Side: SideHead, ContentHash: hex.EncodeToString(make([]byte, 32)), Start: 1, End: 1}
}

func (s *Service) buildUnitCandidates(ctx context.Context, plan Plan, capture Capture, view *HeadView, graph *memoGraph, replay replayState, documents []evidenceDocument, proofs map[string]CitationProof) map[string][]contextpacket.Candidate {
	out := make(map[string][]contextpacket.Candidate, len(plan.PrimaryUnits))
	changed := map[string]bool{}
	for _, p := range plan.ChangedPaths {
		if p.NewPath != "" {
			changed[p.NewPath] = true
		}
		if p.OldPath != "" {
			changed[p.OldPath] = true
		}
	}
	for _, unit := range plan.PrimaryUnits {
		proof := proofs[unit.UnitID]
		citation := contextpacket.Citation{URI: "review://citation/" + unit.UnitID, Path: proof.Path, LineStart: proof.Start, LineEnd: proof.End, ContentHash: proof.ContentHash}
		pathSet := map[string]bool{}
		for _, h := range unit.Hunks {
			for _, rp := range replay.paths {
				if rp.id.Public == h.PathID {
					pathSet[recordPath(rp.record)] = true
				}
			}
		}
		var signals []AttentionSignal
		var outside []string
		for p := range pathSet {
			rp := replay.paths[p]
			signals = append(signals, rp.signals...)
			addJSONCandidate := func(kind string, v any) {
				data, _ := json.Marshal(v)
				out[unit.UnitID] = append(out[unit.UnitID], evidenceCandidate(unit.UnitID+":"+kind+":"+p, kind, p, data, false, citation))
			}
			addJSONCandidate("symbols_signatures", rp.mappings)
			addJSONCandidate("relations", rp.facts.Relations)
			addJSONCandidate("callers_references", rp.facts.Relations.IncludedBy)
			addJSONCandidate("blast", rp.facts.Blast)
			addJSONCandidate("entrypoints", rp.facts.Entrypoints)
			addJSONCandidate("tests", rp.facts.Tests)
			outside = append(outside, rp.facts.Blast.DirectFiles...)
			for _, edge := range append(append([]query.EdgeView(nil), rp.facts.Relations.Includes...), rp.facts.Relations.IncludedBy...) {
				outside = append(outside, edge.File)
			}
		}
		for _, omission := range replay.omissions {
			if omission.Path != "" && !pathSet[omission.Path] {
				continue
			}
			reason := "query:" + omission.Query + ":" + string(omission.ErrorClass)
			out[unit.UnitID] = append(out[unit.UnitID], contextpacket.Candidate{Item: contextpacket.Item{ID: unit.UnitID + ":omission:" + omission.Query + ":" + omission.Path, Kind: "review_omission", Title: "Optional context omission", Summary: reason, WhySelected: []string{"persisted omission accounting"}, Freshness: "current", Confidence: 1, Citations: []contextpacket.Citation{citation}, DetailResource: "review://unit/" + unit.UnitID + "/omissions"}})
		}
		questions := questionsForSignals(canonicalSignals(signals))
		qbytes, _ := json.Marshal(questions)
		out[unit.UnitID] = append(out[unit.UnitID], contextpacket.Candidate{Item: contextpacket.Item{ID: unit.UnitID + ":guidance", Kind: "review_guidance", Title: "Deterministic review questions", WhySelected: []string{"attention signal mapping"}, Freshness: "current", Confidence: 1, Citations: []contextpacket.Citation{citation}, DetailResource: "review://unit/" + unit.UnitID + "/guidance"}, CompactContent: string(qbytes), StandardContent: string(qbytes), FullContent: string(qbytes), DirectMatch: true})
		for _, doc := range documents {
			documentCitation := evidenceDocumentCitation(doc)
			out[unit.UnitID] = append(out[unit.UnitID], evidenceCandidate(unit.UnitID+":"+doc.kind+":"+doc.path, doc.kind, doc.title, doc.content, doc.trusted, documentCitation))
		}
		outside = sortedUniqueStrings(outside)
		for _, p := range outside {
			if p == "" || changed[p] || view.Sources == nil {
				continue
			}
			entry, err := view.Sources.Read(ctx, SideHead, p, maxEvidenceBytes)
			if err != nil || !entry.Present || entry.Binary() {
				continue
			}
			if entry.Path == "" {
				entry.Path = p
			}
			out[unit.UnitID] = append(out[unit.UnitID], outsideDiffCandidate(unit.UnitID, "callers/references", SideHead, entry))
		}
		sort.SliceStable(out[unit.UnitID], func(i, j int) bool { return out[unit.UnitID][i].ID < out[unit.UnitID][j].ID })
	}
	return out
}
func lineCount(content []byte) int {
	if len(content) == 0 {
		return 1
	}
	n := bytes.Count(content, []byte{'\n'})
	if content[len(content)-1] != '\n' {
		n++
	}
	if n < 1 {
		return 1
	}
	return n
}
