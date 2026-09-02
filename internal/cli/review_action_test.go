package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type reviewActionManifest struct {
	Name        string                        `yaml:"name"`
	Description string                        `yaml:"description"`
	Inputs      map[string]reviewActionInput  `yaml:"inputs"`
	Outputs     map[string]reviewActionOutput `yaml:"outputs"`
	Runs        struct {
		Using string             `yaml:"using"`
		Steps []reviewActionStep `yaml:"steps"`
	} `yaml:"runs"`
}

type reviewActionInput struct {
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
}

type reviewActionOutput struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

type reviewActionStep struct {
	Name  string            `yaml:"name"`
	ID    string            `yaml:"id"`
	Uses  string            `yaml:"uses"`
	If    string            `yaml:"if"`
	Shell string            `yaml:"shell"`
	Run   string            `yaml:"run"`
	Env   map[string]string `yaml:"env"`
	With  map[string]any    `yaml:"with"`
}

type reviewWorkflowManifest struct {
	On          map[string]any               `yaml:"on"`
	Permissions map[string]string            `yaml:"permissions"`
	Jobs        map[string]reviewWorkflowJob `yaml:"jobs"`
}

type reviewWorkflowJob struct {
	Needs       any                `yaml:"needs"`
	If          string             `yaml:"if"`
	Permissions map[string]string  `yaml:"permissions"`
	Outputs     map[string]string  `yaml:"outputs"`
	Steps       []reviewActionStep `yaml:"steps"`
}

func TestReviewActionDoctorFailOnRejectsInvalidValueBeforeDoctor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := newDoctorCmd("test")
	command.SetArgs([]string{"--fail-on", "erorr"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)

	err := command.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), `unknown doctor --fail-on value "erorr" (choose none, warn, or error)`) {
		t.Fatalf("invalid --fail-on error = %v", err)
	}
}

func TestReviewActionPlanShellProducesValidatedOutputs(t *testing.T) {
	requireReviewActionShellTools(t)
	action := loadReviewAction(t)
	plan := action.Runs.Steps[reviewStepIndex(action.Runs.Steps, "prowl-agent review plan")]
	temp := t.TempDir()
	writeReviewActionStub(t, temp)
	reviewID := "rvw_" + strings.Repeat("c", 40)
	planJSON := fmt.Sprintf(`{"review_id":%q,"structured_required":true}`, reviewID)
	outputPath := filepath.Join(temp, "github-output")

	err := runReviewActionShell(plan.Run, reviewActionEnvironment(temp, map[string]string{
		"GITHUB_OUTPUT":   outputPath,
		"PROWL_BASE":      strings.Repeat("a", 40),
		"PROWL_HEAD":      strings.Repeat("b", 40),
		"PROWL_FAIL_ON":   "error",
		"PROWL_PLAN_JSON": planJSON,
	}))
	if err != nil {
		t.Fatalf("plan shell failed: %v", err)
	}
	outputs := readReviewActionOutputs(t, outputPath)
	if got := outputs["review-id"]; got != reviewID {
		t.Errorf("review-id output = %q, want %q", got, reviewID)
	}
	if got := outputs["structured-required"]; got != "true" {
		t.Errorf("structured-required output = %q, want true", got)
	}
	planPath := outputs["plan"]
	if !strings.HasPrefix(planPath, temp+string(filepath.Separator)) {
		t.Fatalf("plan output path %q is outside runner temp %q", planPath, temp)
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan artifact: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != planJSON {
		t.Errorf("plan artifact = %q, want %q", got, planJSON)
	}
}

func TestReviewActionAcceptsSHA256InputsBeforeProwl(t *testing.T) {
	requireReviewActionShellTools(t)
	action := loadReviewAction(t)
	temp := t.TempDir()
	writeReviewActionStub(t, temp)
	outputPath := filepath.Join(temp, "github-output")
	sarifPath := filepath.Join(temp, "prowl-results.sarif")
	reviewID := "rvw_" + strings.Repeat("c", 40)
	err := runReviewActionStepsUntilFailure(action.Runs.Steps, reviewActionEnvironment(temp, map[string]string{
		"GITHUB_OUTPUT":            outputPath,
		"PROWL_BASE":               strings.Repeat("a", 64),
		"PROWL_HEAD":               strings.Repeat("b", 64),
		"PROWL_FAIL_ON":            "error",
		"PROWL_PLAN_JSON":          fmt.Sprintf(`{"review_id":%q,"structured_required":false}`, reviewID),
		"PROWL_PROFILE":            "general",
		"PROWL_SARIF_PATH":         sarifPath,
		"PROWL_DOCTOR_GATE_STATUS": "0",
		"PROWL_GIT_OBJECT_FORMAT":  "sha256",
	}))
	if err != nil {
		t.Fatalf("SHA-256 action orchestration failed: %v", err)
	}
	outputs := readReviewActionOutputs(t, outputPath)
	if outputs["review-id"] != reviewID || outputs["structured-required"] != "false" {
		t.Errorf("SHA-256 action outputs = %+v", outputs)
	}
}

func TestReviewActionPlanShellRejectsMalformedOutputsWithoutInjection(t *testing.T) {
	requireReviewActionShellTools(t)
	validID := "rvw_" + strings.Repeat("c", 40)
	cases := map[string]string{
		"malformed JSON":        `{`,
		"newline-bearing ID":    fmt.Sprintf(`{"review_id":%q,"structured_required":true}`, validID+"\ninjected=1"),
		"trailing-newline ID":   fmt.Sprintf(`{"review_id":%q,"structured_required":true}`, validID+"\n"),
		"wrong-type ID":         `{"review_id":7,"structured_required":true}`,
		"wrong-type boolean":    fmt.Sprintf(`{"review_id":%q,"structured_required":"true\ninjected=1"}`, validID),
		"missing required data": `{}`,
	}
	action := loadReviewAction(t)
	plan := action.Runs.Steps[reviewStepIndex(action.Runs.Steps, "prowl-agent review plan")]
	for name, planJSON := range cases {
		t.Run(name, func(t *testing.T) {
			temp := t.TempDir()
			writeReviewActionStub(t, temp)
			outputPath := filepath.Join(temp, "github-output")
			err := runReviewActionShell(plan.Run, reviewActionEnvironment(temp, map[string]string{
				"GITHUB_OUTPUT":   outputPath,
				"PROWL_BASE":      strings.Repeat("a", 40),
				"PROWL_HEAD":      strings.Repeat("b", 40),
				"PROWL_FAIL_ON":   "error",
				"PROWL_PLAN_JSON": planJSON,
			}))
			if err == nil {
				t.Fatal("malformed plan output succeeded")
			}
			if outputs := readReviewActionOutputs(t, outputPath); len(outputs) != 0 {
				t.Fatalf("malformed plan injected outputs: %+v", outputs)
			}
		})
	}
}

func TestReviewActionInvalidInputsStopBeforeAnyProwlCommand(t *testing.T) {
	requireReviewActionShellTools(t)
	cases := map[string]map[string]string{
		"invalid base":     {"PROWL_BASE": "main"},
		"SHA-1 short base": {"PROWL_BASE": strings.Repeat("a", 39)},
		"SHA-1 long head":  {"PROWL_HEAD": strings.Repeat("b", 41)},
		"zero base":        {"PROWL_BASE": strings.Repeat("0", 40)},
		"invalid head":     {"PROWL_HEAD": "HEAD"},
		"zero head":        {"PROWL_HEAD": strings.Repeat("0", 40)},
		"invalid fail-on":  {"PROWL_FAIL_ON": "erorr"},
		"SHA-256 short base": {
			"PROWL_BASE":              strings.Repeat("a", 63),
			"PROWL_HEAD":              strings.Repeat("b", 64),
			"PROWL_GIT_OBJECT_FORMAT": "sha256",
		},
		"SHA-256 long head": {
			"PROWL_BASE":              strings.Repeat("a", 64),
			"PROWL_HEAD":              strings.Repeat("b", 65),
			"PROWL_GIT_OBJECT_FORMAT": "sha256",
		},
		"SHA-256 zero base": {
			"PROWL_BASE":              strings.Repeat("0", 64),
			"PROWL_HEAD":              strings.Repeat("b", 64),
			"PROWL_GIT_OBJECT_FORMAT": "sha256",
		},
	}
	action := loadReviewAction(t)
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			temp := t.TempDir()
			writeReviewActionStub(t, temp)
			outputPath := filepath.Join(temp, "github-output")
			values := map[string]string{
				"GITHUB_OUTPUT":   outputPath,
				"PROWL_BASE":      strings.Repeat("a", 40),
				"PROWL_HEAD":      strings.Repeat("b", 40),
				"PROWL_FAIL_ON":   "error",
				"PROWL_PLAN_JSON": fmt.Sprintf(`{"review_id":%q,"structured_required":false}`, "rvw_"+strings.Repeat("c", 40)),
			}
			for key, value := range override {
				values[key] = value
			}
			err := runReviewActionStepsUntilFailure(action.Runs.Steps, reviewActionEnvironment(temp, values))
			if err == nil {
				t.Fatal("invalid action input succeeded")
			}
			if outputs := readReviewActionOutputs(t, outputPath); len(outputs) != 0 {
				t.Fatalf("invalid action input wrote outputs: %+v", outputs)
			}
			if data, err := os.ReadFile(filepath.Join(temp, "prowl-stub.log")); err == nil && len(data) != 0 {
				t.Fatalf("invalid action input executed prowl-agent before rejection: %s", data)
			}
		})
	}
}

func TestReviewActionFailingDoctorGatePreservesArtifactsForAlwaysUploads(t *testing.T) {
	requireReviewActionShellTools(t)
	action := loadReviewAction(t)
	plan := action.Runs.Steps[reviewStepIndex(action.Runs.Steps, "prowl-agent review plan")]
	doctorIndex := reviewStepIndex(action.Runs.Steps, "prowl-agent doctor --profile")
	doctorSARIF := action.Runs.Steps[doctorIndex]
	gate := action.Runs.Steps[reviewStepIndexAfter(action.Runs.Steps, "prowl-agent doctor --profile", doctorIndex+1)]
	temp := t.TempDir()
	writeReviewActionStub(t, temp)
	outputPath := filepath.Join(temp, "github-output")
	sarifPath := filepath.Join(temp, "prowl-results.sarif")
	common := map[string]string{
		"GITHUB_OUTPUT":            outputPath,
		"PROWL_BASE":               strings.Repeat("a", 40),
		"PROWL_HEAD":               strings.Repeat("b", 40),
		"PROWL_FAIL_ON":            "error",
		"PROWL_PLAN_JSON":          fmt.Sprintf(`{"review_id":%q,"structured_required":false}`, "rvw_"+strings.Repeat("c", 40)),
		"PROWL_PROFILE":            "general",
		"PROWL_SARIF_PATH":         sarifPath,
		"PROWL_DOCTOR_GATE_STATUS": "23",
	}
	if err := runReviewActionShell(plan.Run, reviewActionEnvironment(temp, common)); err != nil {
		t.Fatalf("plan shell failed: %v", err)
	}
	if err := runReviewActionShell(doctorSARIF.Run, reviewActionEnvironment(temp, common)); err != nil {
		t.Fatalf("doctor SARIF shell failed: %v", err)
	}
	err := runReviewActionShell(gate.Run, reviewActionEnvironment(temp, common))
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("doctor gate error = %v, want exit 23", err)
	}
	outputs := readReviewActionOutputs(t, outputPath)
	for _, path := range []string{outputs["plan"], sarifPath} {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Size() == 0 {
			t.Errorf("artifact %q after failing gate: info=%v err=%v", path, info, statErr)
		}
	}

	var workflow reviewWorkflowManifest
	decodeReviewYAML(t, ".github/workflows/prowl-review.yml", &workflow)
	reviewJob := workflow.Jobs["review"]
	for _, output := range []string{"${{ steps.prowl.outputs.plan }}", "${{ steps.prowl.outputs.sarif }}"} {
		index := reviewArtifactUpload(reviewJob.Steps, output)
		if index < 0 || !strings.Contains(reviewJob.Steps[index].If, "always()") {
			t.Errorf("artifact %s is not eligible after a failed gate", output)
		}
	}
}

func TestReviewActionContract(t *testing.T) {
	var action reviewActionManifest
	decodeReviewYAML(t, ".github/actions/prowl-review/action.yml", &action)

	if action.Name != "Prowl review context" {
		t.Errorf("action display name = %q, want %q", action.Name, "Prowl review context")
	}
	if !strings.Contains(strings.ToLower(action.Description), "host coding agent") {
		t.Errorf("action description must assign semantic review to a host coding agent: %q", action.Description)
	}
	for _, name := range []string{"base", "head"} {
		input, ok := action.Inputs[name]
		if !ok {
			t.Errorf("action input %q is missing", name)
			continue
		}
		if !input.Required {
			t.Errorf("action input %q must be required", name)
		}
	}
	for _, name := range []string{"fail-on", "profile", "sarif-path"} {
		if _, ok := action.Inputs[name]; !ok {
			t.Errorf("existing doctor input %q was removed", name)
		}
	}
	for _, name := range []string{"plan", "review-id", "structured-required", "sarif"} {
		if _, ok := action.Outputs[name]; !ok {
			t.Errorf("action output %q is missing", name)
		}
	}
	if action.Runs.Using != "composite" {
		t.Errorf("action runs.using = %q, want composite", action.Runs.Using)
	}

	validationIndex := reviewStepIndex(action.Runs.Steps, "fail-on must be one of")
	initIndex := reviewStepIndex(action.Runs.Steps, "prowl-agent init")
	freshenIndex := reviewStepIndex(action.Runs.Steps, "prowl-agent overview")
	planIndex := reviewStepIndex(action.Runs.Steps, "prowl-agent review plan")
	doctorSARIFIndex := reviewStepIndex(action.Runs.Steps, "prowl-agent doctor --profile")
	gateIndex := reviewStepIndexAfter(action.Runs.Steps, "prowl-agent doctor --profile", doctorSARIFIndex+1)
	if validationIndex < 0 || initIndex < 0 || freshenIndex < 0 || planIndex < 0 || doctorSARIFIndex < 0 || gateIndex < 0 {
		t.Fatalf("action steps do not retain validation/init/freshen/plan/doctor/gate ordering: %+v", action.Runs.Steps)
	}
	if validationIndex != 0 ||
		!(validationIndex < initIndex && initIndex < freshenIndex && freshenIndex < planIndex &&
			planIndex < doctorSARIFIndex && doctorSARIFIndex < gateIndex) {
		t.Errorf("action order = validation:%d init:%d freshen:%d plan:%d doctor:%d gate:%d",
			validationIndex, initIndex, freshenIndex, planIndex, doctorSARIFIndex, gateIndex)
	}
	validation := action.Runs.Steps[validationIndex]
	for _, fragment := range []string{
		`git rev-parse --show-object-format`,
		`sha1`,
		`sha256`,
		`^[0-9a-f]+$`,
		`printf -v zero_oid`,
	} {
		if !strings.Contains(validation.Run, fragment) {
			t.Errorf("input validation omits object-format contract %q:\n%s", fragment, validation.Run)
		}
	}
	if strings.Contains(validation.Run, "{40}") || strings.Contains(validation.Run, "40-hex") {
		t.Error("input validation still assumes SHA-1 OID length")
	}

	plan := action.Runs.Steps[planIndex]
	if plan.Env["PROWL_BASE"] != "${{ inputs.base }}" || plan.Env["PROWL_HEAD"] != "${{ inputs.head }}" {
		t.Errorf("plan does not forward base/head through environment: %+v", plan.Env)
	}

	for _, fragment := range []string{
		`--base "$PROWL_BASE"`,
		`--head "$PROWL_HEAD"`,
		`--format json`,
		`jq -er`,
		`"$GITHUB_OUTPUT"`,
	} {
		if !strings.Contains(plan.Run, fragment) {
			t.Errorf("plan shell omits safe contract %q:\n%s", fragment, plan.Run)
		}
	}
	if strings.Contains(plan.Run, "${{") || strings.Contains(plan.Run, "eval ") {
		t.Errorf("plan shell interpolates an expression or uses eval:\n%s", plan.Run)
	}
	if !strings.Contains(plan.Run, "rvw_[0-9a-f]{40}") || !strings.Contains(plan.Run, `type == "boolean"`) {
		t.Errorf("plan shell does not validate parsed outputs before writing GITHUB_OUTPUT:\n%s", plan.Run)
	}
	for name, want := range map[string]string{
		"plan":                "${{ steps.plan.outputs.plan }}",
		"review-id":           "${{ steps.plan.outputs.review-id }}",
		"structured-required": "${{ steps.plan.outputs.structured-required }}",
	} {
		if got := action.Outputs[name].Value; got != want {
			t.Errorf("action output %q value = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(action.Runs.Steps[doctorSARIFIndex].Run, `--format sarif`) ||
		!strings.Contains(action.Runs.Steps[gateIndex].Run, `--fail-on`) {
		t.Error("doctor SARIF or fail-on behavior was not retained")
	}
}

func TestReviewActionWorkflowRangeShellHandlesImmutablePushScopes(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to exercise workflow shell")
	}
	var workflow reviewWorkflowManifest
	decodeReviewYAML(t, ".github/workflows/prowl-review.yml", &workflow)
	steps := workflow.Jobs["review"].Steps
	rangeIndex := reviewStepIndex(steps, "refs/prowl-review/base")
	if rangeIndex < 0 {
		t.Fatal("workflow has no immutable range preparation step")
	}
	script := steps[rangeIndex].Run
	base := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	base256 := strings.Repeat("c", 64)
	head256 := strings.Repeat("d", 64)

	t.Run("force-push base is fetched by exact OID with ancestry", func(t *testing.T) {
		temp := t.TempDir()
		writeReviewGitStub(t, temp)
		outputPath := filepath.Join(temp, "github-output")
		err := runReviewActionShell(script, reviewActionEnvironment(temp, map[string]string{
			"GITHUB_OUTPUT":           outputPath,
			"PROWL_EVENT_NAME":        "push",
			"PROWL_PUSH_BASE":         base256,
			"PROWL_PUSH_HEAD":         head256,
			"PROWL_GIT_OBJECT_FORMAT": "sha256",
		}))
		if err != nil {
			t.Fatalf("range shell failed: %v", err)
		}
		outputs := readReviewActionOutputs(t, outputPath)
		if outputs["base"] != base256 || outputs["head"] != head256 || outputs["skip"] != "false" {
			t.Errorf("push range outputs = %+v", outputs)
		}
		log, err := os.ReadFile(filepath.Join(temp, "git-stub.log"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"fetch --no-tags --no-recurse-submodules --force origin",
			"+" + base256 + ":refs/prowl-review/base",
			"+" + head256 + ":refs/prowl-review/head",
		} {
			if !strings.Contains(string(log), want) {
				t.Errorf("exact push fetch log omits %q: %s", want, log)
			}
		}
	})

	t.Run("created push skips without forwarding zero base", func(t *testing.T) {
		temp := t.TempDir()
		writeReviewGitStub(t, temp)
		outputPath := filepath.Join(temp, "github-output")
		err := runReviewActionShell(script, reviewActionEnvironment(temp, map[string]string{
			"GITHUB_OUTPUT":    outputPath,
			"PROWL_EVENT_NAME": "push",
			"PROWL_PUSH_BASE":  strings.Repeat("0", 40),
			"PROWL_PUSH_HEAD":  head,
		}))
		if err != nil {
			t.Fatalf("created-push range shell failed: %v", err)
		}
		outputs := readReviewActionOutputs(t, outputPath)
		if len(outputs) != 1 || outputs["skip"] != "true" {
			t.Errorf("created-push outputs = %+v, want only skip=true", outputs)
		}
		if data, err := os.ReadFile(filepath.Join(temp, "git-stub.log")); err == nil && strings.Contains(string(data), "fetch ") {
			t.Errorf("created push fetched or forwarded the zero base: %s", data)
		}
	})

	t.Run("SHA-256 created push skips 64-zero base", func(t *testing.T) {
		temp := t.TempDir()
		writeReviewGitStub(t, temp)
		outputPath := filepath.Join(temp, "github-output")
		err := runReviewActionShell(script, reviewActionEnvironment(temp, map[string]string{
			"GITHUB_OUTPUT":           outputPath,
			"PROWL_EVENT_NAME":        "push",
			"PROWL_PUSH_BASE":         strings.Repeat("0", 64),
			"PROWL_PUSH_HEAD":         head256,
			"PROWL_GIT_OBJECT_FORMAT": "sha256",
		}))
		if err != nil {
			t.Fatalf("SHA-256 created-push range shell failed: %v", err)
		}
		outputs := readReviewActionOutputs(t, outputPath)
		if len(outputs) != 1 || outputs["skip"] != "true" {
			t.Errorf("SHA-256 created-push outputs = %+v, want only skip=true", outputs)
		}
		if data, err := os.ReadFile(filepath.Join(temp, "git-stub.log")); err == nil && strings.Contains(string(data), "fetch ") {
			t.Errorf("SHA-256 created push fetched or forwarded the zero base: %s", data)
		}
	})

	t.Run("invalid push SHA fails before fetch", func(t *testing.T) {
		temp := t.TempDir()
		writeReviewGitStub(t, temp)
		outputPath := filepath.Join(temp, "github-output")
		err := runReviewActionShell(script, reviewActionEnvironment(temp, map[string]string{
			"GITHUB_OUTPUT":    outputPath,
			"PROWL_EVENT_NAME": "push",
			"PROWL_PUSH_BASE":  "main",
			"PROWL_PUSH_HEAD":  head,
		}))
		if err == nil {
			t.Fatal("invalid push SHA succeeded")
		}
		if outputs := readReviewActionOutputs(t, outputPath); len(outputs) != 0 {
			t.Fatalf("invalid push SHA wrote outputs: %+v", outputs)
		}
		if data, err := os.ReadFile(filepath.Join(temp, "git-stub.log")); err == nil && strings.Contains(string(data), "fetch ") {
			t.Fatalf("invalid push SHA reached git fetch: %s", data)
		}
	})

	t.Run("pull request uses present immutable objects without fetch", func(t *testing.T) {
		temp := t.TempDir()
		writeReviewGitStub(t, temp)
		outputPath := filepath.Join(temp, "github-output")
		err := runReviewActionShell(script, reviewActionEnvironment(temp, map[string]string{
			"GITHUB_OUTPUT":    outputPath,
			"PROWL_EVENT_NAME": "pull_request",
			"PROWL_PR_BASE":    base,
			"PROWL_PR_HEAD":    head,
		}))
		if err != nil {
			t.Fatalf("pull-request range shell failed: %v", err)
		}
		outputs := readReviewActionOutputs(t, outputPath)
		if outputs["base"] != base || outputs["head"] != head || outputs["skip"] != "false" {
			t.Errorf("pull-request range outputs = %+v", outputs)
		}
		log, err := os.ReadFile(filepath.Join(temp, "git-stub.log"))
		if err != nil {
			t.Fatal(err)
		}
		for _, oid := range []string{base, head} {
			if !strings.Contains(string(log), "cat-file -e "+oid+"^{commit}") {
				t.Errorf("pull-request range did not verify local OID %s: %s", oid, log)
			}
		}
		if strings.Contains(string(log), "fetch ") {
			t.Errorf("present pull-request base triggered fetch: %s", log)
		}
	})

	t.Run("pull request fetches missing event base by exact OID", func(t *testing.T) {
		temp := t.TempDir()
		writeReviewGitStub(t, temp)
		outputPath := filepath.Join(temp, "github-output")
		err := runReviewActionShell(script, reviewActionEnvironment(temp, map[string]string{
			"GITHUB_OUTPUT":           outputPath,
			"PROWL_EVENT_NAME":        "pull_request",
			"PROWL_PR_BASE":           base256,
			"PROWL_PR_HEAD":           head256,
			"PROWL_GIT_MISSING_OID":   base256,
			"PROWL_GIT_OBJECT_FORMAT": "sha256",
		}))
		if err != nil {
			t.Fatalf("missing-base range shell failed: %v", err)
		}
		outputs := readReviewActionOutputs(t, outputPath)
		if outputs["base"] != base256 || outputs["head"] != head256 || outputs["skip"] != "false" {
			t.Errorf("missing-base range outputs = %+v", outputs)
		}
		log, err := os.ReadFile(filepath.Join(temp, "git-stub.log"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"fetch --no-tags --no-recurse-submodules --force origin",
			"+" + base256 + ":refs/prowl-review/base",
		} {
			if !strings.Contains(string(log), want) {
				t.Errorf("missing-base fetch omits %q: %s", want, log)
			}
		}
		if strings.Contains(string(log), "+"+head256+":") || strings.Contains(string(log), "refs/heads/") {
			t.Errorf("pull-request base fetch uses head or mutable ref: %s", log)
		}
	})

	t.Run("pull request rejects absent checked-out fork head", func(t *testing.T) {
		temp := t.TempDir()
		writeReviewGitStub(t, temp)
		outputPath := filepath.Join(temp, "github-output")
		err := runReviewActionShell(script, reviewActionEnvironment(temp, map[string]string{
			"GITHUB_OUTPUT":         outputPath,
			"PROWL_EVENT_NAME":      "pull_request",
			"PROWL_PR_BASE":         base,
			"PROWL_PR_HEAD":         head,
			"PROWL_GIT_MISSING_OID": head,
		}))
		if err == nil {
			t.Fatal("absent checked-out PR head succeeded")
		}
		if outputs := readReviewActionOutputs(t, outputPath); len(outputs) != 0 {
			t.Fatalf("absent PR head wrote outputs: %+v", outputs)
		}
		log, err := os.ReadFile(filepath.Join(temp, "git-stub.log"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(log), "fetch ") {
			t.Errorf("workflow fetched an absent fork head from base origin: %s", log)
		}
	})
}

func TestReviewActionWorkflowContract(t *testing.T) {
	var workflow reviewWorkflowManifest
	raw := decodeReviewYAML(t, ".github/workflows/prowl-review.yml", &workflow)

	for _, event := range []string{"pull_request", "push"} {
		if _, ok := workflow.On[event]; !ok {
			t.Errorf("workflow event %q is missing", event)
		}
	}
	if got := workflow.Permissions["contents"]; got != "read" {
		t.Errorf("workflow contents permission = %q, want read", got)
	}
	if _, ok := workflow.Permissions["security-events"]; ok {
		t.Error("workflow grants security-events permission to the untrusted review job")
	}

	reviewJob, ok := workflow.Jobs["review"]
	if !ok {
		t.Fatal("review job is missing")
	}
	checkoutIndex := reviewUsesStepIndex(reviewJob.Steps, "actions/checkout@")
	rangeIndex := reviewStepIndex(reviewJob.Steps, "refs/prowl-review/base")
	buildIndex := reviewStepIndex(reviewJob.Steps, "go build")
	actionIndex := reviewUsesStepIndex(reviewJob.Steps, "./.github/actions/prowl-review")
	if checkoutIndex < 0 || rangeIndex < 0 || buildIndex < 0 || actionIndex < 0 ||
		!(checkoutIndex < rangeIndex && rangeIndex < buildIndex && buildIndex < actionIndex) {
		t.Fatalf("checkout/range/install/action order is invalid: checkout:%d range:%d build:%d action:%d",
			checkoutIndex, rangeIndex, buildIndex, actionIndex)
	}
	checkout := reviewJob.Steps[checkoutIndex]
	if depth, ok := checkout.With["fetch-depth"].(int); !ok || depth != 0 {
		t.Errorf("checkout fetch-depth = %#v, want integer 0", checkout.With["fetch-depth"])
	}
	if ref, _ := checkout.With["ref"].(string); !strings.Contains(ref, "pull_request.head.sha") || !strings.Contains(ref, "github.sha") {
		t.Errorf("checkout ref does not select immutable PR/push heads: %#v", checkout.With["ref"])
	}

	rangeStep := reviewJob.Steps[rangeIndex]
	for _, fragment := range []string{
		`git rev-parse --show-object-format`,
		`sha256`,
		`^[0-9a-f]+$`,
		`printf -v zero_oid`,
		`git fetch --no-tags --no-recurse-submodules --force origin`,
		`refs/prowl-review/base`,
		`refs/prowl-review/head`,
		`git cat-file -e`,
	} {
		if !strings.Contains(rangeStep.Run, fragment) {
			t.Errorf("range preparation omits %q:\n%s", fragment, rangeStep.Run)
		}
	}
	if strings.Contains(rangeStep.Run, "--depth") {
		t.Error("exact push-base fetch is shallow and cannot supply ancestry")
	}
	if strings.Contains(rangeStep.Run, "{40}") || strings.Contains(rangeStep.Run, "40-hex") {
		t.Error("range preparation still assumes SHA-1 OID length")
	}
	if strings.Contains(rangeStep.Run, "refs/heads/") || strings.Contains(rangeStep.Run, "github.base_ref") {
		t.Error("range preparation relies on a mutable branch ref")
	}
	actionStep := reviewJob.Steps[actionIndex]
	if actionStep.If != "${{ steps.range.outputs.skip != 'true' }}" {
		t.Errorf("action created-push condition = %q", actionStep.If)
	}
	if got := actionStep.With["base"]; got != "${{ steps.range.outputs.base }}" {
		t.Errorf("action base = %#v, want validated range output", got)
	}
	if got := actionStep.With["head"]; got != "${{ steps.range.outputs.head }}" {
		t.Errorf("action head = %#v, want validated range output", got)
	}
	if strings.Contains(fmt.Sprint(actionStep.With["base"])+fmt.Sprint(actionStep.With["head"]), strings.Repeat("0", 40)) {
		t.Error("action forwards an all-zero object ID")
	}

	planUpload := reviewArtifactUpload(reviewJob.Steps, "${{ steps.prowl.outputs.plan }}")
	sarifUpload := reviewArtifactUpload(reviewJob.Steps, "${{ steps.prowl.outputs.sarif }}")
	if planUpload < 0 {
		t.Error("workflow does not upload the native review plan artifact")
	}
	if sarifUpload < 0 {
		t.Error("workflow does not retain the doctor SARIF artifact")
	}

	publish, ok := workflow.Jobs["publish-sarif"]
	if !ok {
		t.Fatal("privilege-separated publish-sarif job is missing")
	}
	if publish.Permissions["security-events"] != "write" {
		t.Errorf("publish-sarif security-events permission = %q, want write", publish.Permissions["security-events"])
	}
	if !strings.Contains(publish.If, "github.event_name == 'push'") ||
		!strings.Contains(publish.If, "pull_request.head.repo.full_name == github.repository") {
		t.Errorf("publish-sarif does not exclude fork PRs from the privileged path: %q", publish.If)
	}
	for _, step := range publish.Steps {
		if step.Run != "" || strings.HasPrefix(step.Uses, "./") || strings.Contains(step.Uses, "checkout") {
			t.Errorf("privileged publish-sarif job executes checked-out or shell code: %+v", step)
		}
	}
	if reviewUsesStepIndex(publish.Steps, "github/codeql-action/upload-sarif@") < 0 {
		t.Error("publish-sarif no longer uploads doctor SARIF to code scanning")
	}

	lower := strings.ToLower(raw)
	for _, claim := range []string{"prowl reviews every pull request", "doctor alone reviews", "model review comments"} {
		if strings.Contains(lower, claim) {
			t.Errorf("workflow retains unsupported semantic-review claim %q", claim)
		}
	}
}

func decodeReviewYAML(t *testing.T, relative string, target any) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, target); err != nil {
		t.Fatalf("parse %s: %v", relative, err)
	}
	return string(data)
}

func reviewStepIndex(steps []reviewActionStep, fragment string) int {
	return reviewStepIndexAfter(steps, fragment, 0)
}

func reviewStepIndexAfter(steps []reviewActionStep, fragment string, start int) int {
	for index := start; index < len(steps); index++ {
		if strings.Contains(steps[index].Run, fragment) {
			return index
		}
	}
	return -1
}

func reviewUsesStepIndex(steps []reviewActionStep, prefix string) int {
	for index := range steps {
		if strings.HasPrefix(steps[index].Uses, prefix) {
			return index
		}
	}
	return -1
}

func reviewArtifactUpload(steps []reviewActionStep, path string) int {
	for index := range steps {
		if !strings.HasPrefix(steps[index].Uses, "actions/upload-artifact@") {
			continue
		}
		if got, _ := steps[index].With["path"].(string); got == path {
			return index
		}
	}
	return -1
}
func loadReviewAction(t *testing.T) reviewActionManifest {
	t.Helper()
	var action reviewActionManifest
	decodeReviewYAML(t, ".github/actions/prowl-review/action.yml", &action)
	return action
}

func requireReviewActionShellTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required to exercise the composite action shell", tool)
		}
	}
}

func writeReviewActionStub(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "prowl-agent")
	content := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$PROWL_STUB_LOG"
if [ "${1:-}" = "review" ] && [ "${2:-}" = "plan" ]; then
	printf '%s\n' "${PROWL_PLAN_JSON:?}"
	exit 0
fi
if [ "${1:-}" = "doctor" ]; then
	case " $* " in
		*" --format sarif "*)
			printf '%s\n' '{"version":"2.1.0","runs":[]}'
			exit 0
			;;
		*" --fail-on "*)
			exit "${PROWL_DOCTOR_GATE_STATUS:-0}"
			;;
	esac
fi
exit 0
`
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	writeReviewGitStub(t, dir)
}

func reviewActionEnvironment(temp string, overrides map[string]string) []string {
	values := map[string]string{
		"PATH":                     temp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RUNNER_TEMP":              temp,
		"PROWL_STUB_LOG":           filepath.Join(temp, "prowl-stub.log"),
		"PROWL_GIT_STUB_LOG":       filepath.Join(temp, "git-stub.log"),
		"PROWL_GIT_FETCHED_MARKER": filepath.Join(temp, "git-fetched"),
	}
	for key, value := range overrides {
		values[key] = value
	}
	environment := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := values[key]; !replaced {
			environment = append(environment, entry)
		}
	}
	for key, value := range values {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func writeReviewGitStub(t *testing.T, dir string) {
	t.Helper()
	content := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$PROWL_GIT_STUB_LOG"
if [ "${1:-}" = "rev-parse" ] && [ "${2:-}" = "--show-object-format" ]; then
	printf '%s\n' "${PROWL_GIT_OBJECT_FORMAT:-sha1}"
	exit 0
fi
if [ "${1:-}" = "cat-file" ] && [ "${2:-}" = "-e" ]; then
	if [ "${3:-}" = "${PROWL_GIT_MISSING_OID:-}^{commit}" ] &&
		[ ! -e "$PROWL_GIT_FETCHED_MARKER" ]; then
		exit 1
	fi
	exit 0
fi
if [ "${1:-}" = "fetch" ]; then
	: > "$PROWL_GIT_FETCHED_MARKER"
fi
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runReviewActionStepsUntilFailure(steps []reviewActionStep, environment []string) error {
	for _, step := range steps {
		if step.Run == "" {
			continue
		}
		if err := runReviewActionShell(step.Run, environment); err != nil {
			return err
		}
	}
	return nil
}

func runReviewActionShell(script string, environment []string) error {
	command := exec.Command("bash", "-c", script)
	command.Env = environment
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func readReviewActionOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			t.Fatalf("malformed GITHUB_OUTPUT line %q", line)
		}
		outputs[key] = value
	}
	return outputs
}
