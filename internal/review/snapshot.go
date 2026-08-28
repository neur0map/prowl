package review

// snapshot.go materializes an immutable head view for committed range/commit
// review and resolves arbitrary base/head source bytes. A committed head is
// enumerated with sanitized `ls-tree -rz --full-tree`, its regular and symlink
// blobs are read with sanitized `cat-file`, and both are written into a private
// snapshot below Git common state and indexed with a private Prowl store. The
// writer never trusts the enumerated stream: absolute paths, parent traversal,
// duplicate entries, parent/file conflicts, malformed records, unsupported
// modes, and the entry/blob/total caps all fail before a single byte is written.
// A malformed symlink blob (a 120000 blob whose target bytes contain NUL) is
// kept verbatim by the SourceResolver but is never turned into a filesystem
// symlink or indexed; it is recorded as an omission instead.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	contextpacket "github.com/prowl-agent/prowl-agent/internal/context"
	"github.com/prowl-agent/prowl-agent/internal/index"
	"github.com/prowl-agent/prowl-agent/internal/query"
	"github.com/prowl-agent/prowl-agent/internal/store"

	"github.com/prowl-agent/prowl-agent/internal/config"
)

// snapshotGitOutputLimit bounds every snapshot Git subprocess whose stdout is
// not otherwise budget-checked: ls-tree enumeration, cat-file batch-check, and
// each bounded cat-file content batch. A runaway or hostile stream is killed
// rather than buffered without bound.
const snapshotGitOutputLimit = 256 << 20

// snapshotContentBatchBytes bounds the content read into memory by one cat-file
// batch. Blob sizes are proven by a prior batch-check, so the materializer reads
// content in size-bounded waves and writes each blob to disk before the next
// wave, keeping peak memory bounded regardless of the 4 GiB total cap.
const snapshotContentBatchBytes int64 = 128 << 20

// snapshotContentDir is the subdirectory of a snapshot root that holds the
// materialized head tree. The private store lives in a sibling .prowl directory
// so the store database is never itself enumerated as source.
const snapshotContentDir = "content"

// v1 snapshot boundaries. They default to these constants on HeadViewOptions and
// are lowerable by tests so the fail-closed accounting paths run without
// materializing gigabytes.
const (
	// MaxSnapshotEntriesV1 bounds the number of tree entries a head view may
	// materialize.
	MaxSnapshotEntriesV1 = 250_000
	// MaxSnapshotBlobBytesV1 bounds a single materialized regular blob.
	MaxSnapshotBlobBytesV1 int64 = 64 << 20
	// MaxSnapshotTotalBytesV1 bounds the total materialized blob content.
	MaxSnapshotTotalBytesV1 int64 = 4 << 30
)

var (
	// ErrSnapshotUnsafeEntry reports a tree entry the writer refuses to
	// materialize: an absolute path, parent traversal, a duplicate path, a
	// parent/file conflict, a malformed record, or an unsupported mode Git would
	// normally never create. It fails the whole materialization before any write.
	ErrSnapshotUnsafeEntry = errors.New("review: unsafe snapshot tree entry")
	// ErrSnapshotTooManyEntries reports that the enumerated tree exceeded the
	// entry cap.
	ErrSnapshotTooManyEntries = errors.New("review: snapshot tree exceeds its entry cap")
	// ErrSnapshotBlobTooLarge reports a regular blob above the per-blob cap.
	ErrSnapshotBlobTooLarge = errors.New("review: snapshot blob exceeds its byte cap")
	// ErrSnapshotTotalTooLarge reports that total blob content exceeded the
	// per-snapshot cap.
	ErrSnapshotTotalTooLarge = errors.New("review: snapshot content exceeds its total byte cap")
	// ErrSnapshotObjectMissing reports that a required blob is absent from the
	// local object store. A missing object fails closed rather than triggering a
	// promisor/lazy fetch.
	ErrSnapshotObjectMissing = errors.New("review: snapshot object missing from the local object store")
	// ErrSnapshotMalformedStream reports unparseable ls-tree or cat-file output.
	ErrSnapshotMalformedStream = errors.New("review: malformed snapshot object stream")
	// ErrSourceTooLarge reports that a resolved source blob exceeded the caller's
	// per-read byte bound.
	ErrSourceTooLarge = errors.New("review: source blob exceeds the requested byte bound")
	// ErrSourceSideUnavailable reports that a review side has no resolver.
	ErrSourceSideUnavailable = errors.New("review: review side has no source resolver")
)

// ReviewSide names one revision side of a review. It reuses the model's Side type
// (SideBase / SideHead) so identity, capture, and source resolution all agree.
type ReviewSide = Side

// SourceEntry is the exact, classified content of one path on one review side.
// Bytes holds a regular file's exact content or a symbolic link's exact target
// bytes, including a malicious committed link blob containing NUL. It never
// follows a link and never opens a special path.
type SourceEntry struct {
	Path      string
	Side      ReviewSide
	Present   bool
	Mode      uint32
	Kind      string // "regular" | "symlink" | "gitlink" | "tree" | "absent"
	OID       string
	Bytes     []byte
	TextClass TextClass // "text" | "binary"; empty for gitlink/tree/absent
}

// IsSymlink reports whether the entry is a symbolic link.
func (e SourceEntry) IsSymlink() bool { return e.Kind == "symlink" }

// Binary reports whether the entry classified as binary under ThresholdTextV1.
func (e SourceEntry) Binary() bool { return e.TextClass == TextClassBinary }

// MalformedSymlink reports a symlink whose target bytes contain a NUL, which no
// filesystem symlink can hold. Such an entry is kept verbatim here but is never
// materialized or indexed.
func (e SourceEntry) MalformedSymlink() bool {
	return e.IsSymlink() && bytes.IndexByte(e.Bytes, 0) >= 0
}

// SourceResolver resolves the exact bytes and classification of an arbitrary
// (changed or unchanged) path on one review side, reading only from Git objects.
type SourceResolver interface {
	Read(ctx context.Context, side ReviewSide, path string, maxBytes int64) (SourceEntry, error)
}

// gitTreeResolver reads a single side's blobs directly from a resolved tree-ish
// object, never touching a worktree or following a link.
type gitTreeResolver struct {
	runner  GitRunner
	root    string
	width   int
	treeish string
}

// sideSourceResolver dispatches Read to the per-side git tree resolver.
type sideSourceResolver struct {
	base *gitTreeResolver
	head *gitTreeResolver
}

// Read resolves path on side, bounding the read at maxBytes.
func (r *sideSourceResolver) Read(ctx context.Context, side ReviewSide, p string, maxBytes int64) (SourceEntry, error) {
	var res *gitTreeResolver
	switch side {
	case SideBase:
		res = r.base
	case SideHead:
		res = r.head
	default:
		return SourceEntry{}, fmt.Errorf("%w: unknown side %q", ErrSourceSideUnavailable, side)
	}
	if res == nil {
		return SourceEntry{}, fmt.Errorf("%w: %q", ErrSourceSideUnavailable, side)
	}
	entry, err := res.read(ctx, p, maxBytes)
	if err != nil {
		return SourceEntry{}, err
	}
	entry.Side = side
	return entry, nil
}

// read resolves one path from the resolver's tree-ish. An absent path yields a
// non-present absent entry; a directory yields a "tree" entry; a gitlink yields
// a "gitlink" entry; a regular/symlink blob yields its exact bytes and
// ThresholdTextV1 classification. maxBytes, when positive, bounds the blob size;
// a larger blob fails with ErrSourceTooLarge rather than being truncated.
func (g *gitTreeResolver) read(ctx context.Context, p string, maxBytes int64) (SourceEntry, error) {
	clean, err := cleanTreePath(p)
	if err != nil {
		return SourceEntry{}, err
	}
	out, err := g.runner.Output(ctx, g.root, snapshotGitOutputLimit,
		"ls-tree", "-z", "--full-tree", g.treeish, "--", literalPathspec(clean))
	if err != nil {
		return SourceEntry{}, err
	}
	trimmed := bytes.TrimRight(out, "\x00")
	if len(trimmed) == 0 {
		return SourceEntry{Path: clean, Present: false, Kind: "absent"}, nil
	}
	if bytes.IndexByte(trimmed, 0) >= 0 {
		// More than one record: the path names a directory listing rather than a
		// single entry. Treat it as a tree.
		return SourceEntry{Path: clean, Present: true, Kind: "tree"}, nil
	}
	mode, typ, oid, recPath, err := parseLsTreeRecord(trimmed)
	if err != nil {
		return SourceEntry{}, fmt.Errorf("%w: %v", ErrSnapshotMalformedStream, err)
	}
	if recPath != clean {
		return SourceEntry{Path: clean, Present: false, Kind: "absent"}, nil
	}
	switch {
	case typ == "tree" || mode == "040000":
		return SourceEntry{Path: clean, Present: true, Mode: modeTree, Kind: "tree", OID: oid}, nil
	case mode == "160000":
		return SourceEntry{Path: clean, Present: true, Mode: modeGitlink, Kind: "gitlink", OID: oid}, nil
	case isBlobMode(mode):
		return g.readBlob(ctx, clean, mode, oid, maxBytes)
	default:
		return SourceEntry{}, fmt.Errorf("%w: unsupported mode %q for %q", ErrSnapshotUnsafeEntry, mode, clean)
	}
}

// readBlob reads a regular/symlink blob's exact bytes and classifies it.
func (g *gitTreeResolver) readBlob(ctx context.Context, clean, mode, oid string, maxBytes int64) (SourceEntry, error) {
	objs, err := batchBlobs(ctx, g.runner, g.root, []string{oid}, snapshotGitOutputLimit)
	if err != nil {
		return SourceEntry{}, err
	}
	o, ok := objs[oid]
	if !ok || o.Missing {
		return SourceEntry{}, fmt.Errorf("%w: %s", ErrSnapshotObjectMissing, oid)
	}
	if o.Type != "blob" {
		return SourceEntry{}, fmt.Errorf("%w: %s is a %s, not a blob", ErrSnapshotMalformedStream, oid, o.Type)
	}
	if maxBytes > 0 && int64(len(o.Data)) > maxBytes {
		return SourceEntry{}, fmt.Errorf("%w: %q is %d bytes, bound %d", ErrSourceTooLarge, clean, len(o.Data), maxBytes)
	}
	kind, m := "regular", modeRegular
	switch mode {
	case "120000":
		kind, m = "symlink", modeSymlink
	case "100755":
		kind, m = "regular", modeExecutable
	}
	side := BlobSide{Present: true, Bytes: o.Data}
	return SourceEntry{
		Path:      clean,
		Present:   true,
		Mode:      m,
		Kind:      kind,
		OID:       o.OID,
		Bytes:     o.Data,
		TextClass: ThresholdText(clean, clean, side, side),
	}, nil
}

// Omission is a materialized-tree path the writer deliberately skipped, with a
// precise reason. A malformed symlink blob is the primary case: its bytes stay
// available through the SourceResolver, but it is never turned into a filesystem
// link or indexed.
type Omission struct {
	Path   string
	Side   ReviewSide
	Reason string
}

// HeadView is an immutable, indexed view of a review's head revision plus a
// side-aware source resolver. For a committed range/commit head that is not the
// current clean worktree, Root is a private snapshot below Git common state and
// Store/Query/Context are a private Prowl index over it. Close releases the
// private store and removes the snapshot; it is a no-op for a reused view.
type HeadView struct {
	Root      string
	Store     *store.Store
	Query     *query.Querier
	Context   *contextpacket.Service
	Sources   SourceResolver
	Omissions []Omission
	Close     func() error
}

// ReusableView carries the current project services a HeadView reuses when the
// reviewed content is exactly the current clean worktree (or the workspace
// itself). The HeadView does not own these services, so its Close never touches
// them.
type ReusableView struct {
	Root    string
	Store   *store.Store
	Query   *query.Querier
	Context *contextpacket.Service
}

// symlinkFunc creates a symbolic link rooted in a snapshot. It is a seam so a
// test can prove a malformed link is never turned into a filesystem symlink.
type symlinkFunc func(root *os.Root, target, linkname string) error

func rootSymlink(root *os.Root, target, linkname string) error {
	return root.Symlink(target, linkname)
}

// HeadViewOptions configures OpenHeadView. Runner, RepoRoot, and ObjectFormat
// are always required. For a materialized committed head, HeadTreeish must be a
// resolved commit/tree object id; BaseTreeish enables base-side source reads.
// Reuse, when set, wraps existing services instead of materializing. The Max*
// bounds default to their v1 constants when zero.
type HeadViewOptions struct {
	Runner       GitRunner
	RepoRoot     string
	ObjectFormat string
	Config       config.Config

	HeadTreeish string
	BaseTreeish string

	// Reuse wraps current project services for a clean exact-head or workspace
	// view rather than materializing a private snapshot.
	Reuse *ReusableView

	// SnapshotParent is the directory a materialized snapshot is created under.
	// It defaults to <git-common-dir>/prowl/snapshots when empty.
	SnapshotParent string

	MaxEntries    int
	MaxBlobBytes  int64
	MaxTotalBytes int64

	// symlink is the (test-injectable) link creator. It defaults to rootSymlink.
	symlink symlinkFunc
}

func (o HeadViewOptions) maxEntries() int {
	if o.MaxEntries > 0 {
		return o.MaxEntries
	}
	return MaxSnapshotEntriesV1
}

func (o HeadViewOptions) maxBlobBytes() int64 {
	if o.MaxBlobBytes > 0 {
		return o.MaxBlobBytes
	}
	return MaxSnapshotBlobBytesV1
}

func (o HeadViewOptions) maxTotalBytes() int64 {
	if o.MaxTotalBytes > 0 {
		return o.MaxTotalBytes
	}
	return MaxSnapshotTotalBytesV1
}

func (o HeadViewOptions) symlinker() symlinkFunc {
	if o.symlink != nil {
		return o.symlink
	}
	return rootSymlink
}

// OpenHeadView builds an immutable head view. When opts.Reuse is set it wraps
// those services (Close is a no-op). Otherwise it materializes opts.HeadTreeish
// into a private snapshot below Git common state, indexes it with the project's
// ignore/language configuration, and assembles a private query/context service.
// The side-aware SourceResolver resolves arbitrary base/head paths from Git
// objects in every mode.
func OpenHeadView(ctx context.Context, opts HeadViewOptions) (*HeadView, error) {
	if opts.Runner == nil {
		return nil, errors.New("review: OpenHeadView requires a runner")
	}
	width, ok := oidWidthFor(opts.ObjectFormat)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedObjectFormat, opts.ObjectFormat)
	}
	resolver := &sideSourceResolver{}
	if opts.BaseTreeish != "" {
		resolver.base = &gitTreeResolver{runner: opts.Runner, root: opts.RepoRoot, width: width, treeish: opts.BaseTreeish}
	}
	if opts.HeadTreeish != "" {
		resolver.head = &gitTreeResolver{runner: opts.Runner, root: opts.RepoRoot, width: width, treeish: opts.HeadTreeish}
	}

	if opts.Reuse != nil {
		reuse := opts.Reuse
		return &HeadView{
			Root:    reuse.Root,
			Store:   reuse.Store,
			Query:   reuse.Query,
			Context: reuse.Context,
			Sources: resolver,
			Close:   func() error { return nil },
		}, nil
	}

	if opts.HeadTreeish == "" {
		return nil, errors.New("review: OpenHeadView requires a head tree-ish to materialize")
	}
	return materializeHeadView(ctx, opts, width, resolver)
}

// materializeHeadView enumerates and writes the head tree into a private
// snapshot, builds a private index, and returns an owning HeadView.
func materializeHeadView(ctx context.Context, opts HeadViewOptions, width int, resolver *sideSourceResolver) (*HeadView, error) {
	parent := opts.SnapshotParent
	if parent == "" {
		common, err := gitCommonDir(ctx, opts.Runner, opts.RepoRoot)
		if err != nil {
			return nil, err
		}
		parent = filepath.Join(common, "prowl", "snapshots")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(parent, "snap-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(work) }

	contentDir := filepath.Join(work, snapshotContentDir)
	if err := os.MkdirAll(contentDir, 0o700); err != nil {
		cleanup()
		return nil, err
	}

	omissions, err := materializeTree(ctx, opts, width, contentDir)
	if err != nil {
		cleanup()
		return nil, err
	}

	dbDir := filepath.Join(work, ".prowl")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		cleanup()
		return nil, err
	}
	db, err := store.Open(filepath.Join(dbDir, "index.db"))
	if err != nil {
		cleanup()
		return nil, err
	}
	opt := index.Options{Ignore: opts.Config.Ignore, Languages: opts.Config.Languages}
	if _, err := index.IndexWithOptionsContext(ctx, db, contentDir, opt); err != nil {
		_ = db.Close()
		cleanup()
		return nil, err
	}

	querier := query.New(db)
	contextService := &contextpacket.Service{Store: db, Root: contentDir}
	return &HeadView{
		Root:      work,
		Store:     db,
		Query:     querier,
		Context:   contextService,
		Sources:   resolver,
		Omissions: omissions,
		Close: func() error {
			err := db.Close()
			cleanup()
			return err
		},
	}, nil
}

// treeEntry is one validated entry from the head tree enumeration.
type treeEntry struct {
	mode string
	typ  string
	oid  string
	path string
}

// materializeTree enumerates, validates, size-checks, and writes the head tree
// under contentDir, returning the omissions (malformed symlinks). No byte is
// written until every entry has passed validation and every blob size is known,
// so a hostile stream can never leave a partial or escaping snapshot.
func materializeTree(ctx context.Context, opts HeadViewOptions, width int, contentDir string) ([]Omission, error) {
	out, err := opts.Runner.Output(ctx, opts.RepoRoot, snapshotGitOutputLimit,
		"ls-tree", "-rz", "--full-tree", opts.HeadTreeish)
	if err != nil {
		return nil, err
	}
	entries, err := parseTreeEntries(out, width, opts.maxEntries())
	if err != nil {
		return nil, err
	}

	// Size-check every blob before any write so an oversized or missing object
	// fails closed with nothing on disk and no promisor fetch.
	blobOIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		if isBlobMode(e.mode) {
			blobOIDs = append(blobOIDs, e.oid)
		}
	}
	sizes, err := batchCheck(ctx, opts.Runner, opts.RepoRoot, blobOIDs)
	if err != nil {
		return nil, err
	}
	var total int64
	for oid, meta := range sizes {
		if meta.missing {
			return nil, fmt.Errorf("%w: %s", ErrSnapshotObjectMissing, oid)
		}
		if meta.typ != "blob" {
			return nil, fmt.Errorf("%w: %s is a %s, not a blob", ErrSnapshotMalformedStream, oid, meta.typ)
		}
	}
	// Regular blobs are bounded per-blob and in total; symlink target blobs are
	// small and counted only toward the total.
	for _, e := range entries {
		meta, ok := sizes[e.oid]
		if !ok || !isBlobMode(e.mode) {
			continue
		}
		if e.mode != "120000" && meta.size > opts.maxBlobBytes() {
			return nil, fmt.Errorf("%w: %q is %d bytes", ErrSnapshotBlobTooLarge, e.path, meta.size)
		}
		total += meta.size
		if total > opts.maxTotalBytes() {
			return nil, fmt.Errorf("%w: total reached %d bytes", ErrSnapshotTotalTooLarge, total)
		}
	}

	root, err := os.OpenRoot(contentDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	var omissions []Omission
	symlink := opts.symlinker()
	writeBlob := func(e treeEntry, data []byte) error {
		if dir := path.Dir(e.path); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		if e.mode == "120000" {
			if bytes.IndexByte(data, 0) >= 0 {
				// A malformed symlink target cannot become a filesystem link;
				// keep it only in the SourceResolver and record an omission.
				omissions = append(omissions, Omission{Path: e.path, Side: SideHead, Reason: "malformed symlink target (NUL)"})
				return nil
			}
			return symlink(root, string(data), e.path)
		}
		perm := os.FileMode(0o644)
		if e.mode == "100755" {
			perm = 0o755
		}
		return root.WriteFile(e.path, data, perm)
	}

	// Write blob content in size-bounded waves to bound peak memory.
	for start := 0; start < len(entries); {
		wave := make([]string, 0, 64)
		waveBytes := int64(0)
		seen := map[string]bool{}
		i := start
		for ; i < len(entries); i++ {
			e := entries[i]
			if e.mode == "160000" {
				continue // gitlink: no content to write
			}
			if !isBlobMode(e.mode) {
				continue
			}
			meta := sizes[e.oid]
			if !seen[e.oid] {
				if len(wave) > 0 && waveBytes+meta.size > snapshotContentBatchBytes {
					break
				}
				wave = append(wave, e.oid)
				seen[e.oid] = true
				waveBytes += meta.size
			}
		}
		end := i
		objs := map[string]CatFileObject{}
		if len(wave) > 0 {
			objs, err = batchBlobs(ctx, opts.Runner, opts.RepoRoot, wave, snapshotGitOutputLimit)
			if err != nil {
				return nil, err
			}
		}
		for j := start; j < end; j++ {
			e := entries[j]
			if e.mode == "160000" || !isBlobMode(e.mode) {
				continue
			}
			o, ok := objs[e.oid]
			if !ok || o.Missing {
				return nil, fmt.Errorf("%w: %s", ErrSnapshotObjectMissing, e.oid)
			}
			if err := writeBlob(e, o.Data); err != nil {
				return nil, err
			}
		}
		if end == start {
			// No writable blob in this wave (e.g. a run of gitlinks); advance.
			end = i
			if end == start {
				break
			}
		}
		start = end
	}
	return omissions, nil
}

// parseTreeEntries parses `ls-tree -rz --full-tree` output into validated
// entries. It rejects a missing terminal NUL, a malformed record, an invalid or
// non-UTF-8 path, an absolute path, parent traversal, a duplicate path, a
// parent/file conflict, an unsupported mode, and more than maxEntries entries.
func parseTreeEntries(out []byte, width, maxEntries int) ([]treeEntry, error) {
	if len(out) == 0 {
		return nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, fmt.Errorf("%w: ls-tree output not NUL-terminated", ErrSnapshotMalformedStream)
	}
	records := bytes.Split(out[:len(out)-1], []byte{0})
	entries := make([]treeEntry, 0, len(records))
	files := map[string]bool{}
	dirs := map[string]bool{}
	for _, rec := range records {
		if len(rec) == 0 {
			return nil, fmt.Errorf("%w: empty ls-tree record", ErrSnapshotMalformedStream)
		}
		mode, typ, oid, p, err := parseLsTreeRecord(rec)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSnapshotMalformedStream, err)
		}
		if !modeSupported(mode) {
			return nil, fmt.Errorf("%w: unsupported mode %q for %q", ErrSnapshotUnsafeEntry, mode, p)
		}
		if isBlobMode(mode) || mode == "160000" {
			if !isFullOID(oid, width) {
				return nil, fmt.Errorf("%w: entry %q has malformed object id %q", ErrSnapshotUnsafeEntry, p, oid)
			}
		}
		if typ == "commit" && mode != "160000" {
			return nil, fmt.Errorf("%w: entry %q has type commit with mode %q", ErrSnapshotUnsafeEntry, p, mode)
		}
		clean, err := safeTreePath(p)
		if err != nil {
			return nil, err
		}
		if files[clean] || dirs[clean] {
			return nil, fmt.Errorf("%w: duplicate or conflicting path %q", ErrSnapshotUnsafeEntry, clean)
		}
		// A materialized path may not be both a file and a directory prefix.
		for _, ancestor := range ancestorPaths(clean) {
			if files[ancestor] {
				return nil, fmt.Errorf("%w: %q conflicts with file %q", ErrSnapshotUnsafeEntry, clean, ancestor)
			}
			dirs[ancestor] = true
		}
		if dirs[clean] {
			return nil, fmt.Errorf("%w: %q conflicts with an existing directory", ErrSnapshotUnsafeEntry, clean)
		}
		files[clean] = true
		entries = append(entries, treeEntry{mode: mode, typ: typ, oid: oid, path: clean})
		if len(entries) > maxEntries {
			return nil, fmt.Errorf("%w: more than %d entries", ErrSnapshotTooManyEntries, maxEntries)
		}
	}
	return entries, nil
}

// modeSupported reports whether a git tree mode may appear in a head snapshot:
// regular file, executable, symlink, or gitlink. Any other mode (a hard link,
// device, FIFO, socket, or a mode Git would refuse to create) is rejected.
func modeSupported(mode string) bool {
	switch mode {
	case "100644", "100755", "120000", "160000", "040000":
		return true
	default:
		return false
	}
}

// safeTreePath validates a tree entry path: non-empty valid UTF-8, relative
// (not absolute), forward-slash separated, and free of "." / ".." / empty
// components. It returns the cleaned slash path.
func safeTreePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: empty path", ErrSnapshotUnsafeEntry)
	}
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%w: non-UTF-8 path %s", ErrSnapshotUnsafeEntry, strconv.QuoteToASCII(p))
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: absolute path %q", ErrSnapshotUnsafeEntry, p)
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: path %q contains NUL", ErrSnapshotUnsafeEntry, p)
	}
	// Reject a Windows-style absolute or volume path defensively.
	if strings.Contains(p, "\\") {
		return "", fmt.Errorf("%w: backslash in path %q", ErrSnapshotUnsafeEntry, p)
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: unsafe component in path %q", ErrSnapshotUnsafeEntry, p)
		}
	}
	return strings.Join(parts, "/"), nil
}

// cleanTreePath validates a caller-supplied source path for a SourceResolver
// read, applying the same relative/no-traversal rule as materialization.
func cleanTreePath(p string) (string, error) {
	clean, err := safeTreePath(p)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	return clean, nil
}

// ancestorPaths returns the directory prefixes of a slash path, excluding the
// path itself: "a/b/c" -> ["a", "a/b"].
func ancestorPaths(p string) []string {
	segs := strings.Split(p, "/")
	if len(segs) <= 1 {
		return nil
	}
	out := make([]string, 0, len(segs)-1)
	for i := 1; i < len(segs); i++ {
		out = append(out, strings.Join(segs[:i], "/"))
	}
	return out
}

// batchCheckMeta is one object's metadata from cat-file --batch-check.
type batchCheckMeta struct {
	typ     string
	size    int64
	missing bool
}

// batchCheck resolves object metadata for a set of (deduplicated) object ids
// without reading their content, so sizes and existence are known before any
// content read or filesystem write. A missing object is reported, never fetched.
func batchCheck(ctx context.Context, runner GitRunner, root string, oids []string) (map[string]batchCheckMeta, error) {
	order, ok := dedupOIDs(oids)
	result := map[string]batchCheckMeta{}
	if !ok {
		return result, nil
	}
	var stdin bytes.Buffer
	for _, oid := range order {
		stdin.WriteString(oid)
		stdin.WriteByte('\n')
	}
	var stdout bytes.Buffer
	if err := runner.Pipe(ctx, root, snapshotGitOutputLimit, &stdin, &stdout, "cat-file", "--batch-check"); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != len(order) {
		return nil, fmt.Errorf("%w: batch-check returned %d lines, want %d", ErrSnapshotMalformedStream, len(lines), len(order))
	}
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "missing" {
			result[order[i]] = batchCheckMeta{missing: true}
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: bad batch-check line %q", ErrSnapshotMalformedStream, line)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("%w: bad object size in %q", ErrSnapshotMalformedStream, line)
		}
		result[order[i]] = batchCheckMeta{typ: fields[1], size: size}
	}
	return result, nil
}

// batchBlobs reads a set of (deduplicated) object ids in one cat-file batch,
// keyed by the requested id and echoing the full id and exact bytes.
func batchBlobs(ctx context.Context, runner GitRunner, root string, oids []string, limit int64) (map[string]CatFileObject, error) {
	order, ok := dedupOIDs(oids)
	result := map[string]CatFileObject{}
	if !ok {
		return result, nil
	}
	var stdin bytes.Buffer
	for _, oid := range order {
		stdin.WriteString(oid)
		stdin.WriteByte('\n')
	}
	var stdout bytes.Buffer
	if err := runner.Pipe(ctx, root, limit, &stdin, &stdout, "cat-file", "--batch"); err != nil {
		return nil, err
	}
	objs, err := ParseCatFileBatch(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSnapshotMalformedStream, err)
	}
	if len(objs) != len(order) {
		return nil, fmt.Errorf("%w: cat-file returned %d objects, want %d", ErrSnapshotMalformedStream, len(objs), len(order))
	}
	for i, oid := range order {
		result[oid] = objs[i]
	}
	return result, nil
}

// dedupOIDs returns the unique, non-empty object ids in first-seen order and
// whether any remain.
func dedupOIDs(oids []string) ([]string, bool) {
	seen := map[string]bool{}
	order := make([]string, 0, len(oids))
	for _, oid := range oids {
		if oid == "" || seen[oid] {
			continue
		}
		seen[oid] = true
		order = append(order, oid)
	}
	return order, len(order) > 0
}

// modeTree is the git directory mode as an octal uint32.
const modeTree uint32 = 0o040000

// gitCommonDir resolves the absolute Git common metadata directory, which is
// shared by every linked worktree. Review state lives beneath it so it can never
// enter a worktree's tracked status.
func gitCommonDir(ctx context.Context, runner GitRunner, root string) (string, error) {
	out, err := runner.Output(ctx, root, scopeOutputLimit, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("review: resolve git common dir: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", errors.New("review: git common dir is empty")
	}
	return dir, nil
}
