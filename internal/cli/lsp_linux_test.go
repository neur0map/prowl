//go:build linux

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/config"
	lspserver "github.com/neur0map/prowl/internal/lsp"
)

func TestLSPRunCLIInterruptsInheritedStdin(t *testing.T) {
	if os.Getenv("PROWL_CLI_LSP_STDIN_HELPER") == "1" {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		srv := lspserver.New("", "", nil, config.Rules{}, nil)
		if err := runLSP(ctx, srv, os.Stdin, io.Discard); !errors.Is(err, context.Canceled) {
			os.Exit(2)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLSPRunCLIInterruptsInheritedStdin$")
	cmd.Env = append(os.Environ(), "PROWL_CLI_LSP_STDIN_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		_ = stdin.Close()
		if err != nil {
			t.Fatalf("CLI inherited-stdin helper: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		_ = stdin.Close()
		<-done
		t.Fatal("runLSP remained blocked on canceled inherited stdin")
	}
}
