package agenttrial

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClaudeCommandLoadsOnlyExplicitPluginDirectories(t *testing.T) {
	args, _, err := clientCommand(ClientConfig{
		Client:     "claude",
		Skills:     []string{"omp-skill-name"},
		PluginDirs: []string{"/private/review-plugin"},
	}, "review")
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Join(args, "\x00")
	if !strings.Contains(command, "--plugin-dir\x00/private/review-plugin") {
		t.Fatalf("Claude plugin missing: %q", args)
	}
	if strings.Contains(command, "omp-skill-name") {
		t.Fatalf("OMP skill leaked into Claude arguments: %q", args)
	}
}

func TestRunStreamsUsageAndEnforcesMaxPlusOneOutput(t *testing.T) {
	binary := fakeClient(t, `#!/bin/sh
printf '%s\n' '{"type":"assistant","usage":{"input_tokens":2,"output_tokens":3}}'
printf 'x%.0s' $(seq 1 65)
`)
	result, err := Run(context.Background(), t.TempDir(), "review", ClientConfig{
		Client: "omp", OMPBinary: binary, MaxOutputBytes: 128,
		Budget: Budget{MaxModelTokens: 10, MaxToolCalls: 2, MaxSubagents: 1, Timeout: time.Second},
	})
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("error = %v", err)
	}
	if len(result.Stdout)+len(result.Stderr) != 128 {
		t.Fatalf("retained output = %d", len(result.Stdout)+len(result.Stderr))
	}
	if result.Usage.ModelTokens != 5 {
		t.Fatalf("tokens = %d", result.Usage.ModelTokens)
	}
}

func TestRunRejectsMissingTokenUsageWhenRequired(t *testing.T) {
	binary := fakeClient(t, "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"done\"}'\n")
	_, err := Run(context.Background(), t.TempDir(), "review", ClientConfig{Client: "claude", ClaudeBinary: binary, RequireTokenUsage: true, Budget: Budget{Timeout: time.Second}})
	if !errors.Is(err, ErrMissingUsage) {
		t.Fatalf("error = %v", err)
	}
}

func TestRunAggregatesSupportedUsageAndTokenLimit(t *testing.T) {
	binary := fakeClient(t, `#!/bin/sh
printf '%s\n' '{"type":"assistant","id":"a","usage":{"input_tokens":1,"output_tokens":1}}'
printf '%s\n' '{"type":"assistant","id":"b","usage":{"input_tokens":1,"output_tokens":2}}'
sleep 30
`)
	result, err := Run(context.Background(), t.TempDir(), "review", ClientConfig{Client: "claude", ClaudeBinary: binary, Budget: Budget{MaxModelTokens: 4, Timeout: 5 * time.Second}})
	if !errors.Is(err, ErrTokenLimit) {
		t.Fatalf("error = %v", err)
	}
	if result.Usage.InputTokens != 2 || result.Usage.OutputTokens != 3 || result.Usage.ModelTokens != 5 {
		t.Fatalf("usage = %#v", result.Usage)
	}
}

func TestRunUsesFinalCumulativeUsageSnapshot(t *testing.T) {
	binary := fakeClient(t, `#!/bin/sh
printf '%s\n' '{"type":"assistant","usage":{"input_tokens":1,"output_tokens":1}}'
printf '%s\n' '{"type":"result","usage":{"input_tokens":3,"output_tokens":5}}'
`)
	result, err := Run(context.Background(), t.TempDir(), "review", ClientConfig{Client: "claude", ClaudeBinary: binary, RequireTokenUsage: true, Budget: Budget{Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.InputTokens != 3 || result.Usage.OutputTokens != 5 || result.Usage.ModelTokens != 8 {
		t.Fatalf("usage = %#v", result.Usage)
	}
}

func TestRunAcceptsReportedZeroTokenUsage(t *testing.T) {
	binary := fakeClient(t, "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}'\n")
	result, err := Run(context.Background(), t.TempDir(), "review", ClientConfig{Client: "claude", ClaudeBinary: binary, RequireTokenUsage: true, Budget: Budget{Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.ModelTokens != 0 {
		t.Fatalf("usage = %#v", result.Usage)
	}
}

func TestRunEnforcesSubagentAndTimeoutLimits(t *testing.T) {
	t.Run("subagents", func(t *testing.T) {
		binary := fakeClient(t, `#!/bin/sh
printf '%s\n' '{"type":"tool_call","id":"a","name":"Agent"}'
printf '%s\n' '{"type":"tool_call","id":"b","name":"task"}'
sleep 30
`)
		_, err := Run(context.Background(), t.TempDir(), "review", ClientConfig{Client: "omp", OMPBinary: binary, Budget: Budget{MaxSubagents: 1, Timeout: 5 * time.Second}})
		if !errors.Is(err, ErrSubagentLimit) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		binary := fakeClient(t, "#!/bin/sh\nsleep 30\n")
		_, err := Run(context.Background(), t.TempDir(), "review", ClientConfig{Client: "omp", OMPBinary: binary, Budget: Budget{Timeout: 20 * time.Millisecond}})
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRunKillsProcessGroupOnToolLimit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	binary := fakeClient(t, `#!/bin/sh
(sleep 30) &
echo $! > "$AGENTTRIAL_PID_FILE"
printf '%s\n' '{"type":"tool_call","id":"1","name":"read"}'
printf '%s\n' '{"type":"tool_call","id":"2","name":"read"}'
sleep 30
`)
	t.Setenv("AGENTTRIAL_PID_FILE", pidFile)
	_, err := Run(context.Background(), t.TempDir(), "review", ClientConfig{Client: "omp", OMPBinary: binary, Budget: Budget{MaxToolCalls: 1, Timeout: 5 * time.Second}})
	if !errors.Is(err, ErrToolLimit) {
		t.Fatalf("error = %v", err)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("grandchild %d still alive", pid)
}

func fakeClient(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
