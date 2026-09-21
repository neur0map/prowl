// Package wip investigates work in progress: the files an editor or agent has
// touched but not committed, plus the unfinished-work markers left inside them.
// It answers "what was I in the middle of?" so a fresh agent can resume without
// reading the whole tree. All output is bounded and paths stay relative to the
// git root.
package wip

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neur0map/prowl/internal/query"
)

// DefaultMarkers are the unfinished-work tags scanned inside uncommitted files.
var DefaultMarkers = []string{"TODO", "FIXME", "HACK", "XXX", "BUG", "WIP", "OPTIMIZE"}

const (
	defaultMaxFileBytes = 1 << 20 // skip marker scan on files larger than 1 MiB
	defaultMaxMarkers   = 50      // cap markers reported per file
	maxMarkerText       = 200     // trim each marker line to this many bytes
	maxGitOutput        = 1 << 20 // bound git stdout
	gitTimeout          = 5 * time.Second
)

// Blaster computes the structural blast radius of a project-relative file. It is
// satisfied by *query.Querier; the interface keeps this package testable.
type Blaster interface {
	BlastSummarize(path string) (query.BlastSummary, error)
}

// Options tunes the investigation. Zero values fall back to sane defaults.
type Options struct {
	Markers      []string
	MaxFileBytes int64
	MaxMarkers   int
}

func (o Options) markers() []string {
	if len(o.Markers) == 0 {
		return DefaultMarkers
	}
	return o.Markers
}

func (o Options) maxFileBytes() int64 {
	if o.MaxFileBytes <= 0 {
		return defaultMaxFileBytes
	}
	return o.MaxFileBytes
}

func (o Options) maxMarkers() int {
	if o.MaxMarkers <= 0 {
		return defaultMaxMarkers
	}
	return o.MaxMarkers
}

// Report is the full picture of uncommitted work, ready to serialize.
type Report struct {
	Clean  bool         `json:"clean"`
	Counts Counts       `json:"counts"`
	Files  []FileReport `json:"files"`
}

// Counts is a one-line summary an agent can read before the per-file detail.
type Counts struct {
	Staged    int `json:"staged"`
	Modified  int `json:"modified"`
	Untracked int `json:"untracked"`
	Deleted   int `json:"deleted"`
	Markers   int `json:"markers"`
}

// FileReport is one uncommitted path with its status, unfinished markers, and
// (when indexed) downstream blast radius.
type FileReport struct {
	Path    string   `json:"path"`
	Status  string   `json:"status"`
	Indexed bool     `json:"indexed"`
	Markers []Marker `json:"markers,omitempty"`
	Impact  *Impact  `json:"impact,omitempty"`
}

// Marker is one unfinished-work tag found in a working-tree file.
type Marker struct {
	Line int    `json:"line"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Impact mirrors the blast-radius summary used by the changed command.
type Impact struct {
	Total       int                    `json:"total"`
	Direct      int                    `json:"direct"`
	BySubsystem []query.SubsystemCount `json:"by_subsystem,omitempty"`
	DirectFiles []string               `json:"direct_files,omitempty"`
}

type fileState struct {
	staged, modified, untracked, deleted bool
}

// Investigate enumerates uncommitted work under root, scans each surviving file
// for markers, and attaches blast radius for indexed paths. indexed reports
// whether a project-relative path is in the index (nil means unknown, which
// leaves Indexed false and skips impact). blaster may be nil to skip impact.
func Investigate(ctx context.Context, root string, indexed map[string]bool, blaster Blaster, opts Options) (Report, error) {
	if root == "" {
		return Report{Clean: true, Files: []FileReport{}}, nil
	}
	states, err := collectStates(ctx, root)
	if err != nil {
		return Report{}, err
	}
	paths := make([]string, 0, len(states))
	for path := range states {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	pattern, err := markerPattern(opts.markers())
	if err != nil {
		return Report{}, err
	}
	report := Report{Files: make([]FileReport, 0, len(paths))}
	for _, path := range paths {
		state := states[path]
		fr := FileReport{Path: path, Status: state.label(), Indexed: indexed[path]}
		if !state.deleted {
			markers := scanMarkers(filepath.Join(root, filepath.FromSlash(path)), pattern, opts.maxFileBytes(), opts.maxMarkers())
			fr.Markers = markers
			report.Counts.Markers += len(markers)
		}
		if fr.Indexed && blaster != nil {
			if sum, err := blaster.BlastSummarize(path); err == nil {
				fr.Impact = &Impact{Total: sum.Total, Direct: sum.Direct, BySubsystem: sum.BySubsystem, DirectFiles: sum.DirectFiles}
			}
		}
		countState(&report.Counts, state)
		report.Files = append(report.Files, fr)
	}
	report.Clean = len(report.Files) == 0
	return report, nil
}

func (s fileState) label() string {
	switch {
	case s.untracked:
		return "untracked"
	case s.deleted:
		return "deleted"
	case s.staged && s.modified:
		return "staged+modified"
	case s.staged:
		return "staged"
	default:
		return "modified"
	}
}

func countState(c *Counts, s fileState) {
	switch {
	case s.untracked:
		c.Untracked++
	case s.deleted:
		c.Deleted++
	default:
		if s.staged {
			c.Staged++
		}
		if s.modified {
			c.Modified++
		}
	}
}

// collectStates merges staged, unstaged, and untracked git views into one map of
// project-relative slash paths.
func collectStates(ctx context.Context, root string) (map[string]fileState, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	states := map[string]fileState{}
	applyNameStatus := func(args []string, mark func(*fileState, byte)) error {
		out, err := gitOutput(ctx, root, args...)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) < 2 {
				continue
			}
			code := fields[0][0]
			path := filepath.ToSlash(fields[len(fields)-1])
			state := states[path]
			mark(&state, code)
			states[path] = state
		}
		return nil
	}
	if err := applyNameStatus([]string{"diff", "--name-status", "--cached"}, func(s *fileState, code byte) {
		s.staged = true
		if code == 'D' {
			s.deleted = true
		}
	}); err != nil {
		return nil, err
	}
	if err := applyNameStatus([]string{"diff", "--name-status"}, func(s *fileState, code byte) {
		if code == 'D' {
			s.deleted = true
		} else {
			s.modified = true
		}
	}); err != nil {
		return nil, err
	}
	out, err := gitOutput(ctx, root, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		path := filepath.ToSlash(line)
		state := states[path]
		state.untracked = true
		states[path] = state
	}
	return states, nil
}

func markerPattern(markers []string) (*regexp.Regexp, error) {
	quoted := make([]string, 0, len(markers))
	for _, marker := range markers {
		marker = strings.TrimSpace(marker)
		if marker != "" {
			quoted = append(quoted, regexp.QuoteMeta(marker))
		}
	}
	if len(quoted) == 0 {
		return nil, fmt.Errorf("no markers to scan")
	}
	return regexp.Compile(`\b(` + strings.Join(quoted, "|") + `)\b`)
}

// scanMarkers reads a working-tree file and returns its unfinished-work markers.
// Missing, oversized, or binary files yield no markers rather than an error, so
// one unreadable file never fails the whole report.
func scanMarkers(absPath string, pattern *regexp.Regexp, maxBytes int64, maxMarkers int) []Marker {
	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() || info.Size() > maxBytes {
		return nil
	}
	data, err := os.ReadFile(absPath)
	if err != nil || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil
	}
	var markers []Marker
	line := 0
	for _, raw := range strings.Split(string(data), "\n") {
		line++
		loc := pattern.FindStringIndex(raw)
		if loc == nil {
			continue
		}
		text := strings.TrimSpace(raw)
		if len(text) > maxMarkerText {
			text = text[:maxMarkerText]
		}
		markers = append(markers, Marker{Line: line, Kind: raw[loc[0]:loc[1]], Text: text})
		if len(markers) >= maxMarkers {
			break
		}
	}
	return markers
}

func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = root
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	output := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, readErr := stdout.Read(buf)
		output = append(output, buf[:n]...)
		if int64(len(output)) > maxGitOutput {
			_ = command.Process.Kill()
			_ = command.Wait()
			return nil, fmt.Errorf("git %s output exceeds %d bytes", strings.Join(args, " "), maxGitOutput)
		}
		if readErr != nil {
			break
		}
	}
	if err := command.Wait(); err != nil {
		return nil, fmt.Errorf("git %s failed (is this a git repo?): %w", strings.Join(args, " "), err)
	}
	return output, nil
}
