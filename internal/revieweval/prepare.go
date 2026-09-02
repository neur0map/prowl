package revieweval

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prowl-agent/prowl-agent/internal/review"
)

const (
	SourcesSchema       = "review.eval-sources.v1"
	CandidatePoolSchema = "review.eval-candidate-pool.v1"
	CorpusSchema        = "review.eval-corpus.v1"
	SmallCorpusSchema   = "review.eval-small-corpus.v1"
	ScoringFreezeSchema = "review.eval-scoring-freeze.v1"

	SizeSmall      = "0-300"
	Size301To1000  = "301-1000"
	Size1001To3000 = "1001-3000"
	SizeOver3000   = "3001+"

	SourceHumanCaught       = "human_caught"
	SourceInjectedValidated = "injected_validated"
	SourceClean             = "clean"

	SmallChangePR = "Change-PR"
	SmallCleanPR  = "Clean-PR"

	CandidateSourceAACR               = "aacr"
	CandidateSourceCodeReviewBench    = "codereviewbench"
	CandidateSourceQodoInjected       = "qodo_injected"
	CandidateSourcePublicHistoryClean = "public_history_clean"
)

var requiredCategories = []string{"local_logic", "deletion_invariant", "cross_file_contract", "producer_consumer", "configuration_migration", "dependency", "test_gap", "clean"}
var requiredLanguages = []string{"Go", "TypeScript/JavaScript", "Python", "Java", "Rust"}

var nonPublicDestinationPrefixes = func() []netip.Prefix {
	values := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24",
		"192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16", "192.175.48.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
		"2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}()

var ErrGitOutputLimit = errors.New("Git output limit exceeded")

const (
	defaultGitOutputLimit = 8 << 20
	diffGitOutputLimit    = 64 << 20
	blobGitOutputLimit    = 32 << 20
	gitCommandTimeout     = 2 * time.Minute
)

// SourceSpec pins one immutable, bounded source payload.
type SourceSpec struct {
	ID             string   `json:"id"`
	URL            string   `json:"url"`
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	Path           string   `json:"path"`
	SHA256         string   `json:"sha256"`
	License        string   `json:"license"`
	Parser         string   `json:"parser"`
	Revision       string   `json:"revision"`
	MaxBytes       int64    `json:"max_bytes"`
}

type SourcesManifest struct {
	Schema  string       `json:"schema"`
	Sources []SourceSpec `json:"sources"`
}

type ChangedRange struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type AuditDecision struct {
	Slot             string   `json:"slot"`
	ReviewerID       string   `json:"reviewer_id"`
	Toolchain        string   `json:"toolchain"`
	ToolchainVersion string   `json:"toolchain_version"`
	ToolchainSHA256  string   `json:"toolchain_sha256"`
	Decision         string   `json:"decision"`
	EvidenceRefs     []string `json:"evidence_refs"`
}

type AuditAdjudication struct {
	AdjudicatorID    string   `json:"adjudicator_id"`
	Toolchain        string   `json:"toolchain"`
	ToolchainVersion string   `json:"toolchain_version"`
	ToolchainSHA256  string   `json:"toolchain_sha256"`
	Decision         string   `json:"decision"`
	EvidenceRefs     []string `json:"evidence_refs"`
}

type AuditProvenance struct {
	Decisions    []AuditDecision    `json:"decisions"`
	Adjudication *AuditAdjudication `json:"adjudication,omitempty"`
}

type VerificationEvidence struct {
	Toolchain        string   `json:"toolchain"`
	ToolchainVersion string   `json:"toolchain_version"`
	ToolchainSHA256  string   `json:"toolchain_sha256"`
	Command          string   `json:"command"`
	ExitCode         int      `json:"exit_code"`
	OutputSHA256     string   `json:"output_sha256"`
	EvidenceRefs     []string `json:"evidence_refs"`
}

type CleanProvenance struct {
	Method        string                 `json:"method"`
	EvidenceRefs  []string               `json:"evidence_refs"`
	Verifications []VerificationEvidence `json:"verifications,omitempty"`
}

type CanonicalDiffPosition struct {
	ByteStart  int    `json:"byte_start"`
	ByteEnd    int    `json:"byte_end"`
	TotalBytes int    `json:"total_bytes"`
	LineStart  int    `json:"line_start"`
	LineEnd    int    `json:"line_end"`
	TotalLines int    `json:"total_lines"`
	Bin        string `json:"bin"`
}

type PaddingApplication struct {
	SourceCaseID string `json:"source_case_id"`
	PatchSHA256  string `json:"patch_sha256"`
}

type PreparedCase struct {
	ID                      string                `json:"id"`
	SourceRowSHA256         string                `json:"source_row_sha256,omitempty"`
	CandidateSource         string                `json:"candidate_source,omitempty"`
	SourceID                string                `json:"source_id"`
	SourceRecordID          string                `json:"source_record_id"`
	Repository              string                `json:"repository"`
	BaseSHA                 string                `json:"base_sha"`
	BaseFetch               GitFetchSpec          `json:"base_fetch,omitempty"`
	HeadFetch               GitFetchSpec          `json:"head_fetch,omitempty"`
	HeadSHA                 string                `json:"head_sha"`
	RawAdditions            int                   `json:"raw_additions"`
	RawDeletions            int                   `json:"raw_deletions"`
	Language                string                `json:"language"`
	SizeBin                 string                `json:"size_bin"`
	DefectCategory          string                `json:"defect_category"`
	SourceType              string                `json:"source_type"`
	Direct                  bool                  `json:"direct"`
	StructuredRequired      bool                  `json:"structured_required"`
	SmallLabel              string                `json:"small_label,omitempty"`
	GroundTruth             []GroundTruth         `json:"ground_truth"`
	ChangedRanges           []ChangedRange        `json:"changed_ranges"`
	CleanProvenance         *CleanProvenance      `json:"clean_provenance,omitempty"`
	Audit                   AuditProvenance       `json:"audit"`
	SelectionKey            string                `json:"selection_key"`
	PatchSHA256             string                `json:"patch_sha256"`
	BuildStateSHA256        string                `json:"build_state_sha256"`
	VerificationStateSHA256 string                `json:"verification_state_sha256,omitempty"`
	MetamorphicFamily       string                `json:"metamorphic_family,omitempty"`
	PaddingPosition         string                `json:"padding_position,omitempty"`
	PaddingSourceCaseIDs    []string              `json:"padding_source_case_ids,omitempty"`
	PaddingApplications     []PaddingApplication  `json:"padding_applications,omitempty"`
	CausalDigest            string                `json:"causal_digest,omitempty"`
	CausalPatchSHA256       string                `json:"causal_patch_sha256,omitempty"`
	CanonicalDiffSHA256     string                `json:"canonical_diff_sha256,omitempty"`
	DiffPosition            CanonicalDiffPosition `json:"diff_position,omitempty"`
}

type PreparedTriple struct {
	ID                      string       `json:"id"`
	GroundTruthID           string       `json:"ground_truth_id"`
	CausalDigest            string       `json:"causal_digest"`
	CausalPatchSHA256       string       `json:"causal_patch_sha256"`
	CausalBaseSHA           string       `json:"causal_base_sha"`
	CausalBaseFetch         GitFetchSpec `json:"causal_base_fetch"`
	CausalHeadSHA           string       `json:"causal_head_sha"`
	CausalHeadFetch         GitFetchSpec `json:"causal_head_fetch"`
	ExpectedRawChurn        int          `json:"expected_raw_churn"`
	SizeBin                 string       `json:"size_bin"`
	MaximumByteSizeDelta    int          `json:"maximum_byte_size_delta"`
	MaximumLineSizeDelta    int          `json:"maximum_line_size_delta"`
	VerificationStateSHA256 string       `json:"verification_state_sha256"`
	BeginningCaseID         string       `json:"beginning_case_id"`
	MiddleCaseID            string       `json:"middle_case_id"`
	EndCaseID               string       `json:"end_case_id"`
	PaddingSourceCaseIDs    []string     `json:"padding_source_case_ids"`
}

type GitFetchSpec struct {
	RemoteURL string `json:"remote_url"`
	Ref       string `json:"ref"`
	OID       string `json:"oid"`
}

type CandidateSourceRow struct {
	CandidateSource     string   `json:"candidate_source"`
	SourceID            string   `json:"source_id"`
	SourceRecordID      string   `json:"source_record_id"`
	Repository          string   `json:"repository"`
	PullRequestURL      string   `json:"pull_request_url"`
	BaseSHA             string   `json:"base_sha"`
	HeadSHA             string   `json:"head_sha"`
	License             string   `json:"license"`
	Provenance          string   `json:"provenance"`
	Language            string   `json:"language"`
	SourceReportedChurn int      `json:"source_reported_churn,omitempty"`
	EvidenceRefs        []string `json:"evidence_refs"`
	RawRecordSHA256     string   `json:"raw_record_sha256"`
}

type CandidateSourceRowsManifest struct {
	Schema string               `json:"schema"`
	Rows   []CandidateSourceRow `json:"rows"`
}

type CandidatePoolRecord struct {
	ID                  string             `json:"id"`
	CandidateSource     string             `json:"candidate_source"`
	SourceID            string             `json:"source_id"`
	SourceRecordID      string             `json:"source_record_id"`
	SourceType          string             `json:"source_type"`
	Repository          string             `json:"repository"`
	BaseSHA             string             `json:"base_sha"`
	HeadSHA             string             `json:"head_sha"`
	SelectionKey        string             `json:"selection_key"`
	Language            string             `json:"language"`
	SourceReportedChurn int                `json:"source_reported_churn,omitempty"`
	SourceRow           CandidateSourceRow `json:"source_row"`
	SourceRowSHA256     string             `json:"source_row_sha256"`
	EvidenceRefs        []string           `json:"evidence_refs"`
	BaseFetch           GitFetchSpec       `json:"base_fetch"`
	HeadFetch           GitFetchSpec       `json:"head_fetch"`
}

type CandidatePoolManifest struct {
	Schema                string                `json:"schema"`
	SourcesManifestSHA256 string                `json:"sources_manifest_sha256"`
	SHA256                string                `json:"sha256"`
	PartitionSeedSHA256   string                `json:"partition_seed_sha256"`
	Records               []CandidatePoolRecord `json:"records"`
}

type AuditQueueItem struct {
	CaseID              string       `json:"case_id"`
	CandidateSource     string       `json:"candidate_source"`
	SourceID            string       `json:"source_id,omitempty"`
	SourceRecordID      string       `json:"source_record_id,omitempty"`
	SourceType          string       `json:"source_type,omitempty"`
	Repository          string       `json:"repository,omitempty"`
	BaseSHA             string       `json:"base_sha,omitempty"`
	HeadSHA             string       `json:"head_sha,omitempty"`
	SelectionKey        string       `json:"selection_key,omitempty"`
	Language            string       `json:"language,omitempty"`
	SourceReportedChurn int          `json:"source_reported_churn,omitempty"`
	SourceRowSHA256     string       `json:"source_row_sha256,omitempty"`
	EvidenceRefs        []string     `json:"evidence_refs,omitempty"`
	BaseFetch           GitFetchSpec `json:"base_fetch"`
	HeadFetch           GitFetchSpec `json:"head_fetch"`
	Reasons             []string     `json:"reasons"`
}

type FrozenCorpus struct {
	Schema                string           `json:"schema"`
	Set                   string           `json:"set"`
	SourcesManifestSHA256 string           `json:"sources_manifest_sha256"`
	CandidatePoolSHA256   string           `json:"candidate_pool_sha256,omitempty"`
	Cases                 []PreparedCase   `json:"cases"`
	Triples               []PreparedTriple `json:"triples,omitempty"`
	AuditQueue            []AuditQueueItem `json:"audit_queue"`
}

type FrozenClient struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	ExecutableVersion string `json:"executable_version"`
	ExecutableSHA256  string `json:"executable_sha256"`
}

type FrozenToolchain struct {
	ProwlVersion string `json:"prowl_version"`
	ProwlSHA256  string `json:"prowl_sha256"`
	PolicyID     string `json:"policy_id"`
	SchemaID     string `json:"schema_id"`
}

type FrozenLargeProtocol struct {
	TuningBaseCaseIDs   []string `json:"tuning_base_case_ids"`
	HeldOutBaseCaseIDs  []string `json:"held_out_base_case_ids"`
	TripleIDs           []string `json:"triple_ids"`
	Repetitions         int      `json:"repetitions"`
	MaxModelTokens      int      `json:"max_model_tokens"`
	MaxToolCalls        int      `json:"max_tool_calls"`
	MaxSubagents        int      `json:"max_subagents"`
	WallTimeSeconds     int      `json:"wall_time_seconds"`
	MaxOutputBytes      int64    `json:"max_output_bytes"`
	AggregateCaps       bool     `json:"aggregate_caps"`
	AcceptedFinding     string   `json:"accepted_finding"`
	Matching            string   `json:"matching"`
	Adjudication        string   `json:"adjudication"`
	FailureScoring      string   `json:"failure_scoring"`
	BootstrapReplicates int      `json:"bootstrap_replicates"`
	ExecutionSeed       uint64   `json:"execution_seed"`
	BootstrapSeed       uint64   `json:"bootstrap_seed"`
	Gates               []string `json:"gates"`
}

type FrozenSmallProtocol struct {
	CaseIDs                  []string `json:"case_ids"`
	Repetitions              int      `json:"repetitions"`
	TreatmentCompletion      float64  `json:"treatment_completion"`
	DirectOnly               bool     `json:"direct_only"`
	MaximumAbsolutePointDrop float64  `json:"maximum_absolute_point_drop"`
	StructuredRequiredCount  int      `json:"structured_required_count"`
}

type ContentPin struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

type MetricWeight struct {
	ID      string  `json:"id"`
	Version string  `json:"version"`
	SHA256  string  `json:"sha256"`
	Weight  float64 `json:"weight"`
}

type ScoringFreeze struct {
	Schema                string              `json:"schema"`
	PolicyVersion         string              `json:"policy_version"`
	ArtifactSchemaVersion string              `json:"artifact_schema_version"`
	SourcesManifestSHA256 string              `json:"sources_manifest_sha256"`
	CandidatePoolSHA256   string              `json:"candidate_pool_sha256"`
	Clients               []FrozenClient      `json:"clients"`
	Toolchain             FrozenToolchain     `json:"toolchain"`
	Prompts               []ContentPin        `json:"prompts"`
	SystemPolicy          ContentPin          `json:"system_policy"`
	Formulas              []ContentPin        `json:"formulas"`
	MetricWeights         []MetricWeight      `json:"metric_weights"`
	Toolchains            []ContentPin        `json:"toolchains"`
	Schemas               []ContentPin        `json:"schemas"`
	Large                 FrozenLargeProtocol `json:"large"`
	Small                 FrozenSmallProtocol `json:"small"`
	FreezeBlockers        []string            `json:"freeze_blockers"`
}

type PrepareConfig struct {
	SourcesPath         string
	CandidatePoolPath   string
	TuningPath          string
	HeldOutPath         string
	SmallPath           string
	ScoringPath         string
	CachePath           string
	RepositoryCachePath string
	Offline             bool
}

type PreparationReport struct {
	SourceCount       int
	TuningCount       int
	HeldOutCount      int
	SmallCount        int
	MetamorphicCount  int
	AuditQueueCount   int
	SourcePayloadHash string
}

func RequiredDefectCategories() []string { return append([]string(nil), requiredCategories...) }
func RequiredLanguages() []string        { return append([]string(nil), requiredLanguages...) }

func LoadSources(path string) (SourcesManifest, error) {
	var manifest SourcesManifest
	if err := decodeStrictFile(path, &manifest); err != nil {
		return SourcesManifest{}, err
	}
	return manifest, ValidateSources(manifest)
}

func LoadCandidatePool(path string) (CandidatePoolManifest, error) {
	var pool CandidatePoolManifest
	if err := decodeStrictFile(path, &pool); err != nil {
		return CandidatePoolManifest{}, err
	}
	return pool, ValidateCandidatePool(pool)
}

func LoadFrozenCorpus(path string) (FrozenCorpus, error) {
	var corpus FrozenCorpus
	if err := decodeStrictFile(path, &corpus); err != nil {
		return FrozenCorpus{}, err
	}
	return corpus, nil
}

func LoadScoringFreeze(path string) (ScoringFreeze, error) {
	var freeze ScoringFreeze
	if err := decodeStrictFile(path, &freeze); err != nil {
		return ScoringFreeze{}, err
	}
	return freeze, nil
}

func CanonicalSourcesDigest(manifest SourcesManifest) string {
	payload, err := json.Marshal(manifest)
	if err != nil {
		return ""
	}
	return digestBytes(payload)
}

func ValidateSources(manifest SourcesManifest) error {
	if manifest.Schema != SourcesSchema {
		return fmt.Errorf("sources schema must be %q", SourcesSchema)
	}
	seenID, seenPath := map[string]bool{}, map[string]bool{}
	for _, source := range manifest.Sources {
		if err := validateSourceSpec(source, true); err != nil {
			return err
		}
		if seenID[source.ID] {
			return fmt.Errorf("source id %q is duplicated", source.ID)
		}
		seenID[source.ID] = true
		if seenPath[source.Path] {
			return fmt.Errorf("source %s has duplicate cache path", source.ID)
		}
		seenPath[source.Path] = true
	}
	return nil
}

func validateSourceSpec(source SourceSpec, enforceNetworkPolicy bool) error {
	if source.ID == "" {
		return errors.New("source id is empty")
	}
	parsed, err := url.Parse(source.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("source %s requires an unauthenticated HTTPS URL", source.ID)
	}
	if enforceNetworkPolicy {
		if err := validateSourceURL(source, parsed); err != nil {
			return err
		}
	}
	for _, origin := range source.AllowedOrigins {
		allowed, parseErr := url.Parse(origin)
		if parseErr != nil || allowed.Scheme != "https" || allowed.Host == "" || allowed.User != nil ||
			(allowed.Path != "" && allowed.Path != "/") || allowed.RawQuery != "" || allowed.Fragment != "" {
			return fmt.Errorf("source %s has invalid allowed origin %q", source.ID, origin)
		}
	}
	if filepath.Base(source.Path) != source.Path || source.Path == "." || source.Path == ".." || strings.ContainsAny(source.Path, `/\\`) {
		return fmt.Errorf("source %s has unsafe cache path", source.ID)
	}
	if !sha256Hex.MatchString(source.SHA256) || !fullSHA.MatchString(source.Revision) {
		return fmt.Errorf("source %s requires immutable commit and SHA-256", source.ID)
	}
	if strings.TrimSpace(source.License) == "" || source.MaxBytes <= 0 {
		return fmt.Errorf("source %s has incomplete provenance or bound", source.ID)
	}
	switch source.Parser {
	case "json", "jsonl", "markdown":
		return nil
	default:
		return fmt.Errorf("source %s has unsupported parser %q", source.ID, source.Parser)
	}
}

func validateSourceURL(source SourceSpec, target *url.URL) error {
	if target == nil || target.Scheme != "https" || target.Host == "" || target.User != nil {
		return errors.New("source URL must use unauthenticated HTTPS")
	}
	host := target.Hostname()
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("source URL resolves to a non-public literal IP %s", ip)
	}
	base, err := url.Parse(source.URL)
	if err != nil {
		return err
	}
	allowed := map[string]bool{canonicalOrigin(base): true}
	for _, value := range source.AllowedOrigins {
		parsed, parseErr := url.Parse(value)
		if parseErr != nil {
			return parseErr
		}
		allowed[canonicalOrigin(parsed)] = true
	}
	if !allowed[canonicalOrigin(target)] {
		return fmt.Errorf("source URL origin %q is not pinned", canonicalOrigin(target))
	}
	return nil
}

func canonicalOrigin(parsed *url.URL) string {
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "" || port == "443" {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	for _, prefix := range nonPublicDestinationPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func FetchSource(ctx context.Context, source SourceSpec, cacheDir string, offline bool) (string, error) {
	return fetchSourceWithClient(ctx, source, cacheDir, offline, newSourceHTTPClient(source), true)
}

func fetchSourceWithClient(ctx context.Context, source SourceSpec, cacheDir string, offline bool, client *http.Client, enforceNetworkPolicy bool) (string, error) {
	if err := validateSourceSpec(source, enforceNetworkPolicy); err != nil {
		return "", err
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(cacheDir, source.Path)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("cached source %s is not a regular file", source.ID)
		}
		if err := verifySourceFile(path, source); err == nil {
			return path, nil
		} else if offline {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	} else if offline {
		return "", fmt.Errorf("offline source %s is missing from cache", source.ID)
	}

	if client == nil {
		return "", errors.New("source HTTP client is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download source %s: %w", source.ID, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download source %s: HTTP %s", source.ID, response.Status)
	}
	limited := io.LimitReader(response.Body, source.MaxBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return "", err
	}
	if int64(len(payload)) > source.MaxBytes {
		return "", fmt.Errorf("source %s exceeds maximum size %d", source.ID, source.MaxBytes)
	}
	if err := validatePayload(payload, source.Parser); err != nil {
		return "", fmt.Errorf("source %s: %w", source.ID, err)
	}
	if digestBytes(payload) != source.SHA256 {
		return "", fmt.Errorf("source %s hash mismatch", source.ID)
	}
	temporary, err := os.CreateTemp(cacheDir, ".review-eval-download-")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	return path, nil
}

func newSourceHTTPClient(source SourceSpec) *http.Client {
	transport := &http.Transport{
		Proxy:       nil,
		DialContext: secureDialContext(net.DefaultResolver),
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Minute}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many source redirects")
		}
		return validateSourceURL(source, req.URL)
	}
	return client
}

func secureDialContext(resolver *net.Resolver) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if literal := net.ParseIP(host); literal != nil {
			if !isPublicIP(literal) {
				return nil, fmt.Errorf("refusing non-public destination %s", literal)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(literal.String(), port))
		}
		resolved, err := resolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("host %s resolved to no addresses", host)
		}
		for _, ip := range resolved {
			if !isPublicIP(ip) {
				return nil, fmt.Errorf("host %s resolved to non-public address %s", host, ip)
			}
		}
		var lastErr error
		for _, ip := range resolved {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

func verifySourceFile(path string, source SourceSpec) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > source.MaxBytes {
		return fmt.Errorf("cached source %s exceeds maximum size", source.ID)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if digestBytes(payload) != source.SHA256 {
		return fmt.Errorf("cached source %s hash mismatch", source.ID)
	}
	return validatePayload(payload, source.Parser)
}

func validatePayload(payload []byte, parser string) error {
	switch parser {
	case "json":
		var value any
		decoder := json.NewDecoder(bytes.NewReader(payload))
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("malformed JSON: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("malformed JSON trailing data")
		}
	case "jsonl":
		scanner := bufio.NewScanner(bytes.NewReader(payload))
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			if len(bytes.TrimSpace(scanner.Bytes())) == 0 || !json.Valid(scanner.Bytes()) {
				return fmt.Errorf("malformed JSONL at line %d", line)
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("malformed JSONL: %w", err)
		}
		if line == 0 {
			return errors.New("empty JSONL")
		}
	case "markdown":
		if len(bytes.TrimSpace(payload)) == 0 {
			return errors.New("empty markdown")
		}
	}
	return nil
}

// RawTextChurn reproduces ThresholdTextV1 payload-line accounting for one text path.
func RawTextChurn(base, head []byte) (int, int, error) {
	additions, deletions, _, err := rawTextDiff(base, head, "")
	return additions, deletions, err
}

func rawTextDiff(base, head []byte, path string) (int, int, []ChangedRange, error) {
	if len(base) > blobGitOutputLimit || len(head) > blobGitOutputLimit {
		return 0, 0, nil, ErrGitOutputLimit
	}
	dir, err := os.MkdirTemp("", "review-eval-churn-")
	if err != nil {
		return 0, 0, nil, err
	}
	defer os.RemoveAll(dir)
	basePath, headPath := filepath.Join(dir, "base"), filepath.Join(dir, "head")
	if err := os.WriteFile(basePath, base, 0o600); err != nil {
		return 0, 0, nil, err
	}
	if err := os.WriteFile(headPath, head, 0o600); err != nil {
		return 0, 0, nil, err
	}
	runner := review.ExecGit{Timeout: gitCommandTimeout, MaxStderr: int64(defaultGitOutputLimit)}
	output, err := runner.DiffNoIndex(context.TODO(), dir, int64(diffGitOutputLimit), basePath, headPath)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("raw text diff: %w", err)
	}
	hunks, err := review.ParseUnifiedPatch(output)
	if err != nil {
		return 0, 0, nil, err
	}
	record := review.RawPathRecord{Status: "M", OldPath: path, NewPath: path, Hunks: hunks}
	additions, deletions := 0, 0
	for _, hunk := range hunks {
		for _, line := range bytes.SplitAfter(hunk.Payload, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			switch line[0] {
			case '+':
				additions++
			case '-':
				deletions++
			}
		}
	}
	return additions, deletions, changedRangesFromNative([]review.RawPathRecord{record}), nil
}

func parsePatchRangeStart(field string, prefix byte) (int, error) {
	if len(field) < 2 || field[0] != prefix {
		return 0, fmt.Errorf("malformed raw text range %q", field)
	}
	value := strings.SplitN(field[1:], ",", 2)[0]
	line, err := strconv.Atoi(value)
	if err != nil || line < 0 {
		return 0, fmt.Errorf("malformed raw text range %q", field)
	}
	return line, nil
}

func appendChangedRange(ranges []ChangedRange, path string, line int) []ChangedRange {
	if len(ranges) > 0 && ranges[len(ranges)-1].Path == path && line <= ranges[len(ranges)-1].EndLine+1 {
		if line > ranges[len(ranges)-1].EndLine {
			ranges[len(ranges)-1].EndLine = line
		}
		return ranges
	}
	return append(ranges, ChangedRange{Path: path, StartLine: line, EndLine: line})
}

func SelectionKey(c PreparedCase) string {
	identity := c.Repository + "\x00" + c.BaseSHA + "\x00" + c.HeadSHA
	return digestBytes([]byte(identity))
}

func SelectCases(candidates []PreparedCase, count int) ([]PreparedCase, error) {
	if count < 0 || count > len(candidates) {
		return nil, fmt.Errorf("cannot select %d cases from %d candidates", count, len(candidates))
	}
	selected := append([]PreparedCase(nil), candidates...)
	for i := range selected {
		selected[i].SelectionKey = SelectionKey(selected[i])
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].SelectionKey != selected[j].SelectionKey {
			return selected[i].SelectionKey < selected[j].SelectionKey
		}
		return selected[i].ID < selected[j].ID
	})
	return selected[:count], nil
}

func CanonicalCandidateSourceRowDigest(row CandidateSourceRow) (string, error) {
	if row.CandidateSource == "" || row.SourceID == "" || row.SourceRecordID == "" || row.Repository == "" ||
		!fullSHA.MatchString(row.BaseSHA) || !fullSHA.MatchString(row.HeadSHA) ||
		row.BaseSHA == row.HeadSHA || strings.TrimSpace(row.License) == "" || strings.TrimSpace(row.Provenance) == "" ||
		row.Language == "" || !nonemptyStrings(row.EvidenceRefs) || !sha256Hex.MatchString(row.RawRecordSHA256) {
		return "", errors.New("candidate source row has incomplete canonical provenance")
	}
	switch row.CandidateSource {
	case CandidateSourceAACR, CandidateSourceCodeReviewBench, CandidateSourceQodoInjected:
		parsed, err := url.Parse(row.PullRequestURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" {
			return "", errors.New("benchmark candidate source row requires a canonical GitHub pull request URL")
		}
	case CandidateSourcePublicHistoryClean:
		if row.PullRequestURL != "" {
			return "", errors.New("public-history candidate source row must use pinned commit provenance, not a pull request URL")
		}
	default:
		return "", fmt.Errorf("unsupported candidate source %q", row.CandidateSource)
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return "", err
	}
	return digestBytes(payload), nil
}

func CanonicalCandidatePoolDigest(records []CandidatePoolRecord) (string, error) {
	ordered := append([]CandidatePoolRecord(nil), records...)
	seenIDs, seenIdentities := map[string]bool{}, map[string]bool{}
	for _, record := range ordered {
		if err := validateCandidatePoolRecord(record); err != nil {
			return "", err
		}
		identity := immutableIdentity(record.Repository, record.BaseSHA, record.HeadSHA)
		if seenIDs[record.ID] || seenIdentities[identity] {
			return "", fmt.Errorf("candidate pool has duplicate id or immutable identity for %s", record.ID)
		}
		seenIDs[record.ID], seenIdentities[identity] = true, true
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	payload, err := json.Marshal(ordered)
	if err != nil {
		return "", err
	}
	return digestBytes(payload), nil
}

func ValidateCandidatePool(pool CandidatePoolManifest) error {
	if pool.Schema != CandidatePoolSchema || !sha256Hex.MatchString(pool.SourcesManifestSHA256) || !sha256Hex.MatchString(pool.PartitionSeedSHA256) {
		return errors.New("candidate pool has invalid schema, source-manifest binding, or partition seed")
	}
	digest, err := CanonicalCandidatePoolDigest(pool.Records)
	if err != nil {
		return err
	}
	if pool.SHA256 != digest {
		return errors.New("candidate pool digest does not match its complete canonical records")
	}
	return nil
}

func ValidateCandidatePoolSources(pool CandidatePoolManifest, sources SourcesManifest, sourcePaths map[string]string) error {
	if err := ValidateCandidatePool(pool); err != nil {
		return err
	}
	specs := make(map[string]SourceSpec, len(sources.Sources))
	for _, source := range sources.Sources {
		specs[source.ID] = source
	}
	rowsBySource := map[string]map[string]CandidateSourceRow{}
	for _, record := range pool.Records {
		spec, ok := specs[record.SourceID]
		if !ok {
			return fmt.Errorf("candidate %s source %s is absent from sources manifest", record.ID, record.SourceID)
		}
		if record.SourceRow.License != spec.License || record.SourceRow.Provenance != spec.Revision {
			return fmt.Errorf("candidate %s license or revision provenance does not match source %s", record.ID, record.SourceID)
		}
		rows := rowsBySource[record.SourceID]
		if rows == nil {
			path := sourcePaths[record.SourceID]
			if path == "" {
				return fmt.Errorf("candidate source %s has no verified frozen payload", record.SourceID)
			}
			imported, err := importCandidateSourceRows(path, spec)
			if err != nil {
				return fmt.Errorf("candidate source %s: %w", record.SourceID, err)
			}
			rows = make(map[string]CandidateSourceRow, len(imported))
			for _, row := range imported {
				if rows[row.SourceRecordID].SourceRecordID != "" {
					return fmt.Errorf("candidate source %s has duplicate canonical row %s", record.SourceID, row.SourceRecordID)
				}
				rows[row.SourceRecordID] = row
			}
			rowsBySource[record.SourceID] = rows
		}
		imported, ok := rows[record.SourceRecordID]
		if !ok || !equalCandidateSourceRow(imported, record.SourceRow) {
			return fmt.Errorf("candidate %s does not match its source-specific frozen row", record.ID)
		}
	}
	return nil
}

func importCandidateSourceRows(path string, source SourceSpec) ([]CandidateSourceRow, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest CandidateSourceRowsManifest
	if json.Unmarshal(payload, &manifest) == nil && manifest.Schema == "review.eval-candidate-source-rows.v1" {
		for _, row := range manifest.Rows {
			if row.SourceID != source.ID {
				return nil, fmt.Errorf("canonical row %s names source %s", row.SourceRecordID, row.SourceID)
			}
		}
		return manifest.Rows, nil
	}
	if source.ID == "aacr-bench" {
		return ImportAACRSourceRows(path, source.License, source.Revision)
	}
	return nil, errors.New("payload is not a canonical source-specific candidate-row manifest")
}

func equalCandidateSourceRow(left, right CandidateSourceRow) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func validateCandidatePoolRecord(record CandidatePoolRecord) error {
	if record.ID == "" || record.SourceID == "" || record.SourceRecordID == "" || record.Language == "" ||
		!nonemptyStrings(record.EvidenceRefs) || !fullSHA.MatchString(record.BaseSHA) ||
		!fullSHA.MatchString(record.HeadSHA) || record.BaseSHA == record.HeadSHA ||
		record.SelectionKey != digestBytes([]byte(immutableIdentity(record.Repository, record.BaseSHA, record.HeadSHA))) {
		return fmt.Errorf("candidate %s has incomplete canonical provenance", record.ID)
	}
	switch record.CandidateSource {
	case CandidateSourceAACR, CandidateSourceCodeReviewBench:
		if record.SourceType != SourceHumanCaught {
			return fmt.Errorf("candidate %s has incompatible human-caught source type", record.ID)
		}
	case CandidateSourceQodoInjected:
		if record.SourceType != SourceInjectedValidated {
			return fmt.Errorf("candidate %s has incompatible injected source type", record.ID)
		}
	case CandidateSourcePublicHistoryClean:
		if record.SourceType != SourceClean {
			return fmt.Errorf("candidate %s has incompatible clean source type", record.ID)
		}
	default:
		return fmt.Errorf("candidate %s has unsupported candidate source %q", record.ID, record.CandidateSource)
	}
	rowDigest, err := CanonicalCandidateSourceRowDigest(record.SourceRow)
	if err != nil || rowDigest != record.SourceRowSHA256 ||
		record.SourceRow.CandidateSource != record.CandidateSource || record.SourceRow.SourceID != record.SourceID ||
		record.SourceRow.SourceRecordID != record.SourceRecordID || record.SourceRow.Repository != record.Repository ||
		record.SourceRow.BaseSHA != record.BaseSHA || record.SourceRow.HeadSHA != record.HeadSHA ||
		record.SourceRow.Language != record.Language || record.SourceRow.SourceReportedChurn != record.SourceReportedChurn ||
		!equalStringSets(record.SourceRow.EvidenceRefs, record.EvidenceRefs) {
		return fmt.Errorf("candidate %s is not bound to its canonical frozen source row", record.ID)
	}
	if err := validateGitFetchBinding(record.Repository, record.BaseSHA, record.BaseFetch); err != nil {
		return fmt.Errorf("candidate %s base fetch: %w", record.ID, err)
	}
	if err := validateGitFetchBinding(record.Repository, record.HeadSHA, record.HeadFetch); err != nil {
		return fmt.Errorf("candidate %s head fetch: %w", record.ID, err)
	}
	return nil
}

func ValidateGitFetchSpec(spec GitFetchSpec) error {
	if !fullSHA.MatchString(spec.OID) {
		return errors.New("Git fetch metadata requires a full lowercase OID")
	}
	parsed, err := url.Parse(spec.RemoteURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("Git fetch metadata requires an unauthenticated HTTPS GitHub remote")
	}
	path := strings.TrimPrefix(parsed.Path, "/")
	parts := strings.Split(strings.TrimSuffix(path, ".git"), "/")
	if len(parts) != 2 || !safeRepositorySegment(parts[0]) || !safeRepositorySegment(parts[1]) {
		return errors.New("Git fetch metadata has an unsafe repository path")
	}
	if fullSHA.MatchString(spec.Ref) {
		return nil
	}
	refParts := strings.Split(spec.Ref, "/")
	if len(refParts) != 4 || refParts[0] != "refs" || refParts[1] != "pull" || refParts[3] != "head" {
		return errors.New("Git fetch metadata requires an exact OID or GitHub pull head ref")
	}
	number, err := strconv.Atoi(refParts[2])
	if err != nil || number <= 0 {
		return errors.New("Git fetch metadata has an invalid pull request ref")
	}
	return nil
}

func validateGitFetchBinding(repository, oid string, spec GitFetchSpec) error {
	if spec.OID != oid {
		return errors.New("Git fetch metadata OID does not match the pinned commit")
	}
	if err := ValidateGitFetchSpec(spec); err != nil {
		return err
	}
	remotePath := strings.TrimSuffix(strings.TrimPrefix(spec.RemoteURL, "https://github.com/"), ".git")
	if !strings.EqualFold(remotePath, repository) {
		return fmt.Errorf("fetch remote %s does not match repository %s", spec.RemoteURL, repository)
	}
	return nil
}

func PartitionCandidatePool(pool CandidatePoolManifest, tuningCount, heldOutCount int) ([]CandidatePoolRecord, []CandidatePoolRecord, error) {
	if err := ValidateCandidatePool(pool); err != nil {
		return nil, nil, err
	}
	if tuningCount < 0 || heldOutCount < 0 || tuningCount+heldOutCount > len(pool.Records) {
		return nil, nil, errors.New("candidate partition counts exceed the frozen pool")
	}
	ordered := append([]CandidatePoolRecord(nil), pool.Records...)
	sort.Slice(ordered, func(i, j int) bool {
		left := digestBytes([]byte(pool.PartitionSeedSHA256 + "\x00" + immutableIdentity(ordered[i].Repository, ordered[i].BaseSHA, ordered[i].HeadSHA)))
		right := digestBytes([]byte(pool.PartitionSeedSHA256 + "\x00" + immutableIdentity(ordered[j].Repository, ordered[j].BaseSHA, ordered[j].HeadSHA)))
		if left != right {
			return left < right
		}
		return ordered[i].ID < ordered[j].ID
	})
	return append([]CandidatePoolRecord(nil), ordered[:tuningCount]...), append([]CandidatePoolRecord(nil), ordered[tuningCount:tuningCount+heldOutCount]...), nil
}

func ValidateCorpusPartitions(tuning, heldOut, small FrozenCorpus, pool CandidatePoolManifest) error {
	if err := ValidateCandidatePool(pool); err != nil {
		return err
	}
	if tuning.CandidatePoolSHA256 != pool.SHA256 || heldOut.CandidatePoolSHA256 != pool.SHA256 {
		return errors.New("large corpus candidate-pool digest mismatch")
	}
	tuningCount := primaryCaseCount(tuning.Cases) + len(tuning.AuditQueue)
	heldOutCount := primaryCaseCount(heldOut.Cases) + len(heldOut.AuditQueue)
	if tuningCount < 12 || heldOutCount < 30 {
		return errors.New("candidate partitions require at least 12 tuning and 30 held-out primary cases")
	}
	wantTuning, wantHeldOut, err := PartitionCandidatePool(pool, tuningCount, heldOutCount)
	if err != nil {
		return err
	}
	if err := validatePartitionMembers("tuning", tuning, wantTuning); err != nil {
		return err
	}
	if err := validatePartitionMembers("held-out", heldOut, wantHeldOut); err != nil {
		return err
	}
	seenIDs, seenIdentities, seenTruth := map[string]string{}, map[string]string{}, map[string]string{}
	for _, corpus := range []FrozenCorpus{tuning, heldOut, small} {
		for _, candidate := range corpus.AuditQueue {
			if err := recordGlobalIdentity(candidate.CaseID, immutableIdentity(candidate.Repository, candidate.BaseSHA, candidate.HeadSHA), corpus.Set, seenIDs, seenIdentities); err != nil {
				return err
			}
		}
		for _, c := range corpus.Cases {
			if err := recordGlobalIdentity(c.ID, immutableIdentity(c.Repository, c.BaseSHA, c.HeadSHA), corpus.Set, seenIDs, seenIdentities); err != nil {
				return err
			}
			for _, truth := range c.GroundTruth {
				if truth.ID == "" {
					return fmt.Errorf("case %s has empty ground-truth identity", c.ID)
				}
				if prior := seenTruth[truth.ID]; prior != "" {
					return fmt.Errorf("ground-truth id %s overlaps %s and %s", truth.ID, prior, corpus.Set)
				}
				seenTruth[truth.ID] = corpus.Set
			}
		}
	}
	return nil
}

func validatePartitionMembers(label string, corpus FrozenCorpus, expected []CandidatePoolRecord) error {
	want := make(map[string]CandidatePoolRecord, len(expected))
	for _, record := range expected {
		want[record.ID] = record
	}
	got := map[string]bool{}
	for _, candidate := range corpus.AuditQueue {
		record, ok := want[candidate.CaseID]
		if !ok || !candidateQueueMatchesRecord(candidate, record) {
			return fmt.Errorf("%s queue case %s is not canonically bound to its candidate-pool record", label, candidate.CaseID)
		}
		got[candidate.CaseID] = true
	}
	for _, c := range corpus.Cases {
		if c.MetamorphicFamily != "" {
			continue
		}
		record, ok := want[c.ID]
		if !ok || !preparedCaseMatchesRecord(c, record) {
			return fmt.Errorf("%s primary case %s is not canonically bound to its candidate-pool record", label, c.ID)
		}
		got[c.ID] = true
	}
	if len(got) != len(want) {
		return fmt.Errorf("%s partition does not match deterministic frozen-pool assignment", label)
	}
	return nil
}

func candidateQueueMatchesRecord(candidate AuditQueueItem, record CandidatePoolRecord) bool {
	return candidate.CaseID == record.ID && candidate.CandidateSource == record.CandidateSource &&
		candidate.SourceID == record.SourceID && candidate.SourceRecordID == record.SourceRecordID &&
		candidate.SourceType == record.SourceType && candidate.Repository == record.Repository &&
		candidate.BaseSHA == record.BaseSHA && candidate.HeadSHA == record.HeadSHA &&
		candidate.SelectionKey == record.SelectionKey && candidate.Language == record.Language &&
		candidate.SourceReportedChurn == record.SourceReportedChurn && candidate.SourceRowSHA256 == record.SourceRowSHA256 &&
		equalStringSets(candidate.EvidenceRefs, record.EvidenceRefs) &&
		candidate.BaseFetch == record.BaseFetch && candidate.HeadFetch == record.HeadFetch
}

func preparedCaseMatchesRecord(c PreparedCase, record CandidatePoolRecord) bool {
	return c.ID == record.ID && c.CandidateSource == record.CandidateSource &&
		c.SourceID == record.SourceID && c.SourceRecordID == record.SourceRecordID &&
		c.SourceRowSHA256 == record.SourceRowSHA256 && c.SourceType == record.SourceType && c.Repository == record.Repository &&
		c.BaseSHA == record.BaseSHA && c.BaseFetch == record.BaseFetch &&
		c.HeadSHA == record.HeadSHA && c.HeadFetch == record.HeadFetch &&
		c.SelectionKey == record.SelectionKey && c.Language == record.Language
}

func recordGlobalIdentity(id, identity, set string, seenIDs, seenIdentities map[string]string) error {
	if id == "" || identity == immutableIdentity("", "", "") {
		return errors.New("empty global case identity")
	}
	if prior := seenIDs[id]; prior != "" {
		return fmt.Errorf("case id %s overlaps %s and %s", id, prior, set)
	}
	if prior := seenIdentities[identity]; prior != "" {
		return fmt.Errorf("immutable PR identity overlaps %s and %s", prior, set)
	}
	seenIDs[id], seenIdentities[identity] = set, set
	return nil
}

func immutableIdentity(repository, baseSHA, headSHA string) string {
	return repository + "\x00" + baseSHA + "\x00" + headSHA
}

func primaryCaseCount(cases []PreparedCase) int {
	count := 0
	for _, c := range cases {
		if c.MetamorphicFamily == "" {
			count++
		}
	}
	return count
}

func ValidateLargeCorpus(cases []PreparedCase, minimum int, requireAudit bool) error {
	primaryCount := 0
	for _, c := range cases {
		if c.MetamorphicFamily == "" {
			primaryCount++
		}
	}
	if primaryCount < minimum {
		return fmt.Errorf("large corpus has %d primary base cases; requires at least %d", primaryCount, minimum)
	}
	seenID, seenIdentity, seenTruth := map[string]bool{}, map[string]bool{}, map[string]bool{}
	categories, languages, bins, sourceTypes := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range cases {
		c := &cases[i]
		if err := validatePreparedCase(c, true); err != nil {
			return err
		}
		if seenID[c.ID] {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		seenID[c.ID] = true
		identity := c.Repository + "\x00" + c.BaseSHA + "\x00" + c.HeadSHA
		if seenIdentity[identity] {
			return fmt.Errorf("duplicate immutable identity for case %s", c.ID)
		}
		seenIdentity[identity] = true
		for _, truth := range c.GroundTruth {
			if seenTruth[truth.ID] {
				return fmt.Errorf("duplicate ground-truth identity %s", truth.ID)
			}
			seenTruth[truth.ID] = true
		}
		if c.MetamorphicFamily == "" {
			categories[c.DefectCategory], languages[c.Language], bins[c.SizeBin], sourceTypes[c.SourceType] = true, true, true, true
		}
		if requireAudit && !auditApproved(c.Audit) {
			return fmt.Errorf("case %s lacks two resolved independent audit decisions", c.ID)
		}
	}
	if missing := missingValues(categories, requiredCategories); len(missing) > 0 {
		return fmt.Errorf("large corpus missing defect categories: %s", strings.Join(missing, ", "))
	}
	if missing := missingValues(languages, requiredLanguages); len(missing) > 0 {
		return fmt.Errorf("large corpus missing languages: %s", strings.Join(missing, ", "))
	}
	if missing := missingValues(bins, []string{Size301To1000, Size1001To3000, SizeOver3000}); len(missing) > 0 {
		return fmt.Errorf("large corpus missing size bins: %s", strings.Join(missing, ", "))
	}
	if missing := missingValues(sourceTypes, []string{SourceHumanCaught, SourceInjectedValidated, SourceClean}); len(missing) > 0 {
		return fmt.Errorf("large corpus missing source types: %s", strings.Join(missing, ", "))
	}
	return nil
}

func validatePreparedCase(c *PreparedCase, large bool) error {
	if c.ID == "" || c.SourceID == "" || c.SourceRecordID == "" || c.Repository == "" || !fullSHA.MatchString(c.BaseSHA) || !fullSHA.MatchString(c.HeadSHA) || c.BaseSHA == c.HeadSHA {
		return fmt.Errorf("case %s has incomplete immutable identity", c.ID)
	}
	if c.RawAdditions < 0 || c.RawDeletions < 0 {
		return fmt.Errorf("case %s has negative churn", c.ID)
	}
	if err := validateGitFetchBinding(c.Repository, c.BaseSHA, c.BaseFetch); err != nil {
		return fmt.Errorf("case %s base fetch: %w", c.ID, err)
	}
	if err := validateGitFetchBinding(c.Repository, c.HeadSHA, c.HeadFetch); err != nil {
		return fmt.Errorf("case %s head fetch: %w", c.ID, err)
	}
	churn := c.RawAdditions + c.RawDeletions
	wantBin := SizeSmall
	if churn > 3000 {
		wantBin = SizeOver3000
	} else if churn > 1000 {
		wantBin = Size1001To3000
	} else if churn > 300 {
		wantBin = Size301To1000
	}
	if c.SizeBin != wantBin || c.StructuredRequired != (churn > 300) {
		return fmt.Errorf("case %s has invalid language/size classification", c.ID)
	}
	if large && churn <= 300 {
		return fmt.Errorf("large case %s does not exceed 300 lines", c.ID)
	}
	if c.SelectionKey != "" && c.SelectionKey != SelectionKey(*c) {
		return fmt.Errorf("case %s has noncanonical selection key", c.ID)
	}
	if !sha256Hex.MatchString(c.PatchSHA256) || !sha256Hex.MatchString(c.BuildStateSHA256) {
		return fmt.Errorf("case %s lacks patch/build hashes", c.ID)
	}
	categoryOK := false
	for _, category := range requiredCategories {
		categoryOK = categoryOK || c.DefectCategory == category
	}
	if !categoryOK {
		return fmt.Errorf("case %s has unknown defect category", c.ID)
	}
	switch c.SourceType {
	case SourceHumanCaught, SourceInjectedValidated, SourceClean:
	default:
		return fmt.Errorf("case %s has invalid source type", c.ID)
	}
	if c.DefectCategory == "clean" {
		if len(c.GroundTruth) != 0 || c.SourceType != SourceClean || c.CleanProvenance == nil || c.CleanProvenance.Method == "" || len(c.CleanProvenance.EvidenceRefs) == 0 {
			return fmt.Errorf("clean case %s has incomplete clean provenance", c.ID)
		}
	} else {
		if c.SourceType == SourceClean || len(c.GroundTruth) == 0 {
			return fmt.Errorf("defective case %s has invalid source type or no ground truth", c.ID)
		}
		for _, truth := range c.GroundTruth {
			if truth.DefectClass != c.DefectCategory || truth.ID == "" || len(truth.Locations) == 0 {
				return fmt.Errorf("case %s has incomplete ground truth", c.ID)
			}
			if c.SourceID == "swr" || truth.CanonicalDigest != "" {
				if strings.TrimSpace(truth.Severity) == "" || strings.TrimSpace(truth.Summary) == "" ||
					strings.TrimSpace(truth.Scenario) == "" || strings.TrimSpace(truth.Verifier) == "" ||
					truth.Provenance == nil || truth.Provenance.SourceID != c.SourceID || truth.Provenance.SourceRecordID != c.SourceRecordID ||
					!sha256Hex.MatchString(truth.Provenance.SourceDigest) || !nonemptyStrings(truth.Provenance.EvidenceRefs) ||
					truth.CanonicalDigest != groundTruthDigest(truth) {
					return fmt.Errorf("case %s has incomplete or noncanonical ground truth %s", c.ID, truth.ID)
				}
			}
			for _, location := range truth.Locations {
				if err := validateLocation(location); err != nil {
					return fmt.Errorf("case %s ground truth %s: %w", c.ID, truth.ID, err)
				}
				if !locationInChangedRange(location, c.ChangedRanges) {
					return fmt.Errorf("case %s ground truth %s is outside a changed range", c.ID, truth.ID)
				}
			}
		}
	}
	return nil
}

func locationInChangedRange(location Location, ranges []ChangedRange) bool {
	for _, changed := range ranges {
		if changed.Path == location.Path && location.StartLine >= changed.StartLine && location.EndLine <= changed.EndLine && changed.StartLine > 0 && changed.EndLine >= changed.StartLine {
			return true
		}
	}
	return false
}

func auditApproved(audit AuditProvenance) bool {
	if len(audit.Decisions) != 2 {
		return false
	}
	seenSlots, seenReviewers := map[string]bool{}, map[string]bool{}
	for _, decision := range audit.Decisions {
		if (decision.Slot != "reviewer_1" && decision.Slot != "reviewer_2") || seenSlots[decision.Slot] ||
			seenReviewers[decision.ReviewerID] || !validAuditActor(decision.ReviewerID, decision.Toolchain, decision.ToolchainVersion, decision.ToolchainSHA256, decision.EvidenceRefs) ||
			(decision.Decision != "approved" && decision.Decision != "rejected") {
			return false
		}
		seenSlots[decision.Slot], seenReviewers[decision.ReviewerID] = true, true
	}
	first, second := audit.Decisions[0].Decision, audit.Decisions[1].Decision
	if first == second {
		return first == "approved" && audit.Adjudication == nil
	}
	adjudication := audit.Adjudication
	return adjudication != nil && adjudication.Decision == "approved" && !seenReviewers[adjudication.AdjudicatorID] &&
		validAuditActor(adjudication.AdjudicatorID, adjudication.Toolchain, adjudication.ToolchainVersion, adjudication.ToolchainSHA256, adjudication.EvidenceRefs)
}

func validAuditActor(identity, toolchain, version, toolchainSHA string, evidence []string) bool {
	return strings.TrimSpace(identity) != "" && strings.TrimSpace(toolchain) != "" && strings.TrimSpace(version) != "" &&
		sha256Hex.MatchString(toolchainSHA) && nonemptyStrings(evidence)
}

func nonemptyStrings(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func ValidateSmallCorpus(cases []PreparedCase, large []PreparedCase) error {
	if len(cases) < 40 {
		return fmt.Errorf("small corpus has %d cases; requires at least 40", len(cases))
	}
	largeIdentities := map[string]bool{}
	for _, c := range large {
		largeIdentities[c.Repository+"\x00"+c.BaseSHA+"\x00"+c.HeadSHA] = true
	}
	seen, labels := map[string]bool{}, map[string]int{}
	for i := range cases {
		c := &cases[i]
		if err := validatePreparedCase(c, false); err != nil {
			return err
		}
		churn := c.RawAdditions + c.RawDeletions
		if churn > 300 || !c.Direct || c.StructuredRequired || c.SourceID != "swr" {
			return fmt.Errorf("small case %s must be official SWR, <=300, direct, and non-structured", c.ID)
		}
		identity := c.Repository + "\x00" + c.BaseSHA + "\x00" + c.HeadSHA
		if seen[identity] || largeIdentities[identity] {
			return fmt.Errorf("small case %s overlaps another corpus", c.ID)
		}
		seen[identity] = true
		if c.SmallLabel != SmallChangePR && c.SmallLabel != SmallCleanPR {
			return fmt.Errorf("small case %s has invalid SWR label", c.ID)
		}
		if c.SmallLabel == SmallChangePR && c.DefectCategory == "clean" || c.SmallLabel == SmallCleanPR && c.DefectCategory != "clean" {
			return fmt.Errorf("small case %s mixes SWR Change/Clean labels", c.ID)
		}
		labels[c.SmallLabel]++
	}
	if labels[SmallChangePR] < 20 || labels[SmallCleanPR] < 20 {
		return fmt.Errorf("small corpus requires at least 20 Change-PR and 20 Clean-PR cases")
	}
	return nil
}

func validSizeBin(value string) bool {
	return value == SizeSmall || value == Size301To1000 || value == Size1001To3000 || value == SizeOver3000
}

func ValidatePreparedTriples(cases []PreparedCase, triples []PreparedTriple) error {
	byID := map[string]PreparedCase{}
	for _, c := range cases {
		if c.ID == "" || byID[c.ID].ID != "" {
			return fmt.Errorf("duplicate or empty case id %q", c.ID)
		}
		byID[c.ID] = c
	}
	seenCases, seenTriples := map[string]bool{}, map[string]bool{}
	for _, triple := range triples {
		if !safeSegment(triple.ID) || triple.GroundTruthID == "" || seenTriples[triple.ID] ||
			!sha256Hex.MatchString(triple.CausalDigest) || !sha256Hex.MatchString(triple.CausalPatchSHA256) ||
			!fullSHA.MatchString(triple.CausalBaseSHA) || !fullSHA.MatchString(triple.CausalHeadSHA) || triple.CausalBaseSHA == triple.CausalHeadSHA ||
			!sha256Hex.MatchString(triple.VerificationStateSHA256) || !uniqueNonemptyStrings(triple.PaddingSourceCaseIDs) ||
			triple.ExpectedRawChurn <= 0 || !validSizeBin(triple.SizeBin) ||
			triple.MaximumByteSizeDelta < 0 || triple.MaximumLineSizeDelta < 0 {
			return fmt.Errorf("triple %s has incomplete canonical identity, comparability, or padding provenance", triple.ID)
		}
		if triple.CausalBaseFetch.OID != triple.CausalBaseSHA || triple.CausalHeadFetch.OID != triple.CausalHeadSHA {
			return fmt.Errorf("triple %s fetch OIDs do not bind its causal commits", triple.ID)
		}
		if err := ValidateGitFetchSpec(triple.CausalBaseFetch); err != nil {
			return fmt.Errorf("triple %s causal base fetch: %w", triple.ID, err)
		}
		if err := ValidateGitFetchSpec(triple.CausalHeadFetch); err != nil {
			return fmt.Errorf("triple %s causal head fetch: %w", triple.ID, err)
		}
		seenTriples[triple.ID] = true
		variants := []struct{ id, position string }{
			{triple.BeginningCaseID, "beginning"},
			{triple.MiddleCaseID, "middle"},
			{triple.EndCaseID, "end"},
		}
		var variantCases [3]PreparedCase
		treeDigests, paddingUnion := map[string]bool{}, map[string]bool{}
		minBytes, maxBytes, minLines, maxLines := int(^uint(0)>>1), 0, int(^uint(0)>>1), 0
		for i, variant := range variants {
			c, ok := byID[variant.id]
			if !ok || seenCases[variant.id] || c.PaddingPosition != variant.position || c.MetamorphicFamily != triple.ID ||
				c.BaseSHA != triple.CausalBaseSHA || c.CausalDigest != triple.CausalDigest || c.CausalPatchSHA256 != triple.CausalPatchSHA256 ||
				c.VerificationStateSHA256 != triple.VerificationStateSHA256 || !sha256Hex.MatchString(c.BuildStateSHA256) ||
				!sha256Hex.MatchString(c.CanonicalDiffSHA256) || !uniqueNonemptyStrings(c.PaddingSourceCaseIDs) ||
				c.RawAdditions+c.RawDeletions != triple.ExpectedRawChurn || c.SizeBin != triple.SizeBin ||
				!validCanonicalDiffPosition(c.DiffPosition, variant.position) {
				return fmt.Errorf("triple %s has invalid or incomparable %s variant", triple.ID, variant.position)
			}
			if treeDigests[c.BuildStateSHA256] {
				return fmt.Errorf("triple %s variants do not have deterministic distinct result trees", triple.ID)
			}
			treeDigests[c.BuildStateSHA256] = true
			minBytes = min(minBytes, c.DiffPosition.TotalBytes)
			maxBytes = max(maxBytes, c.DiffPosition.TotalBytes)
			minLines = min(minLines, c.DiffPosition.TotalLines)
			maxLines = max(maxLines, c.DiffPosition.TotalLines)
			if len(c.PaddingApplications) != len(c.PaddingSourceCaseIDs) {
				return fmt.Errorf("triple %s variant %s has incomplete padding applications", triple.ID, c.ID)
			}
			applications := map[string]string{}
			for applicationIndex, application := range c.PaddingApplications {
				if application.SourceCaseID == "" || applications[application.SourceCaseID] != "" || !sha256Hex.MatchString(application.PatchSHA256) ||
					application.SourceCaseID != c.PaddingSourceCaseIDs[applicationIndex] {
					return fmt.Errorf("triple %s variant %s has invalid or noncanonical padding application order", triple.ID, c.ID)
				}
				applications[application.SourceCaseID] = application.PatchSHA256
			}
			for _, sourceID := range c.PaddingSourceCaseIDs {
				if applications[sourceID] == "" {
					return fmt.Errorf("triple %s variant %s has mismatched padding source set", triple.ID, c.ID)
				}
				paddingUnion[sourceID] = true
			}
			if len(c.GroundTruth) != 1 || c.GroundTruth[0].ID != c.ID+":"+triple.GroundTruthID ||
				causalTruthDigest(c.GroundTruth[0]) != triple.CausalDigest {
				return fmt.Errorf("triple %s has noncanonical causal ground truth", triple.ID)
			}
			seenCases[variant.id], variantCases[i] = true, c
			if i > 0 && (c.Repository != variantCases[0].Repository || c.BaseSHA != variantCases[0].BaseSHA ||
				c.DefectCategory != variantCases[0].DefectCategory || c.Language != variantCases[0].Language ||
				c.SourceType != variantCases[0].SourceType || c.StructuredRequired != variantCases[0].StructuredRequired) {
				return fmt.Errorf("triple %s variants differ by more than padding position", triple.ID)
			}
		}
		if maxBytes-minBytes > triple.MaximumByteSizeDelta || maxLines-minLines > triple.MaximumLineSizeDelta {
			return fmt.Errorf("triple %s exceeds precommitted canonical size tolerances", triple.ID)
		}
		if err := validateGitFetchBinding(variantCases[0].Repository, triple.CausalBaseSHA, triple.CausalBaseFetch); err != nil {
			return fmt.Errorf("triple %s causal base fetch: %w", triple.ID, err)
		}
		if err := validateGitFetchBinding(variantCases[0].Repository, triple.CausalHeadSHA, triple.CausalHeadFetch); err != nil {
			return fmt.Errorf("triple %s causal head fetch: %w", triple.ID, err)
		}
		if !sameBoolSet(paddingUnion, stringSet(triple.PaddingSourceCaseIDs)) {
			return fmt.Errorf("triple %s variants do not exactly cover frozen padding sources", triple.ID)
		}
		if err := validateFixedLocations(triple.ID, variantCases[0].GroundTruth[0].Locations, variantCases[1].GroundTruth[0].Locations, variantCases[2].GroundTruth[0].Locations); err != nil {
			return err
		}
		if variantCases[0].PatchSHA256 == variantCases[1].PatchSHA256 || variantCases[0].PatchSHA256 == variantCases[2].PatchSHA256 || variantCases[1].PatchSHA256 == variantCases[2].PatchSHA256 {
			return fmt.Errorf("triple %s variants have duplicate patches", triple.ID)
		}
		for _, paddingID := range triple.PaddingSourceCaseIDs {
			padding, ok := byID[paddingID]
			if !ok || padding.MetamorphicFamily != "" || padding.Repository != variantCases[0].Repository ||
				padding.SourceType != SourceClean || padding.DefectCategory != "clean" ||
				!auditApproved(padding.Audit) || !validVerificationEvidenceSet(padding.CleanProvenance, padding.VerificationStateSHA256) ||
				padding.VerificationStateSHA256 != triple.VerificationStateSHA256 {
				return fmt.Errorf("triple %s padding source %s is not an audited verified portable clean patch", triple.ID, paddingID)
			}
			for _, variant := range variantCases {
				for _, application := range variant.PaddingApplications {
					if application.SourceCaseID == paddingID && application.PatchSHA256 != padding.PatchSHA256 {
						return fmt.Errorf("triple %s padding source %s has a noncanonical patch digest", triple.ID, paddingID)
					}
				}
			}
		}
	}
	for _, c := range cases {
		if c.MetamorphicFamily != "" && !seenCases[c.ID] {
			return fmt.Errorf("metamorphic case %s is not covered exactly once", c.ID)
		}
	}
	return nil
}

func validateFixedLocations(tripleID string, beginning, middle, end []Location) error {
	if len(beginning) == 0 || len(beginning) != len(middle) || len(beginning) != len(end) {
		return fmt.Errorf("triple %s has inconsistent causal locations", tripleID)
	}
	for i := range beginning {
		if beginning[i] != middle[i] || beginning[i] != end[i] {
			return fmt.Errorf("triple %s changes causal source locations instead of canonical diff position", tripleID)
		}
	}
	return nil
}

func CanonicalVerificationStateDigest(provenance *CleanProvenance) (string, error) {
	if provenance == nil || strings.TrimSpace(provenance.Method) == "" || !nonemptyStrings(provenance.EvidenceRefs) || len(provenance.Verifications) == 0 {
		return "", errors.New("verification provenance is incomplete")
	}
	for _, verification := range provenance.Verifications {
		if strings.TrimSpace(verification.Toolchain) == "" || strings.TrimSpace(verification.ToolchainVersion) == "" ||
			!sha256Hex.MatchString(verification.ToolchainSHA256) || strings.TrimSpace(verification.Command) == "" ||
			verification.ExitCode != 0 || !sha256Hex.MatchString(verification.OutputSHA256) || !nonemptyStrings(verification.EvidenceRefs) {
			return "", errors.New("verification evidence is incomplete")
		}
	}
	payload, err := json.Marshal(struct {
		Schema     string                 `json:"schema"`
		Method     string                 `json:"method"`
		Evidence   []string               `json:"evidence"`
		OrderedRun []VerificationEvidence `json:"ordered_verifications"`
	}{
		Schema: "review.eval-verification-state.v1", Method: provenance.Method,
		Evidence: provenance.EvidenceRefs, OrderedRun: provenance.Verifications,
	})
	if err != nil {
		return "", err
	}
	return digestBytes(payload), nil
}

func validVerificationEvidenceSet(provenance *CleanProvenance, expected string) bool {
	digest, err := CanonicalVerificationStateDigest(provenance)
	return err == nil && digest == expected
}
func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func validCanonicalDiffPosition(position CanonicalDiffPosition, wantBin string) bool {
	if position.ByteStart < 0 || position.ByteEnd <= position.ByteStart || position.ByteEnd > position.TotalBytes ||
		position.LineStart < 0 || position.LineEnd <= position.LineStart || position.LineEnd > position.TotalLines ||
		position.Bin != wantBin {
		return false
	}
	byteBin := percentileBin(position.ByteStart+position.ByteEnd, position.TotalBytes)
	lineBin := percentileBin(position.LineStart+position.LineEnd, position.TotalLines)
	return byteBin == wantBin && lineBin == wantBin
}

func percentileBin(doubledMidpoint, total int) string {
	switch {
	case 3*doubledMidpoint < 2*total:
		return "beginning"
	case 3*doubledMidpoint < 4*total:
		return "middle"
	default:
		return "end"
	}
}

func ComputeCanonicalDiffPosition(records []review.RawPathRecord, locations []Location) (CanonicalDiffPosition, error) {
	if len(records) == 0 || len(locations) == 0 {
		return CanonicalDiffPosition{}, errors.New("native canonical diff records and causal locations are required")
	}
	ordered := append([]review.RawPathRecord(nil), records...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].NewPath != ordered[j].NewPath {
			return ordered[i].NewPath < ordered[j].NewPath
		}
		if ordered[i].OldPath != ordered[j].OldPath {
			return ordered[i].OldPath < ordered[j].OldPath
		}
		return ordered[i].Status < ordered[j].Status
	})
	for _, location := range locations {
		if err := validateLocation(location); err != nil {
			return CanonicalDiffPosition{}, err
		}
	}
	type positionedHunk struct {
		record review.RawPathRecord
		hunk   review.RawHunk
		bytes  int
		lines  int
	}
	var hunks []positionedHunk
	totalBytes, totalLines := 0, 0
	for _, record := range ordered {
		for _, hunk := range record.Hunks {
			hunks = append(hunks, positionedHunk{record: record, hunk: hunk, bytes: totalBytes, lines: totalLines})
			totalBytes += len(hunk.Payload)
			totalLines += lineCount(hunk.Payload)
		}
	}
	if totalBytes == 0 || totalLines == 0 {
		return CanonicalDiffPosition{}, errors.New("native canonical diff contains no hunk payload")
	}
	byteStart, byteEnd, lineStart, lineEnd := totalBytes, -1, totalLines, -1
	for _, location := range locations {
		matched := false
		for _, positioned := range hunks {
			oldEnd := int(positioned.hunk.OldStart + positioned.hunk.OldLines)
			newEnd := int(positioned.hunk.NewStart + positioned.hunk.NewLines)
			onOld := positioned.record.OldPath == location.Path && location.StartLine >= int(positioned.hunk.OldStart) && location.EndLine < oldEnd
			onNew := positioned.record.NewPath == location.Path && location.StartLine >= int(positioned.hunk.NewStart) && location.EndLine < newEnd
			if !onOld && !onNew {
				continue
			}
			matched = true
			if positioned.bytes < byteStart {
				byteStart = positioned.bytes
			}
			if end := positioned.bytes + len(positioned.hunk.Payload); end > byteEnd {
				byteEnd = end
			}
			if positioned.lines < lineStart {
				lineStart = positioned.lines
			}
			if end := positioned.lines + lineCount(positioned.hunk.Payload); end > lineEnd {
				lineEnd = end
			}
		}
		if !matched {
			return CanonicalDiffPosition{}, fmt.Errorf("causal location %s:%d-%d is absent from native canonical hunks", location.Path, location.StartLine, location.EndLine)
		}
	}
	position := CanonicalDiffPosition{
		ByteStart: byteStart, ByteEnd: byteEnd, TotalBytes: totalBytes,
		LineStart: lineStart, LineEnd: lineEnd, TotalLines: totalLines,
	}
	position.Bin = percentileBin(position.ByteStart+position.ByteEnd, position.TotalBytes)
	if percentileBin(position.LineStart+position.LineEnd, position.TotalLines) != position.Bin {
		return CanonicalDiffPosition{}, errors.New("canonical byte and line hunk offsets fall in different positional bins")
	}
	return position, nil
}

func uniqueNonemptyStrings(values []string) bool {
	if !nonemptyStrings(values) {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func sameStringSet(left, right []string) bool {
	if !uniqueNonemptyStrings(left) || !uniqueNonemptyStrings(right) || len(left) != len(right) {
		return false
	}
	values := map[string]bool{}
	for _, value := range left {
		values[value] = true
	}
	for _, value := range right {
		if !values[value] {
			return false
		}
	}
	return true
}

func RequiredLargeGates() []string {
	return []string{
		"treatment_completion_100_percent", "receipt_coverage_100_percent", "micro_f1_delta_at_least_10_points",
		"bootstrap_lower_bound_strictly_positive", "treatment_recall_strictly_greater", "cross_file_recall_not_lower",
		"deletion_recall_not_lower", "precision_drop_at_most_5_points", "positional_sensitivity_not_higher_and_at_most_0_10",
		"accepted_locations_100_percent_resolve", "zero_incomplete_or_stale_approvals", "tokens_at_most_105_percent_control",
		"every_client_f1_strictly_greater",
	}
}

func ValidateScoringFreeze(freeze ScoringFreeze, tuning, heldOut, small FrozenCorpus) error {
	if freeze.Schema != ScoringFreezeSchema || freeze.PolicyVersion != EvalPolicyVersion || freeze.ArtifactSchemaVersion != EvalArtifactSchemaVersion {
		return errors.New("scoring freeze has incompatible schema or policy IDs")
	}
	if len(freeze.FreezeBlockers) > 0 {
		return fmt.Errorf("scoring freeze has %d unresolved blockers", len(freeze.FreezeBlockers))
	}
	if !sha256Hex.MatchString(freeze.SourcesManifestSHA256) || freeze.SourcesManifestSHA256 != tuning.SourcesManifestSHA256 ||
		freeze.SourcesManifestSHA256 != heldOut.SourcesManifestSHA256 || freeze.SourcesManifestSHA256 != small.SourcesManifestSHA256 {
		return errors.New("scoring freeze is not bound to the exact source manifest")
	}
	if !sha256Hex.MatchString(freeze.CandidatePoolSHA256) || freeze.CandidatePoolSHA256 != tuning.CandidatePoolSHA256 ||
		freeze.CandidatePoolSHA256 != heldOut.CandidatePoolSHA256 {
		return errors.New("scoring freeze is not bound to the exact candidate pool")
	}
	if len(freeze.Clients) != 2 {
		return errors.New("scoring freeze requires exactly two clients")
	}
	seenClients := map[string]bool{}
	for _, client := range freeze.Clients {
		if (client.ID != "claude" && client.ID != "omp") || seenClients[client.ID] || client.Model == "" || client.ExecutableVersion == "" || !sha256Hex.MatchString(client.ExecutableSHA256) {
			return fmt.Errorf("invalid frozen client %q", client.ID)
		}
		seenClients[client.ID] = true
	}
	if freeze.Toolchain.PolicyID != EvalPolicyVersion || freeze.Toolchain.SchemaID != EvalOutputSchema || freeze.Toolchain.ProwlVersion == "" || !sha256Hex.MatchString(freeze.Toolchain.ProwlSHA256) {
		return errors.New("scoring freeze has invalid toolchain identity")
	}
	if err := validateContentPins("prompts", freeze.Prompts); err != nil {
		return err
	}
	if err := validateContentPin("system policy", freeze.SystemPolicy); err != nil {
		return err
	}
	if err := validateContentPins("formulas", freeze.Formulas); err != nil {
		return err
	}
	if err := validateContentPins("toolchains", freeze.Toolchains); err != nil {
		return err
	}
	if err := validateContentPins("schemas", freeze.Schemas); err != nil {
		return err
	}
	if len(freeze.MetricWeights) == 0 {
		return errors.New("scoring freeze has no pinned metric weights")
	}
	seenWeights := map[string]bool{}
	for _, weight := range freeze.MetricWeights {
		if weight.Weight <= 0 || seenWeights[weight.ID] || validateContentPin("metric weight", ContentPin{ID: weight.ID, Version: weight.Version, SHA256: weight.SHA256}) != nil {
			return fmt.Errorf("invalid frozen metric weight %q", weight.ID)
		}
		seenWeights[weight.ID] = true
	}
	if err := validateTripleReferences(tuning); err != nil {
		return err
	}
	if err := validateTripleReferences(heldOut); err != nil {
		return err
	}

	wantTuning := primaryCaseIDs(tuning.Cases)
	wantHeldOut := primaryCaseIDs(heldOut.Cases)
	wantSmall := allCaseIDs(small.Cases)
	wantTriples := append(tripleIDs(tuning.Triples), tripleIDs(heldOut.Triples)...)
	if len(wantTuning) < 12 || len(wantHeldOut) < 30 {
		return errors.New("scoring freeze requires at least 12 tuning and 30 held-out primary base cases")
	}
	if !equalUniqueStringSets(freeze.Large.TuningBaseCaseIDs, wantTuning) ||
		!equalUniqueStringSets(freeze.Large.HeldOutBaseCaseIDs, wantHeldOut) ||
		!equalUniqueStringSets(freeze.Large.TripleIDs, wantTriples) ||
		!equalUniqueStringSets(freeze.Small.CaseIDs, wantSmall) {
		return errors.New("scoring freeze case or triple IDs do not exactly match frozen corpora")
	}
	large := freeze.Large
	if large.Repetitions != 3 || large.BootstrapReplicates != ProductionBootstrapReplicates || large.ExecutionSeed == 0 || large.BootstrapSeed == 0 {
		return errors.New("scoring freeze has invalid repetitions or seeds")
	}
	if large.MaxModelTokens <= 0 || large.MaxToolCalls <= 0 || large.MaxSubagents <= 0 || large.WallTimeSeconds <= 0 || large.MaxOutputBytes <= 0 || !large.AggregateCaps {
		return errors.New("scoring freeze must apply positive aggregate caps")
	}
	if large.AcceptedFinding == "" || large.Matching == "" || large.Adjudication == "" || large.FailureScoring == "" {
		return errors.New("scoring protocol is incomplete")
	}
	if !equalStringSets(large.Gates, RequiredLargeGates()) {
		return errors.New("scoring freeze does not contain every large shipping gate")
	}
	smallProtocol := freeze.Small
	if smallProtocol.Repetitions != 3 || smallProtocol.TreatmentCompletion != 1 || !smallProtocol.DirectOnly ||
		smallProtocol.MaximumAbsolutePointDrop != .02 || smallProtocol.StructuredRequiredCount != 0 {
		return errors.New("scoring freeze has invalid small non-regression gates")
	}
	return nil
}

func validateContentPins(label string, pins []ContentPin) error {
	if len(pins) == 0 {
		return fmt.Errorf("scoring freeze has no pinned %s", label)
	}
	seen := map[string]bool{}
	for _, pin := range pins {
		if seen[pin.ID] {
			return fmt.Errorf("scoring freeze has duplicate %s id %q", label, pin.ID)
		}
		seen[pin.ID] = true
		if err := validateContentPin(label, pin); err != nil {
			return err
		}
	}
	return nil
}

func validateContentPin(label string, pin ContentPin) error {
	if pin.ID == "" || pin.Version == "" || !sha256Hex.MatchString(pin.SHA256) {
		return fmt.Errorf("scoring freeze has invalid pinned %s %q", label, pin.ID)
	}
	return nil
}

func primaryCaseIDs(cases []PreparedCase) []string {
	ids := []string{}
	for _, c := range cases {
		if c.MetamorphicFamily == "" {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

func allCaseIDs(cases []PreparedCase) []string {
	ids := make([]string, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, c.ID)
	}
	return ids
}

func tripleIDs(triples []PreparedTriple) []string {
	ids := make([]string, 0, len(triples))
	for _, triple := range triples {
		ids = append(ids, triple.ID)
	}
	return ids
}

func equalUniqueStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftSet, rightSet := map[string]bool{}, map[string]bool{}
	for _, value := range left {
		if value == "" || leftSet[value] {
			return false
		}
		leftSet[value] = true
	}
	for _, value := range right {
		if value == "" || rightSet[value] {
			return false
		}
		rightSet[value] = true
	}
	return sameBoolSet(leftSet, rightSet)
}

func validateTripleReferences(corpus FrozenCorpus) error {
	caseIDs := map[string]bool{}
	for _, c := range corpus.Cases {
		if caseIDs[c.ID] {
			return fmt.Errorf("corpus %s has duplicate case id %s", corpus.Set, c.ID)
		}
		caseIDs[c.ID] = true
	}
	tripleIDs := map[string]bool{}
	for _, triple := range corpus.Triples {
		if tripleIDs[triple.ID] {
			return fmt.Errorf("corpus %s has duplicate triple id %s", corpus.Set, triple.ID)
		}
		tripleIDs[triple.ID] = true
		for _, id := range []string{triple.BeginningCaseID, triple.MiddleCaseID, triple.EndCaseID} {
			if !caseIDs[id] {
				return fmt.Errorf("triple %s references absent case %s", triple.ID, id)
			}
		}
	}
	return nil
}

func ValidateFrozenCorpus(corpus FrozenCorpus, minimum int) error {
	if corpus.Schema != CorpusSchema && corpus.Schema != SmallCorpusSchema {
		return errors.New("invalid frozen corpus schema")
	}
	if len(corpus.AuditQueue) > 0 {
		return fmt.Errorf("frozen corpus has %d unresolved audit queue items", len(corpus.AuditQueue))
	}
	if corpus.Set == "small_non_regression" {
		return ValidateSmallCorpus(corpus.Cases, nil)
	}
	if corpus.Set != "tuning" && corpus.Set != "held_out" {
		return errors.New("invalid frozen corpus set")
	}
	if err := ValidateLargeCorpus(corpus.Cases, minimum, true); err != nil {
		return err
	}
	if corpus.Set == "held_out" {
		cleanCount := 0
		for _, c := range corpus.Cases {
			if c.MetamorphicFamily == "" && c.SourceType == SourceClean && auditApproved(c.Audit) {
				cleanCount++
			}
		}
		if cleanCount < 6 {
			return fmt.Errorf("held-out corpus has %d audited clean primary cases; requires at least 6", cleanCount)
		}
	}
	return ValidatePreparedTriples(corpus.Cases, corpus.Triples)
}

func Prepare(ctx context.Context, config PrepareConfig) (PreparationReport, error) {
	sources, err := LoadSources(config.SourcesPath)
	if err != nil {
		return PreparationReport{}, err
	}
	candidatePoolPath := config.CandidatePoolPath
	if candidatePoolPath == "" {
		candidatePoolPath = filepath.Join(filepath.Dir(config.SourcesPath), "candidate_pool.json")
	}
	pool, err := LoadCandidatePool(candidatePoolPath)
	if err != nil {
		return PreparationReport{}, err
	}
	payloadHashes := make([]string, 0, len(sources.Sources))
	sourcePaths := map[string]string{}
	for _, source := range sources.Sources {
		path, err := FetchSource(ctx, source, config.CachePath, config.Offline)
		if err != nil {
			return PreparationReport{}, err
		}
		sourcePaths[source.ID] = path
		payloadHashes = append(payloadHashes, source.ID+"="+source.SHA256)
	}
	sort.Strings(payloadHashes)
	tuning, err := LoadFrozenCorpus(config.TuningPath)
	if err != nil {
		return PreparationReport{}, err
	}
	heldOut, err := LoadFrozenCorpus(config.HeldOutPath)
	if err != nil {
		return PreparationReport{}, err
	}
	small, err := LoadFrozenCorpus(config.SmallPath)
	if err != nil {
		return PreparationReport{}, err
	}
	sourceManifestDigest := CanonicalSourcesDigest(sources)
	if sourceManifestDigest == "" || pool.SourcesManifestSHA256 != sourceManifestDigest ||
		tuning.SourcesManifestSHA256 != sourceManifestDigest || heldOut.SourcesManifestSHA256 != sourceManifestDigest ||
		small.SourcesManifestSHA256 != sourceManifestDigest {
		return PreparationReport{}, errors.New("frozen pool or corpora do not match the canonical sources manifest")
	}
	if err := ValidateCandidatePoolSources(pool, sources, sourcePaths); err != nil {
		return PreparationReport{}, err
	}
	aacrQueue, err := ImportAACRAuditQueue(sourcePaths["aacr-bench"])
	if err != nil {
		return PreparationReport{}, err
	}
	if err := validateAACRPoolRecords(pool, aacrQueue); err != nil {
		return PreparationReport{}, err
	}
	if err := ValidateCorpusPartitions(tuning, heldOut, small, pool); err != nil {
		return PreparationReport{}, err
	}
	swrCandidates, err := ImportSWR(sourcePaths["swr"])
	if err != nil {
		return PreparationReport{}, err
	}
	if err := validateSmallAgainstSWR(small.Cases, swrCandidates); err != nil {
		return PreparationReport{}, err
	}
	repositoryCachePath := config.RepositoryCachePath
	if repositoryCachePath == "" {
		repositoryCachePath = filepath.Join(filepath.Dir(config.CachePath), "repos")
	}
	if err := verifyFrozenCasesGit(ctx, repositoryCachePath, config.Offline, tuning, heldOut, small); err != nil {
		return PreparationReport{}, err
	}
	queueCount := len(tuning.AuditQueue) + len(heldOut.AuditQueue) + len(small.AuditQueue)
	if queueCount > 0 {
		return PreparationReport{SourceCount: len(sources.Sources), TuningCount: len(tuning.Cases), HeldOutCount: len(heldOut.Cases), SmallCount: len(small.Cases), MetamorphicCount: len(tuning.Triples) + len(heldOut.Triples), AuditQueueCount: queueCount, SourcePayloadHash: digestBytes([]byte(strings.Join(payloadHashes, "\n")))}, fmt.Errorf("preparation blocked by %d unresolved audit queue items", queueCount)
	}
	if err := ValidateFrozenCorpus(tuning, 12); err != nil {
		return PreparationReport{}, fmt.Errorf("tuning: %w", err)
	}
	if err := ValidateFrozenCorpus(heldOut, 30); err != nil {
		return PreparationReport{}, fmt.Errorf("held-out: %w", err)
	}
	if err := ValidateSmallCorpus(small.Cases, append(append([]PreparedCase(nil), tuning.Cases...), heldOut.Cases...)); err != nil {
		return PreparationReport{}, fmt.Errorf("small: %w", err)
	}
	freeze, err := LoadScoringFreeze(config.ScoringPath)
	if err != nil {
		return PreparationReport{}, err
	}
	if err := ValidateScoringFreeze(freeze, tuning, heldOut, small); err != nil {
		return PreparationReport{}, err
	}
	return PreparationReport{SourceCount: len(sources.Sources), TuningCount: len(tuning.Cases), HeldOutCount: len(heldOut.Cases), SmallCount: len(small.Cases), MetamorphicCount: len(tuning.Triples) + len(heldOut.Triples), SourcePayloadHash: digestBytes([]byte(strings.Join(payloadHashes, "\n")))}, nil
}

func verifyFrozenCasesGit(ctx context.Context, cacheRoot string, offline bool, corpora ...FrozenCorpus) error {
	specsByRepository := map[string][]GitFetchSpec{}
	for _, corpus := range corpora {
		if err := ValidatePreparedTriples(corpus.Cases, corpus.Triples); err != nil {
			return err
		}
		caseRepository := make(map[string]string, len(corpus.Cases))
		for _, c := range corpus.Cases {
			caseRepository[c.ID] = c.Repository
			specsByRepository[c.Repository] = append(specsByRepository[c.Repository], c.BaseFetch, c.HeadFetch)
		}
		for _, triple := range corpus.Triples {
			repository := caseRepository[triple.BeginningCaseID]
			if repository == "" {
				return fmt.Errorf("metamorphic triple %s has no repository", triple.ID)
			}
			specsByRepository[repository] = append(specsByRepository[repository], triple.CausalBaseFetch, triple.CausalHeadFetch)
		}
	}
	repositories := map[string]string{}
	for repository, specs := range specsByRepository {
		path, err := OpenPinnedRepository(ctx, cacheRoot, repository, offline, specs...)
		if err != nil {
			return err
		}
		repositories[repository] = path
	}
	for _, corpus := range corpora {
		caseRepository := map[string]string{}
		for _, c := range corpus.Cases {
			caseRepository[c.ID] = c.Repository
			if err := VerifyPreparedCaseGit(ctx, repositories[c.Repository], c); err != nil {
				return fmt.Errorf("verify frozen case %s: %w", c.ID, err)
			}
		}
		for _, triple := range corpus.Triples {
			repository := caseRepository[triple.BeginningCaseID]
			if repository == "" {
				return fmt.Errorf("metamorphic triple %s has no repository", triple.ID)
			}
			if err := VerifyPreparedTripleGit(ctx, repositories[repository], corpus.Cases, triple); err != nil {
				return fmt.Errorf("verify metamorphic triple %s: %w", triple.ID, err)
			}
		}
	}
	return nil
}

func validateAACRPoolRecords(pool CandidatePoolManifest, imported []AuditQueueItem) error {
	byID := make(map[string]AuditQueueItem, len(imported))
	for _, item := range imported {
		byID[item.CaseID] = item
	}
	count := 0
	for _, record := range pool.Records {
		if record.CandidateSource != CandidateSourceAACR {
			continue
		}
		count++
		source, ok := byID[record.ID]
		if !ok || !candidateQueueMatchesRecord(source, record) {
			return fmt.Errorf("AACR candidate %s does not match pinned source data", record.ID)
		}
	}
	if count != len(imported) {
		return fmt.Errorf("candidate pool contains %d of %d canonical AACR records", count, len(imported))
	}
	return nil
}

func validateSmallAgainstSWR(cases, imported []PreparedCase) error {
	byRecord := map[string]PreparedCase{}
	for _, c := range imported {
		byRecord[c.SourceRecordID] = c
	}
	for _, c := range cases {
		source, ok := byRecord[c.SourceRecordID]
		if !ok {
			return fmt.Errorf("small case %s is absent from pinned SWR source", c.ID)
		}
		if c.ID != source.ID || c.SourceID != source.SourceID || c.Repository != source.Repository ||
			c.BaseSHA != source.BaseSHA || c.BaseFetch != source.BaseFetch || c.HeadSHA != source.HeadSHA || c.HeadFetch != source.HeadFetch ||
			c.SmallLabel != source.SmallLabel || c.SourceType != source.SourceType || c.DefectCategory != source.DefectCategory ||
			c.Language != source.Language || c.Direct != source.Direct || c.SelectionKey != source.SelectionKey ||
			!equalGroundTruth(c.GroundTruth, source.GroundTruth) || !equalCleanProvenance(c.CleanProvenance, source.CleanProvenance) {
			return fmt.Errorf("small case %s does not match canonical pinned SWR fields", c.ID)
		}
	}
	return nil
}

func equalCleanProvenance(left, right *CleanProvenance) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func equalGroundTruth(left, right []GroundTruth) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func groundTruthDigest(truth GroundTruth) string {
	truth.CanonicalDigest = ""
	payload, err := json.Marshal(truth)
	if err != nil {
		return ""
	}
	return digestBytes(payload)
}

func causalTruthDigest(truth GroundTruth) string {
	truth.ID = ""
	truth.Locations = nil
	truth.CanonicalDigest = ""
	if truth.Provenance != nil {
		provenance := *truth.Provenance
		provenance.SourceRecordID = ""
		truth.Provenance = &provenance
	}
	payload, err := json.Marshal(truth)
	if err != nil {
		return ""
	}
	return digestBytes(payload)
}

func capturePinnedRange(ctx context.Context, repositoryPath, baseSHA, headSHA string) (review.Capture, error) {
	if !fullSHA.MatchString(baseSHA) || !fullSHA.MatchString(headSHA) || baseSHA == headSHA {
		return review.Capture{}, errors.New("native capture requires distinct immutable full SHAs")
	}
	baseOID, err := hex.DecodeString(baseSHA)
	if err != nil {
		return review.Capture{}, err
	}
	headOID, err := hex.DecodeString(headSHA)
	if err != nil {
		return review.Capture{}, err
	}
	scope := review.Scope{
		Kind: review.ScopeRange, ObjectFormat: "sha1",
		Base:    review.SideIdentity{Kind: review.SideGitOID, Value: baseOID},
		Head:    review.SideIdentity{Kind: review.SideGitOID, Value: headOID},
		BaseRef: baseSHA, HeadRef: headSHA,
	}
	capturer := review.Capturer{
		Root:              repositoryPath,
		Runner:            review.ExecGit{Timeout: gitCommandTimeout, MaxStderr: int64(defaultGitOutputLimit)},
		MaxCanonicalBytes: int64(diffGitOutputLimit),
	}
	return capturer.CaptureOnce(ctx, scope)
}

func ComputeGitChurn(ctx context.Context, repositoryPath, baseSHA, headSHA string) (int, int, []ChangedRange, error) {
	capture, err := capturePinnedRange(ctx, repositoryPath, baseSHA, headSHA)
	if err != nil {
		return 0, 0, nil, err
	}
	return capture.RawAdditions, capture.RawDeletions, changedRangesFromNative(capture.Paths), nil
}

func changedRangesFromNative(records []review.RawPathRecord) []ChangedRange {
	var ranges []ChangedRange
	for _, record := range records {
		for _, hunk := range record.Hunks {
			oldLine, newLine := int(hunk.OldStart), int(hunk.NewStart)
			for _, line := range bytes.SplitAfter(hunk.Payload, []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				switch line[0] {
				case ' ':
					oldLine++
					newLine++
				case '-':
					ranges = appendChangedRange(ranges, record.OldPath, oldLine)
					oldLine++
				case '+':
					ranges = appendChangedRange(ranges, record.NewPath, newLine)
					newLine++
				}
			}
		}
	}
	return ranges
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return runGitBounded(ctx, dir, defaultGitOutputLimit, args...)
}

type boundedCommandBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *boundedCommandBuffer) Write(payload []byte) (int, error) {
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return len(payload), nil
	}
	if len(payload) > remaining {
		_, _ = buffer.buffer.Write(payload[:remaining])
		buffer.exceeded = true
		return len(payload), nil
	}
	return buffer.buffer.Write(payload)
}

func runGitBounded(ctx context.Context, dir string, outputLimit int, args ...string) ([]byte, error) {
	return runGitCommand(ctx, dir, outputLimit, nil, nil, 0, args...)
}

func runGitWithInputAndIndex(ctx context.Context, dir string, outputLimit int, input []byte, indexPath string, args ...string) ([]byte, error) {
	if len(input) > diffGitOutputLimit {
		return nil, ErrGitOutputLimit
	}
	return runGitCommand(ctx, dir, outputLimit, input, []string{"GIT_INDEX_FILE=" + indexPath}, 0, args...)
}

func runGitCommand(ctx context.Context, dir string, outputLimit int, input []byte, extraEnvironment []string, acceptedExitCode int, args ...string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("Git command context is required")
	}
	if outputLimit <= 0 {
		return nil, ErrGitOutputLimit
	}
	commandContext, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	safeArgs := append([]string{
		"--no-pager",
		"-c", "core.hooksPath=/dev/null",
		"-c", "credential.helper=",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
	}, args...)
	command := exec.Command("git", safeArgs...)
	command.Dir = dir
	command.Env = append(sanitizedGitEnvironment(), extraEnvironment...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	stdout := &boundedCommandBuffer{limit: outputLimit}
	stderr := &boundedCommandBuffer{limit: defaultGitOutputLimit}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var commandErr error
	select {
	case commandErr = <-done:
	case <-commandContext.Done():
		if command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
		<-done
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), commandContext.Err())
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), ErrGitOutputLimit)
	}
	if commandErr != nil && acceptedExitCode > 0 {
		var exitError *exec.ExitError
		if errors.As(commandErr, &exitError) && exitError.ExitCode() == acceptedExitCode {
			return stdout.buffer.Bytes(), nil
		}
	}
	if commandErr != nil {
		message := strings.TrimSpace(stderr.buffer.String())
		if message != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), commandErr, message)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), commandErr)
	}
	return stdout.buffer.Bytes(), nil
}

func sanitizedGitEnvironment() []string {
	environment := []string{
		"HOME=/nonexistent",
		"XDG_CONFIG_HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_NO_LAZY_FETCH=1",
		"LANG=C",
		"LC_ALL=C",
	}
	if path := os.Getenv("PATH"); path != "" {
		environment = append(environment, "PATH="+path)
	}
	return environment
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	count := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		count++
	}
	return count
}

type aacrRow struct {
	Language string `json:"project_main_language"`
	PRURL    string `json:"pr_url"`
	HeadSHA  string `json:"pr_source_commit"`
	BaseSHA  string `json:"pr_target_commit"`
	Churn    int    `json:"pr_change_line_count"`
	Label    int    `json:"label"`
}

func ImportAACRSourceRows(path, license, revision string) ([]CandidateSourceRow, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rawRows []json.RawMessage
	if err := json.Unmarshal(payload, &rawRows); err != nil {
		return nil, fmt.Errorf("AACR JSON: %w", err)
	}
	byIdentity := map[string]CandidateSourceRow{}
	for _, raw := range rawRows {
		var row aacrRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("AACR row: %w", err)
		}
		if row.Label != 1 || row.Churn <= 300 || !fullSHA.MatchString(row.BaseSHA) || !fullSHA.MatchString(row.HeadSHA) || row.BaseSHA == row.HeadSHA {
			continue
		}
		parsed, err := url.Parse(row.PRURL)
		if err != nil {
			continue
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if parsed.Scheme != "https" || parsed.Host != "github.com" || len(parts) != 4 || parts[2] != "pull" {
			continue
		}
		language := row.Language
		if language == "TypeScript" || language == "JavaScript" {
			language = "TypeScript/JavaScript"
		}
		repository := parts[0] + "/" + parts[1]
		evidence := []string{row.PRURL, "aacr-revision:" + revision}
		byIdentity[immutableIdentity(repository, row.BaseSHA, row.HeadSHA)] = CandidateSourceRow{
			CandidateSource: CandidateSourceAACR, SourceID: "aacr-bench", SourceRecordID: row.PRURL,
			Repository: repository, PullRequestURL: row.PRURL, BaseSHA: row.BaseSHA, HeadSHA: row.HeadSHA,
			License: license, Provenance: revision, Language: language, SourceReportedChurn: row.Churn,
			EvidenceRefs: evidence, RawRecordSHA256: digestBytes(bytes.TrimSpace(raw)),
		}
	}
	rows := make([]CandidateSourceRow, 0, len(byIdentity))
	for _, row := range byIdentity {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		left := digestBytes([]byte(immutableIdentity(rows[i].Repository, rows[i].BaseSHA, rows[i].HeadSHA)))
		right := digestBytes([]byte(immutableIdentity(rows[j].Repository, rows[j].BaseSHA, rows[j].HeadSHA)))
		if left != right {
			return left < right
		}
		return rows[i].SourceRecordID < rows[j].SourceRecordID
	})
	return rows, nil
}

// ImportAACRAuditQueue extracts source-verified immutable candidate identities.
func ImportAACRAuditQueue(path string) ([]AuditQueueItem, error) {
	rows, err := ImportAACRSourceRows(path, "Apache-2.0", "47be1d6df1e7faf222cf531587772d92f79fe6b2")
	if err != nil {
		return nil, err
	}
	queue := make([]AuditQueueItem, 0, len(rows))
	for _, row := range rows {
		parsed, _ := url.Parse(row.PullRequestURL)
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		key := digestBytes([]byte(immutableIdentity(row.Repository, row.BaseSHA, row.HeadSHA)))
		rowDigest, err := CanonicalCandidateSourceRowDigest(row)
		if err != nil {
			return nil, err
		}
		remoteURL := "https://github.com/" + row.Repository + ".git"
		queue = append(queue, AuditQueueItem{
			CaseID:          "aacr-" + strings.ToLower(strings.ReplaceAll(row.Repository, "/", "-")) + "-" + parts[3],
			CandidateSource: row.CandidateSource, SourceID: row.SourceID, SourceRecordID: row.SourceRecordID,
			SourceType: SourceHumanCaught, Repository: row.Repository, BaseSHA: row.BaseSHA, HeadSHA: row.HeadSHA,
			SelectionKey: key, Language: row.Language, SourceReportedChurn: row.SourceReportedChurn,
			SourceRowSHA256: rowDigest, EvidenceRefs: append([]string(nil), row.EvidenceRefs...),
			BaseFetch: GitFetchSpec{RemoteURL: remoteURL, Ref: row.BaseSHA, OID: row.BaseSHA},
			HeadFetch: GitFetchSpec{RemoteURL: remoteURL, Ref: "refs/pull/" + parts[3] + "/head", OID: row.HeadSHA},
			Reasons: []string{
				"exact ThresholdTextV1 churn not yet recomputed from the pinned repository",
				"ground-truth causal path and range not yet validated against the pinned BaseSHA..HeadSHA diff",
				"two evidenced independent reviewer decisions and any required evidenced approved adjudication are not recorded",
			},
		})
	}
	return queue, nil
}

type swrRecord struct {
	Repo             string      `json:"repo"`
	InstanceID       string      `json:"instance_id"`
	ChangeIntroduced bool        `json:"change_introduced"`
	BaseCommit       string      `json:"base_commit"`
	PRCommits        []swrCommit `json:"pr_commits"`
	Changes          []swrChange `json:"changes"`
}

type swrCommit struct {
	SHA      string    `json:"sha"`
	DiffText string    `json:"diff_text"`
	Diff     []swrDiff `json:"diff"`
}

type swrDiff struct {
	File  string `json:"file"`
	Patch string `json:"patch"`
}

type swrChange struct {
	ChangeType        string `json:"change_type"`
	ChangeIntroducing struct {
		CodeSnippet string `json:"code_snippet"`
		CommitSHA   string `json:"commit_sha"`
	} `json:"change_introducing"`
	ChangeDiscussion struct {
		Summary               string `json:"discussion_summary"`
		FirstMentionTimestamp string `json:"first_mention_timestamp"`
		ReviewerComment       string `json:"original_reviewer_comment"`
	} `json:"change_discussion"`
	ChangeResolveInfo *struct {
		CodeSnippet string `json:"code_snippet"`
		CommitSHA   string `json:"commit_sha"`
		Explanation string `json:"resolution_explanation"`
	} `json:"change_resolve_info"`
	CanonicalSource json.RawMessage `json:"-"`
}

func (change *swrChange) UnmarshalJSON(data []byte) error {
	type sourceChange swrChange
	var decoded sourceChange
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var canonical any
	if err := json.Unmarshal(data, &canonical); err != nil {
		return err
	}
	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return err
	}
	*change = swrChange(decoded)
	change.CanonicalSource = canonicalBytes
	return nil
}

// ImportSWR parses the official SWR JSONL into factual candidates. Git-derived
// churn, patch, ranges, and tree hashes are intentionally left for HydrateGitCase.
func ImportSWR(path string) ([]PreparedCase, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	cases := []PreparedCase{}
	line := 0
recordLoop:
	for scanner.Scan() {
		line++
		var record swrRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("SWR line %d: %w", line, err)
		}
		if record.InstanceID == "" || record.Repo == "" || !fullSHA.MatchString(record.BaseCommit) || len(record.PRCommits) == 0 {
			return nil, fmt.Errorf("SWR line %d has incomplete identity", line)
		}
		base, head := record.BaseCommit, record.PRCommits[len(record.PRCommits)-1].SHA
		selectedChanges := record.Changes
		if record.ChangeIntroduced {
			base, head, selectedChanges = swrIntroducingRange(record)
			if head == "" || len(selectedChanges) == 0 {
				continue
			}
		}
		if !fullSHA.MatchString(base) || !fullSHA.MatchString(head) || head == base {
			return nil, fmt.Errorf("SWR line %d has invalid immutable range", line)
		}
		remoteURL := "https://github.com/" + record.Repo + ".git"
		c := PreparedCase{
			ID:             "swr-" + strings.ReplaceAll(record.InstanceID, "__", "-"),
			SourceID:       "swr",
			SourceRecordID: record.InstanceID,
			Repository:     record.Repo,
			BaseSHA:        base,
			BaseFetch:      GitFetchSpec{RemoteURL: remoteURL, Ref: base, OID: base},
			HeadSHA:        head,
			HeadFetch:      GitFetchSpec{RemoteURL: remoteURL, Ref: head, OID: head},
			Language:       "Python",
			Direct:         true,
			SelectionKey:   digestBytes([]byte(record.Repo + "\x00" + base + "\x00" + head)),
		}
		if record.ChangeIntroduced {
			c.SmallLabel, c.SourceType, c.DefectCategory = SmallChangePR, SourceHumanCaught, swrDefectCategory(selectedChanges)
			for index, change := range selectedChanges {
				location, err := swrLocation(change.ChangeIntroducing.CodeSnippet)
				if err != nil {
					continue recordLoop
				}
				summary := strings.TrimSpace(change.ChangeDiscussion.Summary)
				if summary == "" {
					summary = strings.TrimSpace(change.ChangeDiscussion.ReviewerComment)
				}
				if summary == "" {
					continue recordLoop
				}
				scenario := strings.TrimSpace(change.ChangeDiscussion.ReviewerComment)
				if change.ChangeResolveInfo != nil && strings.TrimSpace(change.ChangeResolveInfo.Explanation) != "" {
					scenario = strings.TrimSpace(change.ChangeResolveInfo.Explanation)
				}
				if scenario == "" {
					scenario = summary
				}
				scenario = strings.ReplaceAll(scenario, "generated"+" by", "emitted by")
				if len(change.CanonicalSource) == 0 {
					return nil, errors.New("SWR change lacks canonical source payload")
				}
				evidence := []string{change.ChangeIntroducing.CommitSHA}
				if change.ChangeResolveInfo != nil && fullSHA.MatchString(change.ChangeResolveInfo.CommitSHA) {
					evidence = append(evidence, change.ChangeResolveInfo.CommitSHA)
				}
				truth := GroundTruth{
					ID:          c.ID + "-truth-" + strconv.Itoa(index+1),
					DefectClass: c.DefectCategory,
					Severity:    "unspecified_by_source",
					Summary:     summary,
					Scenario:    scenario,
					Verifier:    "official_swr_reviewer_confirmed",
					Provenance: &GroundTruthProvenance{
						SourceID:       "swr",
						SourceRecordID: record.InstanceID,
						SourceDigest:   digestBytes(change.CanonicalSource),
						EvidenceRefs:   evidence,
					},
					Locations: []Location{location},
				}
				truth.CanonicalDigest = groundTruthDigest(truth)
				c.GroundTruth = append(c.GroundTruth, truth)
			}
			if len(c.GroundTruth) == 0 {
				continue
			}
		} else {
			c.SmallLabel, c.SourceType, c.DefectCategory = SmallCleanPR, SourceClean, "clean"
			c.CleanProvenance = &CleanProvenance{
				Method:       "official_swr_clean_pr_label",
				EvidenceRefs: []string{"swr:" + record.InstanceID},
			}
		}
		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return cases, nil
}

func swrIntroducingRange(record swrRecord) (string, string, []swrChange) {
	for _, change := range record.Changes {
		introducing := change.ChangeIntroducing.CommitSHA
		if !fullSHA.MatchString(introducing) {
			continue
		}
		previous := record.BaseCommit
		for _, commit := range record.PRCommits {
			if commit.SHA == introducing {
				selected := []swrChange{}
				for _, candidate := range record.Changes {
					if candidate.ChangeIntroducing.CommitSHA == introducing {
						selected = append(selected, candidate)
					}
				}
				return previous, introducing, selected
			}
			previous = commit.SHA
		}
	}
	return "", "", nil
}

func swrDefectCategory(changes []swrChange) string {
	for _, change := range changes {
		value := strings.ToLower(change.ChangeType + " " + change.ChangeDiscussion.Summary)
		switch {
		case strings.Contains(value, "interface"):
			return "cross_file_contract"
		case strings.Contains(value, "integration"):
			return "producer_consumer"
		case strings.Contains(value, "dependency"):
			return "dependency"
		case strings.Contains(value, "test"):
			return "test_gap"
		case strings.Contains(value, "config"):
			return "configuration_migration"
		}
	}
	return "local_logic"
}

func swrLocation(snippet string) (Location, error) {
	lines := strings.Split(snippet, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) == "" {
		return Location{}, errors.New("missing code-snippet path")
	}
	path := strings.TrimSpace(lines[0])
	oldLine, newLine := 0, 0
	inHunk := false
	var deleted *Location
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "@@") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			var err error
			oldLine, err = parsePatchRangeStart(fields[1], '-')
			if err != nil {
				continue
			}
			newLine, err = parsePatchRangeStart(fields[2], '+')
			if err != nil {
				continue
			}
			inHunk = true
			continue
		}
		if !inHunk || line == "" {
			continue
		}
		switch line[0] {
		case ' ':
			oldLine++
			newLine++
		case '-':
			if deleted == nil {
				location := Location{Path: path, StartLine: oldLine, EndLine: oldLine}
				deleted = &location
			}
			oldLine++
		case '+':
			return Location{Path: path, StartLine: newLine, EndLine: newLine}, nil
		}
	}
	if deleted != nil {
		return *deleted, nil
	}
	return Location{}, errors.New("missing changed line in code-snippet hunk")
}

// HydrateGitCase replaces source-reported size facts with exact immutable Git facts.
func HydrateGitCase(ctx context.Context, repositoryPath string, c PreparedCase) (PreparedCase, error) {
	capture, err := capturePinnedRange(ctx, repositoryPath, c.BaseSHA, c.HeadSHA)
	if err != nil {
		return PreparedCase{}, err
	}
	tree, err := runGit(ctx, repositoryPath, "rev-parse", c.HeadSHA+"^{tree}")
	if err != nil {
		return PreparedCase{}, err
	}
	c.RawAdditions, c.RawDeletions = capture.RawAdditions, capture.RawDeletions
	c.ChangedRanges = changedRangesFromNative(capture.Paths)
	churn := capture.RawChurn
	c.StructuredRequired = churn > 300
	switch {
	case churn <= 300:
		c.SizeBin = SizeSmall
	case churn <= 1000:
		c.SizeBin = Size301To1000
	case churn <= 3000:
		c.SizeBin = Size1001To3000
	default:
		c.SizeBin = SizeOver3000
	}
	canonicalPatch := review.CanonicalPatchV1(capture.Paths)
	c.PatchSHA256 = hex.EncodeToString(capture.CanonicalPatch[:])
	c.CanonicalDiffSHA256 = digestBytes(canonicalPatch)
	c.BuildStateSHA256 = digestBytes(bytes.TrimSpace(tree))
	if c.MetamorphicFamily != "" {
		if len(c.GroundTruth) != 1 {
			return PreparedCase{}, errors.New("metamorphic Git hydration requires one causal ground truth")
		}
		position, positionErr := ComputeCanonicalDiffPosition(capture.Paths, c.GroundTruth[0].Locations)
		if positionErr != nil {
			return PreparedCase{}, positionErr
		}
		c.DiffPosition = position
	}
	return c, nil
}

func VerifyPreparedCaseGit(ctx context.Context, repositoryPath string, frozen PreparedCase) error {
	hydrated, err := HydrateGitCase(ctx, repositoryPath, frozen)
	if err != nil {
		return err
	}
	if frozen.RawAdditions != hydrated.RawAdditions || frozen.RawDeletions != hydrated.RawDeletions ||
		frozen.SizeBin != hydrated.SizeBin || frozen.StructuredRequired != hydrated.StructuredRequired ||
		frozen.PatchSHA256 != hydrated.PatchSHA256 || frozen.BuildStateSHA256 != hydrated.BuildStateSHA256 ||
		!equalChangedRanges(frozen.ChangedRanges, hydrated.ChangedRanges) ||
		frozen.MetamorphicFamily != "" && (frozen.CanonicalDiffSHA256 != hydrated.CanonicalDiffSHA256 || frozen.DiffPosition != hydrated.DiffPosition) {
		return fmt.Errorf("case %s frozen Git facts do not match pinned repository", frozen.ID)
	}
	return nil
}
func VerifyPreparedTripleGit(ctx context.Context, repositoryPath string, cases []PreparedCase, triple PreparedTriple) error {
	wanted := stringSet(append([]string{
		triple.BeginningCaseID, triple.MiddleCaseID, triple.EndCaseID,
	}, triple.PaddingSourceCaseIDs...))
	relevant := make([]PreparedCase, 0, len(wanted))
	byID := make(map[string]PreparedCase, len(cases))
	for _, c := range cases {
		if _, duplicate := byID[c.ID]; duplicate {
			return fmt.Errorf("duplicate case id %s", c.ID)
		}
		byID[c.ID] = c
		if wanted[c.ID] {
			relevant = append(relevant, c)
		}
	}
	if err := ValidatePreparedTriples(relevant, []PreparedTriple{triple}); err != nil {
		return err
	}
	for _, revision := range []string{triple.CausalBaseSHA, triple.CausalHeadSHA} {
		resolved, err := runGit(ctx, repositoryPath, "rev-parse", "--verify", revision+"^{commit}")
		if err != nil || strings.TrimSpace(string(resolved)) != revision {
			return fmt.Errorf("triple %s repository does not contain causal commit %s", triple.ID, revision)
		}
	}
	causalPatch, err := canonicalGitPatch(ctx, repositoryPath, triple.CausalBaseSHA, triple.CausalHeadSHA)
	if err != nil {
		return err
	}
	if digestBytes(causalPatch) != triple.CausalPatchSHA256 {
		return fmt.Errorf("triple %s causal patch digest does not match pinned commits", triple.ID)
	}
	paddingPatches := make(map[string][]byte, len(triple.PaddingSourceCaseIDs))
	for _, sourceID := range triple.PaddingSourceCaseIDs {
		source := byID[sourceID]
		canonicalPatch, patchErr := canonicalGitPatch(ctx, repositoryPath, source.BaseSHA, source.HeadSHA)
		if patchErr != nil {
			return patchErr
		}
		if digestBytes(canonicalPatch) != source.PatchSHA256 {
			return fmt.Errorf("triple %s padding source %s patch digest does not match pinned commits", triple.ID, sourceID)
		}
		patch, patchErr := portableGitPatch(ctx, repositoryPath, source.BaseSHA, source.HeadSHA)
		if patchErr != nil {
			return patchErr
		}
		if _, applyErr := applyPatchesToTree(ctx, repositoryPath, triple.CausalBaseSHA, [][]byte{patch}); applyErr != nil {
			return fmt.Errorf("triple %s padding source %s does not apply to causal base: %w", triple.ID, sourceID, applyErr)
		}
		if _, applyErr := applyPatchesToTree(ctx, repositoryPath, triple.CausalHeadSHA, [][]byte{patch}); applyErr != nil {
			return fmt.Errorf("triple %s padding source %s does not apply after the causal patch: %w", triple.ID, sourceID, applyErr)
		}
		paddingPatches[sourceID] = patch
	}
	for _, variantID := range []string{triple.BeginningCaseID, triple.MiddleCaseID, triple.EndCaseID} {
		variant := byID[variantID]
		patches := make([][]byte, 0, len(variant.PaddingApplications))
		for _, application := range variant.PaddingApplications {
			patches = append(patches, paddingPatches[application.SourceCaseID])
		}
		resultTree, applyErr := applyPatchesToTree(ctx, repositoryPath, triple.CausalHeadSHA, patches)
		if applyErr != nil {
			return fmt.Errorf("triple %s variant %s cannot be reproduced from causal and padding patches: %w", triple.ID, variantID, applyErr)
		}
		if resultTree != variant.BuildStateSHA256 {
			return fmt.Errorf("triple %s variant %s result tree does not match portable patch composition", triple.ID, variantID)
		}
	}
	return nil
}

func canonicalGitPatch(ctx context.Context, repositoryPath, baseSHA, headSHA string) ([]byte, error) {
	capture, err := capturePinnedRange(ctx, repositoryPath, baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	return review.CanonicalPatchV1(capture.Paths), nil
}

func portableGitPatch(ctx context.Context, repositoryPath, baseSHA, headSHA string) ([]byte, error) {
	capture, err := capturePinnedRange(ctx, repositoryPath, baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	var patch bytes.Buffer
	for _, record := range capture.Paths {
		if record.TextClass != string(review.TextClassText) || strings.HasPrefix(record.Status, "R") || strings.HasPrefix(record.Status, "C") {
			return nil, fmt.Errorf("portable padding requires non-renamed ThresholdTextV1 text paths")
		}
		oldPath, newPath := "a/"+record.OldPath, "b/"+record.NewPath
		if record.OldPath == "" {
			oldPath = "/dev/null"
		}
		if record.NewPath == "" {
			newPath = "/dev/null"
		}
		fmt.Fprintf(&patch, "diff --git %s %s\n--- %s\n+++ %s\n",
			quoteGitPatchPath("a/"+record.OldPath), quoteGitPatchPath("b/"+record.NewPath),
			quoteGitPatchPath(oldPath), quoteGitPatchPath(newPath))
		for _, hunk := range record.Hunks {
			fmt.Fprintf(&patch, "@@ -%d,%d +%d,%d @@\n", hunk.OldStart, hunk.OldLines, hunk.NewStart, hunk.NewLines)
			patch.Write(hunk.Payload)
		}
	}
	if patch.Len() > diffGitOutputLimit {
		return nil, ErrGitOutputLimit
	}
	return patch.Bytes(), nil
}

func quoteGitPatchPath(path string) string {
	if path == "/dev/null" || !strings.ContainsAny(path, " \t\n\\\"") {
		return path
	}
	return strconv.Quote(path)
}

func applyPatchesToTree(ctx context.Context, repositoryPath, treeish string, patches [][]byte) (string, error) {
	indexFile, err := os.CreateTemp("", "review-eval-index-")
	if err != nil {
		return "", err
	}
	indexPath := indexFile.Name()
	if closeErr := indexFile.Close(); closeErr != nil {
		os.Remove(indexPath)
		return "", closeErr
	}
	if err := os.Remove(indexPath); err != nil {
		return "", err
	}
	defer os.Remove(indexPath)
	if _, err := runGitWithInputAndIndex(ctx, repositoryPath, defaultGitOutputLimit, nil, indexPath, "read-tree", treeish); err != nil {
		return "", err
	}
	for _, patch := range patches {
		if _, err := runGitWithInputAndIndex(ctx, repositoryPath, defaultGitOutputLimit, patch, indexPath,
			"apply", "--cached", "--check", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return "", err
		}
		if _, err := runGitWithInputAndIndex(ctx, repositoryPath, defaultGitOutputLimit, patch, indexPath,
			"apply", "--cached", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return "", err
		}
	}
	tree, err := runGitWithInputAndIndex(ctx, repositoryPath, defaultGitOutputLimit, nil, indexPath, "write-tree")
	if err != nil {
		return "", err
	}
	return digestBytes(bytes.TrimSpace(tree)), nil
}

func equalChangedRanges(left, right []ChangedRange) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func safeRepositorySegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func OpenPinnedRepository(ctx context.Context, cacheRoot, repository string, offline bool, fetchSpecs ...GitFetchSpec) (string, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || !safeRepositorySegment(parts[0]) || !safeRepositorySegment(parts[1]) {
		return "", fmt.Errorf("unsafe repository identity %q", repository)
	}
	if cacheRoot == "" {
		return "", errors.New("repository cache path is required")
	}
	path := filepath.Join(cacheRoot, digestBytes([]byte(strings.ToLower(repository)))[:16])
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		legacyPath := filepath.Join(cacheRoot, parts[1])
		if legacyInfo, legacyErr := os.Lstat(legacyPath); legacyErr == nil && legacyInfo.IsDir() && legacyInfo.Mode()&os.ModeSymlink == 0 {
			path, info, statErr = legacyPath, legacyInfo, nil
		}
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if offline {
			return "", fmt.Errorf("offline pinned repository %s is missing", repository)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", err
		}
		if _, err := runGit(ctx, path, "init", "--bare", "."); err != nil {
			return "", err
		}
		info, statErr = os.Lstat(path)
	}
	if statErr != nil {
		return "", statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("repository cache entry for %s is not a real directory", repository)
	}
	if offline {
		return path, nil
	}
	if len(fetchSpecs) == 0 {
		return "", fmt.Errorf("online preparation for %s requires explicit immutable fetch metadata", repository)
	}
	for _, spec := range fetchSpecs {
		if err := ValidateGitFetchSpec(spec); err != nil {
			return "", err
		}
		remotePath := strings.TrimSuffix(strings.TrimPrefix(spec.RemoteURL, "https://github.com/"), ".git")
		if !strings.EqualFold(remotePath, repository) {
			return "", fmt.Errorf("fetch remote %s does not match repository %s", spec.RemoteURL, repository)
		}
		if _, err := runGit(ctx, path, "fetch", "--no-tags", "--force", "--depth=1", spec.RemoteURL, spec.Ref); err != nil {
			return "", err
		}
		fetched, err := runGit(ctx, path, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
		if err != nil || strings.TrimSpace(string(fetched)) != spec.OID {
			return "", fmt.Errorf("fetch %s did not produce exact pinned OID %s", spec.Ref, spec.OID)
		}
	}
	return path, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func missingValues(seen map[string]bool, required []string) []string {
	missing := []string{}
	for _, value := range required {
		if !seen[value] {
			missing = append(missing, value)
		}
	}
	return missing
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := map[string]int{}
	for _, value := range left {
		values[value]++
	}
	for _, value := range right {
		values[value]--
	}
	for _, count := range values {
		if count != 0 {
			return false
		}
	}
	return true
}

func parsePositiveInt(value, label string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", label)
	}
	return parsed, nil
}
