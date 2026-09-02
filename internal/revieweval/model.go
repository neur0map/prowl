// Package revieweval implements the condition-neutral native-review evaluation protocol.
package revieweval

import (
	"context"
	"encoding/json"
	"time"

	"github.com/prowl-agent/prowl-agent/internal/agenttrial"
)

const (
	ManifestSchema                = "review.eval-manifest.v1"
	EvalOutputSchema              = "review.eval-output.v1"
	ScoringConfigSchema           = "review.eval-scoring-config.v1"
	AdjudicationSchema            = "review.eval-adjudication.v1"
	BlindAdjudicationSchema       = "review.eval-adjudication-input.v1"
	CollectionSchema              = "review.eval-collection.v1"
	EvalPolicyVersion             = "review-eval-policy.v1"
	EvalArtifactSchemaVersion     = "review-eval-artifacts.v1"
	ConditionControl              = "control"
	ConditionTreatment            = "treatment"
	StatusCompleted               = "completed"
	StatusFailed                  = "failed"
	ProductionBootstrapReplicates = 10_000
)

type Location struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type GroundTruth struct {
	ID          string     `json:"id"`
	DefectClass string     `json:"defect_class"`
	Summary     string     `json:"summary"`
	Scenario    string     `json:"scenario"`
	Critical    bool       `json:"critical,omitempty"`
	CrossFile   bool       `json:"cross_file,omitempty"`
	Deletion    bool       `json:"deletion,omitempty"`
	Locations   []Location `json:"locations"`
}

type Case struct {
	ID           string        `json:"id"`
	Repository   string        `json:"repository"`
	BaseSHA      string        `json:"base_sha"`
	HeadSHA      string        `json:"head_sha"`
	RawAdditions int           `json:"raw_additions"`
	RawDeletions int           `json:"raw_deletions"`
	DefectClass  string        `json:"defect_class"`
	Prompt       string        `json:"prompt,omitempty"`
	GroundTruth  []GroundTruth `json:"ground_truth"`
	Metamorphic  bool          `json:"metamorphic,omitempty"`
}

type MetamorphicTriple struct {
	ID              string `json:"id"`
	GroundTruthID   string `json:"ground_truth_id"`
	BeginningCaseID string `json:"beginning_case_id"`
	MiddleCaseID    string `json:"middle_case_id"`
	EndCaseID       string `json:"end_case_id"`
}

type Manifest struct {
	Schema             string              `json:"schema"`
	Tuning             []Case              `json:"tuning"`
	HeldOut            []Case              `json:"held_out"`
	MetamorphicTriples []MetamorphicTriple `json:"metamorphic_triples,omitempty"`
}

type Finding struct {
	ID                  string     `json:"id"`
	HostID              string     `json:"host_id,omitempty"`
	Condition           string     `json:"condition,omitempty"`
	Category            string     `json:"category"`
	Severity            string     `json:"severity"`
	Confidence          string     `json:"confidence"`
	Summary             string     `json:"summary"`
	Scenario            string     `json:"scenario"`
	Locations           []Location `json:"locations"`
	VerifierDisposition string     `json:"verifier_disposition"`
	VerifierEvidence    string     `json:"verifier_evidence"`
}

type CheckerArtifacts struct {
	ReviewID   string `json:"review_id"`
	PlanPath   string `json:"plan_path"`
	ReportPath string `json:"report_path"`
	CheckPath  string `json:"check_path"`
}

type CheckerEvidence struct {
	Status               string `json:"status"`
	ReviewID             string `json:"review_id"`
	PlanDigest           string `json:"plan_digest"`
	Recommendation       string `json:"recommendation"`
	PrimaryRangesCovered int    `json:"primary_ranges_covered"`
	PrimaryRangesTotal   int    `json:"primary_ranges_total"`
	AuditTargetsCovered  int    `json:"audit_targets_covered"`
	AuditTargetsTotal    int    `json:"audit_targets_total"`
	Stale                bool   `json:"stale"`
	Incomplete           bool   `json:"incomplete"`
	Invalid              bool   `json:"invalid"`
}

type EvalOutput struct {
	Schema         string            `json:"schema"`
	Status         string            `json:"status"`
	Recommendation string            `json:"recommendation"`
	Findings       []Finding         `json:"findings"`
	Usage          agenttrial.Usage  `json:"usage"`
	Checker        *CheckerArtifacts `json:"checker,omitempty"`
}

type EligibilityEdge struct {
	FindingID     string `json:"finding_id"`
	GroundTruthID string `json:"ground_truth_id"`
}

type Adjudication struct {
	Schema          string            `json:"schema"`
	CandidateDigest string            `json:"candidate_digest"`
	Edges           []EligibilityEdge `json:"edges"`
	Frozen          bool              `json:"frozen"`
}

type BlindAdjudicationInput struct {
	Schema           string              `json:"schema"`
	Findings         []Finding           `json:"findings"`
	GroundTruth      []GroundTruth       `json:"ground_truth"`
	FindingCases     map[string][]string `json:"finding_cases"`
	GroundTruthCases map[string][]string `json:"ground_truth_cases"`
}

type ExecutablePin struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

type ToolchainPins struct {
	Claude ExecutablePin `json:"claude"`
	OMP    ExecutablePin `json:"omp"`
	Prowl  ExecutablePin `json:"prowl"`
}

type ExecutableIdentity struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

type ToolchainIdentity struct {
	Claude ExecutableIdentity `json:"claude"`
	OMP    ExecutableIdentity `json:"omp"`
	Prowl  ExecutableIdentity `json:"prowl"`
}

type ScoringConfig struct {
	Schema                       string            `json:"schema"`
	PolicyVersion                string            `json:"policy_version"`
	ArtifactSchemaVersion        string            `json:"artifact_schema_version"`
	Executables                  ToolchainPins     `json:"executables"`
	BaseCaseIDs                  []string          `json:"base_case_ids"`
	MetamorphicTripleIDs         []string          `json:"metamorphic_triple_ids"`
	Clients                      []string          `json:"clients"`
	Model                        string            `json:"model"`
	Budget                       agenttrial.Budget `json:"budget"`
	MaxOutputBytes               int64             `json:"max_output_bytes"`
	Repetitions                  int               `json:"repetitions"`
	Seed                         uint64            `json:"seed"`
	BootstrapReplicates          int               `json:"bootstrap_replicates"`
	MatchingCriteria             string            `json:"matching_criteria"`
	BlindAdjudication            string            `json:"blind_adjudication"`
	AcceptedDispositions         []string          `json:"accepted_dispositions"`
	FailureScoring               string            `json:"failure_scoring"`
	MinimumF1Delta               float64           `json:"minimum_f1_delta"`
	MaximumPrecisionDrop         float64           `json:"maximum_precision_drop"`
	MaximumPositionalSensitivity float64           `json:"maximum_positional_sensitivity"`
	MaximumTokenRatio            float64           `json:"maximum_token_ratio"`
}

type TrialSpec struct {
	Ordinal    int    `json:"ordinal"`
	CaseID     string `json:"case_id"`
	Client     string `json:"client"`
	Condition  string `json:"condition"`
	Repetition int    `json:"repetition"`
}

type TrialRecord struct {
	Ordinal           int              `json:"ordinal"`
	CaseID            string           `json:"case_id"`
	Client            string           `json:"client"`
	Condition         string           `json:"condition"`
	Repetition        int              `json:"repetition"`
	Status            string           `json:"status"`
	Recommendation    string           `json:"recommendation,omitempty"`
	Output            *EvalOutput      `json:"output,omitempty"`
	RawStdout         string           `json:"raw_stdout"`
	RawStderr         string           `json:"raw_stderr"`
	ProwlPlan         json.RawMessage  `json:"prowl_plan,omitempty"`
	ProwlReport       json.RawMessage  `json:"prowl_report,omitempty"`
	CheckResult       json.RawMessage  `json:"check_result,omitempty"`
	Checker           *CheckerEvidence `json:"checker,omitempty"`
	ResolvedLocations int              `json:"resolved_locations"`
	Usage             agenttrial.Usage `json:"usage"`
	ElapsedMS         int64            `json:"elapsed_ms"`
	ClientVersion     string           `json:"client_version,omitempty"`
	ModelVersion      string           `json:"model_version,omitempty"`
	ProwlVersion      string           `json:"prowl_version,omitempty"`
	Error             string           `json:"error,omitempty"`
}

type TrialScore struct {
	CaseID               string           `json:"case_id"`
	Client               string           `json:"client"`
	Condition            string           `json:"condition"`
	Repetition           int              `json:"repetition"`
	DefectClass          string           `json:"defect_class"`
	Metamorphic          bool             `json:"metamorphic"`
	TP                   int              `json:"tp"`
	FP                   int              `json:"fp"`
	FN                   int              `json:"fn"`
	CriticalTP           int              `json:"critical_tp"`
	CriticalTotal        int              `json:"critical_total"`
	CrossFileTP          int              `json:"cross_file_tp"`
	CrossFileTotal       int              `json:"cross_file_total"`
	DeletionTP           int              `json:"deletion_tp"`
	DeletionTotal        int              `json:"deletion_total"`
	SemanticTP           int              `json:"semantic_tp"`
	LocationRecords      int              `json:"location_records"`
	ResolvedLocations    int              `json:"resolved_locations"`
	PrimaryRangesCovered int              `json:"primary_ranges_covered"`
	PrimaryRangesTotal   int              `json:"primary_ranges_total"`
	AuditTargetsCovered  int              `json:"audit_targets_covered"`
	AuditTargetsTotal    int              `json:"audit_targets_total"`
	IncompleteApproval   bool             `json:"incomplete_approval"`
	StaleApproval        bool             `json:"stale_approval"`
	Completed            bool             `json:"completed"`
	Failure              bool             `json:"failure"`
	ElapsedMS            int64            `json:"elapsed_ms"`
	Usage                agenttrial.Usage `json:"usage"`
	MatchedGroundTruth   []string         `json:"matched_ground_truth,omitempty"`
}

type Metric struct {
	Trials               int     `json:"trials"`
	TP                   int     `json:"tp"`
	FP                   int     `json:"fp"`
	FN                   int     `json:"fn"`
	Precision            float64 `json:"precision"`
	Recall               float64 `json:"recall"`
	F1                   float64 `json:"f1"`
	KeyBugInclusion      float64 `json:"key_bug_inclusion"`
	SemanticLocalization float64 `json:"semantic_localization"`
	CrossFileRecall      float64 `json:"cross_file_recall"`
	DeletionRecall       float64 `json:"deletion_recall"`
	ReceiptCoverage      float64 `json:"receipt_coverage"`
	LocationCoverage     float64 `json:"location_coverage"`
	LocationRecords      int     `json:"location_records"`
	ResolvedLocations    int     `json:"resolved_locations"`
	IncompleteApprovals  int     `json:"incomplete_approvals"`
	StaleApprovals       int     `json:"stale_approvals"`
	CompletionRate       float64 `json:"completion_rate"`
	Failures             int     `json:"failures"`
	ElapsedMS            int64   `json:"elapsed_ms"`
	ModelTokens          int64   `json:"model_tokens"`
	ToolCalls            int64   `json:"tool_calls"`
	Subagents            int64   `json:"subagents"`
	RepetitionVariance   float64 `json:"repetition_variance"`
}

type AggregateReport struct {
	Conditions    map[string]Metric            `json:"conditions"`
	ByClient      map[string]map[string]Metric `json:"by_client"`
	ByDefectClass map[string]map[string]Metric `json:"by_defect_class"`
}

type TrialCollection struct {
	Schema              string                 `json:"schema"`
	ManifestDigest      string                 `json:"manifest_digest"`
	ScoringConfigDigest string                 `json:"scoring_config_digest"`
	CandidateDigest     string                 `json:"candidate_digest"`
	Set                 string                 `json:"set"`
	Order               []TrialSpec            `json:"order"`
	Trials              []TrialRecord          `json:"trials"`
	BlindInput          BlindAdjudicationInput `json:"blind_input"`
	Toolchain           ToolchainIdentity      `json:"toolchain"`
}

type RunReport struct {
	Schema      string                       `json:"schema"`
	Order       []TrialSpec                  `json:"order"`
	Trials      []TrialRecord                `json:"trials"`
	Scores      []TrialScore                 `json:"scores"`
	Metrics     AggregateReport              `json:"metrics"`
	Bootstrap   BootstrapInterval            `json:"bootstrap"`
	Metamorphic map[string]MetamorphicMetric `json:"metamorphic"`
	Gates       ShippingGates                `json:"gates"`
}

type BootstrapInterval struct {
	Lower      float64 `json:"lower"`
	Upper      float64 `json:"upper"`
	Replicates int     `json:"replicates"`
	Seed       uint64  `json:"seed"`
}

type MetamorphicMetric struct {
	Beginning   float64 `json:"beginning"`
	Middle      float64 `json:"middle"`
	End         float64 `json:"end"`
	Sensitivity float64 `json:"sensitivity"`
	Triples     int     `json:"triples"`
}

type ShippingGates struct {
	TreatmentCompletion      bool `json:"treatment_completion"`
	TreatmentReceiptCoverage bool `json:"treatment_receipt_coverage"`
	F1Improvement            bool `json:"f1_improvement"`
	BootstrapPositive        bool `json:"bootstrap_positive"`
	RecallImprovement        bool `json:"recall_improvement"`
	CrossFileNonRegression   bool `json:"cross_file_non_regression"`
	DeletionNonRegression    bool `json:"deletion_non_regression"`
	PrecisionNonRegression   bool `json:"precision_non_regression"`
	PositionalSensitivity    bool `json:"positional_sensitivity"`
	LocationCoverage         bool `json:"location_coverage"`
	NoInvalidApprovals       bool `json:"no_invalid_approvals"`
	TokenBudget              bool `json:"token_budget"`
	EveryClientImproves      bool `json:"every_client_improves"`
	Passed                   bool `json:"passed"`
}

type RunConfig struct {
	Clients           []string
	Model             string
	Repetitions       int
	Set               string
	ManifestPath      string
	ScoringConfigPath string
	CollectionPath    string
	AdjudicationPath  string
	OutputDir         string
	PreparedRoot      string
	ProwlBinary       string
	ClaudeBinary      string
	OMPBinary         string
	ReviewSkill       string
	Timeout           time.Duration
	Production        bool
	Launcher          TrialLauncher
}

type TrialLauncher func(ctx context.Context, workDir, prompt string, config agenttrial.ClientConfig) (agenttrial.Result, error)
