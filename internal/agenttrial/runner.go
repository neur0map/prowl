// Package agenttrial runs supported coding-agent clients under hard, measured budgets.
package agenttrial

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	ErrTokenLimit    = errors.New("agent trial model-token limit exceeded")
	ErrToolLimit     = errors.New("agent trial tool-call limit exceeded")
	ErrSubagentLimit = errors.New("agent trial subagent limit exceeded")
	ErrTimeout       = errors.New("agent trial timed out")
	ErrOutputLimit   = errors.New("agent trial output limit exceeded")
	ErrMissingUsage  = errors.New("agent trial did not report token usage")
)

type Budget struct {
	MaxModelTokens int64         `json:"max_model_tokens"`
	MaxToolCalls   int64         `json:"max_tool_calls"`
	MaxSubagents   int64         `json:"max_subagents"`
	Timeout        time.Duration `json:"timeout"`
}

type ClientConfig struct {
	Client              string   `json:"client"`
	Model               string   `json:"model,omitempty"`
	ClaudeBinary        string   `json:"claude_binary,omitempty"`
	OMPBinary           string   `json:"omp_binary,omitempty"`
	MaxOutputBytes      int64    `json:"max_output_bytes"`
	Skills              []string `json:"skills,omitempty"`
	PluginDirs          []string `json:"plugin_dirs,omitempty"`
	Extensions          []string `json:"extensions,omitempty"`
	DisableAmbientRules bool     `json:"disable_ambient_rules"`
	Budget              Budget   `json:"budget"`
	ConfigDir           string   `json:"-"`
	Environment         []string `json:"-"`
	RequireTokenUsage   bool     `json:"-"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	ModelTokens  int64 `json:"model_tokens"`
	ToolCalls    int64 `json:"tool_calls"`
	Subagents    int64 `json:"subagents"`
}

type Result struct {
	Stdout        []byte        `json:"stdout"`
	Stderr        []byte        `json:"stderr"`
	Usage         Usage         `json:"usage"`
	Elapsed       time.Duration `json:"elapsed"`
	ExitCode      int           `json:"exit_code"`
	UsageReported bool          `json:"usage_reported"`
	Limit         string        `json:"limit,omitempty"`
}

func Run(parent context.Context, workDir, prompt string, cfg ClientConfig) (Result, error) {
	if workDir == "" {
		return Result{}, errors.New("agent trial work directory is required")
	}
	args, binary, err := clientCommand(cfg, prompt)
	if err != nil {
		return Result{}, err
	}
	ctx := parent
	cancel := func() {}
	if cfg.Budget.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, cfg.Budget.Timeout)
	}
	defer cancel()
	cmd := exec.Command(binary, args...)
	cmd.Dir = workDir
	if cfg.Environment != nil {
		cmd.Env = append([]string(nil), cfg.Environment...)
	} else {
		cmd.Env = os.Environ()
	}
	if cfg.ConfigDir != "" {
		if err := prepareConfigDir(cfg); err != nil {
			return Result{}, err
		}
		key := "PI_CODING_AGENT_DIR"
		if cfg.Client == "claude" {
			key = "CLAUDE_CONFIG_DIR"
		}
		cmd.Env = replaceEnv(cmd.Env, key, cfg.ConfigDir)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	state := newStreamState(cfg)
	cmd.Stdout = streamWriter{state: state, stdout: true}
	cmd.Stderr = streamWriter{state: state}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	start := time.Now()
	var killOnce sync.Once
	kill := func() { killOnce.Do(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }) }
	state.kill = kill
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()

	var processErr error
	select {
	case processErr = <-wait:
	case <-state.exceeded:
		kill()
		processErr = <-wait
	case <-ctx.Done():
		kill()
		processErr = <-wait
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			state.setLimit(ErrTimeout)
		}
		if parent.Err() != nil && state.limitErr() == nil {
			state.setLimit(parent.Err())
		}
	}
	result := state.result(time.Since(start), processErr)
	if limitErr := state.limitErr(); limitErr != nil {
		return result, limitErr
	}
	if processErr != nil {
		return result, processErr
	}
	if cfg.RequireTokenUsage && !result.UsageReported {
		return result, ErrMissingUsage
	}
	return result, nil
}

func clientCommand(cfg ClientConfig, prompt string) ([]string, string, error) {
	switch cfg.Client {
	case "claude":
		binary := cfg.ClaudeBinary
		if binary == "" {
			binary = "claude"
		}
		args := []string{"-p", "--verbose", "--no-session-persistence", "--output-format", "stream-json", "--include-hook-events", "--setting-sources", "project", "--permission-mode", "dontAsk", "--allowedTools", "Bash,Read,Grep,Glob,Agent,Skill", "--disallowedTools", "Edit,Write,Notebook"}
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
		for _, pluginDir := range cfg.PluginDirs {
			args = append(args, "--plugin-dir", pluginDir)
		}
		return append(args, prompt), binary, nil
	case "omp":
		binary := cfg.OMPBinary
		if binary == "" {
			binary = "omp"
		}
		args := []string{"-p", "--mode", "json", "--no-session", "--no-title", "--tools", "read,bash,grep,glob,lsp"}
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
		if cfg.DisableAmbientRules {
			if len(cfg.Skills) == 0 {
				args = append(args, "--no-skills")
			} else {
				args = append(args, "--skills", strings.Join(cfg.Skills, ","))
			}
			if len(cfg.Extensions) == 0 {
				args = append(args, "--no-extensions")
			}
			args = append(args, "--no-rules")
		} else if len(cfg.Skills) > 0 {
			args = append(args, "--skills", strings.Join(cfg.Skills, ","))
		}
		for _, extension := range cfg.Extensions {
			args = append(args, "-e", extension)
		}
		return append(args, prompt), binary, nil
	default:
		return nil, "", fmt.Errorf("unsupported agent client %q", cfg.Client)
	}
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

func prepareConfigDir(cfg ClientConfig) error {
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return err
	}
	if cfg.Client != "claude" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	source := filepath.Join(home, ".claude", ".credentials.json")
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("Claude credentials are not a regular file")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(filepath.Join(cfg.ConfigDir, ".credentials.json"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Close())
}

type streamWriter struct {
	state  *streamState
	stdout bool
}

func (writer streamWriter) Write(data []byte) (int, error) {
	writer.state.add(data, writer.stdout)
	return len(data), nil
}

type streamState struct {
	mu            sync.Mutex
	cfg           ClientConfig
	stdout        bytes.Buffer
	stderr        bytes.Buffer
	stdoutLine    []byte
	usage         Usage
	usageReported bool
	toolIDs       map[string]struct{}
	subagentIDs   map[string]struct{}
	usageIDs      map[string]struct{}
	totalBytes    int64
	limit         error
	exceeded      chan struct{}
	exceedOnce    sync.Once
	kill          func()
}

func newStreamState(cfg ClientConfig) *streamState {
	return &streamState{cfg: cfg, toolIDs: map[string]struct{}{}, subagentIDs: map[string]struct{}{}, usageIDs: map[string]struct{}{}, exceeded: make(chan struct{})}
}

func (s *streamState) add(data []byte, stdout bool) {
	s.mu.Lock()
	remaining := int64(len(data))
	keep := remaining
	if s.cfg.MaxOutputBytes > 0 {
		available := s.cfg.MaxOutputBytes - s.totalBytes
		if available < 0 {
			available = 0
		}
		if keep > available {
			keep = available
		}
	}
	if keep > 0 {
		chunk := data[:keep]
		if stdout {
			s.stdout.Write(chunk)
			s.stdoutLine = append(s.stdoutLine, chunk...)
			s.parseLinesLocked()
		} else {
			s.stderr.Write(chunk)
		}
	}
	s.totalBytes += remaining
	overflow := s.cfg.MaxOutputBytes > 0 && s.totalBytes > s.cfg.MaxOutputBytes
	if overflow && s.limit == nil {
		s.limit = ErrOutputLimit
	}
	limit := s.checkBudgetsLocked()
	s.mu.Unlock()
	if overflow || limit != nil {
		s.signalExceeded()
	}
}

func (s *streamState) parseLinesLocked() {
	for {
		index := bytes.IndexByte(s.stdoutLine, '\n')
		if index < 0 {
			return
		}
		line := bytes.TrimSpace(append([]byte(nil), s.stdoutLine[:index]...))
		s.stdoutLine = append(s.stdoutLine[:0], s.stdoutLine[index+1:]...)
		var event any
		if json.Unmarshal(line, &event) == nil {
			s.collectEventLocked(event)
		}
	}
}

func (s *streamState) collectEventLocked(value any) {
	switch event := value.(type) {
	case []any:
		for _, child := range event {
			s.collectEventLocked(child)
		}
	case map[string]any:
		typeName := strings.ToLower(textField(event, "type"))
		if usage, ok := event["usage"].(map[string]any); ok {
			s.collectUsageLocked(usage, typeName, textField(event, "id", "message_id"))
		}
		name := textField(event, "name", "tool_name", "toolName")
		id := textField(event, "id", "tool_call_id", "toolCallId", "tool_use_id")
		if nested, ok := event["tool_call"].(map[string]any); ok {
			name, id, typeName = textField(nested, "name", "tool_name"), textField(nested, "id", "tool_call_id"), "tool_call"
		}
		if nested, ok := event["toolCall"].(map[string]any); ok {
			name, id, typeName = textField(nested, "name", "toolName"), textField(nested, "id", "toolCallId"), "tool_call"
		}
		if name != "" && strings.Contains(typeName, "tool") {
			key := id
			if key == "" {
				key = fmt.Sprintf("ordinal:%d", s.usage.ToolCalls)
			}
			if _, exists := s.toolIDs[key]; !exists {
				s.toolIDs[key] = struct{}{}
				s.usage.ToolCalls++
				if isSubagent(name) {
					s.subagentIDs[key] = struct{}{}
					s.usage.Subagents++
				}
			}
		}
		if strings.Contains(typeName, "subagent") {
			key := id
			if key == "" {
				key = fmt.Sprintf("event:%d", s.usage.Subagents)
			}
			if _, exists := s.subagentIDs[key]; !exists {
				s.subagentIDs[key] = struct{}{}
				s.usage.Subagents++
			}
		}
		for key, child := range event {
			if key != "usage" && key != "input" && key != "args" && key != "arguments" && key != "tool_input" && key != "tool_call" && key != "toolCall" {
				s.collectEventLocked(child)
			}
		}
	}
}

func (s *streamState) collectUsageLocked(usage map[string]any, eventType, eventID string) {
	input := integerField(usage, "input_tokens", "inputTokens", "prompt_tokens")
	input += integerField(usage, "cache_creation_input_tokens", "cacheCreationInputTokens")
	input += integerField(usage, "cache_read_input_tokens", "cacheReadInputTokens")
	output := integerField(usage, "output_tokens", "outputTokens", "completion_tokens")
	total := integerField(usage, "model_tokens", "total_tokens", "totalTokens")
	if _, present := firstNumber(usage, "input_tokens", "inputTokens", "prompt_tokens", "output_tokens", "outputTokens", "completion_tokens", "model_tokens", "total_tokens", "totalTokens"); present {
		s.usageReported = true
	}
	if eventID != "" {
		data, _ := json.Marshal(usage)
		key := eventType + "|" + eventID + "|" + string(data)
		if _, duplicate := s.usageIDs[key]; duplicate {
			return
		}
		s.usageIDs[key] = struct{}{}
	}
	if total < input+output {
		total = input + output
	}
	cumulative := eventType == "result" || eventType == "agent_end" || eventType == "message_end"
	if cumulative {
		if total >= s.usage.ModelTokens {
			s.usage.InputTokens = input
			s.usage.OutputTokens = output
			s.usage.ModelTokens = total
		}
		return
	}
	s.usage.InputTokens += input
	s.usage.OutputTokens += output
	s.usage.ModelTokens += total
}

func (s *streamState) checkBudgetsLocked() error {
	if s.limit != nil {
		return s.limit
	}
	if max := s.cfg.Budget.MaxModelTokens; max > 0 && s.usage.ModelTokens > max {
		s.limit = ErrTokenLimit
	}
	if max := s.cfg.Budget.MaxToolCalls; s.limit == nil && max > 0 && s.usage.ToolCalls > max {
		s.limit = ErrToolLimit
	}
	if max := s.cfg.Budget.MaxSubagents; s.limit == nil && max > 0 && s.usage.Subagents > max {
		s.limit = ErrSubagentLimit
	}
	return s.limit
}

func (s *streamState) signalExceeded() {
	s.exceedOnce.Do(func() {
		close(s.exceeded)
		if s.kill != nil {
			s.kill()
		}
	})
}

func (s *streamState) setLimit(err error) {
	s.mu.Lock()
	if s.limit == nil {
		s.limit = err
	}
	s.mu.Unlock()
}
func (s *streamState) limitErr() error { s.mu.Lock(); defer s.mu.Unlock(); return s.limit }

func (s *streamState) result(elapsed time.Duration, processErr error) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.stdoutLine) > 0 {
		var event any
		if json.Unmarshal(bytes.TrimSpace(s.stdoutLine), &event) == nil {
			s.collectEventLocked(event)
		}
	}
	s.checkBudgetsLocked()
	exitCode := 0
	if processErr != nil {
		exitCode = -1
		var exit *exec.ExitError
		if errors.As(processErr, &exit) {
			exitCode = exit.ExitCode()
		}
	}
	result := Result{Stdout: append([]byte(nil), s.stdout.Bytes()...), Stderr: append([]byte(nil), s.stderr.Bytes()...), Usage: s.usage, Elapsed: elapsed, ExitCode: exitCode, UsageReported: s.usageReported}
	if s.limit != nil {
		result.Limit = s.limit.Error()
	}
	return result
}

func textField(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok {
			return text
		}
	}
	return ""
}
func integerField(value map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch number := value[key].(type) {
		case float64:
			return int64(number)
		case json.Number:
			parsed, _ := number.Int64()
			return parsed
		}
	}
	return 0
}
func firstNumber(value map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch number := value[key].(type) {
		case float64:
			return int64(number), true
		case json.Number:
			parsed, err := number.Int64()
			return parsed, err == nil
		}
	}
	return 0, false
}
func isSubagent(name string) bool {
	name = strings.ToLower(filepath.Base(name))
	return name == "agent" || name == "task" || name == "subagent"
}
