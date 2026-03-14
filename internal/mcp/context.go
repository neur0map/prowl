package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileContext holds all context data for a single file, assembled from the
// .prowl/context/<path>/ directory structure.
type FileContext struct {
	Path       string   `json:"path"`
	Community  string   `json:"community,omitempty"`
	Exports    []string `json:"exports"`
	Signatures []string `json:"signatures"`
	Calls      []string `json:"calls"`
	Callers    []string `json:"callers"`
	Imports    []string `json:"imports"`
	Upstream   []string `json:"upstream"`
}

// readFileContext reads the 7 context files for a given source file from disk.
// contextDir is the root .prowl/context/ directory. filePath is the project-relative path.
// Returns an error if the context directory for this file doesn't exist.
func readFileContext(contextDir, filePath string) (*FileContext, error) {
	dir := filepath.Join(contextDir, filePath)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, fmt.Errorf("no context for file: %s", filePath)
	}

	fc := &FileContext{
		Path:       filePath,
		Community:  readSingleLine(filepath.Join(dir, ".community")),
		Exports:    readLines(filepath.Join(dir, ".exports")),
		Signatures: readLines(filepath.Join(dir, ".signatures")),
		Calls:      readLines(filepath.Join(dir, ".calls")),
		Callers:    readLines(filepath.Join(dir, ".callers")),
		Imports:    readLines(filepath.Join(dir, ".imports")),
		Upstream:   readLines(filepath.Join(dir, ".upstream")),
	}
	return fc, nil
}

// readLines reads a file and returns non-empty lines.
func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// readSingleLine reads the first non-empty line from a file.
func readSingleLine(path string) string {
	lines := readLines(path)
	if len(lines) > 0 {
		return lines[0]
	}
	return ""
}
