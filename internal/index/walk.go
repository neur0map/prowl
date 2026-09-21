// Package index walks a project, hashes files, drives Tree-sitter extraction, and
// keeps the SQLite graph incrementally up to date.
package index

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cespare/xxhash/v2"

	"github.com/neur0map/prowl/internal/boundedio"
)

// alwaysSkipDirs are never walked.
var alwaysSkipDirs = map[string]bool{
	".git": true, ".prowl": true, "node_modules": true,
	".cursor": true, ".vscode": true, ".zed": true, ".idea": true, ".helix": true,
	".omp": true, ".factory": true,
}

// sourceCandidate is an accepted regular source file. Bounded walks keep the
// validated descriptor open so inspection cannot re-open a different target.
type sourceCandidate struct {
	path string
	file *os.File
}

func (candidate sourceCandidate) close() {
	if candidate.file != nil {
		_ = candidate.file.Close()
	}
}

func canonicalSourceRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func isWithinSourceRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func isSymlinkEntry(entry fs.DirEntry) bool {
	if entry.Type()&fs.ModeSymlink != 0 {
		return true
	}
	info, err := entry.Info()
	return err == nil && info.Mode()&fs.ModeSymlink != 0
}

func classifySourceCandidate(root, canonicalRoot, rel string, entry fs.DirEntry) (sourceCandidate, bool, error) {
	info, err := entry.Info()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return sourceCandidate{}, false, nil
		}
		return sourceCandidate{}, false, err
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	if info.Mode()&fs.ModeSymlink != 0 {
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			// A dangling or otherwise unresolved link cannot be a source file.
			return sourceCandidate{}, false, nil
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return sourceCandidate{}, false, err
		}
		if !isWithinSourceRoot(canonicalRoot, path) {
			return sourceCandidate{}, false, nil
		}
		info, err = os.Stat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return sourceCandidate{}, false, nil
			}
			return sourceCandidate{}, false, err
		}
	}
	if !info.Mode().IsRegular() {
		return sourceCandidate{}, false, nil
	}
	return sourceCandidate{path: path}, true, nil
}

func classifyPinnedSourceCandidate(root *os.Root, rel string, entry fs.DirEntry) (sourceCandidate, bool, error) {
	file, err := boundedio.OpenRegular(root, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, boundedio.ErrNonRegular) || isSymlinkEntry(entry) {
			return sourceCandidate{}, false, nil
		}
		return sourceCandidate{}, false, err
	}
	return sourceCandidate{file: file}, true, nil
}

// walkFilesContext invokes fn for each non-ignored file under root, honoring .gitignore
// (the root one and every nested one, each scoped to its own directory) and
// extra ignore globs, and always skipping .prowl/, .git/, node_modules/.
func walkFilesContext(ctx context.Context, root string, ignore []string, fn func(rel string, d fs.DirEntry) error) error {
	gitignores := map[string][]string{"": loadGitignore(root)} // rel dir -> patterns
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if alwaysSkipDirs[d.Name()] || ignoredBy(gitignores, ignore, rel, true) {
				return filepath.SkipDir
			}
			if pats := loadGitignore(p); len(pats) > 0 {
				gitignores[rel] = pats // applies to this directory's subtree
			}
			return nil
		}
		if ignoredBy(gitignores, ignore, rel, false) {
			return nil
		}
		return fn(rel, d)
	})
}

// walkSourceFilesContext applies the source-candidate contract to the
// unbounded legacy traversal: only regular files inside the resolved root reach
// fn.
func walkSourceFilesContext(ctx context.Context, root string, ignore []string, fn func(rel string, d fs.DirEntry, candidate sourceCandidate) error) error {
	canonicalRoot, err := canonicalSourceRoot(root)
	if err != nil {
		return err
	}
	return walkFilesContext(ctx, root, ignore, func(rel string, entry fs.DirEntry) error {
		candidate, accepted, err := classifySourceCandidate(root, canonicalRoot, rel, entry)
		if err != nil {
			return err
		}
		if !accepted {
			return nil
		}
		defer candidate.close()
		return fn(rel, entry, candidate)
	})
}

// walkSourceFilesCandidateLimitContext applies the same source-candidate
// contract through a pinned root. It reads directory entries in bounded batches
// so a single huge directory cannot postpone cancellation by forcing
// filepath.WalkDir to load and sort it. The max+1 accepted candidate is rejected
// before inspection or hashing.
func walkSourceFilesCandidateLimitContext(ctx context.Context, root *os.Root, ignore []string, maxCandidates int, fn func(rel string, d fs.DirEntry, candidate sourceCandidate) error) error {
	rootPatterns, err := loadPinnedGitignore(ctx, root, "")
	if err != nil {
		return err
	}
	gitignores := map[string][]string{"": rootPatterns}
	candidates := 0
	var walkDirectory func(string) error
	walkDirectory = func(relativeDirectory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		directoryName := relativeDirectory
		if directoryName == "" {
			directoryName = "."
		}
		handle, err := boundedio.OpenDirectory(root, directoryName)
		if err != nil {
			return err
		}
		defer handle.Close()
		for {
			entries, readErr := handle.ReadDir(128)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				rel := entry.Name()
				if relativeDirectory != "" {
					rel = relativeDirectory + "/" + rel
				}
				if entry.IsDir() {
					if alwaysSkipDirs[entry.Name()] || ignoredBy(gitignores, ignore, rel, true) {
						continue
					}
					patterns, err := loadPinnedGitignore(ctx, root, rel)
					if err != nil {
						return err
					}
					if len(patterns) > 0 {
						gitignores[rel] = patterns
					}
					if err := walkDirectory(rel); err != nil {
						return err
					}
					continue
				}
				if ignoredBy(gitignores, ignore, rel, false) {
					continue
				}
				candidate, accepted, err := classifyPinnedSourceCandidate(root, rel, entry)
				if err != nil {
					return err
				}
				if !accepted {
					continue
				}
				candidates++
				if candidates > maxCandidates {
					candidate.close()
					return CandidateLimitError{Limit: maxCandidates}
				}
				err = fn(rel, entry, candidate)
				candidate.close()
				if err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	return walkDirectory("")
}

// ignoredBy reports whether rel is ignored, composing the root and every
// ancestor-directory .gitignore (each matched against the path relative to its
// own directory, shallowest first so a deeper directory's rule wins) and finally
// the extra ignore globs. A directory's own .gitignore never ignores itself.
func ignoredBy(gitignores map[string][]string, ignore []string, rel string, isDir bool) bool {
	ignored := false
	for _, a := range ancestorDirs(rel) {
		pats, ok := gitignores[a]
		if !ok {
			continue
		}
		sub := rel
		if a != "" {
			sub = rel[len(a)+1:]
		}
		if v, m := matchAny(pats, sub, isDir); m {
			ignored = v
		}
	}
	if v, m := matchAny(ignore, rel, isDir); m {
		ignored = v
	}
	return ignored
}

// ancestorDirs returns rel's ancestor directories from the root ("") down to its
// parent: the directories whose .gitignore could apply to rel.
func ancestorDirs(rel string) []string {
	dirs := []string{""}
	parts := strings.Split(rel, "/")
	for i := 0; i < len(parts)-1; i++ {
		dirs = append(dirs, strings.Join(parts[:i+1], "/"))
	}
	return dirs
}

// Walk returns rel paths under root, honoring .gitignore and extra ignore globs,
// and always skipping .prowl/, .git/, node_modules/.
func Walk(root string, ignore []string) ([]string, error) {
	return WalkContext(context.Background(), root, ignore)
}

// WalkContext is Walk with cancellation checks during directory traversal.
func WalkContext(ctx context.Context, root string, ignore []string) ([]string, error) {
	var out []string
	err := walkSourceFilesContext(ctx, root, ignore, func(rel string, _ fs.DirEntry, _ sourceCandidate) error {
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Signature fingerprints the indexing policy and every tracked file's path,
// size, modification time, and content. Content closes the same-size/same-mtime
// replacement hole; the streamed read remains context-cancellable.
func Signature(root string, ignore []string) (uint64, error) {
	return SignatureWithOptions(root, Options{Ignore: ignore})
}

// SignatureWithOptions includes indexing policy in the freshness fingerprint so
// changing configured languages invalidates an otherwise unchanged file tree.
func SignatureWithOptions(root string, opt Options) (uint64, error) {
	return SignatureWithOptionsContext(context.Background(), root, opt)
}

// SignatureWithOptionsContext is SignatureWithOptions with cancellation checks
// during traversal and fingerprint construction.
func SignatureWithOptionsContext(ctx context.Context, root string, opt Options) (uint64, error) {
	snapshot, err := SourceSnapshotWithOptionsContext(ctx, root, opt)
	return snapshot.Signature, err
}

// SourceSnapshot binds a freshness signature to the exact path set used to
// construct it. Publication validation uses Paths to reject an index walk that
// transiently omitted a source present in the accepted pre-snapshot.
type SourceSnapshot struct {
	Signature uint64
	Paths     []string
}

// CandidateLimitError reports that a bounded source snapshot encountered more
// non-ignored file candidates than its configured limit.
type CandidateLimitError struct {
	Limit int
}

func (e CandidateLimitError) Error() string {
	return fmt.Sprintf("source snapshot candidate limit exceeded: %d", e.Limit)
}

// SourceSnapshotWithOptionsContext constructs a content-aware source snapshot.
func SourceSnapshotWithOptionsContext(ctx context.Context, root string, opt Options) (SourceSnapshot, error) {
	return sourceSnapshotWithOptionsContext(ctx, root, opt, noCandidateLimit)
}

// noCandidateLimit is reserved for the unbounded legacy API. The exported
// bounded API rejects nonpositive limits.
const noCandidateLimit = 0

func sourceSnapshotWithOptionsContext(ctx context.Context, root string, opt Options, maxCandidates int) (SourceSnapshot, error) {
	return sourceSnapshotWithOptionsInspectContext(ctx, root, opt, maxCandidates, snapshotCandidateEntry)
}

type snapshotCandidateInspector func(context.Context, sourceCandidate, string, fs.DirEntry) (string, error)

func snapshotCandidateEntry(ctx context.Context, candidate sourceCandidate, rel string, d fs.DirEntry) (string, error) {
	// Preserve the canonical snapshot contract: DirEntry.Info describes the
	// directory entry itself (including symlink metadata), while content is
	// hashed through a validated source target.
	info, err := d.Info()
	if err != nil {
		return "", err
	}
	var content uint64
	if candidate.file == nil {
		content, err = fileSignature(ctx, candidate.path)
	} else {
		content, err = fileDescriptorSignature(ctx, candidate.file)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\x00%d\x00%d\x00%x", rel, info.Size(), info.ModTime().UnixNano(), content), nil
}

func sourceSnapshotWithOptionsInspectContext(ctx context.Context, root string, opt Options, maxCandidates int, inspect snapshotCandidateInspector) (SourceSnapshot, error) {
	var entries []string
	var paths []string
	var pinned *os.Root
	inspectCandidate := func(rel string, d fs.DirEntry, candidate sourceCandidate) error {
		entry, err := inspect(ctx, candidate, rel, d)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
		paths = append(paths, rel)
		return nil
	}
	var err error
	if maxCandidates > 0 {
		if err := ctx.Err(); err != nil {
			return SourceSnapshot{}, err
		}
		var rootInfo fs.FileInfo
		rootInfo, err = os.Lstat(root)
		if err != nil {
			return SourceSnapshot{}, err
		}
		if rootInfo.IsDir() {
			pinned, err = os.OpenRoot(root)
			if err != nil {
				return SourceSnapshot{}, err
			}
			defer pinned.Close()
			var pinnedInfo fs.FileInfo
			pinnedInfo, err = pinned.Stat(".")
			if err != nil {
				return SourceSnapshot{}, err
			}
			if !os.SameFile(rootInfo, pinnedInfo) {
				return SourceSnapshot{}, errors.New("source root changed while opening bounded snapshot")
			}
			err = walkSourceFilesCandidateLimitContext(ctx, pinned, opt.Ignore, maxCandidates, inspectCandidate)
		}
	} else {
		err = walkSourceFilesContext(ctx, root, opt.Ignore, inspectCandidate)
	}
	if err != nil {
		return SourceSnapshot{}, err
	}
	sort.Strings(entries)
	h := xxhash.New()
	languages := append([]string(nil), opt.Languages...)
	for i := range languages {
		languages[i] = strings.ToLower(strings.TrimSpace(languages[i]))
	}
	if len(languages) == 0 {
		languages = []string{"auto"}
	}
	sort.Strings(languages)
	_, _ = h.WriteString("languages\x00" + strings.Join(languages, ",") + "\n")
	for _, pattern := range opt.Ignore {
		_, _ = h.WriteString("ignore\x00" + pattern + "\n")
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return SourceSnapshot{}, err
		}
		_, _ = h.WriteString(e)
		_, _ = h.WriteString("\n")
	}
	sort.Strings(paths)
	return SourceSnapshot{Signature: h.Sum64(), Paths: paths}, nil
}

// SourceSnapshotWithOptionsLimitContext constructs a content-aware source
// snapshot while allowing at most maxCandidates non-ignored files.
func SourceSnapshotWithOptionsLimitContext(ctx context.Context, root string, opt Options, maxCandidates int) (SourceSnapshot, error) {
	if maxCandidates <= 0 {
		return SourceSnapshot{}, fmt.Errorf("maxCandidates must be positive: %d", maxCandidates)
	}
	return sourceSnapshotWithOptionsContext(ctx, root, opt, maxCandidates)
}

func fileSignature(ctx context.Context, path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return fileDescriptorSignature(ctx, file)
}

func fileDescriptorSignature(ctx context.Context, file *os.File) (uint64, error) {
	h := xxhash.New()
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if readErr == io.EOF {
			return h.Sum64(), nil
		}
		if readErr != nil {
			return 0, readErr
		}
	}
}

const maxGitignoreBytes int64 = 1 << 20

func loadPinnedGitignore(ctx context.Context, root *os.Root, relativeDirectory string) ([]string, error) {
	name := ".gitignore"
	if relativeDirectory != "" {
		name = relativeDirectory + "/.gitignore"
	}
	file, err := boundedio.OpenRegular(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := boundedio.ReadAllContext(ctx, file, maxGitignoreBytes)
	if err != nil {
		return nil, err
	}
	return parseGitignore(data), nil
}

func loadGitignore(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil
	}
	return parseGitignore(data)
}

func parseGitignore(data []byte) []string {
	var pats []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pats = append(pats, line)
	}
	return pats
}

// matchAny reports whether rel is ignored by the gitignore-style patterns and
// whether any pattern matched at all (so callers can compose multiple gitignore
// files, deeper-wins). Order matters: a later matching pattern overrides an
// earlier one, and a leading "!" re-includes. A trailing "/" restricts a pattern
// to directories. The supported subset is basename globs, anchored path globs
// (no "**"), and bare directory names; negation lets a repo ignore a tree but
// keep a subtree (e.g. `packages/*/*/` then `!packages/*/src/`).
func matchAny(pats []string, rel string, isDir bool) (ignored, matched bool) {
	base := filepath.Base(rel)
	segs := strings.Split(rel, "/")
	for _, p := range pats {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		negate := strings.HasPrefix(p, "!")
		if negate {
			p = p[1:]
		}
		dirOnly := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/")
		if p == "" || (dirOnly && !isDir) {
			continue
		}
		if matchPattern(p, base, rel, segs) {
			ignored, matched = !negate, true
		}
	}
	return
}

// matchPattern reports whether a single gitignore pattern matches rel. A pattern
// with no slash is a basename glob matching at any depth (or a bare directory
// name matching any path segment); a pattern with a slash is anchored to the
// project root and matched against the whole path.
func matchPattern(p, base, rel string, segs []string) bool {
	if !strings.Contains(p, "/") {
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
		for _, s := range segs {
			if s == p {
				return true
			}
		}
		return false
	}
	ok, _ := filepath.Match(p, rel)
	return ok
}
