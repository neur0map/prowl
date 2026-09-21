//go:build linux

package cli

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// findProwlServers returns the PIDs of prowl serve/lsp processes matching
// scope (see matchProwlServer), skipping this process. It uses Linux /proc.
func findProwlServers(scope string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		cwd, _ := os.Readlink("/proc/" + entry.Name() + "/cwd")
		if matchProwlServer(args, cwd, scope) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// stopServers SIGTERMs the given PIDs, returning how many were signaled.
func stopServers(pids []int) int {
	n := 0
	for _, pid := range pids {
		if syscall.Kill(pid, syscall.SIGTERM) == nil {
			n++
		}
	}
	return n
}
