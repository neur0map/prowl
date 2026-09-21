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
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/neur0map/prowl/internal/agenttrial"
	"github.com/neur0map/prowl/internal/boundedio"
	"github.com/neur0map/prowl/internal/review"
)

func BuildOrder(cases []Case, clients []string, repetitions int, seed uint64) ([]TrialSpec, error) {
	if len(cases) == 0 || len(clients) == 0 || repetitions < 1 {
		return nil, errors.New("trial matrix requires cases, clients, and positive repetitions")
	}
	seenCases, seenClients := map[string]bool{}, map[string]bool{}
	for _, c := range cases {
		if c.ID == "" || seenCases[c.ID] {
			return nil, fmt.Errorf("invalid or duplicate case id %q", c.ID)
		}
		seenCases[c.ID] = true
	}
	for _, client := range clients {
		if client != "claude" && client != "omp" || seenClients[client] {
			return nil, fmt.Errorf("invalid or duplicate client %q", client)
		}
		seenClients[client] = true
	}
	order := make([]TrialSpec, 0, len(cases)*len(clients)*2*repetitions)
	for _, c := range cases {
		for _, client := range clients {
			for _, condition := range []string{ConditionControl, ConditionTreatment} {
				for repetition := 1; repetition <= repetitions; repetition++ {
					order = append(order, TrialSpec{CaseID: c.ID, Client: client, Condition: condition, Repetition: repetition})
				}
			}
		}
	}
	rng := rand.New(rand.NewPCG(seed, seed^0xd1b54a32d192ed03))
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	for index := range order {
		order[index].Ordinal = index
	}
	return order, nil
}

func Collect(ctx context.Context, cfg RunConfig, manifest Manifest, scoring ScoringConfig) (TrialCollection, error) {
	cfg, cases, order, err := prepareEvaluation(cfg, manifest, scoring)
	if err != nil {
		return TrialCollection{}, err
	}
	toolchain, err := preflightToolchain(ctx, &cfg, scoring)
	if err != nil {
		return TrialCollection{}, err
	}
	preparedRoot := cfg.PreparedRoot
	if preparedRoot == "" {
		preparedRoot = "."
	}
	for _, c := range cases {
		if err := verifyPreparedCase(ctx, preparedRoot, c); err != nil {
			return TrialCollection{}, err
		}
	}
	outputRoot, err := prepareOutputRoot(cfg.OutputDir)
	if err != nil {
		return TrialCollection{}, err
	}
	if err := writeJSON(filepath.Join(outputRoot, "order.json"), order); err != nil {
		return TrialCollection{}, err
	}
	if err := writeJSON(filepath.Join(outputRoot, "manifest.json"), manifest); err != nil {
		return TrialCollection{}, err
	}
	if err := writeJSON(filepath.Join(outputRoot, "scoring-config.json"), scoring); err != nil {
		return TrialCollection{}, err
	}
	if err := writeJSON(filepath.Join(outputRoot, "toolchain.json"), toolchain); err != nil {
		return TrialCollection{}, err
	}
	caseByID := map[string]Case{}
	for _, c := range cases {
		caseByID[c.ID] = c
	}
	launcher := cfg.Launcher
	if launcher == nil {
		launcher = agenttrial.Run
	}
	records := make([]TrialRecord, 0, len(order))
	for _, spec := range order {
		trialRoot, err := os.MkdirTemp("", "prowl-review-eval-")
		if err != nil {
			return TrialCollection{}, err
		}
		record := runOne(ctx, cfg, scoring, launcher, toolchain, preparedRoot, trialRoot, spec, caseByID[spec.CaseID])
		records = append(records, record)
		artifactDir := filepath.Join(outputRoot, "trials", fmt.Sprintf("%04d-%s-%s-%s-%02d", spec.Ordinal, spec.CaseID, spec.Client, spec.Condition, spec.Repetition))
		persistErr := persistTrial(artifactDir, record)
		removeErr := os.RemoveAll(trialRoot)
		if persistErr != nil {
			return TrialCollection{}, persistErr
		}
		if removeErr != nil {
			return TrialCollection{}, removeErr
		}
	}
	blindInput := BuildBlindAdjudicationInput(records, cases)
	candidateDigest, err := jsonDigest(blindInput)
	if err != nil {
		return TrialCollection{}, err
	}
	manifestDigest, err := jsonDigest(manifest)
	if err != nil {
		return TrialCollection{}, err
	}
	scoringDigest, err := jsonDigest(scoring)
	if err != nil {
		return TrialCollection{}, err
	}
	collection := TrialCollection{
		Schema:              CollectionSchema,
		ManifestDigest:      manifestDigest,
		ScoringConfigDigest: scoringDigest,
		CandidateDigest:     candidateDigest,
		Set:                 cfg.Set,
		Order:               order,
		Trials:              records,
		BlindInput:          blindInput,
		Toolchain:           toolchain,
	}
	if err := writeJSON(filepath.Join(outputRoot, "adjudication-input.json"), blindInput); err != nil {
		return TrialCollection{}, err
	}
	if err := writeJSON(filepath.Join(outputRoot, "collection.json"), collection); err != nil {
		return TrialCollection{}, err
	}
	return collection, nil
}

func ScoreCollection(cfg RunConfig, manifest Manifest, scoring ScoringConfig, collection TrialCollection, adjudication Adjudication) (RunReport, error) {
	cfg, cases, order, err := prepareEvaluation(cfg, manifest, scoring)
	if err != nil {
		return RunReport{}, err
	}
	if err := validateCollection(collection, cfg.Set, manifest, scoring, cases, order, cfg.Production); err != nil {
		return RunReport{}, err
	}
	if err := ValidateAdjudication(adjudication, collection.BlindInput); err != nil {
		return RunReport{}, err
	}
	if adjudication.CandidateDigest != collection.CandidateDigest {
		return RunReport{}, errors.New("adjudication candidate digest does not match retained collection")
	}
	caseByID := make(map[string]Case, len(cases))
	caseTruth := make(map[string][]GroundTruth, len(cases))
	caseMeta := make(map[string]bool, len(cases))
	for _, c := range cases {
		caseByID[c.ID] = c
		caseTruth[c.ID] = c.GroundTruth
		caseMeta[c.ID] = c.Metamorphic
	}
	scores := make([]TrialScore, 0, len(collection.Trials))
	for _, record := range collection.Trials {
		score := ScoreTrial(record, caseTruth[record.CaseID], adjudication.Edges)
		score.Metamorphic = caseMeta[record.CaseID]
		score.DefectClass = caseByID[record.CaseID].DefectClass
		scores = append(scores, score)
	}
	metrics := Aggregate(scores)
	bootstrap, err := Bootstrap(scores, scoring.Seed, scoring.BootstrapReplicates, cfg.Production)
	if err != nil {
		return RunReport{}, err
	}
	metamorphic := ScoreMetamorphic(scores, selectedTriples(manifest.MetamorphicTriples, scoring.MetamorphicTripleIDs))
	gates := EvaluateShippingGates(metrics, scores, metamorphic, bootstrap, scoring)
	report := RunReport{Schema: "review.eval-report.v1", Order: collection.Order, Trials: collection.Trials, Scores: scores, Metrics: metrics, Bootstrap: bootstrap, Metamorphic: metamorphic, Gates: gates}
	outputRoot, err := prepareOutputRoot(cfg.OutputDir)
	if err != nil {
		return RunReport{}, err
	}
	if err := writeJSON(filepath.Join(outputRoot, "adjudication.json"), adjudication); err != nil {
		return RunReport{}, err
	}
	if err := writeJSON(filepath.Join(outputRoot, "report.json"), report); err != nil {
		return RunReport{}, err
	}
	return report, nil
}

func prepareEvaluation(cfg RunConfig, manifest Manifest, scoring ScoringConfig) (RunConfig, []Case, []TrialSpec, error) {
	if cfg.Set == "" {
		cfg.Set = "tuning"
	}
	cfg.Production = cfg.Production || cfg.Set == "held_out"
	if err := ValidateManifest(manifest); err != nil {
		return RunConfig{}, nil, nil, err
	}
	if err := ValidateScoringConfig(scoring, manifest, cfg.Production); err != nil {
		return RunConfig{}, nil, nil, err
	}
	if cfg.OutputDir == "" {
		return RunConfig{}, nil, nil, errors.New("output directory is required")
	}
	if err := validateRelative(cfg.OutputDir, "output"); err != nil {
		return RunConfig{}, nil, nil, err
	}
	if cfg.Repetitions == 0 {
		cfg.Repetitions = scoring.Repetitions
	}
	if cfg.Repetitions != scoring.Repetitions {
		return RunConfig{}, nil, nil, errors.New("runtime repetitions differ from frozen scoring config")
	}
	if cfg.Model != "" && cfg.Model != scoring.Model {
		return RunConfig{}, nil, nil, errors.New("runtime model differs from frozen scoring config")
	}
	cfg.Model = scoring.Model
	if cfg.Timeout > 0 && cfg.Timeout != scoring.Budget.Timeout {
		return RunConfig{}, nil, nil, errors.New("runtime timeout differs from frozen scoring config")
	}
	if len(cfg.Clients) == 0 {
		cfg.Clients = append([]string(nil), scoring.Clients...)
	}
	if !orderedStringsEqual(cfg.Clients, scoring.Clients) {
		return RunConfig{}, nil, nil, errors.New("runtime clients differ in value or order from frozen scoring clients")
	}
	var available []Case
	switch cfg.Set {
	case "tuning":
		available = manifest.Tuning
	case "held_out":
		available = manifest.HeldOut
	default:
		return RunConfig{}, nil, nil, fmt.Errorf("unknown evaluation set %q", cfg.Set)
	}
	cases, err := selectRunCases(available, manifest.MetamorphicTriples, scoring)
	if err != nil {
		return RunConfig{}, nil, nil, err
	}
	order, err := BuildOrder(cases, scoring.Clients, cfg.Repetitions, scoring.Seed)
	if err != nil {
		return RunConfig{}, nil, nil, err
	}
	return cfg, cases, order, nil
}

func runOne(ctx context.Context, cfg RunConfig, scoring ScoringConfig, launcher TrialLauncher, toolchain ToolchainIdentity, preparedRoot, trialRoot string, spec TrialSpec, c Case) TrialRecord {
	record := TrialRecord{Ordinal: spec.Ordinal, CaseID: spec.CaseID, Client: spec.Client, Condition: spec.Condition, Repetition: spec.Repetition, Status: StatusFailed}
	repository := filepath.Join(trialRoot, "repo")
	source := filepath.Join(preparedRoot, filepath.FromSlash(c.Repository))
	if err := copyTree(source, repository); err != nil {
		record.Error = "copy prepared repository: " + err.Error()
		return record
	}
	clientRoot := filepath.Join(trialRoot, "client")
	if err := os.MkdirAll(clientRoot, 0o700); err != nil {
		record.Error = err.Error()
		return record
	}
	environment := fixedTrialEnvironment(os.Environ(), filepath.Join(clientRoot, "bin"))
	clientCfg := agenttrial.ClientConfig{
		Client:              spec.Client,
		Model:               cfg.Model,
		ClaudeBinary:        cfg.ClaudeBinary,
		OMPBinary:           cfg.OMPBinary,
		MaxOutputBytes:      scoring.MaxOutputBytes,
		DisableAmbientRules: true,
		Budget:              scoring.Budget,
		ConfigDir:           clientRoot,
		Environment:         environment,
		RequireTokenUsage:   cfg.Production,
	}
	if spec.Condition == ConditionTreatment {
		installed, err := installTreatment(clientRoot, cfg.ReviewSkill, cfg.ProwlBinary, spec.Client)
		if err != nil {
			record.Error = err.Error()
			return record
		}
		if installed.Skill != "" {
			clientCfg.Skills = []string{installed.Skill}
		}
		if installed.PluginDir != "" {
			clientCfg.PluginDirs = []string{installed.PluginDir}
		}
	} else if err := installControlGuard(clientRoot); err != nil {
		record.Error = err.Error()
		return record
	}
	prompt := c.Prompt
	if prompt == "" {
		prompt = "Review the change from " + c.BaseSHA + " to " + c.HeadSHA + ". Emit only a review.eval-output.v1 JSON object."
	}
	result, runErr := launcher(ctx, repository, prompt, clientCfg)
	record.RawStdout, record.RawStderr = string(result.Stdout), string(result.Stderr)
	record.Usage, record.ElapsedMS = result.Usage, result.Elapsed.Milliseconds()
	clientIdentity := toolchain.OMP
	if spec.Client == "claude" {
		clientIdentity = toolchain.Claude
	}
	record.ClientVersion = clientIdentity.Version
	record.ModelVersion = clientCfg.Model
	if spec.Condition == ConditionTreatment {
		record.ProwlVersion = toolchain.Prowl.Version
	}
	if runErr != nil {
		record.Error = runErr.Error()
		return record
	}
	if cfg.Production && !result.UsageReported {
		record.Error = agenttrial.ErrMissingUsage.Error()
		return record
	}
	payload, err := evalPayload(result.Stdout)
	if err != nil {
		record.Error = err.Error()
		return record
	}
	var output EvalOutput
	if err := decodeStrict(payload, &output); err != nil {
		record.Error = err.Error()
		return record
	}
	CanonicalizeFindingIDs(&output)
	record.Output, record.Recommendation = &output, output.Recommendation
	if err := ValidateEvalOutput(output, spec.Condition == ConditionTreatment); err != nil {
		record.Error = err.Error()
		return record
	}
	if output.Status != StatusCompleted {
		record.Error = "client reported failed trial"
		return record
	}
	record.ResolvedLocations = resolveLocations(repository, output.Findings)
	if spec.Condition == ConditionTreatment {
		evidence, planData, reportData, checkData, err := validateCheckerArtifacts(repository, *output.Checker, c, scoring.MaxOutputBytes, filepath.Join(clientRoot, "bin", "prowl"), toolchain.Prowl.SHA256)
		record.ProwlPlan, record.ProwlReport, record.CheckResult, record.Checker = planData, reportData, checkData, evidence
		if err != nil {
			record.Error = err.Error()
			return record
		}
		if err := validateEvalReportBinding(output, reportData); err != nil {
			record.Error = err.Error()
			return record
		}
		if evidence.Status != "complete" {
			record.Error = "persisted checker result is not complete"
			return record
		}
	}
	record.Status = StatusCompleted
	return record
}

type treatmentInstall struct {
	Skill       string
	PluginDir   string
	ProwlBinary string
}

func installTreatment(clientRoot, skillSource, prowlSource, client string) (treatmentInstall, error) {
	if skillSource == "" {
		return treatmentInstall{}, errors.New("treatment review skill is required")
	}
	binaryRoot := filepath.Join(clientRoot, "bin")
	if err := os.MkdirAll(binaryRoot, 0o700); err != nil {
		return treatmentInstall{}, err
	}
	privateProwl := filepath.Join(binaryRoot, "prowl")
	if err := copyRegular(prowlSource, privateProwl, 0o700); err != nil {
		return treatmentInstall{}, fmt.Errorf("install prowl binary: %w", err)
	}
	switch client {
	case "omp":
		skillRoot := filepath.Join(clientRoot, "skills", "prowl-pr-review")
		if err := copyTree(skillSource, skillRoot); err != nil {
			return treatmentInstall{}, fmt.Errorf("install review skill: %w", err)
		}
		return treatmentInstall{Skill: "prowl-pr-review", ProwlBinary: privateProwl}, nil
	case "claude":
		pluginRoot := filepath.Join(clientRoot, "plugins", "prowl-pr-review")
		skillRoot := filepath.Join(pluginRoot, "skills", "prowl-pr-review")
		if err := copyTree(skillSource, skillRoot); err != nil {
			return treatmentInstall{}, fmt.Errorf("install review skill plugin: %w", err)
		}
		manifestRoot := filepath.Join(pluginRoot, ".claude-plugin")
		if err := os.MkdirAll(manifestRoot, 0o700); err != nil {
			return treatmentInstall{}, err
		}
		pluginManifest := struct {
			Name        string `json:"name"`
			Version     string `json:"version"`
			Description string `json:"description"`
		}{"prowl-review-eval", "1.0.0", "Private Prowl review evaluation plugin"}
		if err := writeJSON(filepath.Join(manifestRoot, "plugin.json"), pluginManifest); err != nil {
			return treatmentInstall{}, err
		}
		return treatmentInstall{PluginDir: pluginRoot, ProwlBinary: privateProwl}, nil
	default:
		return treatmentInstall{}, fmt.Errorf("unsupported treatment client %q", client)
	}
}

func installControlGuard(clientRoot string) error {
	binaryRoot := filepath.Join(clientRoot, "bin")
	if err := os.MkdirAll(binaryRoot, 0o700); err != nil {
		return err
	}
	const unavailable = "#!/bin/sh\nprintf '%s\\n' 'prowl is unavailable in the control condition' >&2\nexit 127\n"
	if err := os.WriteFile(filepath.Join(binaryRoot, "prowl"), []byte(unavailable), 0o700); err != nil {
		return fmt.Errorf("disable control Prowl binary: %w", err)
	}
	return nil
}

func evalPayload(stdout []byte) ([]byte, error) {
	var candidate []byte
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	maxToken := len(stdout) + 1
	if maxToken < 4096 {
		maxToken = 4096
	}
	scanner.Buffer(make([]byte, 4096), maxToken)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		var direct EvalOutput
		if decodeStrict(line, &direct) == nil && direct.Schema == EvalOutputSchema {
			candidate = append(candidate[:0], line...)
			continue
		}
		var event any
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		for _, text := range eventTexts(event) {
			trimmed := bytes.TrimSpace([]byte(text))
			if decodeStrict(trimmed, &direct) == nil && direct.Schema == EvalOutputSchema {
				candidate = append(candidate[:0], trimmed...)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(candidate) == 0 {
		return nil, errors.New("client stream contains no review.eval-output.v1 object")
	}
	return candidate, nil
}

func eventTexts(value any) []string {
	var out []string
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, child := range v {
				walk(child)
			}
		case map[string]any:
			for _, key := range []string{"result", "final", "text", "content", "message"} {
				if text, ok := v[key].(string); ok {
					out = append(out, stripFence(text))
				}
			}
			for key, child := range v {
				if key != "usage" {
					walk(child)
				}
			}
		}
	}
	walk(value)
	return out
}

func stripFence(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "```") {
		if newline := strings.IndexByte(value, '\n'); newline >= 0 {
			value = value[newline+1:]
		}
		value = strings.TrimSuffix(strings.TrimSpace(value), "```")
	}
	return strings.TrimSpace(value)
}

func resolveLocations(root string, findings []Finding) int {
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return 0
	}
	defer rootHandle.Close()
	resolved := 0
	for _, finding := range findings {
		if !acceptedDisposition(finding.VerifierDisposition) {
			continue
		}
		for _, location := range finding.Locations {
			file, err := boundedio.OpenRegular(rootHandle, filepath.FromSlash(location.Path))
			if err != nil {
				continue
			}
			ok := hasLine(file, location.EndLine)
			_ = file.Close()
			if ok {
				resolved++
			}
		}
	}
	return resolved
}

func hasLine(file *os.File, target int) bool {
	if target < 1 {
		return false
	}
	lines := 0
	sawBytes := false
	var last byte
	buffer := make([]byte, 32<<10)
	for {
		count, err := file.Read(buffer)
		if count > 0 {
			sawBytes = true
			last = buffer[count-1]
			for _, value := range buffer[:count] {
				if value == '\n' {
					lines++
				}
			}
			if lines >= target {
				return true
			}
		}
		if errors.Is(err, io.EOF) {
			if sawBytes && last != '\n' {
				lines++
			}
			return lines >= target
		}
		if err != nil {
			return false
		}
	}
}

func persistTrial(dir string, record TrialRecord) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.jsonl"), []byte(record.RawStdout), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr.txt"), []byte(record.RawStderr), 0o644); err != nil {
		return err
	}
	if record.Output != nil {
		if err := writeJSON(filepath.Join(dir, "normalized.json"), record.Output); err != nil {
			return err
		}
	}
	if len(record.ProwlPlan) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "prowl-plan.json"), append(append([]byte(nil), record.ProwlPlan...), '\n'), 0o644); err != nil {
			return err
		}
	}
	if len(record.ProwlReport) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "prowl-report.json"), append(append([]byte(nil), record.ProwlReport...), '\n'), 0o644); err != nil {
			return err
		}
	}
	if len(record.CheckResult) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "check-result.json"), append(append([]byte(nil), record.CheckResult...), '\n'), 0o644); err != nil {
			return err
		}
	}
	if record.Checker != nil {
		if err := writeJSON(filepath.Join(dir, "checker-evidence.json"), record.Checker); err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(dir, "usage.json"), record.Usage); err != nil {
		return err
	}
	versions := struct {
		Client string `json:"client"`
		Model  string `json:"model"`
		Prowl  string `json:"prowl,omitempty"`
	}{record.ClientVersion, record.ModelVersion, record.ProwlVersion}
	if err := writeJSON(filepath.Join(dir, "versions.json"), versions); err != nil {
		return err
	}
	if record.Error != "" {
		if err := os.WriteFile(filepath.Join(dir, "error.txt"), []byte(record.Error+"\n"), 0o644); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(dir, "trial.json"), record)
}

func readRelativeArtifact(root, relative string, max int64) json.RawMessage {
	if relative == "" || validateRelative(relative, "artifact") != nil {
		return nil
	}
	if max <= 0 {
		max = 16 << 20
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer rootHandle.Close()
	file, err := boundedio.OpenRegular(rootHandle, filepath.FromSlash(relative))
	if err != nil {
		return nil
	}
	defer file.Close()
	data, err := boundedio.ReadAllContext(context.Background(), file, max)
	if err != nil || !json.Valid(data) {
		return nil
	}
	return data
}

func validateCheckerArtifacts(repository string, refs CheckerArtifacts, c Case, maxBytes int64, prowlBinary, expectedProwlSHA string) (*CheckerEvidence, json.RawMessage, json.RawMessage, json.RawMessage, error) {
	planData := readRelativeArtifact(repository, refs.PlanPath, maxBytes)
	reportData := readRelativeArtifact(repository, refs.ReportPath, maxBytes)
	checkData := readRelativeArtifact(repository, refs.CheckPath, maxBytes)
	if len(planData) == 0 || len(reportData) == 0 || len(checkData) == 0 {
		return nil, planData, reportData, checkData, errors.New("checker plan, report, or result artifact is missing or invalid")
	}
	var plan review.Plan
	if err := decodeStrict(planData, &plan); err != nil {
		return nil, planData, reportData, checkData, fmt.Errorf("decode review plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return nil, planData, reportData, checkData, err
	}
	storeContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	store, err := review.OpenPlanStore(storeContext, review.ExecGit{Binary: "git", Timeout: 30 * time.Second}, repository)
	if err != nil {
		cancel()
		return nil, planData, reportData, checkData, fmt.Errorf("open persisted review plan: %w", err)
	}
	storedArtifacts, err := store.Load(storeContext, refs.ReviewID)
	cancel()
	if err != nil {
		return nil, planData, reportData, checkData, fmt.Errorf("load persisted review plan: %w", err)
	}
	artifactPlanDigest, err := jsonDigest(plan)
	if err != nil {
		return nil, planData, reportData, checkData, err
	}
	storedPlanDigest, err := jsonDigest(storedArtifacts.Plan)
	if err != nil {
		return nil, planData, reportData, checkData, err
	}
	if artifactPlanDigest != storedPlanDigest {
		return nil, planData, reportData, checkData, errors.New("plan artifact differs from Prowl's verified persisted plan")
	}
	var report review.Report
	if err := decodeStrict(reportData, &report); err != nil {
		return nil, planData, reportData, checkData, fmt.Errorf("decode review report: %w", err)
	}
	if err := report.Validate(); err != nil {
		return nil, planData, reportData, checkData, err
	}
	var check review.CheckResult
	if err := decodeStrict(checkData, &check); err != nil {
		return nil, planData, reportData, checkData, fmt.Errorf("decode review check result: %w", err)
	}
	if err := check.Validate(); err != nil {
		return nil, planData, reportData, checkData, err
	}
	if refs.ReviewID != plan.ReviewID || report.ReviewID != plan.ReviewID || check.ReviewID != plan.ReviewID {
		return nil, planData, reportData, checkData, errors.New("checker artifacts disagree on review ID")
	}
	if report.PlanDigest != plan.PlanDigest {
		return nil, planData, reportData, checkData, errors.New("review report is not bound to the stored plan digest")
	}
	if !sameReviewIdentity(report.Base, plan.Scope.Base) || !sameReviewIdentity(report.Head, plan.Scope.Head) || check.Mode != plan.Mode || check.Recommendation != report.Recommendation {
		return nil, planData, reportData, checkData, errors.New("checker artifacts disagree on scope, mode, or recommendation")
	}
	if !sameGitSide(plan.Scope.Base, c.BaseSHA) || !sameGitSide(plan.Scope.Head, c.HeadSHA) {
		return nil, planData, reportData, checkData, errors.New("stored plan is not bound to the evaluation base/head")
	}
	if plan.Stats.RawAdditions != c.RawAdditions || plan.Stats.RawDeletions != c.RawDeletions || plan.Stats.RawChurn != c.RawAdditions+c.RawDeletions {
		return nil, planData, reportData, checkData, errors.New("stored plan raw churn differs from the evaluation manifest")
	}
	if c.RawAdditions+c.RawDeletions > 300 && (plan.Mode != review.ModeStructured || !plan.StructuredRequired) {
		return nil, planData, reportData, checkData, errors.New("large review plan is not structured")
	}
	primaryCovered, primaryTotal, auditCovered, auditTotal, err := deriveReceiptCoverage(plan, report)
	if err != nil {
		return nil, planData, reportData, checkData, err
	}
	coverageComplete := primaryCovered == primaryTotal && auditCovered == auditTotal
	if plan.Mode == review.ModeStructured && (check.Coverage == review.CoverageComplete) != coverageComplete {
		return nil, planData, reportData, checkData, errors.New("checker coverage disagrees with stored plan/report receipts")
	}
	actualProwlSHA, err := fileSHA256(prowlBinary)
	if err != nil {
		return nil, planData, reportData, checkData, err
	}
	if actualProwlSHA != expectedProwlSHA {
		return nil, planData, reportData, checkData, errors.New("private Prowl executable changed after preflight")
	}
	freshCheck, err := invokeReviewCheck(repository, prowlBinary, refs)
	if err != nil {
		return nil, planData, reportData, checkData, err
	}
	storedDigest, _ := jsonDigest(check)
	freshDigest, _ := jsonDigest(freshCheck)
	if storedDigest != freshDigest {
		return nil, planData, reportData, checkData, errors.New("stored checker result differs from an independent Prowl check")
	}
	evidence := &CheckerEvidence{
		Status:               string(freshCheck.Status),
		ReviewID:             plan.ReviewID,
		PlanDigest:           plan.PlanDigest,
		Recommendation:       report.Recommendation,
		PrimaryRangesCovered: primaryCovered,
		PrimaryRangesTotal:   primaryTotal,
		AuditTargetsCovered:  auditCovered,
		AuditTargetsTotal:    auditTotal,
		Stale:                freshCheck.Status == review.CheckStale,
		Incomplete:           freshCheck.Status == review.CheckIncomplete,
		Invalid:              freshCheck.Status == review.CheckInvalid,
	}
	return evidence, planData, reportData, checkData, nil
}

func validateEvalReportBinding(output EvalOutput, reportData json.RawMessage) error {
	var report review.Report
	if err := decodeStrict(reportData, &report); err != nil {
		return err
	}
	if output.Recommendation != report.Recommendation || len(output.Findings) != len(report.Findings) {
		return errors.New("eval output differs from the canonical review report")
	}
	for index, finding := range output.Findings {
		native := report.Findings[index]
		verifierEvidence, err := canonicalVerifierEvidence(native.VerifierEvidence)
		if err != nil {
			return fmt.Errorf("derive eval finding %d verifier evidence: %w", index, err)
		}
		if finding.HostID != native.ID || finding.Category != native.Category || finding.Severity != native.Severity ||
			finding.Confidence != native.Confidence || finding.Summary != native.Summary || finding.Scenario != native.Scenario ||
			finding.VerifierDisposition != native.Verifier || finding.VerifierEvidence != verifierEvidence || len(finding.Locations) != len(native.Locations) {
			return fmt.Errorf("eval finding %d differs from the canonical review report", index)
		}
		for locationIndex, location := range finding.Locations {
			nativeLocation := native.Locations[locationIndex]
			if nativeLocation.Kind != string(review.LocationRange) || location.Path != nativeLocation.Path || location.StartLine != nativeLocation.Start || location.EndLine != nativeLocation.End {
				return fmt.Errorf("eval finding %d location %d differs from the canonical review report", index, locationIndex)
			}
		}
	}
	return nil
}

func canonicalVerifierEvidence(citations []review.Citation) (string, error) {
	if len(citations) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(citations)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func sameReviewIdentity(left, right review.SideIdentity) bool {
	return left.Kind == right.Kind && bytes.Equal(left.Value, right.Value)
}

func sameGitSide(side review.SideIdentity, sha string) bool {
	return side.Kind == review.SideGitOID && hex.EncodeToString(side.Value) == sha
}

func deriveReceiptCoverage(plan review.Plan, report review.Report) (int, int, int, int, error) {
	if plan.Mode == review.ModeDirect {
		if len(report.PrimaryReceipts) != 0 || len(report.AuditReceipts) != 0 {
			return 0, 0, 0, 0, errors.New("direct review carries structured receipts")
		}
		return 0, 0, 0, 0, nil
	}
	units := make(map[string]int, len(plan.PrimaryUnits))
	primaryTotal := 0
	for _, unit := range plan.PrimaryUnits {
		units[unit.UnitID] = len(unit.Hunks)
		primaryTotal += len(unit.Hunks)
	}
	seenUnits := map[string]bool{}
	primaryCovered := 0
	for _, receipt := range report.PrimaryReceipts {
		expected, ok := units[receipt.UnitID]
		if !ok || seenUnits[receipt.UnitID] {
			return 0, 0, 0, 0, fmt.Errorf("invalid or duplicate primary receipt %q", receipt.UnitID)
		}
		seenUnits[receipt.UnitID] = true
		if hasDuplicateOrEmpty(receipt.AcknowledgedPrimaryHunkIDs) {
			return 0, 0, 0, 0, fmt.Errorf("primary receipt %s has duplicate or empty hunk IDs", receipt.UnitID)
		}
		if len(receipt.AcknowledgedPrimaryHunkIDs) <= expected {
			primaryCovered += len(receipt.AcknowledgedPrimaryHunkIDs)
		}
	}
	audits := make(map[string][]string, len(plan.RequiredAudits))
	auditTotal := 0
	for _, audit := range plan.RequiredAudits {
		audits[audit.AuditID] = audit.TargetIDs
		auditTotal += len(audit.TargetIDs)
	}
	seenAudits := map[string]bool{}
	auditCovered := 0
	for _, receipt := range report.AuditReceipts {
		targets, ok := audits[receipt.AuditID]
		if !ok || seenAudits[receipt.AuditID] {
			return 0, 0, 0, 0, fmt.Errorf("invalid or duplicate audit receipt %q", receipt.AuditID)
		}
		seenAudits[receipt.AuditID] = true
		if hasDuplicateOrEmpty(receipt.AcknowledgedAuditTargetIDs) {
			return 0, 0, 0, 0, fmt.Errorf("audit receipt %s has duplicate or empty target IDs", receipt.AuditID)
		}
		expected := make(map[string]bool, len(targets))
		for _, target := range targets {
			expected[target] = true
		}
		for _, target := range receipt.AcknowledgedAuditTargetIDs {
			if !expected[target] {
				return 0, 0, 0, 0, fmt.Errorf("audit receipt %s acknowledges foreign target %s", receipt.AuditID, target)
			}
			auditCovered++
		}
	}
	return primaryCovered, primaryTotal, auditCovered, auditTotal, nil
}

func hasDuplicateOrEmpty(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func invokeReviewCheck(repository, prowlBinary string, refs CheckerArtifacts) (review.CheckResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, stderr := &boundedBuffer{limit: 1 << 20}, &boundedBuffer{limit: 64 << 10}
	command := exec.CommandContext(ctx, prowlBinary, "review", "check", "--review", refs.ReviewID, "--report", refs.ReportPath, "--format", "json")
	command.Dir, command.Stdout, command.Stderr = repository, stdout, stderr
	runErr := command.Run()
	if stdout.overflow || stderr.overflow {
		return review.CheckResult{}, errors.New("independent Prowl check output exceeded limit")
	}
	var check review.CheckResult
	if err := decodeStrict(stdout.data, &check); err != nil {
		return review.CheckResult{}, fmt.Errorf("decode independent Prowl check: %w: %s", err, strings.TrimSpace(string(stderr.data)))
	}
	if err := check.Validate(); err != nil {
		return review.CheckResult{}, err
	}
	if check.Status == review.CheckComplete && runErr != nil {
		return review.CheckResult{}, fmt.Errorf("complete independent Prowl check failed: %w", runErr)
	}
	if check.Status != review.CheckComplete && runErr == nil {
		return review.CheckResult{}, errors.New("non-complete independent Prowl check exited successfully")
	}
	return check, nil
}

func copyTree(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("prepared repository %s is not a directory", source)
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			mode := os.FileMode(0o755)
			if details, statErr := entry.Info(); statErr == nil {
				mode = details.Mode().Perm()
			}
			return os.MkdirAll(target, mode)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		details, err := entry.Info()
		if err != nil {
			return err
		}
		if !details.Mode().IsRegular() {
			return nil
		}
		return copyRegular(path, target, details.Mode().Perm())
	})
}

func copyRegular(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Close())
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func fixedTrialEnvironment(environment []string, privateBin string) []string {
	const systemPath = "/usr/local/bin:/usr/bin:/bin"
	value := privateBin + string(os.PathListSeparator) + systemPath
	for index, entry := range environment {
		if strings.HasPrefix(entry, "PATH=") {
			copy := append([]string(nil), environment...)
			copy[index] = "PATH=" + value
			return copy
		}
	}
	return append(append([]string(nil), environment...), "PATH="+value)
}

type boundedBuffer struct {
	mu       sync.Mutex
	data     []byte
	limit    int
	overflow bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		buffer.data = append(buffer.data, data[:remaining]...)
	}
	if remaining < len(data) {
		buffer.overflow = true
	}
	return len(data), nil
}

func resolveExecutable(parent context.Context, label, requested string) (ExecutableIdentity, error) {
	resolved, err := exec.LookPath(requested)
	if err != nil {
		return ExecutableIdentity{}, fmt.Errorf("resolve %s executable %q: %w", label, requested, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return ExecutableIdentity{}, err
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return ExecutableIdentity{}, fmt.Errorf("resolve %s executable symlinks: %w", label, err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return ExecutableIdentity{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ExecutableIdentity{}, fmt.Errorf("%s executable is not an executable regular file", label)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return ExecutableIdentity{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return ExecutableIdentity{}, err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	versionOutput := &boundedBuffer{limit: 4096}
	command := exec.CommandContext(ctx, resolved, "--version")
	command.Stdout, command.Stderr = versionOutput, versionOutput
	if err := command.Run(); err != nil {
		return ExecutableIdentity{}, fmt.Errorf("%s --version: %w", label, err)
	}
	if versionOutput.overflow {
		return ExecutableIdentity{}, fmt.Errorf("%s --version output exceeds 4096 bytes", label)
	}
	version := strings.TrimSpace(string(versionOutput.data))
	if version == "" {
		return ExecutableIdentity{}, fmt.Errorf("%s --version returned empty output", label)
	}
	return ExecutableIdentity{Path: resolved, Version: version, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func preflightToolchain(ctx context.Context, cfg *RunConfig, scoring ScoringConfig) (ToolchainIdentity, error) {
	if cfg.ProwlBinary == "" {
		cfg.ProwlBinary = "prowl"
	}
	if cfg.ClaudeBinary == "" {
		cfg.ClaudeBinary = "claude"
	}
	if cfg.OMPBinary == "" {
		cfg.OMPBinary = "omp"
	}
	var identity ToolchainIdentity
	var err error
	identity.Prowl, err = resolveExecutable(ctx, "prowl", cfg.ProwlBinary)
	if err != nil {
		return ToolchainIdentity{}, err
	}
	cfg.ProwlBinary = identity.Prowl.Path
	for _, client := range scoring.Clients {
		switch client {
		case "claude":
			identity.Claude, err = resolveExecutable(ctx, "claude", cfg.ClaudeBinary)
			cfg.ClaudeBinary = identity.Claude.Path
		case "omp":
			identity.OMP, err = resolveExecutable(ctx, "omp", cfg.OMPBinary)
			cfg.OMPBinary = identity.OMP.Path
		}
		if err != nil {
			return ToolchainIdentity{}, err
		}
	}
	if cfg.Production {
		for name, pair := range map[string]struct {
			got ExecutableIdentity
			pin ExecutablePin
		}{
			"claude": {identity.Claude, scoring.Executables.Claude},
			"omp":    {identity.OMP, scoring.Executables.OMP},
			"prowl":  {identity.Prowl, scoring.Executables.Prowl},
		} {
			if pair.got.Version != pair.pin.Version || pair.got.SHA256 != pair.pin.SHA256 {
				return ToolchainIdentity{}, fmt.Errorf("%s executable differs from frozen version/hash pin", name)
			}
		}
	}
	return identity, nil
}

func orderedStringsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func jsonDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func validateCollection(collection TrialCollection, set string, manifest Manifest, scoring ScoringConfig, cases []Case, order []TrialSpec, production bool) error {
	if collection.Schema != CollectionSchema {
		return fmt.Errorf("collection schema must be %q", CollectionSchema)
	}
	manifestDigest, err := jsonDigest(manifest)
	if err != nil {
		return err
	}
	if collection.ManifestDigest != manifestDigest {
		return errors.New("collection manifest digest mismatch")
	}
	scoringDigest, err := jsonDigest(scoring)
	if err != nil {
		return err
	}
	if collection.ScoringConfigDigest != scoringDigest {
		return errors.New("collection scoring-config digest mismatch")
	}
	if collection.Set != set {
		return errors.New("collection evaluation set mismatch")
	}
	expectedOrderDigest, err := jsonDigest(order)
	if err != nil {
		return err
	}
	orderDigest, err := jsonDigest(collection.Order)
	if err != nil {
		return err
	}
	if orderDigest != expectedOrderDigest || len(collection.Trials) != len(order) {
		return errors.New("collection trial order does not match frozen schedule")
	}
	for index, spec := range order {
		record := collection.Trials[index]
		if record.Ordinal != spec.Ordinal || record.CaseID != spec.CaseID || record.Client != spec.Client || record.Condition != spec.Condition || record.Repetition != spec.Repetition {
			return fmt.Errorf("collection trial %d does not match frozen schedule", index)
		}
	}
	blindInput := BuildBlindAdjudicationInput(collection.Trials, cases)
	blindDigest, err := jsonDigest(blindInput)
	if err != nil {
		return err
	}
	storedBlindDigest, err := jsonDigest(collection.BlindInput)
	if err != nil {
		return err
	}
	if blindDigest != storedBlindDigest || collection.CandidateDigest != blindDigest {
		return errors.New("collection blind-candidate input does not match retained trials")
	}
	if production {
		for name, pair := range map[string]struct {
			got ExecutableIdentity
			pin ExecutablePin
		}{
			"claude": {collection.Toolchain.Claude, scoring.Executables.Claude},
			"omp":    {collection.Toolchain.OMP, scoring.Executables.OMP},
			"prowl":  {collection.Toolchain.Prowl, scoring.Executables.Prowl},
		} {
			if pair.got.Version != pair.pin.Version || pair.got.SHA256 != pair.pin.SHA256 {
				return fmt.Errorf("collection %s executable identity differs from frozen pin", name)
			}
		}
	}
	return nil
}
func selectedTriples(all []MetamorphicTriple, ids []string) []MetamorphicTriple {
	if len(ids) == 0 {
		return nil
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var out []MetamorphicTriple
	for _, triple := range all {
		if wanted[triple.ID] {
			out = append(out, triple)
		}
	}
	return out
}

func selectRunCases(available []Case, triples []MetamorphicTriple, scoring ScoringConfig) ([]Case, error) {
	wanted := map[string]bool{}
	for _, id := range scoring.BaseCaseIDs {
		wanted[id] = true
	}
	for _, triple := range selectedTriples(triples, scoring.MetamorphicTripleIDs) {
		wanted[triple.BeginningCaseID] = true
		wanted[triple.MiddleCaseID] = true
		wanted[triple.EndCaseID] = true
	}
	selected := make([]Case, 0, len(wanted))
	for _, c := range available {
		if wanted[c.ID] {
			selected = append(selected, c)
			delete(wanted, c.ID)
		}
	}
	if len(wanted) != 0 {
		missing := make([]string, 0, len(wanted))
		for id := range wanted {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("scoring config references cases outside selected set: %s", strings.Join(missing, ", "))
	}
	if len(selected) == 0 {
		return nil, errors.New("selected evaluation set is empty")
	}
	return selected, nil
}

func prepareOutputRoot(relative string) (string, error) {
	if err := validateRelative(relative, "output"); err != nil {
		return "", err
	}
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for _, component := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("output component %s is not a real directory", current)
		}
	}
	return current, nil
}

func verifyPreparedCase(parent context.Context, preparedRoot string, c Case) error {
	repository := filepath.Join(preparedRoot, filepath.FromSlash(c.Repository))
	info, err := os.Lstat(repository)
	if err != nil {
		return fmt.Errorf("case %s prepared repository: %w", c.ID, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("case %s prepared repository is not a real directory", c.ID)
	}
	head, err := preflightGitOutput(parent, repository, 4096, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("case %s resolve checked-out HEAD: %w", c.ID, err)
	}
	if strings.TrimSpace(string(head)) != c.HeadSHA {
		return fmt.Errorf("case %s prepared repository HEAD does not equal pinned head %s", c.ID, c.HeadSHA)
	}
	status, err := preflightGitOutput(parent, repository, 1<<20, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("case %s inspect prepared worktree: %w", c.ID, err)
	}
	if len(status) != 0 {
		return fmt.Errorf("case %s prepared repository is not clean, including untracked files", c.ID)
	}
	baseBytes, err := hex.DecodeString(c.BaseSHA)
	if err != nil {
		return fmt.Errorf("case %s base SHA: %w", c.ID, err)
	}
	headBytes, err := hex.DecodeString(c.HeadSHA)
	if err != nil {
		return fmt.Errorf("case %s head SHA: %w", c.ID, err)
	}
	objectFormat := "sha1"
	if len(headBytes) == 32 {
		objectFormat = "sha256"
	}
	scope := review.Scope{
		Kind:         review.ScopeRange,
		ObjectFormat: objectFormat,
		Base:         review.SideIdentity{Kind: review.SideGitOID, Value: baseBytes},
		Head:         review.SideIdentity{Kind: review.SideGitOID, Value: headBytes},
	}
	runner := review.ExecGit{Binary: "git", Timeout: 30 * time.Second}
	capture, err := (&review.Capturer{Root: repository, Runner: runner}).CaptureOnce(parent, scope)
	if err != nil {
		return fmt.Errorf("case %s compute native review churn: %w", c.ID, err)
	}
	if capture.RawAdditions != c.RawAdditions || capture.RawDeletions != c.RawDeletions {
		return fmt.Errorf("case %s raw churn mismatch: manifest +%d/-%d, repository +%d/-%d", c.ID, c.RawAdditions, c.RawDeletions, capture.RawAdditions, capture.RawDeletions)
	}
	if capture.RawChurn <= 300 {
		return fmt.Errorf("case %s native raw churn must be strictly greater than 300", c.ID)
	}
	return nil
}

func preflightGitOutput(parent context.Context, repository string, limit int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	environment := os.Environ()
	for key, value := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
		"GIT_TERMINAL_PROMPT": "0", "GIT_NO_LAZY_FETCH": "1", "GIT_OPTIONAL_LOCKS": "0",
	} {
		environment = replaceEnvironment(environment, key, value)
	}
	output := &boundedBuffer{limit: limit}
	commandArgs := append([]string{"-C", repository, "-c", "protocol.allow=never"}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Env, command.Stdout, command.Stderr = environment, output, output
	if err := command.Run(); err != nil {
		return nil, err
	}
	if output.overflow {
		return nil, errors.New("git preflight output exceeded limit")
	}
	return append([]byte(nil), output.data...), nil
}

func replaceEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}
