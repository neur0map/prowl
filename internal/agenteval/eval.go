package agenteval

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prowl-agent/prowl-agent/internal/agenttrial"
)

type Case struct {
	ID              string   `json:"id"`
	Fixture         string   `json:"fixture"`
	Source          string   `json:"source"`
	Prompt          string   `json:"prompt"`
	Class           string   `json:"class"`
	ExpectedPaths   []string `json:"expected_paths"`
	ExpectedSymbols []string `json:"expected_symbols"`
}

type Manifest struct {
	Tuning  []Case `json:"tuning"`
	HeldOut []Case `json:"held_out"`
}

type ToolCall struct {
	Index     int             `json:"index"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input,omitempty"`
	Prowl     bool            `json:"prowl"`
	Broad     bool            `json:"broad"`
	WholeFile bool            `json:"whole_file"`
}

type ParsedTrial struct {
	RawStdout      string     `json:"raw_stdout"`
	RawStderr      string     `json:"raw_stderr"`
	Malformed      []string   `json:"malformed,omitempty"`
	Tools          []ToolCall `json:"tools"`
	FirstDiscovery string     `json:"first_discovery,omitempty"`
	FinalAnswer    string     `json:"final_answer"`
	Citations      []string   `json:"citations"`
	ElapsedMS      int64      `json:"elapsed_ms"`
	ToolCount      int        `json:"tool_count"`
	OutputBytes    int        `json:"output_bytes"`
}

type Trial struct {
	CaseID          string      `json:"case_id"`
	Class           string      `json:"class"`
	Client          string      `json:"client"`
	Condition       string      `json:"condition"`
	Repetition      int         `json:"repetition"`
	Parsed          ParsedTrial `json:"parsed"`
	Correct         bool        `json:"correct"`
	CitationQuality bool        `json:"citation_quality"`
	ProwlFirst      bool        `json:"prowl_first"`
	NativeControl   bool        `json:"native_control"`
	ProcessError    string      `json:"process_error,omitempty"`
}

type Metrics struct {
	Trials            int     `json:"trials"`
	ProwlFirstRate    float64 `json:"prowl_first_rate"`
	MedianBroadCalls  float64 `json:"median_broad_calls"`
	MedianToolCalls   float64 `json:"median_tool_calls"`
	MedianElapsedMS   float64 `json:"median_elapsed_ms"`
	MedianOutputBytes float64 `json:"median_output_bytes"`
}

type Gates struct {
	ProwlFirstRate       float64 `json:"prowl_first_rate"`
	BroadSearchReduction float64 `json:"broad_search_reduction"`
	AllCorrect           bool    `json:"all_correct"`
	ControlsNative       bool    `json:"controls_native"`
	Passed               bool    `json:"passed"`
}

type Report struct {
	Trials    []Trial `json:"trials"`
	Control   Metrics `json:"control"`
	Treatment Metrics `json:"treatment"`
	Gates     Gates   `json:"gates"`
}

type Config struct {
	Clients      []string
	Model        string
	Repetitions  int
	Set          string
	Fixture      string
	OutputDir    string
	ManifestPath string
	ProwlBinary  string
	ClaudeBinary string
	OMPBinary    string
	Timeout      time.Duration
}

var forbiddenPrompt = regexp.MustCompile(`(?i)\b(prowl|grep|glob|bash|read|test|evaluate|benchmark|preferred)\b`)

func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, err
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ValidateManifest(manifest Manifest) error {
	seen := map[string]bool{}
	validate := func(set string, cases []Case) error {
		for _, c := range cases {
			if c.ID == "" || c.Fixture == "" || c.Source == "" || c.Prompt == "" {
				return fmt.Errorf("%s case has an empty required field", set)
			}
			if !safePathSegment(c.ID) {
				return fmt.Errorf("case id %q is not a safe artifact path segment", c.ID)
			}
			if seen[c.ID] {
				return fmt.Errorf("duplicate case id %q", c.ID)
			}
			seen[c.ID] = true
			switch c.Class {
			case "structural", "literal", "filename", "known_file":
			default:
				return fmt.Errorf("case %s has unknown class %q", c.ID, c.Class)
			}
			if forbiddenPrompt.MatchString(c.Prompt) {
				return fmt.Errorf("case %s prompt contains an evaluation or routing hint", c.ID)
			}
			if len(c.ExpectedPaths)+len(c.ExpectedSymbols) == 0 {
				return fmt.Errorf("case %s has no ground truth", c.ID)
			}
		}
		return nil
	}
	if err := validate("tuning", manifest.Tuning); err != nil {
		return err
	}
	if err := validate("held_out", manifest.HeldOut); err != nil {
		return err
	}
	structural := 0
	controls := map[string]bool{}
	for _, c := range manifest.HeldOut {
		if c.Class == "structural" {
			structural++
		} else {
			controls[c.Class] = true
		}
	}
	if structural < 5 || !controls["literal"] || !controls["filename"] || !controls["known_file"] {
		return errors.New("held_out requires five structural cases and literal, filename, and known_file controls")
	}
	return nil
}

func ParseStream(client string, stdout, stderr []byte, elapsed time.Duration) ParsedTrial {
	parsed := ParsedTrial{RawStdout: string(stdout), RawStderr: string(stderr), ElapsedMS: elapsed.Milliseconds(), OutputBytes: len(stdout) + len(stderr)}
	scanner := bufio.NewScanner(strings.NewReader(string(stdout)))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			parsed.Malformed = append(parsed.Malformed, fmt.Sprintf("line %d: %v", line, err))
			continue
		}
		collectEvent(value, &parsed)
	}
	if err := scanner.Err(); err != nil {
		parsed.Malformed = append(parsed.Malformed, err.Error())
	}
	parsed.Tools = uniqueToolCalls(parsed.Tools)
	for i := range parsed.Tools {
		parsed.Tools[i].Index = i
		classifyTool(&parsed.Tools[i])
		if parsed.FirstDiscovery == "" {
			parsed.FirstDiscovery = parsed.Tools[i].Name
		}
	}
	parsed.ToolCount = len(parsed.Tools)
	parsed.Citations = citations(parsed.FinalAnswer)
	return parsed
}

func collectEvent(value any, parsed *ParsedTrial) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			collectEvent(item, parsed)
		}
	case map[string]any:
		typeName := stringField(v, "type")
		if call, ok := eventTool(v, typeName); ok {
			parsed.Tools = append(parsed.Tools, call)
		}
		if elapsed := numberField(v, "elapsed_ms", "duration_ms", "duration_api_ms"); elapsed > parsed.ElapsedMS {
			parsed.ElapsedMS = elapsed
		}
		if isAnswerEvent(v, typeName) {
			if text := eventText(v); strings.TrimSpace(text) != "" {
				parsed.FinalAnswer = text
			}
		}
		for key, child := range v {
			if key == "input" || key == "args" || key == "arguments" || key == "tool_input" || key == "toolCall" || key == "tool_call" {
				continue
			}
			collectEvent(child, parsed)
		}
	}
}

func eventTool(v map[string]any, typeName string) (ToolCall, bool) {
	candidate := v
	for _, key := range []string{"toolCall", "tool_call"} {
		if nested, ok := v[key].(map[string]any); ok {
			candidate = nested
			typeName = "tool_call"
			break
		}
	}
	name := stringField(candidate, "name", "tool_name", "toolName")
	if name == "" || (!strings.Contains(strings.ToLower(typeName), "tool") && stringField(v, "tool_name", "toolName") == "") {
		return ToolCall{}, false
	}
	input := firstField(candidate, "input", "args", "arguments", "tool_input")
	data, _ := json.Marshal(input)
	id := stringField(candidate, "id", "toolCallId", "tool_call_id", "tool_use_id")
	if id == "" {
		id = stringField(v, "id", "toolCallId", "tool_call_id", "tool_use_id")
	}
	return ToolCall{ID: id, Name: name, Input: data}, true
}
func uniqueToolCalls(calls []ToolCall) []ToolCall {
	indexByID := map[string]int{}
	out := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		if call.ID != "" {
			if index, seen := indexByID[call.ID]; seen {
				if !hasToolInput(out[index].Input) && hasToolInput(call.Input) {
					out[index].Name = call.Name
					out[index].Input = call.Input
				}
				continue
			}
			indexByID[call.ID] = len(out)
		}
		out = append(out, call)
	}
	return out
}

func hasToolInput(input json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(input))
	return trimmed != "" && trimmed != "null" && trimmed != "{}" && trimmed != "[]"
}

func isAnswerEvent(v map[string]any, typeName string) bool {
	role := strings.ToLower(stringField(v, "role"))
	t := strings.ToLower(typeName)
	return role == "assistant" || t == "result" || t == "assistant" || t == "agent_end" || t == "message_end"
}

func eventText(v map[string]any) string {
	for _, key := range []string{"result", "final", "text", "content", "message"} {
		if value, ok := v[key]; ok {
			if text := flattenText(value); text != "" {
				return text
			}
		}
	}
	return ""
}

func flattenText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var out []string
		for _, item := range v {
			if text := flattenText(item); text != "" {
				out = append(out, text)
			}
		}
		return strings.Join(out, "\n")
	case map[string]any:
		if kind := stringField(v, "type"); kind != "" && kind != "text" && kind != "output_text" {
			return ""
		}
		return stringField(v, "text", "content")
	default:
		return ""
	}
}

func stringField(v map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := v[key].(string); ok {
			return value
		}
	}
	return ""
}

func numberField(v map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if value, ok := v[key].(float64); ok {
			return int64(value)
		}
	}
	return 0
}

func firstField(v map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := v[key]; ok {
			return value
		}
	}
	return map[string]any{}
}

var prowlCommand = regexp.MustCompile(`(?:^|[|;&]\s*)(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)*(?:"[^"]*(?i:prowl-agent(?:\.exe)?)"|'[^']*(?i:prowl-agent(?:\.exe)?)'|(?:\S*[\\/])?(?i:prowl-agent(?:\.exe)?))(?:\s|$)`)
var citedPath = regexp.MustCompile(`[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+(?::\d+(?:-\d+)?)?`)

func classifyTool(call *ToolCall) {
	var input map[string]any
	_ = json.Unmarshal(call.Input, &input)
	name := strings.ToLower(call.Name)
	switch name {
	case "bash", "shell":
		command := stringField(input, "command", "cmd")
		call.Prowl = prowlCommand.MatchString(command)
		call.Broad = shellBroad(command)
	case "grep", "glob":
		path := strings.TrimSpace(stringField(input, "path"))
		call.Broad = path == "" || path == "." || path == "./" || path == "/"
	case "read":
		path := stringField(input, "path", "file_path")
		_, hasOffset := input["offset"]
		_, hasLimit := input["limit"]
		call.WholeFile = path != "" && !hasOffset && !hasLimit && !strings.Contains(path, ":")
		call.Broad = call.WholeFile
	}
}

func shellBroad(command string) bool {
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool { return r == '|' || r == ';' || r == '&' || r == '\n' }) {
		fields := strings.Fields(segment)
		for len(fields) > 0 && strings.Contains(fields[0], "=") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		name := filepath.Base(fields[0])
		if name != "rg" && name != "grep" && name != "find" {
			continue
		}
		if name == "find" {
			if len(fields) == 1 || fields[1] == "." || fields[1] == "./" || fields[1] == "/" {
				return true
			}
			continue
		}
		paths := searchPaths(fields[1:])
		if len(paths) == 0 {
			return true
		}
		for _, path := range paths {
			if path == "." || path == "./" || path == "/" {
				return true
			}
		}
	}
	return false
}

func searchPaths(fields []string) []string {
	patternSeen := false
	var paths []string
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if field == "-e" || field == "--regexp" || field == "-f" || field == "--file" {
			patternSeen = true
			i++
			continue
		}
		if strings.HasPrefix(field, "--regexp=") || strings.HasPrefix(field, "--file=") {
			patternSeen = true
			continue
		}
		if searchFlagTakesValue(field) {
			i++
			continue
		}
		if strings.HasPrefix(field, "-") {
			continue
		}
		if !patternSeen {
			patternSeen = true
			continue
		}
		paths = append(paths, strings.Trim(field, `"'`))
	}
	return paths
}

func searchFlagTakesValue(field string) bool {
	switch field {
	case "-g", "--glob", "--iglob", "--include", "--exclude", "--exclude-dir",
		"-t", "--type", "--type-add", "--encoding", "--engine", "--pre", "--pre-glob",
		"-m", "--max-count", "--max-depth", "--sort", "--sortr":
		return true
	default:
		return false
	}
}

func citations(answer string) []string {
	seen := map[string]bool{}
	var out []string
	for _, match := range citedPath.FindAllString(answer, -1) {
		if !seen[match] {
			seen[match] = true
			out = append(out, match)
		}
	}
	return out
}

func Score(c Case, client, condition string, repetition int, parsed ParsedTrial, processErr error) Trial {
	trial := Trial{CaseID: c.ID, Class: c.Class, Client: client, Condition: condition, Repetition: repetition, Parsed: parsed}
	if processErr != nil {
		trial.ProcessError = processErr.Error()
	}
	trial.Correct = processErr == nil && len(parsed.Malformed) == 0 && containsAll(parsed.FinalAnswer, append(append([]string{}, c.ExpectedPaths...), c.ExpectedSymbols...))
	trial.CitationQuality = containsAll(parsed.FinalAnswer, c.ExpectedPaths)
	firstProwl, firstBroad := -1, -1
	for _, call := range parsed.Tools {
		if call.Prowl && firstProwl < 0 {
			firstProwl = call.Index
		}
		if call.Broad && firstBroad < 0 {
			firstBroad = call.Index
		}
	}
	trial.ProwlFirst = c.Class == "structural" && firstProwl >= 0 && (firstBroad < 0 || firstProwl < firstBroad)
	trial.NativeControl = c.Class != "structural" && firstProwl < 0 && trial.Correct
	return trial
}

func containsAll(text string, expected []string) bool {
	for _, value := range expected {
		if !strings.Contains(text, value) {
			return false
		}
	}
	return true
}

func BuildReport(trials []Trial) Report {
	report := Report{Trials: trials}
	report.Control = metricsFor(trials, "control")
	report.Treatment = metricsFor(trials, "treatment")
	structural, prowlFirst := 0, 0
	allCorrect, controlsNative := len(trials) > 0, true
	for _, trial := range trials {
		if !trial.Correct {
			allCorrect = false
		}
		if trial.Condition == "treatment" && trial.Class == "structural" {
			structural++
			if trial.ProwlFirst {
				prowlFirst++
			}
		}
		if trial.Condition == "treatment" && trial.Class != "structural" && !trial.NativeControl {
			controlsNative = false
		}
	}
	rate := 0.0
	if structural > 0 {
		rate = float64(prowlFirst) / float64(structural)
	}
	reduction := 0.0
	if report.Control.MedianBroadCalls > 0 {
		reduction = (report.Control.MedianBroadCalls - report.Treatment.MedianBroadCalls) / report.Control.MedianBroadCalls
	}
	report.Gates = Gates{ProwlFirstRate: rate, BroadSearchReduction: reduction, AllCorrect: allCorrect, ControlsNative: controlsNative}
	report.Gates.Passed = rate >= .8 && reduction >= .5 && allCorrect && controlsNative
	return report
}

func metricsFor(trials []Trial, condition string) Metrics {
	var broad, tools, elapsed, bytes []float64
	prowl, structural := 0, 0
	for _, trial := range trials {
		if trial.Condition != condition {
			continue
		}
		count := 0
		for _, call := range trial.Parsed.Tools {
			if call.Broad {
				count++
			}
		}
		broad = append(broad, float64(count))
		tools = append(tools, float64(trial.Parsed.ToolCount))
		elapsed = append(elapsed, float64(trial.Parsed.ElapsedMS))
		bytes = append(bytes, float64(trial.Parsed.OutputBytes))
		if trial.Class == "structural" {
			structural++
			if trial.ProwlFirst {
				prowl++
			}
		}
	}
	m := Metrics{Trials: len(broad), MedianBroadCalls: median(broad), MedianToolCalls: median(tools), MedianElapsedMS: median(elapsed), MedianOutputBytes: median(bytes)}
	if structural > 0 {
		m.ProwlFirstRate = float64(prowl) / float64(structural)
	}
	return m
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	middle := len(copyValues) / 2
	if len(copyValues)%2 == 1 {
		return copyValues[middle]
	}
	return (copyValues[middle-1] + copyValues[middle]) / 2
}

func resolveFixture(configured, manifestFixture string) string {
	if configured != "" {
		return configured
	}
	return manifestFixture
}

func safePathSegment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, `/\`)
}

func validateClients(clients []string) error {
	for _, client := range clients {
		if client != "claude" && client != "omp" {
			return fmt.Errorf("unknown client %q", client)
		}
	}
	return nil
}

func caseWorkDir(root, source string) (string, error) {
	clean := filepath.Clean(source)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source %q escapes its fixture", source)
	}
	path := filepath.Join(root, clean)
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source %q is not a directory", source)
	}
	return path, nil
}

func Run(ctx context.Context, cfg Config, manifest Manifest) (Report, error) {
	if cfg.OutputDir == "" || filepath.IsAbs(cfg.OutputDir) || strings.HasPrefix(filepath.Clean(cfg.OutputDir), "..") {
		return Report{}, errors.New("output must be an explicit local relative directory")
	}
	if cfg.Repetitions < 1 {
		cfg.Repetitions = 1
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Minute
	}
	if cfg.ProwlBinary == "" {
		cfg.ProwlBinary = "prowl-agent"
	}
	if cfg.ClaudeBinary == "" {
		cfg.ClaudeBinary = "claude"
	}
	if cfg.OMPBinary == "" {
		cfg.OMPBinary = "omp"
	}
	if len(cfg.Clients) == 0 {
		cfg.Clients = []string{"claude", "omp"}
	}
	if err := validateClients(cfg.Clients); err != nil {
		return Report{}, err
	}
	if cfg.Set == "" {
		cfg.Set = "tuning"
	}
	cases := manifest.Tuning
	if cfg.Set == "held_out" {
		cases = manifest.HeldOut
	} else if cfg.Set != "" && cfg.Set != "tuning" {
		return Report{}, fmt.Errorf("unknown prompt set %q", cfg.Set)
	}
	if len(cases) == 0 {
		return Report{}, errors.New("selected prompt set is empty")
	}
	fixtureName := cases[0].Fixture
	for _, c := range cases {
		if c.Fixture != fixtureName {
			return Report{}, fmt.Errorf("case %s selects fixture %q; runner fixture is %q", c.ID, c.Fixture, fixtureName)
		}
	}
	fixture := resolveFixture(cfg.Fixture, fixtureName)
	temp, err := os.MkdirTemp("", "prowl-agent-adoption-")
	if err != nil {
		return Report{}, err
	}
	defer os.RemoveAll(temp)
	work := filepath.Join(temp, "fixture")
	if err := copyFixture(fixture, work, cfg.OutputDir); err != nil {
		return Report{}, err
	}
	initCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	initCmd := exec.CommandContext(initCtx, cfg.ProwlBinary, "init", "--no-input", "--integrations", "none")
	initCmd.Dir = work
	initOutput, initErr := initCmd.CombinedOutput()
	cancel()
	if initErr != nil {
		return Report{}, fmt.Errorf("initialize fixture: %w: %s", initErr, initOutput)
	}
	removeRouting(work)
	outputRoot, err := filepath.Abs(cfg.OutputDir)
	if err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		return Report{}, err
	}
	var trials []Trial
	for repetitionIndex := range cfg.Repetitions {
		repetition := repetitionIndex + 1
		for _, c := range cases {
			for _, client := range cfg.Clients {
				for _, condition := range []string{"control", "treatment"} {
					trialWork := filepath.Join(temp, "fixtures", c.ID, client, condition, fmt.Sprintf("%02d", repetition))
					if err := copyPreparedFixture(work, trialWork); err != nil {
						return Report{}, err
					}
					caseDir, err := caseWorkDir(trialWork, c.Source)
					if err != nil {
						return Report{}, fmt.Errorf("case %s source: %w", c.ID, err)
					}
					clientRoot := filepath.Join(temp, "clients", c.ID, client, condition, fmt.Sprintf("%02d", repetition))
					stdout, stderr, elapsed, processErr := runClient(ctx, cfg, caseDir, clientRoot, client, condition, c.Prompt)
					parsed := ParseStream(client, stdout, stderr, elapsed)
					trial := Score(c, client, condition, repetition, parsed, processErr)
					trials = append(trials, trial)
					if err := os.RemoveAll(trialWork); err != nil {
						return Report{}, err
					}
					dir := filepath.Join(outputRoot, cfg.Set, c.ID, client, condition, fmt.Sprintf("%02d", repetition))
					if err := writeTrial(dir, trial); err != nil {
						return Report{}, err
					}
				}
			}
		}
	}
	report := BuildReport(trials)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return Report{}, err
	}
	if err := os.WriteFile(filepath.Join(outputRoot, "report.json"), append(data, '\n'), 0o644); err != nil {
		return Report{}, err
	}
	if err := os.WriteFile(filepath.Join(outputRoot, "report.txt"), []byte(RenderTable(report)), 0o644); err != nil {
		return Report{}, err
	}
	return report, nil
}

func runClient(parent context.Context, cfg Config, work, clientRoot, client, condition, prompt string) ([]byte, []byte, time.Duration, error) {
	environment, err := clientEnvironment(clientRoot, client, condition)
	if err != nil {
		return nil, nil, 0, err
	}
	clientCfg := agenttrial.ClientConfig{
		Client:              client,
		Model:               cfg.Model,
		ClaudeBinary:        cfg.ClaudeBinary,
		OMPBinary:           cfg.OMPBinary,
		Environment:         environment,
		DisableAmbientRules: condition == "control",
		Budget:              agenttrial.Budget{Timeout: cfg.Timeout},
	}
	if condition == "treatment" {
		home, _ := os.UserHomeDir()
		switch client {
		case "claude":
			clientCfg.PluginDirs = []string{filepath.Join(home, ".claude", "skills", "prowl")}
		case "omp":
			clientCfg.Skills = []string{"code-search"}
			clientCfg.Extensions = []string{filepath.Join(home, ".omp", "agent", "extensions", "prowl-routing.ts")}
			clientCfg.DisableAmbientRules = true
		}
	}
	result, err := agenttrial.Run(parent, work, prompt, clientCfg)
	return result.Stdout, result.Stderr, result.Elapsed, err
}

func claudeArgs(cfg Config, condition, home, prompt string) []string {
	args := []string{
		"-p", "--verbose", "--no-session-persistence",
		"--output-format", "stream-json", "--include-hook-events",
		"--setting-sources", "project", "--permission-mode", "dontAsk",
		"--allowedTools", "Bash,Read,Grep,Glob,Agent,Skill",
		"--disallowedTools", "Edit,Write,Notebook",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if condition == "treatment" {
		args = append(args, "--plugin-dir", filepath.Join(home, ".claude", "skills", "prowl"))
	}
	return append(args, prompt)
}
func clientEnvironment(root, client, condition string) ([]string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	environment := os.Environ()
	switch client {
	case "claude":
		environment = replaceEnv(environment, "CLAUDE_CONFIG_DIR", root)
		credential := filepath.Join(home, ".claude", ".credentials.json")
		if info, statErr := os.Stat(credential); statErr == nil && info.Mode().IsRegular() {
			if err := copyFile(credential, filepath.Join(root, ".credentials.json"), 0o600); err != nil {
				return nil, err
			}
		}
		if condition == "treatment" {
			plugin := filepath.Join(home, ".claude", "skills", "prowl")
			if info, statErr := os.Stat(plugin); statErr != nil || !info.IsDir() {
				return nil, fmt.Errorf("Claude treatment package missing at %s", plugin)
			}
		}
	case "omp":
		environment = replaceEnv(environment, "PI_CODING_AGENT_DIR", root)
		if condition == "treatment" {
			skill := filepath.Join(home, ".omp", "agent", "skills", "code-search")
			if info, statErr := os.Stat(skill); statErr != nil || !info.IsDir() {
				return nil, fmt.Errorf("OMP treatment skill missing at %s", skill)
			}
			if err := copyFixture(skill, filepath.Join(root, "skills", "code-search"), ""); err != nil {
				return nil, err
			}
			extension := filepath.Join(home, ".omp", "agent", "extensions", "prowl-routing.ts")
			if info, statErr := os.Stat(extension); statErr != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("OMP treatment extension missing at %s", extension)
			}
		}
	default:
		return nil, fmt.Errorf("unknown client %q", client)
	}
	return environment, nil
}

func replaceEnv(environment []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return errors.Join(err, input.Close())
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, input.Close(), output.Close())
}

func writeTrial(dir string, trial Trial) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.jsonl"), []byte(trial.Parsed.RawStdout), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr.txt"), []byte(trial.Parsed.RawStderr), 0o644); err != nil {
		return err
	}
	data, err := json.MarshalIndent(trial, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "trial.json"), append(data, '\n'), 0o644)
}

func copyFixture(source, destination, output string) error {
	absoluteOutput, _ := filepath.Abs(output)
	return copyTree(source, destination, func(path string, entry fs.DirEntry) bool {
		name := entry.Name()
		return entry.IsDir() && (name == ".git" || name == ".worktrees" || name == ".prowl" || name == "bin" || (absoluteOutput != "" && path == absoluteOutput))
	})
}

func copyPreparedFixture(source, destination string) error {
	return copyTree(source, destination, nil)
}

func copyTree(source, destination string, skip func(string, fs.DirEntry) bool) error {
	absoluteSource, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	return filepath.WalkDir(absoluteSource, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if skip != nil && skip(path, entry) {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(absoluteSource, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(destination, 0o755)
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(destination, rel), 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		sourceFile, err := os.Open(path)
		if err != nil {
			return err
		}
		target, err := os.OpenFile(filepath.Join(destination, rel), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return errors.Join(err, sourceFile.Close())
		}
		_, copyErr := io.Copy(target, sourceFile)
		return errors.Join(copyErr, sourceFile.Close(), target.Close())
	})
}

func removeRouting(root string) {
	for _, rel := range []string{"AGENTS.md", ".mcp.json", ".claude", ".omp", ".agents"} {
		_ = os.RemoveAll(filepath.Join(root, rel))
	}
}

func RenderTable(report Report) string {
	return fmt.Sprintf("condition  trials  prowl-first  broad-median  tool-median  elapsed-ms\ncontrol    %6d  %11.0f%%  %12.1f  %11.1f  %10.1f\ntreatment  %6d  %11.0f%%  %12.1f  %11.1f  %10.1f\ngates: prowl-first %.0f%%, broad reduction %.0f%%, correct %t, controls-native %t, pass %t\n",
		report.Control.Trials, report.Control.ProwlFirstRate*100, report.Control.MedianBroadCalls, report.Control.MedianToolCalls, report.Control.MedianElapsedMS,
		report.Treatment.Trials, report.Treatment.ProwlFirstRate*100, report.Treatment.MedianBroadCalls, report.Treatment.MedianToolCalls, report.Treatment.MedianElapsedMS,
		report.Gates.ProwlFirstRate*100, report.Gates.BroadSearchReduction*100, report.Gates.AllCorrect, report.Gates.ControlsNative, report.Gates.Passed)
}
