package revieweval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

const (
	CandidateRejectionsSchema        = "review.eval-candidate-rejections.v3"
	CandidateRejectionEvidenceSchema = "review.eval-candidate-rejection-evidence.v1"
	AuditPacketsSchema               = "review.eval-audit-packets.v3"
)

type CandidateRejectionEvidenceSpec struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	SHA256       string `json:"sha256"`
	SourcePath   string `json:"source_path"`
	SourceSHA256 string `json:"source_sha256"`
}

type CandidateRejectionEvidenceRecord struct {
	CaseID             string  `json:"case_id"`
	ReviewerIdentity   string  `json:"reviewer_identity"`
	Decision           string  `json:"decision"`
	Evidence           string  `json:"evidence"`
	VerifiedBaseSHA    *string `json:"verified_base_sha"`
	VerifiedHeadSHA    *string `json:"verified_head_sha"`
	RawAdditions       *int    `json:"raw_additions"`
	RawDeletions       *int    `json:"raw_deletions"`
	CausalPath         *string `json:"causal_path"`
	CausalStart        *int    `json:"causal_start"`
	CausalEnd          *int    `json:"causal_end"`
	DefectCategory     string  `json:"defect_category"`
	IntroducedByChange bool    `json:"introduced_by_change"`
	SourceRecordDigest string  `json:"source_record_digest"`
	Blocker            string  `json:"blocker"`
	SourceRepository   string  `json:"source_repository"`
	SourceBaseSHA      string  `json:"source_base_sha"`
	SourceHeadSHA      string  `json:"source_head_sha"`
	SourceDigestKind   string  `json:"source_digest_kind"`
}

type CandidateRejectionSourceRecord struct {
	CaseID             string  `json:"case_id"`
	ReviewerIdentity   string  `json:"reviewer_identity"`
	Decision           string  `json:"decision"`
	Evidence           string  `json:"evidence"`
	VerifiedBaseSHA    *string `json:"verified_base_sha"`
	VerifiedHeadSHA    *string `json:"verified_head_sha"`
	RawAdditions       *int    `json:"raw_additions"`
	RawDeletions       *int    `json:"raw_deletions"`
	CausalPath         *string `json:"causal_path"`
	CausalStart        *int    `json:"causal_start"`
	CausalEnd          *int    `json:"causal_end"`
	DefectCategory     string  `json:"defect_category"`
	IntroducedByChange bool    `json:"introduced_by_change"`
	SourceRecordDigest string  `json:"source_record_digest"`
	Blocker            string  `json:"blocker"`
}

type CandidateRejectionEvidence struct {
	Schema              string                             `json:"schema"`
	SourcePayloadSHA256 string                             `json:"source_payload_sha256"`
	ReviewerIdentity    string                             `json:"reviewer_identity"`
	Records             []CandidateRejectionEvidenceRecord `json:"records"`
}

type CandidateRejection struct {
	CaseID                string `json:"case_id"`
	Repository            string `json:"repository"`
	SourceBaseSHA         string `json:"source_base_sha"`
	SourceHeadSHA         string `json:"source_head_sha"`
	LegacySourceRowSHA256 string `json:"legacy_overwritten_source_row_sha256,omitempty"`
	RawSourceRecordSHA256 string `json:"raw_source_record_sha256,omitempty"`
	EvidenceID            string `json:"evidence_id"`
	EvidenceRecordSHA256  string `json:"evidence_record_sha256"`
	ReviewerID            string `json:"reviewer_id"`
	Decision              string `json:"decision"`
	Blocker               string `json:"blocker"`
	AuditedRangeBaseSHA   string `json:"audited_range_base_sha,omitempty"`
	AuditedRangeHeadSHA   string `json:"audited_range_head_sha,omitempty"`
	RawAdditions          *int   `json:"raw_additions"`
	RawDeletions          *int   `json:"raw_deletions"`
	IntroducedByChange    bool   `json:"introduced_by_change"`
}

type CandidateRejectionsManifest struct {
	Schema                string                           `json:"schema"`
	SourcesManifestSHA256 string                           `json:"sources_manifest_sha256"`
	Evidence              []CandidateRejectionEvidenceSpec `json:"evidence"`
	Rejections            []CandidateRejection             `json:"rejections"`
	evidenceRecords       map[string]CandidateRejectionEvidenceRecord
}

type MechanicalBlocker struct {
	Code                 string `json:"code"`
	Stage                string `json:"stage"`
	Detail               string `json:"detail,omitempty"`
	Actual               *int   `json:"actual,omitempty"`
	Deepen               int    `json:"deepen,omitempty"`
	EvidenceID           string `json:"evidence_id,omitempty"`
	EvidenceRecordSHA256 string `json:"evidence_record_sha256,omitempty"`
}
type candidateMechanicalError struct {
	blocker MechanicalBlocker
}

func (err candidateMechanicalError) Error() string {
	return err.blocker.Code
}

func rejectionPacketBlocker(rejection CandidateRejection) MechanicalBlocker {
	return MechanicalBlocker{
		Code: "rejected_by_audit", Stage: "independent_audit", Detail: rejection.Blocker,
		EvidenceID: rejection.EvidenceID, EvidenceRecordSHA256: rejection.EvidenceRecordSHA256,
	}
}

func auditQueueMechanicalBlockers(queue AuditQueueItem) []MechanicalBlocker {
	blockers := make([]MechanicalBlocker, 0, len(queue.Reasons))
	for _, reason := range queue.Reasons {
		blockers = append(blockers, MechanicalBlocker{Code: "semantic_audit_required", Stage: "semantic_audit", Detail: reason})
	}
	return blockers
}

type ClaimAudit struct {
	CandidateClaim
	PathExists       bool                `json:"path_exists"`
	InChangedRange   bool                `json:"in_changed_range"`
	SemanticDecision string              `json:"semantic_decision"`
	Blockers         []MechanicalBlocker `json:"blockers,omitempty"`
}

type CandidateAuditPacket struct {
	CaseID          string              `json:"case_id"`
	Disposition     string              `json:"disposition"`
	SourceRow       CandidateSourceRow  `json:"source_row"`
	SourceRowSHA256 string              `json:"source_row_sha256"`
	NativeBaseSHA   string              `json:"native_base_sha,omitempty"`
	QueueItem       *AuditQueueItem     `json:"queue_item,omitempty"`
	Claims          []ClaimAudit        `json:"claims,omitempty"`
	Blockers        []MechanicalBlocker `json:"blockers"`
}

type AuditPacketsManifest struct {
	Schema                    string                 `json:"schema"`
	SourcesManifestSHA256     string                 `json:"sources_manifest_sha256"`
	CandidateRejectionsSHA256 string                 `json:"candidate_rejections_sha256"`
	CandidatePoolSHA256       string                 `json:"candidate_pool_sha256"`
	PartitionSeedSHA256       string                 `json:"partition_seed_sha256"`
	TuningDeficit             int                    `json:"tuning_deficit"`
	HeldOutDeficit            int                    `json:"held_out_deficit"`
	CoverageDeficits          []string               `json:"coverage_deficits"`
	Packets                   []CandidateAuditPacket `json:"packets"`
	CanonicalSHA256           string                 `json:"canonical_sha256"`
}

type CandidateDiscoveryConfig struct {
	SourcesPath         string
	RejectionsPath      string
	CachePath           string
	RepositoryCachePath string
	CandidatePoolPath   string
	TuningPath          string
	HeldOutPath         string
	AuditPacketsPath    string
	PartitionSeedSHA256 string
	Offline             bool
}

type CandidateDiscoveryReport struct {
	SourceIdentityCount int
	RejectedCount       int
	QuarantinedCount    int
	CandidateCount      int
	TuningCount         int
	HeldOutCount        int
	TuningDeficit       int
	HeldOutDeficit      int
	CountsBySource      map[string]int
	CountsByType        map[string]int
	CountsByLanguage    map[string]int
	CoverageDeficits    []string
	CountsBySizeBin     map[string]int
}

func LoadCandidateRejections(path string) (CandidateRejectionsManifest, error) {
	var manifest CandidateRejectionsManifest
	if err := decodeStrictFile(path, &manifest); err != nil {
		return CandidateRejectionsManifest{}, err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return CandidateRejectionsManifest{}, err
	}
	canonical, err := canonicalIndentedJSON(manifest)
	if err != nil || !bytes.Equal(payload, canonical) {
		return CandidateRejectionsManifest{}, errors.New("candidate rejections are not canonical JSON")
	}
	records, err := loadCandidateRejectionEvidenceFiles(filepath.Dir(path), manifest.Evidence)
	if err != nil {
		return CandidateRejectionsManifest{}, err
	}
	manifest.evidenceRecords = records
	if err := ValidateCandidateRejections(manifest); err != nil {
		return CandidateRejectionsManifest{}, err
	}
	return manifest, nil
}

func loadCandidateRejectionEvidenceFiles(baseDir string, specs []CandidateRejectionEvidenceSpec) (map[string]CandidateRejectionEvidenceRecord, error) {
	records := map[string]CandidateRejectionEvidenceRecord{}
	usedPaths := map[string]bool{}
	for index, spec := range specs {
		if spec.ID == "" || !safeRelativeArtifactPath(spec.Path) || !safeRelativeArtifactPath(spec.SourcePath) ||
			!sha256Hex.MatchString(spec.SHA256) || !sha256Hex.MatchString(spec.SourceSHA256) ||
			index > 0 && specs[index-1].ID >= spec.ID || usedPaths[spec.Path] || usedPaths[spec.SourcePath] ||
			spec.Path == spec.SourcePath {
			return nil, errors.New("candidate rejection evidence specs are incomplete or noncanonical")
		}
		usedPaths[spec.Path], usedPaths[spec.SourcePath] = true, true
		path := filepath.Join(baseDir, spec.Path)
		payload, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if digestBytes(payload) != spec.SHA256 {
			return nil, fmt.Errorf("candidate rejection evidence %s hash mismatch", spec.ID)
		}
		var evidence CandidateRejectionEvidence
		if err := decodeStrictFile(path, &evidence); err != nil {
			return nil, err
		}
		canonical, err := canonicalIndentedJSON(evidence)
		if err != nil || !bytes.Equal(payload, canonical) {
			return nil, fmt.Errorf("candidate rejection evidence %s is not canonical JSON", spec.ID)
		}
		if err := validateCandidateRejectionEvidence(evidence); err != nil {
			return nil, fmt.Errorf("candidate rejection evidence %s: %w", spec.ID, err)
		}
		if err := validateCandidateRejectionSource(filepath.Join(baseDir, spec.SourcePath), spec, evidence); err != nil {
			return nil, fmt.Errorf("candidate rejection source %s: %w", spec.ID, err)
		}
		for _, record := range evidence.Records {
			digest := canonicalCandidateRejectionEvidenceRecordDigest(record)
			key := spec.ID + "\x00" + digest
			if digest == "" || records[key].CaseID != "" {
				return nil, fmt.Errorf("candidate rejection evidence %s has duplicate records", spec.ID)
			}
			records[key] = record
		}
	}
	return records, nil
}

func safeRelativeArtifactPath(path string) bool {
	return path != "" && !filepath.IsAbs(path) && filepath.Clean(path) == path && path != "." &&
		!strings.HasPrefix(path, ".."+string(filepath.Separator))
}

func validateCandidateRejectionSource(path string, spec CandidateRejectionEvidenceSpec, evidence CandidateRejectionEvidence) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if digestBytes(payload) != spec.SourceSHA256 || evidence.SourcePayloadSHA256 != spec.SourceSHA256 {
		return errors.New("original source payload hash mismatch")
	}
	var sourceRecords []CandidateRejectionSourceRecord
	if err := decodeStrictFile(path, &sourceRecords); err != nil {
		return err
	}
	if len(sourceRecords) != len(evidence.Records) {
		return errors.New("normalized evidence does not cover every original source record")
	}
	byCase := make(map[string]CandidateRejectionSourceRecord, len(sourceRecords))
	for _, record := range sourceRecords {
		if record.CaseID == "" || byCase[record.CaseID].CaseID != "" {
			return errors.New("original source payload has missing or duplicate case ids")
		}
		byCase[record.CaseID] = record
	}
	for _, record := range evidence.Records {
		source, found := byCase[record.CaseID]
		if !found || !reflect.DeepEqual(source, sourceFieldsFromEvidence(record)) {
			return fmt.Errorf("normalized evidence record %s does not exactly derive from original source", record.CaseID)
		}
		delete(byCase, record.CaseID)
	}
	if len(byCase) != 0 {
		return errors.New("original source payload contains unconsumed records")
	}
	return nil
}

func sourceFieldsFromEvidence(record CandidateRejectionEvidenceRecord) CandidateRejectionSourceRecord {
	return CandidateRejectionSourceRecord{
		CaseID: record.CaseID, ReviewerIdentity: record.ReviewerIdentity, Decision: record.Decision,
		Evidence: record.Evidence, VerifiedBaseSHA: record.VerifiedBaseSHA, VerifiedHeadSHA: record.VerifiedHeadSHA,
		RawAdditions: record.RawAdditions, RawDeletions: record.RawDeletions,
		CausalPath: record.CausalPath, CausalStart: record.CausalStart, CausalEnd: record.CausalEnd,
		DefectCategory: record.DefectCategory, IntroducedByChange: record.IntroducedByChange,
		SourceRecordDigest: record.SourceRecordDigest, Blocker: record.Blocker,
	}
}

func validateCandidateRejectionEvidence(evidence CandidateRejectionEvidence) error {
	if evidence.Schema != CandidateRejectionEvidenceSchema || !sha256Hex.MatchString(evidence.SourcePayloadSHA256) ||
		strings.TrimSpace(evidence.ReviewerIdentity) == "" || len(evidence.Records) == 0 {
		return errors.New("invalid evidence envelope")
	}
	for index, record := range evidence.Records {
		causalComplete := record.CausalPath != nil && record.CausalStart != nil && record.CausalEnd != nil
		causalEmpty := record.CausalPath == nil && record.CausalStart == nil && record.CausalEnd == nil
		if record.CaseID == "" || record.ReviewerIdentity != evidence.ReviewerIdentity || record.Decision != "rejected" ||
			strings.TrimSpace(record.Evidence) == "" || strings.TrimSpace(record.Blocker) == "" ||
			strings.TrimSpace(record.DefectCategory) == "" || !fullSHA.MatchString(record.SourceBaseSHA) ||
			!fullSHA.MatchString(record.SourceHeadSHA) || !sha256Hex.MatchString(record.SourceRecordDigest) ||
			(record.SourceDigestKind != "legacy_overwritten_source_row" && record.SourceDigestKind != "aacr_raw_row") ||
			(record.VerifiedBaseSHA != nil && !fullSHA.MatchString(*record.VerifiedBaseSHA)) ||
			(record.VerifiedHeadSHA != nil && !fullSHA.MatchString(*record.VerifiedHeadSHA)) ||
			(record.RawAdditions == nil) != (record.RawDeletions == nil) ||
			(record.RawAdditions != nil && (*record.RawAdditions < 0 || *record.RawDeletions < 0)) ||
			(!causalComplete && !causalEmpty) ||
			(causalComplete && (strings.TrimSpace(*record.CausalPath) == "" || *record.CausalStart <= 0 || *record.CausalEnd < *record.CausalStart)) ||
			record.IntroducedByChange || index > 0 && evidence.Records[index-1].CaseID >= record.CaseID {
			return fmt.Errorf("record %s is incomplete or noncanonical", record.CaseID)
		}
	}
	return nil
}

func ValidateCandidateRejections(manifest CandidateRejectionsManifest) error {
	if manifest.Schema != CandidateRejectionsSchema || !sha256Hex.MatchString(manifest.SourcesManifestSHA256) ||
		len(manifest.Evidence) == 0 || len(manifest.evidenceRecords) == 0 {
		return errors.New("candidate rejections lack loaded canonical evidence")
	}
	seen, usedEvidence := map[string]bool{}, map[string]bool{}
	for index, rejection := range manifest.Rejections {
		identity := immutableIdentity(strings.ToLower(rejection.Repository), rejection.SourceBaseSHA, rejection.SourceHeadSHA)
		hasLegacyDigest := sha256Hex.MatchString(rejection.LegacySourceRowSHA256)
		hasRawDigest := sha256Hex.MatchString(rejection.RawSourceRecordSHA256)
		recordKey := rejection.EvidenceID + "\x00" + rejection.EvidenceRecordSHA256
		record, found := manifest.evidenceRecords[recordKey]
		if rejection.CaseID == "" || index > 0 && manifest.Rejections[index-1].CaseID >= rejection.CaseID ||
			seen[identity] || usedEvidence[recordKey] ||
			!fullSHA.MatchString(rejection.SourceBaseSHA) || !fullSHA.MatchString(rejection.SourceHeadSHA) ||
			hasLegacyDigest == hasRawDigest || !sha256Hex.MatchString(rejection.EvidenceRecordSHA256) || !found ||
			!candidateRejectionMatchesEvidence(rejection, record) {
			return fmt.Errorf("candidate rejection %s is incomplete or not exactly bound to evidence", rejection.CaseID)
		}
		seen[identity], usedEvidence[recordKey] = true, true
	}
	if len(usedEvidence) != len(manifest.evidenceRecords) {
		return errors.New("candidate rejection evidence contains unused records")
	}
	return nil
}

func candidateRejectionMatchesEvidence(rejection CandidateRejection, record CandidateRejectionEvidenceRecord) bool {
	sourceDigestMatches := record.SourceDigestKind == "legacy_overwritten_source_row" &&
		rejection.LegacySourceRowSHA256 == record.SourceRecordDigest && rejection.RawSourceRecordSHA256 == "" ||
		record.SourceDigestKind == "aacr_raw_row" &&
			rejection.RawSourceRecordSHA256 == record.SourceRecordDigest && rejection.LegacySourceRowSHA256 == ""
	return sourceDigestMatches && rejection.CaseID == record.CaseID &&
		strings.EqualFold(rejection.Repository, record.SourceRepository) &&
		rejection.SourceBaseSHA == record.SourceBaseSHA && rejection.SourceHeadSHA == record.SourceHeadSHA &&
		rejection.ReviewerID == record.ReviewerIdentity && rejection.Decision == record.Decision &&
		rejection.Blocker == record.Blocker && rejection.IntroducedByChange == record.IntroducedByChange &&
		rejection.AuditedRangeBaseSHA == stringPointerValue(record.VerifiedBaseSHA) &&
		rejection.AuditedRangeHeadSHA == stringPointerValue(record.VerifiedHeadSHA) &&
		equalOptionalInt(rejection.RawAdditions, record.RawAdditions) &&
		equalOptionalInt(rejection.RawDeletions, record.RawDeletions)
}

func canonicalCandidateRejectionEvidenceRecordDigest(record CandidateRejectionEvidenceRecord) string {
	payload, err := json.Marshal(record)
	if err != nil {
		return ""
	}
	return digestBytes(payload)
}

func canonicalIndentedJSON(value any) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func equalOptionalInt(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func ValidateCandidateRejectionsAgainstAACR(rejections CandidateRejectionsManifest, rows []CandidateSourceRow) error {
	byIdentity := make(map[string]CandidateSourceRow, len(rows))
	for _, row := range rows {
		byIdentity[immutableIdentity(strings.ToLower(row.Repository), row.BaseSHA, row.HeadSHA)] = row
	}
	for _, rejection := range rejections.Rejections {
		identity := immutableIdentity(strings.ToLower(rejection.Repository), rejection.SourceBaseSHA, rejection.SourceHeadSHA)
		row, ok := byIdentity[identity]
		if !ok || aacrCaseID(row) != rejection.CaseID {
			return fmt.Errorf("candidate rejection %s does not bind an imported AACR identity", rejection.CaseID)
		}
		if rejection.RawSourceRecordSHA256 != "" && !stringSet(row.RawRecordSHA256s)[rejection.RawSourceRecordSHA256] {
			return fmt.Errorf("candidate rejection %s does not bind an accepted AACR raw row", rejection.CaseID)
		}
	}
	return nil
}

func ValidateCandidatePoolRejections(pool CandidatePoolManifest, rejections CandidateRejectionsManifest) error {
	blocked := make(map[string]CandidateRejection, len(rejections.Rejections))
	for _, rejection := range rejections.Rejections {
		blocked[immutableIdentity(strings.ToLower(rejection.Repository), rejection.SourceBaseSHA, rejection.SourceHeadSHA)] = rejection
	}
	for _, record := range pool.Records {
		sourceBase := record.SourceRow.BaseSHA
		if rejection, ok := blocked[immutableIdentity(strings.ToLower(record.Repository), sourceBase, record.SourceRow.HeadSHA)]; ok {
			return fmt.Errorf("rejected candidate %s re-entered pool as %s", rejection.CaseID, record.ID)
		}
	}
	return nil
}

func CanonicalCandidateRejectionsDigest(manifest CandidateRejectionsManifest) string {
	payload, err := json.Marshal(manifest)
	if err != nil {
		return ""
	}
	return digestBytes(payload)
}

func LoadAuditPackets(path string) (AuditPacketsManifest, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return AuditPacketsManifest{}, err
	}
	var manifest AuditPacketsManifest
	if err := decodeStrictFile(path, &manifest); err != nil {
		return AuditPacketsManifest{}, err
	}
	canonical, err := canonicalIndentedJSON(manifest)
	if err != nil || !bytes.Equal(payload, canonical) {
		return AuditPacketsManifest{}, errors.New("audit packets are not canonical JSON")
	}
	if err := validateAuditPacketsEnvelope(manifest); err != nil {
		return AuditPacketsManifest{}, err
	}
	return manifest, nil
}

func CanonicalAuditPacketsDigest(manifest AuditPacketsManifest) (string, error) {
	manifest.CanonicalSHA256 = ""
	for index, packet := range manifest.Packets {
		if packet.CaseID == "" || index > 0 && manifest.Packets[index-1].CaseID >= packet.CaseID {
			return "", errors.New("audit packets are not ordered by unique case id")
		}
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return digestBytes(payload), nil
}

func validateAuditPacketsEnvelope(manifest AuditPacketsManifest) error {
	if manifest.Schema != AuditPacketsSchema || !sha256Hex.MatchString(manifest.SourcesManifestSHA256) ||
		!sha256Hex.MatchString(manifest.CandidateRejectionsSHA256) ||
		!sha256Hex.MatchString(manifest.CandidatePoolSHA256) ||
		!sha256Hex.MatchString(manifest.PartitionSeedSHA256) ||
		manifest.TuningDeficit < 0 || manifest.HeldOutDeficit < 0 {
		return errors.New("audit packet envelope has invalid schema or immutable bindings")
	}
	digest, err := CanonicalAuditPacketsDigest(manifest)
	if err != nil {
		return err
	}
	if digest != manifest.CanonicalSHA256 {
		return errors.New("audit packet canonical digest mismatch")
	}
	return nil
}

func ValidateAuditPackets(manifest AuditPacketsManifest, rows []CandidateSourceRow, rejections CandidateRejectionsManifest, pool CandidatePoolManifest, tuning, heldOut FrozenCorpus) error {
	if err := ValidateCandidatePool(pool); err != nil {
		return err
	}
	if err := ValidateCandidateRejections(rejections); err != nil {
		return err
	}
	if err := validateAuditPacketsEnvelope(manifest); err != nil {
		return err
	}
	if manifest.SourcesManifestSHA256 != pool.SourcesManifestSHA256 ||
		manifest.CandidateRejectionsSHA256 != CanonicalCandidateRejectionsDigest(rejections) ||
		manifest.CandidatePoolSHA256 != pool.SHA256 ||
		manifest.PartitionSeedSHA256 != pool.PartitionSeedSHA256 {
		return errors.New("audit packet envelope does not bind the current sources, evidence, pool, and partition seed")
	}
	tuningCount := min(12, len(pool.Records))
	heldOutCount := min(30, len(pool.Records)-tuningCount)
	expectedTuning, expectedHeldOut, err := PartitionCandidatePool(pool, tuningCount, heldOutCount)
	if err != nil {
		return err
	}
	if tuning.Set != "tuning" || heldOut.Set != "held_out" ||
		!reflect.DeepEqual(tuning.AuditQueue, auditQueueFromRecords(expectedTuning)) ||
		!reflect.DeepEqual(heldOut.AuditQueue, auditQueueFromRecords(expectedHeldOut)) {
		return errors.New("audit packets do not match deterministic tuning and held-out partitions")
	}
	if manifest.TuningDeficit != 12-tuningCount || manifest.HeldOutDeficit != 30-heldOutCount ||
		!reflect.DeepEqual(manifest.CoverageDeficits, candidateCoverageDeficits(pool.Records)) {
		return errors.New("audit packet deficits do not match the current candidate pool")
	}
	rowsByCase := make(map[string]CandidateSourceRow, len(rows))
	for _, row := range rows {
		caseID := aacrCaseID(row)
		if rowsByCase[caseID].SourceID != "" {
			return fmt.Errorf("duplicate imported AACR case %s", caseID)
		}
		rowsByCase[caseID] = row
	}
	rejectedByCase := make(map[string]CandidateRejection, len(rejections.Rejections))
	for _, rejection := range rejections.Rejections {
		rejectedByCase[rejection.CaseID] = rejection
	}
	poolByCase := make(map[string]CandidatePoolRecord, len(pool.Records))
	for _, record := range pool.Records {
		poolByCase[record.ID] = record
	}
	if len(manifest.Packets) != len(rowsByCase) {
		return errors.New("audit packets do not cover every imported AACR source identity")
	}
	for _, packet := range manifest.Packets {
		row, found := rowsByCase[packet.CaseID]
		if !found || !reflect.DeepEqual(packet.SourceRow, row) {
			return fmt.Errorf("audit packet %s does not contain its exact freshly imported source row", packet.CaseID)
		}
		rowDigest, err := CanonicalCandidateSourceRowDigest(row)
		if err != nil || packet.SourceRowSHA256 != rowDigest {
			return fmt.Errorf("audit packet %s source-row digest mismatch", packet.CaseID)
		}
		if rejection, rejected := rejectedByCase[packet.CaseID]; rejected {
			if packet.Disposition != "rejected" || packet.NativeBaseSHA != "" ||
				packet.QueueItem != nil || len(packet.Claims) != 0 ||
				!reflect.DeepEqual(packet.Blockers, []MechanicalBlocker{rejectionPacketBlocker(rejection)}) {
				return fmt.Errorf("audit packet %s does not match its exact rejection evidence", packet.CaseID)
			}
			continue
		}
		if record, candidate := poolByCase[packet.CaseID]; candidate {
			expectedQueue := auditQueueFromRecord(record)
			if packet.Disposition != "audit_required" || packet.NativeBaseSHA != record.BaseSHA ||
				packet.QueueItem == nil || !reflect.DeepEqual(*packet.QueueItem, expectedQueue) ||
				!reflect.DeepEqual(packet.Blockers, auditQueueMechanicalBlockers(expectedQueue)) ||
				validateAuditPacketClaims(packet, row, stringSet(record.EligibleClaimSHA256s), record.BaseSHA) != nil {
				return fmt.Errorf("audit packet %s does not exactly match its candidate and claim audit", packet.CaseID)
			}
			continue
		}
		if err := validateQuarantinedAuditPacket(packet, row); err != nil {
			return fmt.Errorf("audit packet %s has invalid quarantine disposition: %w", packet.CaseID, err)
		}
	}
	return nil
}

func validateQuarantinedAuditPacket(packet CandidateAuditPacket, row CandidateSourceRow) error {
	if packet.Disposition != "quarantined" || packet.QueueItem != nil || len(packet.Blockers) != 1 {
		return errors.New("quarantine must contain one deterministic blocker and no queue item")
	}
	blocker := packet.Blockers[0]
	if !validMechanicalBlocker(blocker) {
		return errors.New("quarantine blocker is unknown or structurally invalid")
	}
	switch blocker.Code {
	case "no_human_claim":
		if packet.NativeBaseSHA != "" || hasHumanClaim(row.Claims) || len(packet.Claims) != 0 {
			return errors.New("no-human-claim blocker contradicts the imported source row")
		}
	case "no_eligible_human_claim":
		if !fullSHA.MatchString(packet.NativeBaseSHA) || !hasHumanClaim(row.Claims) {
			return errors.New("claim-eligibility blocker lacks its native base or human source claim")
		}
		if err := validateAuditPacketClaims(packet, row, map[string]bool{}, packet.NativeBaseSHA); err != nil {
			return err
		}
	case "native_base_unavailable", "churn_not_large":
		if !fullSHA.MatchString(packet.NativeBaseSHA) || len(packet.Claims) != 0 {
			return errors.New("post-merge-base blocker lacks its native base or contains claim audits")
		}
	case "source_oid_unavailable", "merge_base_bounded":
		if packet.NativeBaseSHA != "" || len(packet.Claims) != 0 {
			return errors.New("pre-merge-base blocker contains native-base or claim facts")
		}
	default:
		return errors.New("blocker is not a quarantine outcome")
	}
	return nil
}

func validMechanicalBlocker(blocker MechanicalBlocker) bool {
	noFreeform := blocker.Detail == "" && blocker.EvidenceID == "" && blocker.EvidenceRecordSHA256 == ""
	switch blocker.Code {
	case "no_human_claim":
		return noFreeform && blocker.Stage == "source_claims" && blocker.Actual == nil && blocker.Deepen == 0
	case "source_oid_unavailable":
		return noFreeform && blocker.Stage == "source_hydration" && blocker.Actual == nil && blocker.Deepen == 0
	case "merge_base_bounded":
		return noFreeform && blocker.Stage == "merge_base" && blocker.Actual == nil && blocker.Deepen == 168
	case "native_base_unavailable":
		return noFreeform && blocker.Stage == "native_range" && blocker.Actual == nil && blocker.Deepen == 0
	case "churn_not_large":
		return noFreeform && blocker.Stage == "threshold_text_v1" && blocker.Actual != nil &&
			*blocker.Actual >= 0 && *blocker.Actual <= 300 && blocker.Deepen == 0
	case "no_eligible_human_claim":
		return noFreeform && blocker.Stage == "claim_binding" && blocker.Actual == nil && blocker.Deepen == 0
	default:
		return false
	}
}

func validateAuditPacketClaims(packet CandidateAuditPacket, row CandidateSourceRow, expectedEligible map[string]bool, nativeBaseSHA string) error {
	if len(packet.Claims) != len(row.Claims) {
		return errors.New("claim audit count does not match the source row")
	}
	actualEligible := map[string]bool{}
	for index, claim := range packet.Claims {
		expectedBlockers := expectedClaimBlockers(claim.CandidateClaim, row.BaseSHA, nativeBaseSHA, claim.PathExists, claim.InChangedRange)
		if !reflect.DeepEqual(claim.CandidateClaim, row.Claims[index]) || claim.SemanticDecision != "unresolved" ||
			claim.InChangedRange && !claim.PathExists || !reflect.DeepEqual(claim.Blockers, expectedBlockers) {
			return errors.New("claim audit does not exactly bind the source claim and structured blockers")
		}
		if !claim.IsAIComment && claim.PathExists && claim.InChangedRange {
			actualEligible[claim.RawRecordSHA256] = true
		}
	}
	if expectedEligible != nil && !reflect.DeepEqual(actualEligible, expectedEligible) {
		return errors.New("claim audit eligibility does not match the candidate pool")
	}
	return nil
}
func expectedClaimBlockers(claim CandidateClaim, sourceBaseSHA, nativeBaseSHA string, pathExists, inChangedRange bool) []MechanicalBlocker {
	var blockers []MechanicalBlocker
	if !pathExists {
		blockers = append(blockers, MechanicalBlocker{Code: "claim_path_absent", Stage: "claim_binding"})
	}
	nativeCoordinates := claimCoordinatesMatchNativeRange(claim.Side, sourceBaseSHA, nativeBaseSHA)
	rangeValid := validateLocation(Location{Path: claim.Path, StartLine: claim.FromLine, EndLine: claim.ToLine}) == nil
	if !nativeCoordinates {
		blockers = append(blockers, MechanicalBlocker{Code: "source_base_not_native_base", Stage: "claim_binding"})
	} else if !rangeValid {
		blockers = append(blockers, MechanicalBlocker{Code: "claim_range_invalid", Stage: "claim_binding"})
	} else if !inChangedRange {
		blockers = append(blockers, MechanicalBlocker{Code: "claim_outside_changed_range", Stage: "claim_binding"})
	}
	if claim.IsAIComment {
		blockers = append(blockers, MechanicalBlocker{Code: "ai_authored_claim", Stage: "claim_binding"})
	}
	return blockers
}

func DiscoverCandidates(ctx context.Context, config CandidateDiscoveryConfig) (CandidateDiscoveryReport, error) {
	sources, err := LoadSources(config.SourcesPath)
	if err != nil {
		return CandidateDiscoveryReport{}, err
	}
	sourceDigest := CanonicalSourcesDigest(sources)
	if sourceDigest == "" {
		return CandidateDiscoveryReport{}, errors.New("cannot digest sources manifest")
	}
	rejections, err := LoadCandidateRejections(config.RejectionsPath)
	if err != nil {
		return CandidateDiscoveryReport{}, err
	}
	if rejections.SourcesManifestSHA256 != sourceDigest {
		return CandidateDiscoveryReport{}, errors.New("candidate rejections do not match sources manifest")
	}
	var aacrSpec SourceSpec
	for _, source := range sources.Sources {
		if source.ID == "aacr-bench" {
			aacrSpec = source
			break
		}
	}
	if aacrSpec.ID == "" {
		return CandidateDiscoveryReport{}, errors.New("sources manifest has no AACR source")
	}
	aacrPath, err := FetchSource(ctx, aacrSpec, config.CachePath, config.Offline)
	if err != nil {
		return CandidateDiscoveryReport{}, err
	}
	rows, err := ImportAACRSourceRows(aacrPath, aacrSpec.License, aacrSpec.Revision)
	if err != nil {
		return CandidateDiscoveryReport{}, err
	}
	if err := ValidateCandidateRejectionsAgainstAACR(rejections, rows); err != nil {
		return CandidateDiscoveryReport{}, err
	}
	blocked := make(map[string]CandidateRejection, len(rejections.Rejections))
	for _, rejection := range rejections.Rejections {
		blocked[immutableIdentity(strings.ToLower(rejection.Repository), rejection.SourceBaseSHA, rejection.SourceHeadSHA)] = rejection
	}

	report := CandidateDiscoveryReport{SourceIdentityCount: len(rows)}
	packets := make([]CandidateAuditPacket, 0, len(rows))
	records := make([]CandidatePoolRecord, 0, len(rows))
	for _, row := range rows {
		packet := CandidateAuditPacket{CaseID: aacrCaseID(row), SourceRow: row}
		rowDigest, digestErr := CanonicalCandidateSourceRowDigest(row)
		if digestErr != nil {
			return CandidateDiscoveryReport{}, digestErr
		}
		packet.SourceRowSHA256 = rowDigest
		if rejection, rejected := blocked[immutableIdentity(strings.ToLower(row.Repository), row.BaseSHA, row.HeadSHA)]; rejected {
			report.RejectedCount++
			packet.Disposition = "rejected"
			packet.Blockers = []MechanicalBlocker{rejectionPacketBlocker(rejection)}
			packets = append(packets, packet)
			continue
		}
		record, claims, blockers, nativeBaseSHA, discoveryErr := discoverAACRRecord(ctx, config.RepositoryCachePath, config.Offline, row, rowDigest)
		if discoveryErr != nil {
			return CandidateDiscoveryReport{}, fmt.Errorf("discover candidate %s: %w", packet.CaseID, discoveryErr)
		}
		packet.Claims, packet.Blockers, packet.NativeBaseSHA = claims, blockers, nativeBaseSHA
		if len(blockers) != 0 {
			report.QuarantinedCount++
			packet.Disposition = "quarantined"
			packets = append(packets, packet)
			continue
		}
		queue := auditQueueFromRecord(record)
		packet.Disposition = "audit_required"
		packet.QueueItem = &queue
		packet.Blockers = auditQueueMechanicalBlockers(queue)
		records = append(records, record)
		packets = append(packets, packet)
	}

	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	pool := CandidatePoolManifest{
		Schema: CandidatePoolSchema, SourcesManifestSHA256: sourceDigest,
		PartitionSeedSHA256: config.PartitionSeedSHA256, Records: records,
	}
	if !sha256Hex.MatchString(pool.PartitionSeedSHA256) {
		return CandidateDiscoveryReport{}, errors.New("candidate discovery requires a pinned partition seed SHA-256")
	}
	pool.SHA256, err = CanonicalCandidatePoolDigest(pool)
	if err != nil {
		return CandidateDiscoveryReport{}, err
	}
	if err := ValidateCandidatePoolRejections(pool, rejections); err != nil {
		return CandidateDiscoveryReport{}, err
	}
	tuningCount := min(12, len(pool.Records))
	heldOutCount := min(30, len(pool.Records)-tuningCount)
	tuningRecords, heldOutRecords, err := PartitionCandidatePool(pool, tuningCount, heldOutCount)
	if err != nil {
		return CandidateDiscoveryReport{}, err
	}
	tuning := FrozenCorpus{Schema: CorpusSchema, Set: "tuning", SourcesManifestSHA256: sourceDigest, CandidatePoolSHA256: pool.SHA256, Cases: []PreparedCase{}, AuditQueue: auditQueueFromRecords(tuningRecords)}
	heldOut := FrozenCorpus{Schema: CorpusSchema, Set: "held_out", SourcesManifestSHA256: sourceDigest, CandidatePoolSHA256: pool.SHA256, Cases: []PreparedCase{}, AuditQueue: auditQueueFromRecords(heldOutRecords)}
	report.CandidateCount, report.TuningCount, report.HeldOutCount = len(records), tuningCount, heldOutCount
	report.TuningDeficit, report.HeldOutDeficit = 12-tuningCount, 30-heldOutCount
	report.CountsBySource, report.CountsByType, report.CountsByLanguage, report.CountsBySizeBin = candidateCounts(records)
	report.CoverageDeficits = candidateCoverageDeficits(records)
	sort.Slice(packets, func(i, j int) bool { return packets[i].CaseID < packets[j].CaseID })
	auditPackets := AuditPacketsManifest{
		Schema: AuditPacketsSchema, SourcesManifestSHA256: sourceDigest,
		CandidateRejectionsSHA256: CanonicalCandidateRejectionsDigest(rejections),
		CandidatePoolSHA256:       pool.SHA256, PartitionSeedSHA256: pool.PartitionSeedSHA256,
		TuningDeficit: report.TuningDeficit, HeldOutDeficit: report.HeldOutDeficit,
		CoverageDeficits: append([]string(nil), report.CoverageDeficits...), Packets: packets,
	}
	auditPackets.CanonicalSHA256, err = CanonicalAuditPacketsDigest(auditPackets)
	if err != nil {
		return CandidateDiscoveryReport{}, err
	}
	if err := ValidateAuditPackets(auditPackets, rows, rejections, pool, tuning, heldOut); err != nil {
		return CandidateDiscoveryReport{}, err
	}
	outputs := []struct {
		path  string
		value any
	}{
		{config.CandidatePoolPath, pool},
		{config.TuningPath, tuning},
		{config.HeldOutPath, heldOut},
		{config.AuditPacketsPath, auditPackets},
	}
	for _, output := range outputs {
		if err := writeCanonicalJSON(output.path, output.value); err != nil {
			return CandidateDiscoveryReport{}, err
		}
	}
	return report, nil
}

func regenerateAndCompareCandidateArtifacts(
	ctx context.Context,
	config CandidateDiscoveryConfig,
	expectedPool CandidatePoolManifest,
	expectedPackets AuditPacketsManifest,
) error {
	outputDir, err := os.MkdirTemp("", "review-eval-regenerate-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(outputDir)
	config.CandidatePoolPath = filepath.Join(outputDir, "candidate_pool.json")
	config.TuningPath = filepath.Join(outputDir, "tuning.json")
	config.HeldOutPath = filepath.Join(outputDir, "held_out.json")
	config.AuditPacketsPath = filepath.Join(outputDir, "audit_packets.json")
	if _, err := DiscoverCandidates(ctx, config); err != nil {
		return fmt.Errorf("regenerate candidate artifacts: %w", err)
	}
	actualPool, err := LoadCandidatePool(config.CandidatePoolPath)
	if err != nil {
		return err
	}
	actualPackets, err := LoadAuditPackets(config.AuditPacketsPath)
	if err != nil {
		return err
	}
	return compareRegeneratedCandidateArtifacts(expectedPool, actualPool, expectedPackets, actualPackets)
}

func compareRegeneratedCandidateArtifacts(
	expectedPool, actualPool CandidatePoolManifest,
	expectedPackets, actualPackets AuditPacketsManifest,
) error {
	if !reflect.DeepEqual(expectedPool, actualPool) {
		return errors.New("candidate pool differs from deterministic regeneration")
	}
	if !reflect.DeepEqual(expectedPackets, actualPackets) {
		return errors.New("audit packets differ from deterministic regeneration")
	}
	return nil
}

func discoverAACRRecord(ctx context.Context, cacheRoot string, offline bool, row CandidateSourceRow, rowDigest string) (CandidatePoolRecord, []ClaimAudit, []MechanicalBlocker, string, error) {
	if !hasHumanClaim(row.Claims) {
		return CandidatePoolRecord{}, nil, []MechanicalBlocker{{Code: "no_human_claim", Stage: "source_claims"}}, "", nil
	}
	remoteURL := "https://github.com/" + row.Repository + ".git"
	sourceBaseFetch := GitFetchSpec{RemoteURL: remoteURL, Ref: row.BaseSHA, OID: row.BaseSHA}
	headFetch := GitFetchSpec{RemoteURL: remoteURL, Ref: row.HeadSHA, OID: row.HeadSHA}
	repositoryPath, err := OpenPinnedRepository(ctx, cacheRoot, row.Repository, offline, sourceBaseFetch, headFetch)
	if err != nil {
		if blocker, ok := pinnedDiscoveryBlocker(err, "source_oid_unavailable", "source_hydration"); ok {
			return CandidatePoolRecord{}, nil, []MechanicalBlocker{blocker}, "", nil
		}
		return CandidatePoolRecord{}, nil, nil, "", err
	}
	ancestryPath, err := openCandidateAncestryRepository(ctx, cacheRoot, repositoryPath, row, offline)
	if err != nil {
		return CandidatePoolRecord{}, nil, nil, "", err
	}
	mergeBase, err := resolveMergeBase(ctx, ancestryPath, remoteURL, row.BaseSHA, row.HeadSHA, offline)
	if err != nil {
		if blocker, ok := boundedAncestryDiscoveryBlocker(err); ok {
			return CandidatePoolRecord{}, nil, []MechanicalBlocker{blocker}, "", nil
		}
		return CandidatePoolRecord{}, nil, nil, "", err
	}
	baseFetch := GitFetchSpec{RemoteURL: remoteURL, Ref: mergeBase, OID: mergeBase}
	if _, err := OpenPinnedRepository(ctx, cacheRoot, row.Repository, offline, baseFetch); err != nil {
		if blocker, ok := pinnedDiscoveryBlocker(err, "native_base_unavailable", "native_range"); ok {
			return CandidatePoolRecord{}, nil, []MechanicalBlocker{blocker}, mergeBase, nil
		}
		return CandidatePoolRecord{}, nil, nil, "", err
	}
	candidate := PreparedCase{BaseSHA: mergeBase, HeadSHA: row.HeadSHA}
	hydrated, capture, err := hydrateGitCaseWithCapture(ctx, repositoryPath, candidate)
	if err != nil {
		return CandidatePoolRecord{}, nil, nil, "", err
	}
	if churn := hydrated.RawAdditions + hydrated.RawDeletions; churn <= 300 {
		return CandidatePoolRecord{}, nil, []MechanicalBlocker{{Code: "churn_not_large", Stage: "threshold_text_v1", Actual: &churn}}, mergeBase, nil
	}
	claimAudits := make([]ClaimAudit, 0, len(row.Claims))
	claimRanges := map[string][]ChangedRange{
		"left":  changedRangesFromNativeSide(capture.Paths, "left"),
		"right": changedRangesFromNativeSide(capture.Paths, "right"),
	}
	hasEligibleHumanClaim := false
	eligibleClaimDigests := []string{}
	for _, claim := range row.Claims {
		revision := row.BaseSHA
		if claim.Side == "right" {
			revision = row.HeadSHA
		}
		pathExists, pathErr := gitPathExists(ctx, repositoryPath, revision, claim.Path)
		if pathErr != nil {
			return CandidatePoolRecord{}, nil, nil, "", pathErr
		}
		location := Location{Path: claim.Path, StartLine: claim.FromLine, EndLine: claim.ToLine}
		rangeValid := validateLocation(location) == nil
		nativeCoordinates := claimCoordinatesMatchNativeRange(claim.Side, row.BaseSHA, mergeBase)
		inChangedRange := rangeValid && nativeCoordinates && locationInChangedRange(location, claimRanges[claim.Side])
		assessment := ClaimAudit{CandidateClaim: claim, PathExists: pathExists, InChangedRange: inChangedRange, SemanticDecision: "unresolved"}
		if !pathExists {
			assessment.Blockers = append(assessment.Blockers, MechanicalBlocker{Code: "claim_path_absent", Stage: "claim_binding"})
		}
		if !nativeCoordinates {
			assessment.Blockers = append(assessment.Blockers, MechanicalBlocker{Code: "source_base_not_native_base", Stage: "claim_binding"})
		} else if !rangeValid {
			assessment.Blockers = append(assessment.Blockers, MechanicalBlocker{Code: "claim_range_invalid", Stage: "claim_binding"})
		} else if !inChangedRange {
			assessment.Blockers = append(assessment.Blockers, MechanicalBlocker{Code: "claim_outside_changed_range", Stage: "claim_binding"})
		}
		if claim.IsAIComment {
			assessment.Blockers = append(assessment.Blockers, MechanicalBlocker{Code: "ai_authored_claim", Stage: "claim_binding"})
		} else if pathExists && inChangedRange {
			hasEligibleHumanClaim = true
			eligibleClaimDigests = append(eligibleClaimDigests, claim.RawRecordSHA256)
		}
		claimAudits = append(claimAudits, assessment)
	}
	if !hasEligibleHumanClaim {
		return CandidatePoolRecord{}, claimAudits, []MechanicalBlocker{{Code: "no_eligible_human_claim", Stage: "claim_binding"}}, mergeBase, nil
	}
	caseID := aacrCaseID(row)
	record := CandidatePoolRecord{
		ID: caseID, CandidateSource: CandidateSourceAACR, SourceID: row.SourceID, SourceRecordID: row.SourceRecordID,
		SourceType: SourceHumanCaught, Repository: row.Repository, SourceBaseSHA: row.BaseSHA, SourceBaseFetch: sourceBaseFetch,
		EligibleClaimSHA256s: eligibleClaimDigests,
		BaseSHA:              mergeBase, HeadSHA: row.HeadSHA, RangeSemantics: "github_merge_base_to_head",
		SelectionKey: digestBytes([]byte(immutableIdentity(row.Repository, mergeBase, row.HeadSHA))), Language: row.Language,
		SourceReportedChurn: row.SourceReportedChurn, RawAdditions: hydrated.RawAdditions, RawDeletions: hydrated.RawDeletions,
		SizeBin: hydrated.SizeBin, ChangedRanges: hydrated.ChangedRanges, PatchSHA256: hydrated.PatchSHA256,
		BuildStateSHA256: hydrated.BuildStateSHA256, SourceRow: row, SourceRowSHA256: rowDigest,
		EvidenceRefs: append([]string(nil), row.EvidenceRefs...), BaseFetch: baseFetch, HeadFetch: headFetch,
	}
	return record, claimAudits, nil, mergeBase, nil
}

func pinnedDiscoveryBlocker(err error, code, stage string) (MechanicalBlocker, bool) {
	if !isPinnedRemoteVerificationError(err) {
		return MechanicalBlocker{}, false
	}
	return MechanicalBlocker{Code: code, Stage: stage}, true
}

func boundedAncestryDiscoveryBlocker(err error) (MechanicalBlocker, bool) {
	var mechanical candidateMechanicalError
	if !errors.As(err, &mechanical) ||
		mechanical.blocker.Code != "merge_base_bounded" ||
		!validMechanicalBlocker(mechanical.blocker) {
		return MechanicalBlocker{}, false
	}
	return mechanical.blocker, true
}

func claimCoordinatesMatchNativeRange(side, sourceBaseSHA, mergeBaseSHA string) bool {
	return side == "right" || side == "left" && sourceBaseSHA == mergeBaseSHA
}

const candidateAncestryAlgorithm = "review-eval-merge-base-v2"

type candidateAncestryMetadata struct {
	Schema         string `json:"schema"`
	Algorithm      string `json:"algorithm"`
	IdentitySHA256 string `json:"identity_sha256"`
	Repository     string `json:"repository"`
	TargetSHA      string `json:"target_sha"`
	HeadSHA        string `json:"head_sha"`
}

type candidateFailureMarker struct {
	Schema            string `json:"schema"`
	Algorithm         string `json:"algorithm"`
	Stage             string `json:"stage"`
	IdentitySHA256    string `json:"identity_sha256"`
	ObjectStateSHA256 string `json:"object_state_sha256"`
	Failure           string `json:"failure"`
	Deepen            int    `json:"deepen"`
}

func openCandidateAncestryRepository(ctx context.Context, cacheRoot, repositoryPath string, row CandidateSourceRow, offline bool) (string, error) {
	identity := immutableIdentity(strings.ToLower(row.Repository), row.BaseSHA, row.HeadSHA)
	identityDigest := digestBytes([]byte(identity))
	path := filepath.Join(cacheRoot, ".candidate-ancestry", candidateAncestryAlgorithm, identityDigest)
	metadata := candidateAncestryMetadata{
		Schema: "review.eval-candidate-ancestry.v1", Algorithm: candidateAncestryAlgorithm,
		IdentitySHA256: identityDigest, Repository: row.Repository, TargetSHA: row.BaseSHA, HeadSHA: row.HeadSHA,
	}
	if !offline {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("candidate ancestry cache entry is a symlink")
		}
		if err := os.RemoveAll(path); err != nil {
			return "", err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", err
		}
		if _, err := runGit(ctx, path, "init", "--bare", "."); err != nil {
			return "", err
		}
		objectsPath, err := filepath.Abs(filepath.Join(repositoryPath, "objects"))
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Join(path, "objects", "info"), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(path, "objects", "info", "alternates"), []byte(objectsPath+"\n"), 0o600); err != nil {
			return "", err
		}
		for _, revision := range []string{row.BaseSHA, row.HeadSHA} {
			if _, err := runGit(ctx, path, "cat-file", "-e", revision+"^{commit}"); err != nil {
				return "", fmt.Errorf("isolated ancestry object %s is unavailable: %w", revision, err)
			}
		}
		if _, err := runGit(ctx, path, "update-ref", "refs/review-eval/base", row.BaseSHA); err != nil {
			return "", err
		}
		if _, err := runGit(ctx, path, "update-ref", "refs/review-eval/head", row.HeadSHA); err != nil {
			return "", err
		}
		boundaries := []string{row.BaseSHA, row.HeadSHA}
		sort.Strings(boundaries)
		if err := os.WriteFile(filepath.Join(path, "shallow"), []byte(strings.Join(boundaries, "\n")+"\n"), 0o600); err != nil {
			return "", err
		}
		if err := writeCanonicalJSON(filepath.Join(path, "metadata.json"), metadata); err != nil {
			return "", err
		}
		return path, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("isolated ancestry cache is absent")
	}
	var cached candidateAncestryMetadata
	if err := decodeStrictFile(filepath.Join(path, "metadata.json"), &cached); err != nil {
		return "", err
	}
	payload, err := os.ReadFile(filepath.Join(path, "metadata.json"))
	if err != nil {
		return "", err
	}
	canonical, err := canonicalIndentedJSON(cached)
	if err != nil || !bytes.Equal(payload, canonical) || !reflect.DeepEqual(cached, metadata) {
		return "", errors.New("isolated ancestry cache metadata is stale or noncanonical")
	}
	for _, revision := range []string{row.BaseSHA, row.HeadSHA} {
		if _, err := runGit(ctx, path, "cat-file", "-e", revision+"^{commit}"); err != nil {
			return "", fmt.Errorf("isolated ancestry cache lacks exact object %s", revision)
		}
	}
	return path, nil
}

func resolveMergeBase(ctx context.Context, repositoryPath, remoteURL, targetSHA, headSHA string, offline bool) (string, error) {
	markerPath := filepath.Join(repositoryPath, "failure.json")
	identityDigest := digestBytes([]byte(targetSHA + "\x00" + headSHA))
	readMergeBase := func() (string, bool, error) {
		output, err := runGitCommand(ctx, repositoryPath, defaultGitOutputLimit, nil, nil, 1,
			"merge-base", "--all", targetSHA, headSHA)
		if err != nil {
			return "", false, err
		}
		values := strings.Fields(string(output))
		switch {
		case len(values) == 0:
			return "", false, nil
		case len(values) == 1 && fullSHA.MatchString(values[0]):
			return values[0], true, nil
		default:
			return "", false, errors.New("Git merge-base returned ambiguous or malformed output")
		}
	}
	if mergeBase, found, err := readMergeBase(); err != nil {
		return "", err
	} else if found {
		return mergeBase, nil
	}
	stateDigest, err := candidateAncestryStateDigest(ctx, repositoryPath, targetSHA, headSHA)
	if err != nil {
		return "", err
	}
	if marker, ok := loadCandidateFailureMarker(markerPath); ok &&
		marker.Algorithm == candidateAncestryAlgorithm && marker.Stage == "merge-base" &&
		marker.IdentitySHA256 == identityDigest && marker.ObjectStateSHA256 == stateDigest {
		if blocker, valid := candidateFailureMarkerBlocker(marker); valid {
			return "", candidateMechanicalError{blocker: blocker}
		}
	}
	if offline {
		return "", errors.New("offline candidate ancestry cache lacks a verified merge base or bounded-exhaustion marker")
	}
	for _, deepen := range []int{8, 32, 128} {
		if _, err := runGit(ctx, repositoryPath, "fetch", "--no-tags", "--force", fmt.Sprintf("--deepen=%d", deepen), remoteURL, targetSHA, headSHA); err != nil {
			return "", fmt.Errorf("hydrate merge-base ancestry at depth %d: %w", deepen, err)
		}
		if mergeBase, found, err := readMergeBase(); err != nil {
			return "", err
		} else if found {
			return mergeBase, nil
		}
	}
	if err := writeCandidateFailureMarker(ctx, repositoryPath, targetSHA, headSHA, identityDigest, "bounded_exhausted", 168); err != nil {
		return "", fmt.Errorf("record bounded merge-base failure: %w", err)
	}
	return "", candidateMechanicalError{blocker: MechanicalBlocker{Code: "merge_base_bounded", Stage: "merge_base", Deepen: 168}}
}

func candidateAncestryStateDigest(ctx context.Context, repositoryPath, targetSHA, headSHA string) (string, error) {
	resolved := make([]string, 0, 2)
	for _, revision := range []string{targetSHA, headSHA} {
		output, err := runGit(ctx, repositoryPath, "rev-parse", "--verify", revision+"^{commit}")
		if err != nil || strings.TrimSpace(string(output)) != revision {
			return "", fmt.Errorf("isolated ancestry state lacks exact object %s", revision)
		}
		resolved = append(resolved, strings.TrimSpace(string(output)))
	}
	shallow, err := os.ReadFile(filepath.Join(repositoryPath, "shallow"))
	if errors.Is(err, os.ErrNotExist) {
		shallow = []byte("complete\n")
	} else if err != nil {
		return "", err
	}
	return digestBytes([]byte(candidateAncestryAlgorithm + "\x00" + strings.Join(resolved, "\x00") + "\x00" + string(shallow))), nil
}

func loadCandidateFailureMarker(path string) (candidateFailureMarker, bool) {
	var marker candidateFailureMarker
	if err := decodeStrictFile(path, &marker); err != nil {
		return candidateFailureMarker{}, false
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return candidateFailureMarker{}, false
	}
	canonical, err := canonicalIndentedJSON(marker)
	if err != nil || !bytes.Equal(payload, canonical) || marker.Schema != "review.eval-candidate-failure.v1" {
		return candidateFailureMarker{}, false
	}
	return marker, true
}

func candidateFailureMarkerBlocker(marker candidateFailureMarker) (MechanicalBlocker, bool) {
	if marker.Failure == "bounded_exhausted" && marker.Deepen == 168 {
		return MechanicalBlocker{Code: "merge_base_bounded", Stage: "merge_base", Deepen: 168}, true
	}
	return MechanicalBlocker{}, false
}

func writeCandidateFailureMarker(ctx context.Context, repositoryPath, targetSHA, headSHA, identityDigest, failure string, deepen int) error {
	stateDigest, err := candidateAncestryStateDigest(ctx, repositoryPath, targetSHA, headSHA)
	if err != nil {
		return err
	}
	marker := candidateFailureMarker{
		Schema: "review.eval-candidate-failure.v1", Algorithm: candidateAncestryAlgorithm,
		Stage: "merge-base", IdentitySHA256: identityDigest, ObjectStateSHA256: stateDigest,
		Failure: failure, Deepen: deepen,
	}
	if _, valid := candidateFailureMarkerBlocker(marker); !valid {
		return errors.New("candidate failure marker is not a derived algorithm outcome")
	}
	return writeCanonicalJSON(filepath.Join(repositoryPath, "failure.json"), marker)
}

func gitPathExists(ctx context.Context, repositoryPath, revision, path string) (bool, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
		return false, errors.New("Git path check requires a nonempty NUL-free literal path")
	}
	output, err := runGit(ctx, repositoryPath, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", revision, "--", path)
	if err != nil {
		return false, err
	}
	if len(output) == 0 {
		return false, nil
	}
	suffix := append([]byte{'\t'}, []byte(path)...)
	suffix = append(suffix, 0)
	if bytes.Count(output, []byte{0}) != 1 || !bytes.HasSuffix(output, suffix) {
		return false, errors.New("Git path check returned an ambiguous or malformed tree record")
	}
	return true, nil
}

func aacrCaseID(row CandidateSourceRow) string {
	parts := strings.Split(strings.Trim(row.PullRequestURL, "/"), "/")
	number := parts[len(parts)-1]
	return "aacr-" + strings.ToLower(strings.ReplaceAll(row.Repository, "/", "-")) + "-" + number
}

func auditQueueFromRecord(record CandidatePoolRecord) AuditQueueItem {
	return AuditQueueItem{
		CaseID: record.ID, CandidateSource: record.CandidateSource, SourceID: record.SourceID,
		SourceRecordID: record.SourceRecordID, SourceType: record.SourceType, Repository: record.Repository,
		SourceBaseSHA: record.SourceBaseSHA, SourceBaseFetch: record.SourceBaseFetch,
		EligibleClaimSHA256s: append([]string(nil), record.EligibleClaimSHA256s...),
		BaseSHA:              record.BaseSHA, HeadSHA: record.HeadSHA,
		RangeSemantics: record.RangeSemantics, SelectionKey: record.SelectionKey, Language: record.Language,
		SourceReportedChurn: record.SourceReportedChurn, RawAdditions: record.RawAdditions, RawDeletions: record.RawDeletions,
		SizeBin: record.SizeBin, ChangedRanges: append([]ChangedRange(nil), record.ChangedRanges...),
		PatchSHA256: record.PatchSHA256, BuildStateSHA256: record.BuildStateSHA256,
		SourceRowSHA256: record.SourceRowSHA256, EvidenceRefs: append([]string(nil), record.EvidenceRefs...),
		BaseFetch: record.BaseFetch, HeadFetch: record.HeadFetch,
		Reasons: []string{
			"semantic causal validity and introduced-by-change status require two independent evidenced reviews",
			"approved ground truth must bind one accepted human source-row digest and preserve its exact causal path/range",
			"defect category, severity, scenario, and verifier evidence remain unresolved",
		},
	}
}

func auditQueueFromRecords(records []CandidatePoolRecord) []AuditQueueItem {
	queue := make([]AuditQueueItem, 0, len(records))
	for _, record := range records {
		queue = append(queue, auditQueueFromRecord(record))
	}
	return queue
}

func candidateCounts(records []CandidatePoolRecord) (map[string]int, map[string]int, map[string]int, map[string]int) {
	bySource, byType, byLanguage, bySize := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	for _, record := range records {
		bySource[record.CandidateSource]++
		byType[record.SourceType]++
		byLanguage[record.Language]++
		bySize[record.SizeBin]++
	}
	return bySource, byType, byLanguage, bySize
}

func candidateCoverageDeficits(records []CandidatePoolRecord) []string {
	deficits := mechanicalCoverageDeficits(records)
	return append(deficits, "defect-category coverage remains unresolved until semantic audit")
}

func mechanicalCoverageDeficits(records []CandidatePoolRecord) []string {
	bySource, byType, byLanguage, bySize := candidateCounts(records)
	deficits := []string{}
	for _, sourceType := range []string{SourceHumanCaught, SourceInjectedValidated, SourceClean} {
		if byType[sourceType] == 0 {
			deficits = append(deficits, "no mechanically qualified "+sourceType+" large candidate")
		}
	}
	for _, language := range requiredLanguages {
		if byLanguage[language] == 0 {
			deficits = append(deficits, "no mechanically qualified "+language+" large candidate")
		}
	}
	for _, sizeBin := range []string{Size301To1000, Size1001To3000, SizeOver3000} {
		if bySize[sizeBin] == 0 {
			deficits = append(deficits, "no mechanically qualified "+sizeBin+" large candidate")
		}
	}
	if bySource[CandidateSourceCodeReviewBench] == 0 {
		deficits = append(deficits, "current CodeReviewBench pins do not identify immutable reviewed revisions")
	}
	if bySource[CandidateSourceQodoInjected] == 0 {
		deficits = append(deficits, "current Qodo mapping pin does not itself bind immutable ground-truth records")
	}
	if bySource[CandidateSourcePublicHistoryClean] == 0 {
		deficits = append(deficits, "no pinned qualifying public-history clean-candidate source is present")
	}
	return deficits
}

func writeCanonicalJSON(path string, value any) error {
	if path == "" {
		return errors.New("candidate discovery output path is required")
	}
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".review-eval-candidates-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
