package revieweval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	fullSHA   = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func LoadManifest(path string) (Manifest, error) {
	var manifest Manifest
	if err := decodeStrictFile(path, &manifest); err != nil {
		return Manifest{}, err
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ValidateManifest(manifest Manifest) error {
	if manifest.Schema != ManifestSchema {
		return fmt.Errorf("manifest schema must be %q", ManifestSchema)
	}
	seenIDs := map[string]string{}
	seenIdentity := map[string]string{}
	groundTruthCases := map[string][]string{}
	validateSet := func(set string, cases []Case) error {
		for i := range cases {
			c := &cases[i]
			if c.ID == "" || c.Repository == "" || c.BaseSHA == "" || c.HeadSHA == "" || c.DefectClass == "" {
				return fmt.Errorf("%s case %d has an empty required field", set, i)
			}
			if prior := seenIDs[c.ID]; prior != "" {
				return fmt.Errorf("duplicate case id %q in %s and %s", c.ID, prior, set)
			}
			seenIDs[c.ID] = set
			if !safeSegment(c.ID) {
				return fmt.Errorf("case id %q is not a safe artifact path segment", c.ID)
			}
			if err := validateRelative(c.Repository, "repository"); err != nil {
				return fmt.Errorf("case %s: %w", c.ID, err)
			}
			if !fullSHA.MatchString(c.BaseSHA) || !fullSHA.MatchString(c.HeadSHA) || c.BaseSHA == c.HeadSHA {
				return fmt.Errorf("case %s requires distinct immutable full lowercase SHAs", c.ID)
			}
			large := c.RawAdditions >= 0 && c.RawDeletions >= 0 && (c.RawDeletions > 300 || c.RawAdditions > 300-c.RawDeletions)
			if !large {
				return fmt.Errorf("case %s raw churn must be strictly greater than 300", c.ID)
			}
			identity := c.Repository + "\x00" + c.BaseSHA + "\x00" + c.HeadSHA
			if prior := seenIdentity[identity]; prior != "" {
				return fmt.Errorf("case %s duplicates immutable identity from %s", c.ID, prior)
			}
			seenIdentity[identity] = c.ID
			switch c.DefectClass {
			case "local_logic", "deletion_invariant", "cross_file_contract", "producer_consumer", "configuration_migration", "dependency", "test_gap", "clean":
			default:
				return fmt.Errorf("case %s has invalid defect class %q", c.ID, c.DefectClass)
			}
			if c.DefectClass == "clean" && len(c.GroundTruth) != 0 {
				return fmt.Errorf("clean case %s has ground truth", c.ID)
			}
			if c.DefectClass != "clean" && len(c.GroundTruth) == 0 {
				return fmt.Errorf("defective case %s has no ground truth", c.ID)
			}
			groundIDs := map[string]bool{}
			for _, truth := range c.GroundTruth {
				if truth.ID == "" || truth.DefectClass != c.DefectClass || strings.TrimSpace(truth.Summary) == "" || strings.TrimSpace(truth.Scenario) == "" {
					return fmt.Errorf("case %s has incomplete or mismatched ground truth", c.ID)
				}
				for _, secret := range []string{truth.ID, truth.Summary, truth.Scenario} {
					if c.Prompt != "" && strings.Contains(strings.ToLower(c.Prompt), strings.ToLower(secret)) {
						return fmt.Errorf("case %s prompt leaks ground truth", c.ID)
					}
				}
				if groundIDs[truth.ID] {
					return fmt.Errorf("case %s has duplicate ground-truth id %q", c.ID, truth.ID)
				}
				groundIDs[truth.ID] = true
				groundTruthCases[truth.ID] = append(groundTruthCases[truth.ID], c.ID)
				if len(truth.Locations) == 0 {
					return fmt.Errorf("case %s ground truth %s has no causal location", c.ID, truth.ID)
				}
				for _, location := range truth.Locations {
					if err := validateLocation(location); err != nil {
						return fmt.Errorf("case %s ground truth %s: %w", c.ID, truth.ID, err)
					}
				}
			}
		}
		return nil
	}
	if err := validateSet("tuning", manifest.Tuning); err != nil {
		return err
	}
	if err := validateSet("held_out", manifest.HeldOut); err != nil {
		return err
	}
	all := append(append([]Case(nil), manifest.Tuning...), manifest.HeldOut...)
	if err := ValidateMetamorphicTriples(all, manifest.MetamorphicTriples); err != nil {
		return err
	}
	tripleCasesByTruth := map[string]map[string]bool{}
	for _, triple := range manifest.MetamorphicTriples {
		if tripleCasesByTruth[triple.GroundTruthID] != nil {
			return fmt.Errorf("ground-truth id %q is ambiguous across metamorphic triples", triple.GroundTruthID)
		}
		tripleCasesByTruth[triple.GroundTruthID] = map[string]bool{
			triple.BeginningCaseID: true,
			triple.MiddleCaseID:    true,
			triple.EndCaseID:       true,
		}
	}
	for truthID, caseIDs := range groundTruthCases {
		if len(caseIDs) == 1 {
			continue
		}
		allowed := tripleCasesByTruth[truthID]
		if len(allowed) != 3 || len(caseIDs) != 3 {
			return fmt.Errorf("ground-truth id %q is ambiguous outside one metamorphic triple", truthID)
		}
		for _, caseID := range caseIDs {
			if !allowed[caseID] {
				return fmt.Errorf("ground-truth id %q is ambiguous outside one metamorphic triple", truthID)
			}
		}
	}
	setByCase := map[string]string{}
	for _, c := range manifest.Tuning {
		setByCase[c.ID] = "tuning"
	}
	for _, c := range manifest.HeldOut {
		setByCase[c.ID] = "held_out"
	}
	for _, triple := range manifest.MetamorphicTriples {
		set := setByCase[triple.BeginningCaseID]
		if set == "" || setByCase[triple.MiddleCaseID] != set || setByCase[triple.EndCaseID] != set {
			return fmt.Errorf("metamorphic triple %s crosses tuning and held-out sets", triple.ID)
		}
	}
	return nil
}

func LoadScoringConfig(path string, manifest Manifest, production bool) (ScoringConfig, error) {
	var cfg ScoringConfig
	if err := decodeStrictFile(path, &cfg); err != nil {
		return ScoringConfig{}, err
	}
	if err := ValidateScoringConfig(cfg, manifest, production); err != nil {
		return ScoringConfig{}, err
	}
	return cfg, nil
}

func ValidateScoringConfig(cfg ScoringConfig, manifest Manifest, production bool) error {
	if cfg.Schema != ScoringConfigSchema {
		return fmt.Errorf("scoring config schema must be %q", ScoringConfigSchema)
	}
	if cfg.PolicyVersion != "" && cfg.PolicyVersion != EvalPolicyVersion {
		return fmt.Errorf("scoring policy version must be %q", EvalPolicyVersion)
	}
	if cfg.ArtifactSchemaVersion != "" && cfg.ArtifactSchemaVersion != EvalArtifactSchemaVersion {
		return fmt.Errorf("artifact schema version must be %q", EvalArtifactSchemaVersion)
	}
	if production {
		if cfg.PolicyVersion != EvalPolicyVersion || cfg.ArtifactSchemaVersion != EvalArtifactSchemaVersion {
			return errors.New("production scoring config must pin policy and artifact schema versions")
		}
		for name, pin := range map[string]ExecutablePin{"claude": cfg.Executables.Claude, "omp": cfg.Executables.OMP, "prowl": cfg.Executables.Prowl} {
			if strings.TrimSpace(pin.Version) == "" || !sha256Hex.MatchString(pin.SHA256) {
				return fmt.Errorf("production scoring config has invalid %s executable pin", name)
			}
		}
	}
	if len(cfg.BaseCaseIDs) == 0 || len(cfg.Clients) == 0 || cfg.Model == "" || cfg.Repetitions < 1 || cfg.BootstrapReplicates < 1 {
		return errors.New("scoring config has empty required fields")
	}
	if cfg.MatchingCriteria == "" || cfg.BlindAdjudication == "" || cfg.FailureScoring == "" {
		return errors.New("scoring protocol text must be precommitted")
	}
	if !equalStrings(cfg.AcceptedDispositions, []string{"confirmed", "plausible"}) {
		return errors.New("accepted dispositions must be exactly confirmed and plausible")
	}
	if cfg.Budget.MaxModelTokens <= 0 || cfg.Budget.MaxToolCalls <= 0 || cfg.Budget.MaxSubagents <= 0 || cfg.Budget.Timeout <= 0 || cfg.MaxOutputBytes <= 0 {
		return errors.New("all production budgets must be positive")
	}
	if production && cfg.Repetitions != 3 {
		return errors.New("production repetitions must equal 3")
	}
	if production && cfg.BootstrapReplicates != ProductionBootstrapReplicates {
		return fmt.Errorf("production bootstrap replicates must equal %d", ProductionBootstrapReplicates)
	}
	if production && len(cfg.Clients) < 2 {
		return errors.New("production requires at least two clients")
	}
	if production && len(cfg.BaseCaseIDs) < 30 {
		return errors.New("production held-out scoring requires at least thirty base cases")
	}
	if production && len(cfg.MetamorphicTripleIDs) == 0 {
		return errors.New("production requires metamorphic triples")
	}
	if production && (cfg.MinimumF1Delta != .10 || cfg.MaximumPrecisionDrop != .05 || cfg.MaximumPositionalSensitivity != .10 || cfg.MaximumTokenRatio != 1.05) {
		return errors.New("production shipping thresholds must equal the documented frozen gates")
	}
	caseSet := map[string]Case{}
	for _, c := range manifest.Tuning {
		caseSet[c.ID] = c
	}
	if production {
		caseSet = map[string]Case{}
	}
	for _, c := range manifest.HeldOut {
		caseSet[c.ID] = c
	}
	seen := map[string]bool{}
	for _, id := range cfg.BaseCaseIDs {
		if seen[id] {
			return fmt.Errorf("duplicate base case id %q", id)
		}
		seen[id] = true
		c, ok := caseSet[id]
		if !ok || c.Metamorphic {
			return fmt.Errorf("base case id %q is not an eligible base case", id)
		}
	}
	tripleSet := map[string]bool{}
	heldOutCases := map[string]bool{}
	for _, c := range manifest.HeldOut {
		heldOutCases[c.ID] = true
	}
	for _, triple := range manifest.MetamorphicTriples {
		if !production || heldOutCases[triple.BeginningCaseID] {
			tripleSet[triple.ID] = true
		}
	}
	for _, id := range cfg.MetamorphicTripleIDs {
		if !tripleSet[id] {
			return fmt.Errorf("unknown metamorphic triple %q", id)
		}
	}
	if production {
		expectedBase := map[string]bool{}
		for _, c := range manifest.HeldOut {
			if !c.Metamorphic {
				expectedBase[c.ID] = true
			}
		}
		if !sameBoolSet(seen, expectedBase) {
			return errors.New("production base case IDs must exactly match held-out base cases")
		}
		selectedTriple := map[string]bool{}
		for _, id := range cfg.MetamorphicTripleIDs {
			selectedTriple[id] = true
		}
		if !sameBoolSet(selectedTriple, tripleSet) {
			return errors.New("production triple IDs must exactly match held-out metamorphic triples")
		}
	}
	clients := map[string]bool{}
	for _, client := range cfg.Clients {
		if client != "claude" && client != "omp" {
			return fmt.Errorf("unsupported client %q", client)
		}
		if clients[client] {
			return fmt.Errorf("duplicate client %q", client)
		}
		clients[client] = true
	}
	return nil
}

func ParseEvalOutput(data []byte, treatment bool) (EvalOutput, error) {
	var output EvalOutput
	if err := decodeStrict(data, &output); err != nil {
		return EvalOutput{}, err
	}
	if err := ValidateEvalOutput(output, treatment); err != nil {
		return EvalOutput{}, err
	}
	CanonicalizeFindingIDs(&output)
	return output, nil
}

func ValidateEvalOutput(output EvalOutput, treatment bool) error {
	if output.Schema != EvalOutputSchema {
		return fmt.Errorf("eval output schema must be %q", EvalOutputSchema)
	}
	if output.Status != StatusCompleted && output.Status != StatusFailed {
		return fmt.Errorf("invalid trial status %q", output.Status)
	}
	if output.Status == StatusCompleted {
		switch output.Recommendation {
		case "approve", "request_changes", "comment", "incomplete":
		default:
			return fmt.Errorf("invalid recommendation %q", output.Recommendation)
		}
	}
	for i, finding := range output.Findings {
		if finding.Condition != "" {
			return fmt.Errorf("finding %d leaks a condition label", i)
		}
		switch finding.Category {
		case "functional_correctness", "security_privacy", "stability_availability", "data_integrity_integration", "performance_scalability", "maintainability_quality":
		default:
			return fmt.Errorf("finding %d has invalid category %q", i, finding.Category)
		}
		switch finding.Severity {
		case "critical", "major", "minor":
		default:
			return fmt.Errorf("finding %d has invalid severity %q", i, finding.Severity)
		}
		switch finding.Confidence {
		case "high", "medium", "low":
		default:
			return fmt.Errorf("finding %d has invalid confidence %q", i, finding.Confidence)
		}
		if strings.TrimSpace(finding.Summary) == "" || strings.TrimSpace(finding.Scenario) == "" || finding.VerifierEvidence == "" {
			return fmt.Errorf("finding %d has an empty required field", i)
		}
		switch finding.VerifierDisposition {
		case "confirmed", "plausible", "rejected", "unverified":
		default:
			return fmt.Errorf("finding %d has invalid verifier disposition", i)
		}
		if len(finding.Locations) == 0 {
			return fmt.Errorf("finding %d has no location", i)
		}
		for _, location := range finding.Locations {
			if err := validateLocation(location); err != nil {
				return fmt.Errorf("finding %d: %w", i, err)
			}
		}
	}
	if output.Usage.InputTokens < 0 || output.Usage.OutputTokens < 0 || output.Usage.ModelTokens < 0 || output.Usage.ToolCalls < 0 || output.Usage.Subagents < 0 {
		return errors.New("usage counters cannot be negative")
	}
	if !treatment && output.Checker != nil {
		return errors.New("control output must not claim Prowl checker artifacts")
	}
	if treatment && output.Status == StatusCompleted && output.Checker == nil {
		return errors.New("completed treatment requires checker artifact references")
	}
	if output.Checker != nil {
		checker := output.Checker
		if checker.ReviewID == "" || checker.PlanPath == "" || checker.ReportPath == "" || checker.CheckPath == "" {
			return errors.New("checker record is missing immutable artifact references")
		}
		for _, artifact := range []string{checker.PlanPath, checker.ReportPath, checker.CheckPath} {
			if err := validateRelative(artifact, "checker artifact"); err != nil {
				return err
			}
		}
	}
	return nil
}

func CanonicalizeFindingIDs(output *EvalOutput) {
	for ordinal := range output.Findings {
		output.Findings[ordinal].ID = CanonicalFindingID(output.Findings[ordinal], ordinal)
	}
}

func CanonicalFindingID(finding Finding, ordinal int) string {
	canonical := struct {
		Schema              string     `json:"schema"`
		Category            string     `json:"category"`
		Severity            string     `json:"severity"`
		Confidence          string     `json:"confidence"`
		Summary             string     `json:"summary"`
		Scenario            string     `json:"scenario"`
		Locations           []Location `json:"locations"`
		VerifierDisposition string     `json:"verifier_disposition"`
		VerifierEvidence    string     `json:"verifier_evidence"`
		Ordinal             int        `json:"ordinal"`
	}{EvalOutputSchema, finding.Category, finding.Severity, finding.Confidence, finding.Summary, finding.Scenario, finding.Locations, finding.VerifierDisposition, finding.VerifierEvidence, ordinal}
	data, err := json.Marshal(canonical)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func LoadAdjudication(path string) (Adjudication, error) {
	var adjudication Adjudication
	if err := decodeStrictFile(path, &adjudication); err != nil {
		return Adjudication{}, err
	}
	if !adjudication.Frozen {
		return Adjudication{}, errors.New("adjudication matrix must be frozen")
	}
	if adjudication.Schema != AdjudicationSchema {
		return Adjudication{}, fmt.Errorf("adjudication schema must be %q", AdjudicationSchema)
	}
	if !sha256Hex.MatchString(adjudication.CandidateDigest) {
		return Adjudication{}, errors.New("adjudication matrix has invalid candidate digest")
	}
	seen := map[EligibilityEdge]bool{}
	for _, edge := range adjudication.Edges {
		if edge.FindingID == "" || edge.GroundTruthID == "" {
			return Adjudication{}, errors.New("eligibility edge has an empty id")
		}
		if seen[edge] {
			return Adjudication{}, errors.New("duplicate eligibility edge")
		}
		seen[edge] = true
	}
	return adjudication, nil
}

func LoadCollection(path string) (TrialCollection, error) {
	var collection TrialCollection
	if err := decodeStrictFile(path, &collection); err != nil {
		return TrialCollection{}, err
	}
	if collection.Schema != CollectionSchema {
		return TrialCollection{}, fmt.Errorf("collection schema must be %q", CollectionSchema)
	}
	return collection, nil
}

func decodeStrictFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrict(data, target)
}
func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
func validateLocation(location Location) error {
	if err := validateRelative(location.Path, "location path"); err != nil {
		return err
	}
	if location.StartLine < 1 || location.EndLine < location.StartLine {
		return fmt.Errorf("location %s has invalid line range", location.Path)
	}
	return nil
}
func validateRelative(value, label string) error {
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, "\\") {
		return fmt.Errorf("%s %q must be a rooted relative path", label, value)
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean != value {
		return fmt.Errorf("%s %q must be a rooted relative path without traversal", label, value)
	}
	return nil
}
func safeSegment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, `/\\`)
}
func equalStrings(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	return fmt.Sprint(left) == fmt.Sprint(right)
}
func sameBoolSet(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if !right[key] {
			return false
		}
	}
	return true
}
