package review

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/prowl-agent/prowl-agent/internal/boundedio"
)

// captureGitOutputLimit bounds every capture Git subprocess whose stdout is not
// otherwise budget-checked: raw status, untracked enumeration, cat-file batch,
// ls-tree/ls-files gitlink resolution, and each forced-text no-index diff. It is
// a hard ceiling; a runaway or hostile stream is killed rather than buffered.
const captureGitOutputLimit = 256 << 20

// captureTrackedReadCeiling bounds an individual tracked or object-backed
// workspace read for memory safety. It is deliberately not the untracked
// exact-accounting cap: tracked binary/mode content above 64 MiB must not fail
// under the untracked limits, only under this generous ceiling.
const captureTrackedReadCeiling int64 = 256 << 20

// Git canonical file modes as octal uint32 values.
const (
	modeRegular    uint32 = 0o100644
	modeExecutable uint32 = 0o100755
	modeSymlink    uint32 = 0o120000
	modeGitlink    uint32 = 0o160000
)

var (
	// ErrInvalidPathEncoding reports a tracked, renamed, or untracked path whose
	// raw bytes are not valid UTF-8. v1 requires valid UTF-8 paths; the error
	// carries the offending bytes as an ASCII-quoted literal.
	ErrInvalidPathEncoding = errors.New("review: changed path is not valid UTF-8")
	// ErrCaptureFileTooLarge reports an untracked file above the exact per-file
	// accounting bound. Capture fails rather than estimate or truncate churn.
	ErrCaptureFileTooLarge = errors.New("review: untracked file exceeds the exact-accounting byte bound")
	// ErrCaptureTotalTooLarge reports that untracked content read for exact
	// accounting exceeded the per-plan total bound.
	ErrCaptureTotalTooLarge = errors.New("review: untracked content exceeds the total exact-accounting byte bound")
	// ErrCanonicalPatchOverflow reports that canonical raw hunk payload exceeded
	// its byte cap. Capture fails rather than downgrade to an estimate.
	ErrCanonicalPatchOverflow = errors.New("review: canonical raw hunk payload exceeds its byte cap")
	// ErrTooManyChangedPaths reports that the changed-path count exceeded its cap.
	ErrTooManyChangedPaths = errors.New("review: changed path count exceeds its cap")
	// ErrUnsupportedCaptureScope reports a capture request for a scope kind this
	// implementation does not support.
	ErrUnsupportedCaptureScope = errors.New("review: unsupported capture scope kind")
	// ErrConcurrentModification reports that a workspace entry changed type or
	// vanished between enumeration and its rooted read. A replacement is never
	// followed; the capture fails so a later transaction can retry.
	ErrConcurrentModification = errors.New("review: workspace entry changed during capture")
	// ErrHeadMoved reports that the live workspace HEAD no longer equals the
	// resolved scope base. Workspace capture is pinned to the resolved base OID
	// so two passes cannot silently diff against different bases.
	ErrHeadMoved = errors.New("review: workspace HEAD moved from the resolved scope base")
	// ErrGitlinkUnresolved reports that a submodule (gitlink) object id could not
	// be resolved to a full identity, so capture fails closed rather than emit an
	// absent or truncated gitlink side.
	ErrGitlinkUnresolved = errors.New("review: gitlink object id could not be resolved")
)

// CaptureRunner is the sanitized Git surface CaptureOnce needs: the object
// protocols of GitRunner plus raw status and forced-text no-index diffs. ExecGit
// satisfies it. Capture never calls a patch subcommand through the generic
// runners; every diff flows through the pinned, attribute-free helpers.
type CaptureRunner interface {
	GitRunner
	RawStatus(ctx context.Context, root string, limit int64, args ...string) ([]byte, error)
	DiffNoIndex(ctx context.Context, root string, limit int64, base, head string) ([]byte, error)
}

// Capturer performs one deterministic raw change capture of a resolved scope.
// For a workspace scope, Root is the pinned workspace directory and every
// workspace-side read is opened relative to a descriptor for it with no-follow
// semantics; commit/range scopes read only from Git objects and never touch the
// worktree. Runner is the sanitized Git surface. AfterFingerprint, when set, is
// invoked once after every rooted descriptor has been read and every fingerprint
// computed, so a test can force a concurrent type/content change and prove the
// capture is immutable.
//
// The Max* bounds default to their v1 constants when zero; production capture
// uses the defaults, and tests may lower them to exercise the fail-closed
// accounting bounds without materializing gigabytes.
type Capturer struct {
	Root             string
	Runner           CaptureRunner
	AfterFingerprint func() // test seam only

	MaxFileBytes      int64
	MaxTotalBytes     int64
	MaxCanonicalBytes int64
	MaxChangedPaths   int
}

func (c *Capturer) maxFileBytes() int64 {
	if c.MaxFileBytes > 0 {
		return c.MaxFileBytes
	}
	return MaxUntrackedFileBytesV1
}

func (c *Capturer) maxTotalBytes() int64 {
	if c.MaxTotalBytes > 0 {
		return c.MaxTotalBytes
	}
	return MaxUntrackedTotalBytesV1
}

func (c *Capturer) maxCanonicalBytes() int64 {
	if c.MaxCanonicalBytes > 0 {
		return c.MaxCanonicalBytes
	}
	return MaxCanonicalPatchBytesV1
}

func (c *Capturer) maxChangedPaths() int {
	if c.MaxChangedPaths > 0 {
		return c.MaxChangedPaths
	}
	return MaxChangedPathsV1
}

// CaptureOnce performs a single immutable capture of the resolved scope. It
// classifies every existing side with ThresholdTextV1, diffs text paths through
// attribute-free forced-text no-index diffs, counts exact raw additions and
// deletions, and produces canonical path/hunk records, content/OID digests, and
// the finalized scope digest. It never computes reviewable_churn; hunk
// reviewability sizing is a later phase. The complete two-capture/two-refresh
// retry transaction belongs to the plan service; CaptureOnce is one capture.
func (c *Capturer) CaptureOnce(ctx context.Context, scope Scope) (Capture, error) {
	width, ok := oidWidthFor(scope.ObjectFormat)
	if !ok {
		return Capture{}, fmt.Errorf("%w: %q", ErrUnsupportedObjectFormat, scope.ObjectFormat)
	}
	st := &captureState{c: c, ctx: ctx, scope: scope, width: width, blobs: map[string]catBlob{}}
	switch scope.Kind {
	case ScopeWorkspace:
		return st.captureWorkspace()
	case ScopeCommit, ScopeRange:
		return st.captureCommitted()
	default:
		return Capture{}, fmt.Errorf("%w: %q", ErrUnsupportedCaptureScope, scope.Kind)
	}
}

// captureState carries the per-capture accounting so the phase helpers stay
// small and share the byte budgets and computed sides.
type captureState struct {
	c         *Capturer
	ctx       context.Context
	scope     Scope
	width     int
	root      *os.Root // workspace only
	committed bool
	baseHex   string
	headHex   string

	tracked   []RawChange
	untracked []string
	blobs     map[string]catBlob

	records     []RawPathRecord
	treeRecords []WorkspaceTreeRecord
	rawAdd      int
	rawDel      int

	untrackedRead  int64
	canonicalBytes int64
}

// catBlob is one resolved blob: the full object id git echoed for a possibly
// abbreviated request, plus its exact bytes.
type catBlob struct {
	fullOID string
	bytes   []byte
}

// sideData is one fully-read revision side ready for classification and framing.
type sideData struct {
	present bool
	special bool // FIFO/socket/device/other: unreviewable, never opened
	gitlink bool // submodule: no blob bytes, identity is a resolved commit id
	kind    string
	mode    uint32
	bytes   []byte
	oidHex  string
}

func (d sideData) identity() SideIdentity {
	if !d.present || d.special || d.oidHex == "" {
		return AbsentSide()
	}
	raw, err := hex.DecodeString(d.oidHex)
	if err != nil {
		return AbsentSide()
	}
	return SideIdentity{Kind: SideGitOID, Value: raw}
}

func (d sideData) blobSide() BlobSide {
	if !d.present || d.special || d.gitlink {
		return BlobSide{}
	}
	return BlobSide{Present: true, Bytes: d.bytes}
}

// ---- workspace capture ----------------------------------------------------

// captureWorkspace captures the current worktree against the resolved base OID
// (not the movable HEAD), pinning the base so two passes cannot silently switch
// it, then enumerates tracked and untracked changes, reads new sides through
// rooted no-follow descriptors, and derives the workspace head fingerprint.
func (s *captureState) captureWorkspace() (Capture, error) {
	root, err := os.OpenRoot(s.c.Root)
	if err != nil {
		return Capture{}, fmt.Errorf("review: open workspace root: %w", err)
	}
	defer root.Close()
	s.root = root
	s.baseHex = hex.EncodeToString(s.scope.Base.Value)

	head, err := s.revParseHead()
	if err != nil {
		return Capture{}, err
	}
	if !strings.EqualFold(head, s.baseHex) {
		return Capture{}, fmt.Errorf("%w: HEAD %s != base %s", ErrHeadMoved, head, s.baseHex)
	}

	rawOut, err := s.c.Runner.RawStatus(s.ctx, s.c.Root, captureGitOutputLimit, s.baseHex)
	if err != nil {
		return Capture{}, err
	}
	if err := s.parseTracked(rawOut); err != nil {
		return Capture{}, err
	}

	untrackedOut, err := s.c.Runner.Output(s.ctx, s.c.Root, captureGitOutputLimit,
		"ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Capture{}, err
	}
	paths, err := splitUntrackedZ(untrackedOut)
	if err != nil {
		return Capture{}, err
	}
	s.untracked = paths

	if total := len(s.tracked) + len(s.untracked); total > s.c.maxChangedPaths() {
		return Capture{}, fmt.Errorf("%w: %d paths", ErrTooManyChangedPaths, total)
	}

	if err := s.resolveBlobs(s.workspaceBlobOIDs()); err != nil {
		return Capture{}, err
	}

	for _, ch := range s.tracked {
		rec, tree, err := s.workspaceTrackedRecord(ch)
		if err != nil {
			return Capture{}, err
		}
		s.records = append(s.records, rec)
		if tree != nil {
			s.treeRecords = append(s.treeRecords, *tree)
		}
	}
	for _, p := range s.untracked {
		rec, tree, err := s.untrackedRecord(p)
		if err != nil {
			return Capture{}, err
		}
		s.records = append(s.records, rec)
		if tree != nil {
			s.treeRecords = append(s.treeRecords, *tree)
		}
	}

	// Every rooted descriptor has been read; the fingerprint below is derived
	// only from in-memory bytes, so a post-read change cannot leak in.
	return s.finish(WorkspaceHeadIdentity(s.treeRecords))
}

// revParseHead returns the current HEAD object id, used to pin the workspace
// capture to the resolved scope base.
func (s *captureState) revParseHead() (string, error) {
	out, err := s.c.Runner.Output(s.ctx, s.c.Root, captureGitOutputLimit, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// workspaceBlobOIDs are the base blob object ids the workspace capture must
// resolve: one per non-addition change whose base is a regular file or symlink.
func (s *captureState) workspaceBlobOIDs() []string {
	var oids []string
	for _, ch := range s.tracked {
		if !isAdd(ch.Status) && isBlobMode(ch.OldMode) {
			oids = append(oids, ch.OldOID)
		}
	}
	return oids
}

// workspaceTrackedRecord builds one canonical record for a tracked change:
// the base side from a resolved blob or the base tree (gitlink), and the new
// side read from the workspace through rooted no-follow descriptors.
func (s *captureState) workspaceTrackedRecord(ch RawChange) (RawPathRecord, *WorkspaceTreeRecord, error) {
	oldPath, newPath := changePaths(ch)

	old, err := s.workspaceOldSide(ch, oldPath)
	if err != nil {
		return RawPathRecord{}, nil, err
	}
	var new sideData
	if newPath != "" {
		new, err = s.workspaceNewSide(ch, newPath)
		if err != nil {
			return RawPathRecord{}, nil, err
		}
	}
	return s.assemble("tracked", ch.Status, oldPath, newPath, old, new)
}

// workspaceOldSide builds the base side of a tracked change from a resolved blob
// or the base tree for a gitlink.
func (s *captureState) workspaceOldSide(ch RawChange, oldPath string) (sideData, error) {
	if ch.OldMode == "000000" || ch.OldMode == "" {
		return sideData{}, nil
	}
	if ch.OldMode == "160000" {
		return s.gitlinkTreeSide(s.baseHex, oldPath)
	}
	if isBlobMode(ch.OldMode) {
		return s.blobSide(ch.OldOID, ch.OldMode)
	}
	return sideData{}, nil
}

// workspaceNewSide builds the new side of a tracked change: a gitlink resolved
// from the index, or a rooted no-follow read of the workspace entry.
func (s *captureState) workspaceNewSide(ch RawChange, newPath string) (sideData, error) {
	if ch.NewMode == "160000" {
		return s.gitlinkIndexSide(newPath)
	}
	return s.readNewSide(newPath, false)
}

// untrackedRecord builds one canonical all-addition record for an untracked
// path read from the workspace with the untracked accounting bounds applied.
func (s *captureState) untrackedRecord(path string) (RawPathRecord, *WorkspaceTreeRecord, error) {
	new, err := s.readNewSide(path, true)
	if err != nil {
		return RawPathRecord{}, nil, err
	}
	return s.assemble("untracked", "A", "", path, sideData{}, new)
}

// ---- committed (commit/range) capture -------------------------------------

// captureCommitted captures the change between the resolved base and head trees
// directly from Git objects, with no worktree involvement. Both sides are read
// from Git blobs (regular/symlink) or resolved from the trees (gitlinks); the
// scope head is already an object id, so no workspace fingerprint is derived.
func (s *captureState) captureCommitted() (Capture, error) {
	s.committed = true
	s.baseHex = hex.EncodeToString(s.scope.Base.Value)
	s.headHex = hex.EncodeToString(s.scope.Head.Value)

	rawOut, err := s.c.Runner.RawStatus(s.ctx, s.c.Root, captureGitOutputLimit, s.baseHex, s.headHex)
	if err != nil {
		return Capture{}, err
	}
	if err := s.parseTracked(rawOut); err != nil {
		return Capture{}, err
	}
	if len(s.tracked) > s.c.maxChangedPaths() {
		return Capture{}, fmt.Errorf("%w: %d paths", ErrTooManyChangedPaths, len(s.tracked))
	}
	if err := s.resolveBlobs(s.committedBlobOIDs()); err != nil {
		return Capture{}, err
	}
	for _, ch := range s.tracked {
		rec, err := s.committedRecord(ch)
		if err != nil {
			return Capture{}, err
		}
		s.records = append(s.records, rec)
	}
	return s.finish(s.scope.Head)
}

// committedBlobOIDs are the base and head blob object ids the committed capture
// must resolve: base blobs of non-additions and head blobs of non-deletions.
func (s *captureState) committedBlobOIDs() []string {
	var oids []string
	for _, ch := range s.tracked {
		if !isAdd(ch.Status) && isBlobMode(ch.OldMode) {
			oids = append(oids, ch.OldOID)
		}
		if !isDelete(ch.Status) && isBlobMode(ch.NewMode) {
			oids = append(oids, ch.NewOID)
		}
	}
	return oids
}

// committedRecord builds one canonical record for a committed change, reading
// both sides from Git objects (blobs) or resolving gitlinks from the trees.
func (s *captureState) committedRecord(ch RawChange) (RawPathRecord, error) {
	oldPath, newPath := changePaths(ch)

	old, err := s.committedSide(ch.OldMode, ch.OldOID, s.baseHex, oldPath)
	if err != nil {
		return RawPathRecord{}, err
	}
	new, err := s.committedSide(ch.NewMode, ch.NewOID, s.headHex, newPath)
	if err != nil {
		return RawPathRecord{}, err
	}
	rec, _, err := s.assemble("tracked", ch.Status, oldPath, newPath, old, new)
	return rec, err
}

// committedSide builds one object-backed side: absent when the mode is zero, a
// gitlink resolved from its tree, or a blob from the resolved batch.
func (s *captureState) committedSide(mode, oid, rev, path string) (sideData, error) {
	if mode == "000000" || mode == "" {
		return sideData{}, nil
	}
	if mode == "160000" {
		return s.gitlinkTreeSide(rev, path)
	}
	if isBlobMode(mode) {
		return s.blobSide(oid, mode)
	}
	return sideData{}, nil
}

// ---- shared helpers -------------------------------------------------------

// parseTracked parses NUL-delimited raw status and rejects any changed path
// whose raw bytes are not valid UTF-8 before it becomes a Go string.
func (s *captureState) parseTracked(rawOut []byte) error {
	changes, err := ParseRawStatusZ(rawOut)
	if err != nil {
		return err
	}
	for _, ch := range changes {
		if !utf8.ValidString(ch.Path) {
			return fmt.Errorf("%w: %s", ErrInvalidPathEncoding, strconv.QuoteToASCII(ch.Path))
		}
		if ch.OldPath != "" && !utf8.ValidString(ch.OldPath) {
			return fmt.Errorf("%w: %s", ErrInvalidPathEncoding, strconv.QuoteToASCII(ch.OldPath))
		}
	}
	s.tracked = changes
	return nil
}

// resolveBlobs resolves a set of (possibly abbreviated) blob object ids in one
// cat-file batch, keyed by the requested id and echoing the full id and bytes.
func (s *captureState) resolveBlobs(oids []string) error {
	var order []string
	seen := map[string]bool{}
	for _, oid := range oids {
		if oid == "" || seen[oid] {
			continue
		}
		seen[oid] = true
		order = append(order, oid)
	}
	if len(order) == 0 {
		return nil
	}
	var stdin bytes.Buffer
	for _, oid := range order {
		stdin.WriteString(oid)
		stdin.WriteByte('\n')
	}
	var stdout bytes.Buffer
	if err := s.c.Runner.Pipe(s.ctx, s.c.Root, captureGitOutputLimit, &stdin, &stdout, "cat-file", "--batch"); err != nil {
		return err
	}
	objs, err := ParseCatFileBatch(stdout.Bytes())
	if err != nil {
		return err
	}
	if len(objs) != len(order) {
		return fmt.Errorf("review: cat-file returned %d objects, want %d", len(objs), len(order))
	}
	for i, oid := range order {
		o := objs[i]
		if o.Missing {
			return fmt.Errorf("review: object %s missing from the local object store", oid)
		}
		if o.Type != "blob" {
			return fmt.Errorf("review: object %s is a %s, not a blob", oid, o.Type)
		}
		s.blobs[oid] = catBlob{fullOID: o.OID, bytes: o.Data}
	}
	return nil
}

// blobSide builds a regular/symlink side from an already-resolved blob.
func (s *captureState) blobSide(oid, mode string) (sideData, error) {
	blob, ok := s.blobs[oid]
	if !ok {
		return sideData{}, fmt.Errorf("review: blob %s was not resolved before use", oid)
	}
	side := sideData{present: true, bytes: blob.bytes, oidHex: blob.fullOID}
	switch mode {
	case "120000":
		side.kind, side.mode = "symlink", modeSymlink
	case "100755":
		side.kind, side.mode = "regular", modeExecutable
	default:
		side.kind, side.mode = "regular", modeRegular
	}
	return side, nil
}

// gitlinkTreeSide resolves a gitlink's full commit id from a tree so different
// submodule commits at the same path yield distinct identities.
func (s *captureState) gitlinkTreeSide(rev, path string) (sideData, error) {
	if path == "" || rev == "" {
		return sideData{}, fmt.Errorf("%w: missing tree/path for gitlink", ErrGitlinkUnresolved)
	}
	out, err := s.c.Runner.Output(s.ctx, s.c.Root, captureGitOutputLimit, "ls-tree", "-z", "--full-tree", rev, "--", path)
	if err != nil {
		return sideData{}, err
	}
	oid, err := parseLsTreeOID(out)
	if err != nil {
		return sideData{}, fmt.Errorf("%w: %q@%s: %v", ErrGitlinkUnresolved, path, rev, err)
	}
	return s.gitlinkSide(oid, path)
}

// gitlinkIndexSide resolves a workspace gitlink's full commit id from the index,
// failing closed when the entry is absent or its id is zero/short.
func (s *captureState) gitlinkIndexSide(path string) (sideData, error) {
	out, err := s.c.Runner.Output(s.ctx, s.c.Root, captureGitOutputLimit, "ls-files", "-s", "-z", "--", path)
	if err != nil {
		return sideData{}, err
	}
	oid, err := parseLsFilesOID(out)
	if err != nil {
		return sideData{}, fmt.Errorf("%w: %q in index: %v", ErrGitlinkUnresolved, path, err)
	}
	return s.gitlinkSide(oid, path)
}

// gitlinkSide validates a resolved gitlink id to the object-format width and
// builds an unreviewable gitlink side carrying that identity.
func (s *captureState) gitlinkSide(oid, path string) (sideData, error) {
	if !isFullOID(oid, s.width) {
		return sideData{}, fmt.Errorf("%w: %q resolved to %q", ErrGitlinkUnresolved, path, oid)
	}
	return sideData{present: true, gitlink: true, kind: "gitlink", mode: modeGitlink, oidHex: oid}, nil
}

// readNewSide inspects a workspace path with a rooted no-follow Lstat and reads
// only verified regular files (through OpenRegularNoFollow) and symlinks
// (through ReadlinkNoFollow). FIFO/socket/device/other special entries are
// recorded unreviewable without being opened or followed. The untracked flag
// selects the exact-accounting bounds: untracked reads enforce the 64 MiB/512
// MiB caps, tracked reads use only the memory-safety ceiling.
func (s *captureState) readNewSide(path string, untracked bool) (sideData, error) {
	info, err := s.root.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return sideData{}, fmt.Errorf("%w: %q vanished: %v", ErrConcurrentModification, path, err)
		}
		return sideData{}, err
	}
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		target, err := boundedio.ReadlinkNoFollow(s.root, path)
		if err != nil {
			return sideData{}, err
		}
		b := []byte(target)
		if untracked {
			if err := s.accountUntracked(path, int64(len(b))); err != nil {
				return sideData{}, err
			}
		}
		return sideData{present: true, kind: "symlink", mode: modeSymlink, bytes: b, oidHex: s.blobOID(b)}, nil
	case mode.IsRegular():
		return s.readRegular(path, untracked)
	default:
		// FIFO, socket, device, directory, or other special entry: unreviewable
		// and never opened or followed.
		return sideData{present: true, special: true, kind: "special"}, nil
	}
}

// readRegular reads a verified regular file through a no-follow descriptor. An
// untracked read enforces the exact per-file and per-plan accounting caps; a
// tracked read is bounded only by the memory-safety ceiling so large tracked
// content never fails under the untracked limits. Either way capture fails
// closed rather than estimating or truncating.
func (s *captureState) readRegular(path string, untracked bool) (sideData, error) {
	f, err := boundedio.OpenRegularNoFollow(s.root, path)
	if err != nil {
		if errors.Is(err, boundedio.ErrNonRegular) || errors.Is(err, boundedio.ErrSymlink) {
			return sideData{}, fmt.Errorf("%w: %q: %v", ErrConcurrentModification, path, err)
		}
		return sideData{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sideData{}, err
	}
	size := info.Size()

	bound := captureTrackedReadCeiling
	if untracked {
		bound = s.c.maxFileBytes()
		if size > bound {
			return sideData{}, fmt.Errorf("%w: %q is %d bytes", ErrCaptureFileTooLarge, path, size)
		}
		if s.untrackedRead+size > s.c.maxTotalBytes() {
			return sideData{}, fmt.Errorf("%w: %q would bring the total to %d bytes", ErrCaptureTotalTooLarge, path, s.untrackedRead+size)
		}
	}

	b, err := boundedio.ReadAllContext(s.ctx, f, bound)
	if err != nil {
		if errors.Is(err, boundedio.ErrTooLarge) {
			if untracked {
				return sideData{}, fmt.Errorf("%w: %q grew past its bound during capture", ErrCaptureFileTooLarge, path)
			}
			return sideData{}, fmt.Errorf("review: tracked workspace file %q exceeds the %d-byte read ceiling: %w", path, bound, err)
		}
		return sideData{}, err
	}
	if untracked {
		if err := s.accountUntracked(path, int64(len(b))); err != nil {
			return sideData{}, err
		}
	}
	mode := modeRegular
	if info.Mode()&0o111 != 0 {
		mode = modeExecutable
	}
	return sideData{present: true, kind: "regular", mode: mode, bytes: b, oidHex: s.blobOID(b)}, nil
}

// accountUntracked adds n untracked bytes to the running total and fails when
// the exact per-plan accounting bound is exceeded.
func (s *captureState) accountUntracked(path string, n int64) error {
	s.untrackedRead += n
	if s.untrackedRead > s.c.maxTotalBytes() {
		return fmt.Errorf("%w: %q brought the total to %d bytes", ErrCaptureTotalTooLarge, path, s.untrackedRead)
	}
	return nil
}

// assemble classifies a path from its two sides using each side's own path,
// diffs text paths, and produces the canonical record plus the workspace-tree
// record for its final state.
func (s *captureState) assemble(kind, status, oldPath, newPath string, old, new sideData) (RawPathRecord, *WorkspaceTreeRecord, error) {
	rec := RawPathRecord{
		Kind:    kind,
		Status:  status,
		OldPath: oldPath,
		NewPath: newPath,
		OldMode: old.mode,
		NewMode: new.mode,
		OldSide: old.identity(),
		NewSide: new.identity(),
	}
	if old.special || new.special || old.gitlink || new.gitlink {
		rec.TextClass = string(TextClassBinary)
		return rec, treeRecordFor(newPath, new), nil
	}

	class := ThresholdText(oldPath, newPath, old.blobSide(), new.blobSide())
	rec.TextClass = string(class)
	if old.present {
		d := Digest(sha256.Sum256(old.bytes))
		rec.OldContentDigest = &d
	}
	if new.present {
		d := Digest(sha256.Sum256(new.bytes))
		rec.NewContentDigest = &d
	}
	if class == TextClassText {
		hunks, add, del, err := s.diffSides(old, new)
		if err != nil {
			return RawPathRecord{}, nil, err
		}
		rec.Hunks = hunks
		rec.Additions = uint64(add)
		rec.Deletions = uint64(del)
		s.rawAdd += add
		s.rawDel += del
	}
	return rec, treeRecordFor(newPath, new), nil
}

// treeRecordFor produces the workspace head tree record for a path's final
// state, or nil when the path no longer exists (a deletion). Regular and symlink
// final states carry a content digest; gitlink and special states do not.
func treeRecordFor(newPath string, new sideData) *WorkspaceTreeRecord {
	if newPath == "" || !new.present {
		return nil
	}
	tr := WorkspaceTreeRecord{Path: newPath, Mode: new.mode, Kind: new.kind}
	if !new.special && !new.gitlink {
		d := Digest(sha256.Sum256(new.bytes))
		tr.Content = &d
	}
	return &tr
}

// diffSides produces canonical hunks and exact add/delete counts for a text path
// by diffing the two raw side blobs through a sanitized, attribute-free
// forced-text no-index diff over private temporary files. Identical sides (a
// pure rename or mode change) produce no hunks and no churn. The accumulated
// canonical payload is capped; overflow fails rather than estimating.
func (s *captureState) diffSides(old, new sideData) ([]RawHunk, int, int, error) {
	var oldBytes, newBytes []byte
	if old.present {
		oldBytes = old.bytes
	}
	if new.present {
		newBytes = new.bytes
	}
	if bytes.Equal(oldBytes, newBytes) {
		return nil, 0, 0, nil
	}
	dir, err := os.MkdirTemp("", "prowl-review-diff-")
	if err != nil {
		return nil, 0, 0, err
	}
	defer os.RemoveAll(dir)
	oldFile := filepath.Join(dir, "old")
	newFile := filepath.Join(dir, "new")
	if err := os.WriteFile(oldFile, oldBytes, 0o600); err != nil {
		return nil, 0, 0, err
	}
	if err := os.WriteFile(newFile, newBytes, 0o600); err != nil {
		return nil, 0, 0, err
	}
	out, err := s.c.Runner.DiffNoIndex(s.ctx, dir, captureGitOutputLimit, oldFile, newFile)
	if err != nil {
		return nil, 0, 0, err
	}
	hunks, err := ParseUnifiedPatch(out)
	if err != nil {
		return nil, 0, 0, err
	}
	var add, del int
	for i := range hunks {
		a, d := hunkPayloadChurn(hunks[i].Payload)
		add += a
		del += d
		s.canonicalBytes += int64(len(hunks[i].Payload))
		if s.canonicalBytes > s.c.maxCanonicalBytes() {
			return nil, 0, 0, fmt.Errorf("%w: %d bytes", ErrCanonicalPatchOverflow, s.canonicalBytes)
		}
	}
	return hunks, add, del, nil
}

// finish computes the canonical patch digest and the finalized scope digest for
// the given head identity, then returns the immutable capture. The
// AfterFingerprint seam fires once, after all reads and fingerprints, so a race
// injected there cannot alter this result. reviewable_churn is left zero.
func (s *captureState) finish(head SideIdentity) (Capture, error) {
	canonicalDigest := CanonicalPatchDigest(s.records)
	scope := s.scope
	scope.Head = head
	scope.Digest = ScopeDigest(scope.ObjectFormat, scope.Base, head, scope.Kind, canonicalDigest)
	if err := scope.Validate(); err != nil {
		return Capture{}, err
	}
	if s.c.AfterFingerprint != nil {
		s.c.AfterFingerprint()
	}
	return Capture{
		Scope:           scope,
		Paths:           s.records,
		RawAdditions:    s.rawAdd,
		RawDeletions:    s.rawDel,
		RawChurn:        s.rawAdd + s.rawDel,
		ReviewableChurn: 0,
		ChangedPaths:    len(s.records),
		CanonicalPatch:  canonicalDigest,
	}, nil
}

// blobOID computes a side's Git blob object id in the repository object format
// without writing the object: SHA over "blob <size>\x00" followed by the exact
// content bytes (the target bytes for a symlink).
func (s *captureState) blobOID(content []byte) string {
	header := "blob " + strconv.Itoa(len(content)) + "\x00"
	if s.scope.ObjectFormat == "sha256" {
		h := sha256.New()
		h.Write([]byte(header))
		h.Write(content)
		return hex.EncodeToString(h.Sum(nil))
	}
	h := sha1.New()
	h.Write([]byte(header))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// ---- small pure helpers ---------------------------------------------------

// changePaths derives the old and new paths of a change from its status and
// modes: renames/copies carry both, an addition has no old path, a deletion no
// new path, and every other change shares one path on both sides.
func changePaths(ch RawChange) (oldPath, newPath string) {
	if strings.HasPrefix(ch.Status, "R") || strings.HasPrefix(ch.Status, "C") {
		return ch.OldPath, ch.Path
	}
	oldPath, newPath = ch.Path, ch.Path
	if ch.OldMode == "000000" || ch.OldMode == "" {
		oldPath = ""
	}
	if ch.NewMode == "000000" || ch.NewMode == "" {
		newPath = ""
	}
	return oldPath, newPath
}

func isAdd(status string) bool    { return strings.HasPrefix(status, "A") }
func isDelete(status string) bool { return strings.HasPrefix(status, "D") }

// isBlobMode reports whether a git mode names a regular file or symlink blob.
func isBlobMode(mode string) bool {
	switch mode {
	case "100644", "100755", "120000":
		return true
	}
	return false
}

// isFullOID reports whether oid is a full, non-zero object id of the format's
// hex width.
func isFullOID(oid string, width int) bool {
	if len(oid) != width*2 {
		return false
	}
	zero := true
	for _, c := range oid {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
		if c != '0' {
			zero = false
		}
	}
	return !zero
}

// splitUntrackedZ splits git's NUL-delimited untracked enumeration, rejecting a
// missing terminator, an empty record, and any path whose raw bytes are not
// valid UTF-8 before converting it to a Go string.
func splitUntrackedZ(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("%w: untracked list not NUL-terminated", ErrMalformedRawStatus)
	}
	var paths []string
	for _, seg := range bytes.Split(data[:len(data)-1], []byte{0}) {
		if len(seg) == 0 {
			return nil, fmt.Errorf("%w: empty untracked path", ErrMalformedRawStatus)
		}
		if !utf8.Valid(seg) {
			return nil, fmt.Errorf("%w: %s", ErrInvalidPathEncoding, strconv.QuoteToASCII(string(seg)))
		}
		paths = append(paths, string(seg))
	}
	return paths, nil
}

// parseLsTreeOID extracts the object id from `git ls-tree -z` output for a
// single entry: "<mode> <type> <oid>\t<path>\0".
func parseLsTreeOID(out []byte) (string, error) {
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		fields := strings.Fields(string(rec[:tab]))
		if len(fields) >= 3 {
			return fields[2], nil
		}
	}
	return "", errors.New("no tree entry")
}

// parseLsFilesOID extracts the object id from `git ls-files -s -z` output for a
// single entry: "<mode> <oid> <stage>\t<path>\0".
func parseLsFilesOID(out []byte) (string, error) {
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		fields := strings.Fields(string(rec[:tab]))
		if len(fields) >= 2 {
			return fields[1], nil
		}
	}
	return "", errors.New("no index entry")
}
