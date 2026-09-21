// Package workspace manages a project's .prowl/ directory, the global registry
// of initialized projects, and gitignore wiring.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/neur0map/prowl/internal/boundedio"
)

// Dir is the per-project workspace directory name.
const Dir = ".prowl"

// ErrNotFound is returned when no .prowl workspace is found.
var ErrNotFound = errors.New("no .prowl workspace found (run 'prowl init')")

// Workspace locates a project's shared canonical state and per-worktree
// derived state.
type Workspace struct {
	Root      string // active project root (the linked worktree when applicable)
	Path      string // shared configuration .prowl/
	Derived   string // worktree-specific derived state root
	DB        string // derived SQLite index
	Knowledge string // shared canonical, trackable OKF bundle
	Proposals string // shared reviewable, optionally trackable proposal inbox
	Cache     string // derived cache and vector state
	Logs      string // derived logs
}

func at(root string) *Workspace {
	d := filepath.Join(root, Dir)
	return atPaths(root, d, d)
}

func atPaths(root, shared, derived string) *Workspace {
	return &Workspace{
		Root: root, Path: shared, Derived: derived,
		DB:        filepath.Join(derived, "index.db"),
		Knowledge: filepath.Join(shared, "knowledge"),
		Proposals: filepath.Join(shared, "proposals"),
		Cache:     filepath.Join(derived, "cache"),
		Logs:      filepath.Join(derived, "logs"),
	}
}

// Create makes the .prowl/ workspace (and logs dir) under root.
func Create(root string) (*Workspace, error) {
	w := at(root)
	if err := os.MkdirAll(w.Logs, 0o755); err != nil {
		return nil, err
	}
	return w, nil
}

// ResolveContext bounds workspace discovery even when an underlying filesystem
// metadata operation cannot itself observe context cancellation. Resolve retains
// its synchronous compatibility behavior.
func ResolveContext(ctx context.Context, start string) (*Workspace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		workspace *Workspace
		err       error
	}
	resolved := make(chan result, 1)
	go func() {
		workspace, err := Resolve(start)
		resolved <- result{workspace: workspace, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-resolved:
		return result.workspace, result.err
	}
}

// Resolve walks up from start to find an existing .prowl/ workspace. A linked
// Git worktree uses its own top level and derived state while sharing only the
// primary worktree's canonical .prowl state.
func Resolve(start string) (*Workspace, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	for {
		cand := filepath.Join(dir, Dir)
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return at(dir), nil
		}
		shared, derived, boundary, err := linkedStatePaths(dir)
		if err != nil {
			return nil, err
		}
		if boundary {
			if shared != "" {
				return atPaths(dir, shared, derived), nil
			}
			return nil, ErrNotFound
		}
		if bareRepositoryBoundary(dir) {
			return nil, ErrNotFound
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, ErrNotFound
		}
		dir = parent
	}
}

const maxGitMetadataBytes int64 = 4096

// linkedStatePaths treats every existing .git entry as a repository boundary.
// A supported linked marker is accepted only when its admin directory is an
// immediate child of the common directory's worktrees/ directory, commondir
// points back to that common directory, and the bounded gitdir backlink names
// this root's exact marker.
func linkedStatePaths(root string) (shared, derived string, boundary bool, err error) {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		_, markerErr := os.Lstat(filepath.Join(root, ".git"))
		return "", "", !errors.Is(markerErr, os.ErrNotExist), nil
	}
	defer rootFS.Close()
	info, err := rootFS.Lstat(".git")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", false, nil
		}
		return "", "", true, nil
	}
	boundary = true
	if !info.Mode().IsRegular() {
		return "", "", boundary, nil
	}

	gitDir, ok := readGitPath(rootFS, ".git", root, "gitdir: ")
	if !ok {
		return "", "", boundary, nil
	}
	worktreesDir := filepath.Dir(gitDir)
	adminName := filepath.Base(gitDir)
	if filepath.Base(worktreesDir) != "worktrees" || adminName == "." || adminName == string(filepath.Separator) {
		return "", "", boundary, nil
	}
	commonDir := filepath.Dir(worktreesDir)
	if filepath.Base(commonDir) != ".git" ||
		!samePath(gitDir, filepath.Join(commonDir, "worktrees", adminName)) {
		return "", "", boundary, nil
	}

	primary := filepath.Dir(commonDir)
	primaryFS, err := os.OpenRoot(primary)
	if err != nil {
		return "", "", boundary, nil
	}
	defer primaryFS.Close()
	adminRel := filepath.Join(".git", "worktrees", adminName)
	resolvedCommon, ok := readGitPath(primaryFS, filepath.Join(adminRel, "commondir"), gitDir, "")
	if !ok || !samePath(resolvedCommon, commonDir) {
		return "", "", boundary, nil
	}
	backlink, ok := readGitPath(primaryFS, filepath.Join(adminRel, "gitdir"), gitDir, "")
	if !ok || !samePath(backlink, filepath.Join(root, ".git")) {
		return "", "", boundary, nil
	}
	state, err := primaryFS.Lstat(Dir)
	if err != nil || !state.IsDir() {
		return "", "", boundary, nil
	}

	derivedRel := filepath.Join(adminRel, "prowl")
	if err := primaryFS.MkdirAll(filepath.Join(derivedRel, "logs"), 0o755); err != nil {
		return "", "", boundary, err
	}
	return filepath.Join(primary, Dir), filepath.Join(gitDir, "prowl"), boundary, nil
}

func readGitPath(root *os.Root, name, relativeTo, prefix string) (string, bool) {
	file, err := boundedio.OpenRegularNoFollow(root, name)
	if err != nil {
		return "", false
	}
	defer file.Close()
	data, err := boundedio.ReadAllContext(context.Background(), file, maxGitMetadataBytes)
	if err != nil || len(data) == 0 || bytes.IndexByte(data, 0) >= 0 {
		return "", false
	}
	value := strings.TrimSuffix(string(data), "\n")
	value = strings.TrimSuffix(value, "\r")
	if strings.ContainsAny(value, "\r\n") || !strings.HasPrefix(value, prefix) {
		return "", false
	}
	value = strings.TrimPrefix(value, prefix)
	if value == "" {
		return "", false
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(relativeTo, value)
	}
	return filepath.Clean(value), true
}

func samePath(a, b string) bool {
	relative, err := filepath.Rel(a, b)
	return err == nil && relative == "."
}

// bareRepositoryBoundary recognizes the structural metadata of a bare
// repository. HEAD and config are opened through the same bounded no-follow
// path used for linked metadata, so corrupt special files cannot block
// discovery. Once all four bare markers exist, any unsupported shape fails
// closed as a boundary.
func bareRepositoryBoundary(dir string) bool {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer root.Close()
	for _, name := range []string{"HEAD", "config", "objects", "refs"} {
		if _, err := root.Lstat(name); err != nil {
			return !errors.Is(err, os.ErrNotExist)
		}
	}
	_, _ = readGitPath(root, "HEAD", dir, "")
	_, _ = readGitPath(root, "config", dir, "")
	return true
}
