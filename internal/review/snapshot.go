package review

// snapshot.go materializes an immutable head view for committed range/commit
// review and resolves arbitrary base/head source bytes. A committed head is
// enumerated with sanitized `ls-tree -rz --full-tree`, its regular and symlink
// blobs are read with sanitized `cat-file` (each echoed id, type, and size bound
// to the request before a byte is written), and both are written into a private
// snapshot below Git common state and indexed with a private Prowl store. The
// writer never trusts the enumerated stream: absolute paths, parent traversal,
// duplicate entries, parent/file conflicts, malformed records, mode/type
// mismatches, and the entry/blob/total caps all fail before a single byte is
// written, with the entry stream parsed incrementally and stopped at the cap.
// A malformed symlink blob (a 120000 blob whose target bytes contain NUL) is
// kept verbatim by the SourceResolver but is never turned into a filesystem
// symlink or indexed; it is recorded as an omission instead. A current, clean,
// verified worktree at the resolved head is reused instead of materialized.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/prowl-agent/prowl-agent/internal/boundedio"
	"github.com/prowl-agent/prowl-agent/internal/config"
	contextpacket "github.com/prowl-agent/prowl-agent/internal/context"
	"github.com/prowl-agent/prowl-agent/internal/index"
	"github.com/prowl-agent/prowl-agent/internal/query"
	"github.com/prowl-agent/prowl-agent/internal/store"
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

// snapshotContentDir holds the materialized head tree; the private store lives in
// a sibling .prowl directory so the store database is never enumerated as source.
const snapshotContentDir = "content"

// snapshotReadCeiling bounds a workspace-side source read when no explicit
// per-read bound is supplied.
const snapshotReadCeiling int64 = 256 << 20

// v1 snapshot boundaries. They default to these constants on HeadViewOptions and
// are lowerable by tests so the fail-closed accounting paths run without
// materializing gigabytes.
const (
	MaxSnapshotEntriesV1          = 250_000
	MaxSnapshotBlobBytesV1  int64 = 64 << 20
	MaxSnapshotTotalBytesV1 int64 = 4 << 30
	// MaxSymlinkTargetBytesV1 bounds a materialized symlink's target. A genuine
	// link target is well under this; a larger 120000 blob is malformed/hostile
	// and is omitted rather than materialized.
	MaxSymlinkTargetBytesV1 int64 = 1 << 20
)

var (
	// ErrSnapshotUnsafeEntry reports a tree entry the writer refuses to
	// materialize: an absolute path, parent traversal, a duplicate path, a
	// parent/file conflict, a malformed record, a mode/type mismatch, or a mode
	// Git would normally never create. It fails the whole materialization before
	// any write.
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
	// ErrSnapshotMalformedStream reports unparseable or unbound ls-tree/cat-file
	// output.
	ErrSnapshotMalformedStream = errors.New("review: malformed snapshot object stream")
	// ErrSourceTooLarge reports that a resolved source blob exceeded the caller's
	// per-read byte bound.
	ErrSourceTooLarge = errors.New("review: source blob exceeds the requested byte bound")
	// ErrSourceSideUnavailable reports that a review side has no resolver.
	ErrSourceSideUnavailable = errors.New("review: review side has no source resolver")
	// ErrReuseUnavailable reports that a requested index reuse is not eligible and
	// no head tree-ish is available to materialize instead.
	ErrReuseUnavailable = errors.New("review: index reuse is not eligible and no head tree-ish is available")
	// ErrSnapshotTreeishNotResolved reports that a base or head tree-ish is not a
	// full-width resolved object id. A mutable ref could name different content
	// between materialization and later source reads, so only immutable OIDs are
	// accepted.
	ErrSnapshotTreeishNotResolved = errors.New("review: snapshot tree-ish must be a full resolved object id")
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
	Kind      string // "regular" | "symlink" | "gitlink" | "tree" | "special" | "absent"
	OID       string
	Bytes     []byte
	TextClass TextClass // "text" | "binary"; empty for gitlink/tree/special/absent
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
// (changed or unchanged) path on one review side.
type SourceResolver interface {
	Read(ctx context.Context, side ReviewSide, path string, maxBytes int64) (SourceEntry, error)
}

// sideReader resolves one side's paths.
type sideReader interface {
	read(ctx context.Context, path string, maxBytes int64) (SourceEntry, error)
}

// sideSourceResolver dispatches Read to the per-side reader.
type sideSourceResolver struct {
	base sideReader
	head sideReader
}

// Read resolves path on side, bounding the read at maxBytes.
func (r *sideSourceResolver) Read(ctx context.Context, side ReviewSide, p string, maxBytes int64) (SourceEntry, error) {
	var reader sideReader
	switch side {
	case SideBase:
		reader = r.base
	case SideHead:
		reader = r.head
	default:
		return SourceEntry{}, fmt.Errorf("%w: unknown side %q", ErrSourceSideUnavailable, side)
	}
	if reader == nil {
		return SourceEntry{}, fmt.Errorf("%w: %q", ErrSourceSideUnavailable, side)
	}
	entry, err := reader.read(ctx, p, maxBytes)
	if err != nil {
		return SourceEntry{}, err
	}
	entry.Side = side
	return entry, nil
}

// gitTreeResolver reads a side's blobs directly from a resolved tree-ish object,
// never touching a worktree or following a link.
type gitTreeResolver struct {
	runner  GitRunner
	root    string
	width   int
	treeish string
}

// read resolves one path from the resolver's tree-ish with strict stream
// binding: exactly one NUL-terminated record, an exhaustive mode/type matrix,
// full-width object ids, and a size checked before any content read.
func (g *gitTreeResolver) read(ctx context.Context, p string, maxBytes int64) (SourceEntry, error) {
	clean, err := safeTreePath(p)
	if err != nil {
		return SourceEntry{}, err
	}
	out, err := g.runner.Output(ctx, g.root, snapshotGitOutputLimit,
		"ls-tree", "-z", "--full-tree", "--end-of-options", g.treeish, "--", literalPathspec(clean))
	if err != nil {
		return SourceEntry{}, err
	}
	if len(out) == 0 {
		return SourceEntry{Path: clean, Present: false, Kind: "absent"}, nil
	}
	rec, err := singleNULRecord(out)
	if err != nil {
		return SourceEntry{}, fmt.Errorf("%w: source lookup: %v", ErrSnapshotMalformedStream, err)
	}
	mode, typ, oid, recPath, err := parseLsTreeRecord(rec)
	if err != nil {
		return SourceEntry{}, fmt.Errorf("%w: %v", ErrSnapshotMalformedStream, err)
	}
	if recPath != clean {
		return SourceEntry{}, fmt.Errorf("%w: source lookup returned %q, want %q", ErrSnapshotMalformedStream, recPath, clean)
	}
	entryKind, err := classifyEntry(mode, typ)
	if err != nil {
		return SourceEntry{}, err
	}
	switch entryKind {
	case "tree":
		if !isFullOID(oid, g.width) {
			return SourceEntry{}, fmt.Errorf("%w: tree %q has malformed id %q", ErrSnapshotUnsafeEntry, clean, oid)
		}
		return SourceEntry{Path: clean, Present: true, Mode: modeTree, Kind: "tree", OID: oid}, nil
	case "gitlink":
		if !isFullOID(oid, g.width) {
			return SourceEntry{}, fmt.Errorf("%w: gitlink %q has malformed id %q", ErrSnapshotUnsafeEntry, clean, oid)
		}
		return SourceEntry{Path: clean, Present: true, Mode: modeGitlink, Kind: "gitlink", OID: oid}, nil
	default: // regular | symlink
		if !isFullOID(oid, g.width) {
			return SourceEntry{}, fmt.Errorf("%w: blob %q has malformed id %q", ErrSnapshotUnsafeEntry, clean, oid)
		}
		return g.readBlob(ctx, clean, mode, oid, maxBytes)
	}
}

// readBlob checks the blob's size before reading its content, then binds the
// echoed object's id, type, and payload size to the request.
func (g *gitTreeResolver) readBlob(ctx context.Context, clean, mode, oid string, maxBytes int64) (SourceEntry, error) {
	sizes, err := batchCheck(ctx, g.runner, g.root, []string{oid})
	if err != nil {
		return SourceEntry{}, err
	}
	meta, ok := sizes[oid]
	if !ok || meta.missing {
		return SourceEntry{}, fmt.Errorf("%w: %s", ErrSnapshotObjectMissing, oid)
	}
	if meta.typ != "blob" {
		return SourceEntry{}, fmt.Errorf("%w: %s is a %s, not a blob", ErrSnapshotMalformedStream, oid, meta.typ)
	}
	if maxBytes > 0 && meta.size > maxBytes {
		return SourceEntry{}, fmt.Errorf("%w: %q is %d bytes, bound %d", ErrSourceTooLarge, clean, meta.size, maxBytes)
	}
	objs, err := batchBlobs(ctx, g.runner, g.root, []string{oid}, snapshotGitOutputLimit)
	if err != nil {
		return SourceEntry{}, err
	}
	o, ok := objs[oid]
	if !ok || o.Missing {
		return SourceEntry{}, fmt.Errorf("%w: %s", ErrSnapshotObjectMissing, oid)
	}
	if o.Type != "blob" || int64(len(o.Data)) != meta.size {
		return SourceEntry{}, fmt.Errorf("%w: %s payload does not match its checked size/type", ErrSnapshotMalformedStream, oid)
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

// fsHeadResolver reads a reused workspace head's paths from the live worktree
// with rooted, no-follow semantics. It never follows a link or opens a special
// path.
type fsHeadResolver struct {
	root  string
	width int
}

func (r *fsHeadResolver) read(ctx context.Context, p string, maxBytes int64) (SourceEntry, error) {
	clean, err := safeTreePath(p)
	if err != nil {
		return SourceEntry{}, err
	}
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return SourceEntry{}, err
	}
	defer root.Close()
	info, err := root.Lstat(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SourceEntry{Path: clean, Present: false, Kind: "absent"}, nil
		}
		return SourceEntry{}, err
	}
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		target, err := boundedio.ReadlinkNoFollow(root, clean)
		if err != nil {
			return SourceEntry{}, err
		}
		b := []byte(target)
		side := BlobSide{Present: true, Bytes: b}
		return SourceEntry{Path: clean, Present: true, Mode: modeSymlink, Kind: "symlink", Bytes: b, TextClass: ThresholdText(clean, clean, side, side)}, nil
	case mode.IsRegular():
		f, err := boundedio.OpenRegularNoFollow(root, clean)
		if err != nil {
			return SourceEntry{}, err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return SourceEntry{}, err
		}
		bound := snapshotReadCeiling
		if maxBytes > 0 {
			if st.Size() > maxBytes {
				return SourceEntry{}, fmt.Errorf("%w: %q is %d bytes, bound %d", ErrSourceTooLarge, clean, st.Size(), maxBytes)
			}
			bound = maxBytes
		}
		data, err := boundedio.ReadAllContext(ctx, f, bound)
		if err != nil {
			if errors.Is(err, boundedio.ErrTooLarge) {
				return SourceEntry{}, fmt.Errorf("%w: %q grew past its bound", ErrSourceTooLarge, clean)
			}
			return SourceEntry{}, err
		}
		m := modeRegular
		if st.Mode()&0o111 != 0 {
			m = modeExecutable
		}
		side := BlobSide{Present: true, Bytes: data}
		return SourceEntry{Path: clean, Present: true, Mode: m, Kind: "regular", Bytes: data, TextClass: ThresholdText(clean, clean, side, side)}, nil
	default:
		return SourceEntry{Path: clean, Present: true, Kind: "special"}, nil
	}
}

// classifyEntry validates a git (mode, type) pair against the exhaustive matrix
// and returns the entry kind.
func classifyEntry(mode, typ string) (string, error) {
	switch mode {
	case "040000":
		if typ != "tree" {
			return "", fmt.Errorf("%w: mode 040000 with type %q", ErrSnapshotUnsafeEntry, typ)
		}
		return "tree", nil
	case "160000":
		if typ != "commit" {
			return "", fmt.Errorf("%w: mode 160000 with type %q", ErrSnapshotUnsafeEntry, typ)
		}
		return "gitlink", nil
	case "100644", "100755":
		if typ != "blob" {
			return "", fmt.Errorf("%w: mode %s with type %q", ErrSnapshotUnsafeEntry, mode, typ)
		}
		return "regular", nil
	case "120000":
		if typ != "blob" {
			return "", fmt.Errorf("%w: mode 120000 with type %q", ErrSnapshotUnsafeEntry, typ)
		}
		return "symlink", nil
	default:
		return "", fmt.Errorf("%w: unsupported mode %q", ErrSnapshotUnsafeEntry, mode)
	}
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

// ReusableView carries the current project store a HeadView may reuse when the
// reviewed content is exactly the current clean worktree at the resolved head
// (or the live workspace). The HeadView does not own the store, so its Close
// never touches it. Eligibility is always verified by OpenHeadView from observed
// state (index completeness, published signature, on-disk row bytes, and - for a
// committed head - HEAD equality over a clean worktree); a caller never asserts
// reuse. The Query and Context services are constructed by OpenHeadView from this
// verified Store and Root, never accepted from the caller, so they can never be
// bound to a different index than the one that was verified.
type ReusableView struct {
	Root  string
	Store *store.Store
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
// Reuse, when set, wraps existing services if and only if reuse is verified. The
// Max* bounds default to their v1 constants when zero.
type HeadViewOptions struct {
	Runner       GitRunner
	RepoRoot     string
	ObjectFormat string
	Config       config.Config

	HeadTreeish string
	BaseTreeish string

	Reuse *ReusableView

	// SnapshotParent is the directory a materialized snapshot is created under.
	// It defaults to <git-common-dir>/prowl/snapshots when empty.
	SnapshotParent string

	MaxEntries    int
	MaxBlobBytes  int64
	MaxTotalBytes int64
	// MaxSymlinkBytes bounds the target size of a materialized symlink. A symlink
	// blob larger than this (a real link target is tiny; an oversized one is
	// malformed/hostile) is recorded as an omission rather than read into memory,
	// keeping the view available. It defaults to MaxSymlinkTargetBytesV1.
	MaxSymlinkBytes int64

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

func (o HeadViewOptions) maxSymlinkBytes() int64 {
	if o.MaxSymlinkBytes > 0 {
		return o.MaxSymlinkBytes
	}
	return MaxSymlinkTargetBytesV1
}

func (o HeadViewOptions) symlinker() symlinkFunc {
	if o.symlink != nil {
		return o.symlink
	}
	return rootSymlink
}

// OpenHeadView builds an immutable head view. When opts.Reuse is set and reuse
// is verified (matching root, complete published index equal to the current
// worktree, and - for a committed head - the current HEAD equal to the resolved
// head over a clean worktree) it wraps those services with a no-op Close.
// Otherwise it materializes opts.HeadTreeish into a private snapshot below Git
// common state and indexes it. The side-aware SourceResolver resolves arbitrary
// base/head paths in every mode.
func OpenHeadView(ctx context.Context, opts HeadViewOptions) (*HeadView, error) {
	if opts.Runner == nil {
		return nil, errors.New("review: OpenHeadView requires a runner")
	}
	width, ok := oidWidthFor(opts.ObjectFormat)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedObjectFormat, opts.ObjectFormat)
	}
	opt := index.Options{Ignore: opts.Config.Ignore, Languages: opts.Config.Languages}

	// A base tree-ish, when present, is used for immutable source reads on both the
	// reuse and materialize paths; require a full resolved OID so a mutable ref can
	// never name different content later.
	if opts.BaseTreeish != "" && !isFullOID(opts.BaseTreeish, width) {
		return nil, fmt.Errorf("%w: base %q", ErrSnapshotTreeishNotResolved, opts.BaseTreeish)
	}

	if opts.Reuse != nil {
		eligible, err := eligibleReuse(ctx, opts, opt, width)
		if err != nil {
			return nil, err
		}
		if eligible {
			return newReuseHeadView(opts, width), nil
		}
		if opts.HeadTreeish == "" {
			return nil, ErrReuseUnavailable
		}
	}

	if opts.HeadTreeish == "" {
		return nil, errors.New("review: OpenHeadView requires a head tree-ish to materialize")
	}
	if !isFullOID(opts.HeadTreeish, width) {
		return nil, fmt.Errorf("%w: head %q", ErrSnapshotTreeishNotResolved, opts.HeadTreeish)
	}
	return materializeHeadView(ctx, opts, width, opt)
}

// newSourceResolver builds the side-aware resolver for the given options.
func newSourceResolver(opts HeadViewOptions, width int, workspaceHead bool) *sideSourceResolver {
	resolver := &sideSourceResolver{}
	if opts.BaseTreeish != "" {
		resolver.base = &gitTreeResolver{runner: opts.Runner, root: opts.RepoRoot, width: width, treeish: opts.BaseTreeish}
	}
	switch {
	case workspaceHead:
		resolver.head = &fsHeadResolver{root: opts.RepoRoot, width: width}
	case opts.HeadTreeish != "":
		resolver.head = &gitTreeResolver{runner: opts.Runner, root: opts.RepoRoot, width: width, treeish: opts.HeadTreeish}
	}
	return resolver
}

// newReuseHeadView wraps the verified reused store, constructing the query and
// context services from that store and root so they can never be bound to a
// different index than the one eligibleReuse verified.
func newReuseHeadView(opts HeadViewOptions, width int) *HeadView {
	reuse := opts.Reuse
	return &HeadView{
		Root:    reuse.Root,
		Store:   reuse.Store,
		Query:   query.New(reuse.Store),
		Context: &contextpacket.Service{Store: reuse.Store, Root: reuse.Root},
		Sources: newSourceResolver(opts, width, opts.HeadTreeish == ""),
		Close:   func() error { return nil },
	}
}

// eligibleReuse verifies every condition under which the current index may stand
// in for the reviewed head. It trusts no caller assertion: it requires complete
// non-nil reused services rooted at the capture root, a published index whose
// recomputed signature matches the stored one, and every indexed row's bytes
// still present unchanged on disk (index.ValidateSnapshotContext, which closes
// the stale-row-with-same-meta hole). For a committed head it additionally
// requires the current HEAD to equal the full resolved head over a clean
// tracked+untracked worktree. A workspace reuse (no resolved head tree-ish)
// reviews the live worktree, so it has no HEAD/clean condition. Any unmet
// condition returns false (materialize / unavailable); only a genuine error
// (cancellation, git/index failure) returns an error.
func eligibleReuse(ctx context.Context, opts HeadViewOptions, opt index.Options, width int) (bool, error) {
	r := opts.Reuse
	if r.Root == "" || r.Store == nil || r.Root != opts.RepoRoot {
		return false, nil
	}
	state, err := r.Store.GetMeta("index_state")
	if err != nil {
		return false, err
	}
	if state != "complete" {
		return false, nil
	}
	sig, err := index.SignatureWithOptionsContext(ctx, r.Root, opt)
	if err != nil {
		return false, err
	}
	stored, err := r.Store.GetMeta("cli_sig")
	if err != nil {
		return false, err
	}
	if stored != strconv.FormatUint(sig, 16) {
		return false, nil
	}
	// Prove every published row was derived from the exact bytes still on disk;
	// a stale row whose freshness meta happens to match is caught here.
	if err := index.ValidateSnapshotContext(ctx, r.Store, r.Root); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, nil
	}
	// Workspace reuse: the reviewed head is the live worktree, with no resolved
	// head OID to match. The signature+row validation above already binds the
	// reused index to the current worktree bytes.
	if opts.HeadTreeish == "" {
		return true, nil
	}
	// Committed reuse: the current HEAD must equal the full resolved head over a
	// clean tracked+untracked worktree.
	if !isFullOID(opts.HeadTreeish, width) {
		return false, nil
	}
	head, err := resolveCommitOID(ctx, opts.Runner, r.Root, "HEAD")
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(head, opts.HeadTreeish) {
		return false, nil
	}
	return worktreeClean(ctx, opts.Runner, r.Root)
}

// worktreeClean reports whether the worktree has no tracked or untracked change.
func worktreeClean(ctx context.Context, runner GitRunner, root string) (bool, error) {
	out, err := runner.Output(ctx, root, snapshotGitOutputLimit, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimRight(out, "\x00")) == 0, nil
}

// materializeHeadView enumerates and writes the head tree into a private
// snapshot, builds a private index, and returns an owning HeadView.
func materializeHeadView(ctx context.Context, opts HeadViewOptions, width int, opt index.Options) (*HeadView, error) {
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
	if _, err := index.IndexWithOptionsContext(ctx, db, contentDir, opt); err != nil {
		_ = db.Close()
		cleanup()
		return nil, err
	}

	return &HeadView{
		Root:      work,
		Store:     db,
		Query:     query.New(db),
		Context:   &contextpacket.Service{Store: db, Root: contentDir},
		Sources:   newSourceResolver(opts, width, false),
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
	// Stream ls-tree through Pipe into an incremental NUL parser that stops at
	// maxEntries+1. When the parser rejects a record (cap exceeded or a hostile
	// entry) its Write returns an error, which the runner surfaces as a terminal
	// failure and uses to kill the git producer, so a runaway stream is never
	// fully buffered.
	parser := newTreeStreamParser(width, opts.maxEntries())
	pipeErr := opts.Runner.Pipe(ctx, opts.RepoRoot, snapshotGitOutputLimit,
		bytes.NewReader(nil), parser, "ls-tree", "-rz", "--full-tree", "--end-of-options", opts.HeadTreeish)
	if parser.err != nil {
		return nil, parser.err
	}
	if pipeErr != nil {
		return nil, pipeErr
	}
	entries, err := parser.result()
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
	// Preflight: prove every blob's size before any content read or write, so an
	// oversized or missing object fails closed with nothing on disk. An oversized
	// symlink target (a real link is tiny; a larger one is malformed/hostile) is
	// recorded as an omission and never read, keeping the view available instead
	// of aborting materialization on a hostile commit.
	var omissions []Omission
	skip := make([]bool, len(entries))
	remaining := opts.maxTotalBytes()
	for i, e := range entries {
		meta, ok := sizes[e.oid]
		if !ok || !isBlobMode(e.mode) {
			continue
		}
		if meta.missing {
			return nil, fmt.Errorf("%w: %s", ErrSnapshotObjectMissing, e.oid)
		}
		if meta.typ != "blob" {
			return nil, fmt.Errorf("%w: %s is a %s, not a blob", ErrSnapshotMalformedStream, e.oid, meta.typ)
		}
		if e.mode == "120000" {
			if meta.size > opts.maxSymlinkBytes() {
				omissions = append(omissions, Omission{Path: e.path, Side: SideHead, Reason: "oversized symlink target"})
				skip[i] = true
				continue
			}
		} else if meta.size > opts.maxBlobBytes() {
			return nil, fmt.Errorf("%w: %q is %d bytes", ErrSnapshotBlobTooLarge, e.path, meta.size)
		}
		// Overflow-safe total accounting: subtraction never overflows.
		if meta.size > remaining {
			return nil, fmt.Errorf("%w: %q exceeds the remaining budget", ErrSnapshotTotalTooLarge, e.path)
		}
		remaining -= meta.size
	}

	root, err := os.OpenRoot(contentDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	symlink := opts.symlinker()
	writeBlob := func(e treeEntry, data []byte) error {
		if dir := path.Dir(e.path); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		if e.mode == "120000" {
			if bytes.IndexByte(data, 0) >= 0 {
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wave := make([]string, 0, 64)
		waveRemaining := snapshotContentBatchBytes
		seen := map[string]bool{}
		i := start
		for ; i < len(entries); i++ {
			e := entries[i]
			if e.mode == "160000" || !isBlobMode(e.mode) || skip[i] {
				continue
			}
			meta := sizes[e.oid]
			if seen[e.oid] {
				continue
			}
			// Remaining-based accounting: never add sizes that could overflow.
			if len(wave) > 0 && meta.size > waveRemaining {
				break
			}
			wave = append(wave, e.oid)
			seen[e.oid] = true
			waveRemaining -= meta.size
		}
		end := i
		objs := map[string]CatFileObject{}
		if len(wave) > 0 {
			objs, err = batchBlobs(ctx, opts.Runner, opts.RepoRoot, wave, snapshotContentBatchBytes+int64(len(wave))*128+2)
			if err != nil {
				return nil, err
			}
		}
		for j := start; j < end; j++ {
			e := entries[j]
			if e.mode == "160000" || !isBlobMode(e.mode) || skip[j] {
				continue
			}
			o, ok := objs[e.oid]
			if !ok || o.Missing {
				return nil, fmt.Errorf("%w: %s", ErrSnapshotObjectMissing, e.oid)
			}
			if o.Type != "blob" || int64(len(o.Data)) != sizes[e.oid].size {
				return nil, fmt.Errorf("%w: %s payload does not match its checked size/type", ErrSnapshotMalformedStream, e.oid)
			}
			if err := writeBlob(e, o.Data); err != nil {
				return nil, err
			}
		}
		start = end
	}
	return omissions, nil
}

// treeStreamParser is an io.Writer that parses `ls-tree -rz --full-tree` output
// incrementally as it streams from git. It validates each NUL-terminated record
// the moment it completes and returns a terminal error from Write as soon as a
// record is malformed, unsafe, or the entry cap is exceeded, so the runner kills
// the git producer instead of buffering an unbounded stream. Partial records are
// buffered across writes.
type treeStreamParser struct {
	width      int
	maxEntries int
	buf        []byte
	entries    []treeEntry
	files      map[string]bool
	dirs       map[string]bool
	err        error
}

func newTreeStreamParser(width, maxEntries int) *treeStreamParser {
	return &treeStreamParser{
		width:      width,
		maxEntries: maxEntries,
		files:      map[string]bool{},
		dirs:       map[string]bool{},
	}
}

// Write consumes a stream chunk, completing and validating every whole record it
// contains. A terminal validation error is recorded and returned so the runner
// stops the producer.
func (p *treeStreamParser) Write(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, 0)
		if i < 0 {
			break
		}
		rec := p.buf[:i]
		p.buf = p.buf[i+1:]
		if err := p.consume(rec); err != nil {
			p.err = err
			return 0, err
		}
	}
	return len(b), nil
}

// consume validates and records one complete record.
func (p *treeStreamParser) consume(rec []byte) error {
	if len(rec) == 0 {
		return fmt.Errorf("%w: empty ls-tree record", ErrSnapshotMalformedStream)
	}
	mode, typ, oid, path, err := parseLsTreeRecord(rec)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotMalformedStream, err)
	}
	if _, err := classifyEntry(mode, typ); err != nil {
		return err
	}
	if !isFullOID(oid, p.width) {
		return fmt.Errorf("%w: entry %q has malformed object id %q", ErrSnapshotUnsafeEntry, path, oid)
	}
	clean, err := safeTreePath(path)
	if err != nil {
		return err
	}
	if p.files[clean] || p.dirs[clean] {
		return fmt.Errorf("%w: duplicate or conflicting path %q", ErrSnapshotUnsafeEntry, clean)
	}
	for _, ancestor := range ancestorPaths(clean) {
		if p.files[ancestor] {
			return fmt.Errorf("%w: %q conflicts with file %q", ErrSnapshotUnsafeEntry, clean, ancestor)
		}
		p.dirs[ancestor] = true
	}
	p.files[clean] = true
	p.entries = append(p.entries, treeEntry{mode: mode, typ: typ, oid: oid, path: clean})
	if len(p.entries) > p.maxEntries {
		return fmt.Errorf("%w: more than %d entries", ErrSnapshotTooManyEntries, p.maxEntries)
	}
	return nil
}

// result returns the parsed entries once the stream is exhausted, rejecting a
// trailing partial record (missing terminal NUL).
func (p *treeStreamParser) result() ([]treeEntry, error) {
	if p.err != nil {
		return nil, p.err
	}
	if len(p.buf) != 0 {
		return nil, fmt.Errorf("%w: ls-tree output not NUL-terminated", ErrSnapshotMalformedStream)
	}
	return p.entries, nil
}

// parseTreeEntries parses a complete `ls-tree -rz --full-tree` byte slice through
// the same incremental parser the streaming materializer uses.
func parseTreeEntries(out []byte, width, maxEntries int) ([]treeEntry, error) {
	p := newTreeStreamParser(width, maxEntries)
	if _, err := p.Write(out); err != nil {
		return nil, err
	}
	return p.result()
}

// safeTreePath validates a tree entry path: non-empty valid UTF-8, relative
// (not absolute), forward-slash separated, and free of "." / ".." / empty
// components and NUL/backslash. It returns the cleaned slash path.
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
	if strings.Contains(p, "\\") {
		return "", fmt.Errorf("%w: backslash in path %q", ErrSnapshotUnsafeEntry, p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: unsafe component in path %q", ErrSnapshotUnsafeEntry, p)
		}
	}
	return p, nil
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
// without reading their content, verifying echoed id and request order and
// rejecting a negative, overflowing, or MaxInt64 size. A missing object is
// reported, never fetched.
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
	// git cat-file --batch-check emits exactly one newline-terminated line per
	// requested object. Require exact framing: a trailing newline and precisely
	// one line per request, so a truncated or padded stream is rejected.
	raw := stdout.String()
	if !strings.HasSuffix(raw, "\n") {
		return nil, fmt.Errorf("%w: batch-check output not newline-terminated", ErrSnapshotMalformedStream)
	}
	lines := strings.Split(raw[:len(raw)-1], "\n")
	if len(lines) != len(order) {
		return nil, fmt.Errorf("%w: batch-check returned %d lines, want %d", ErrSnapshotMalformedStream, len(lines), len(order))
	}
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], order[i]) {
			return nil, fmt.Errorf("%w: batch-check echoed %q, want %s", ErrSnapshotMalformedStream, line, order[i])
		}
		if len(fields) == 2 && fields[1] == "missing" {
			result[order[i]] = batchCheckMeta{missing: true}
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: bad batch-check line %q", ErrSnapshotMalformedStream, line)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 || size == math.MaxInt64 {
			return nil, fmt.Errorf("%w: bad object size in %q", ErrSnapshotMalformedStream, line)
		}
		result[order[i]] = batchCheckMeta{typ: fields[1], size: size}
	}
	return result, nil
}

// batchBlobs reads a set of (deduplicated) object ids in one cat-file batch,
// verifying the echoed id and request order and returning exact bytes.
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
		if !strings.EqualFold(objs[i].OID, oid) {
			return nil, fmt.Errorf("%w: cat-file echoed %q, want %s", ErrSnapshotMalformedStream, objs[i].OID, oid)
		}
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
