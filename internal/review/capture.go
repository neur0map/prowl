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
// and each forced-text no-index diff. It is a hard ceiling; a runaway or hostile
// stream is killed rather than buffered without bound.
const captureGitOutputLimit = 256 << 20

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
	// ErrCaptureFileTooLarge reports a workspace file above the exact per-file
	// accounting bound. Capture fails rather than estimate or truncate churn.
	ErrCaptureFileTooLarge = errors.New("review: workspace file exceeds the exact-accounting byte bound")
	// ErrCaptureTotalTooLarge reports that workspace content read for exact
	// accounting exceeded the per-plan total bound.
	ErrCaptureTotalTooLarge = errors.New("review: workspace content exceeds the total exact-accounting byte bound")
	// ErrCanonicalPatchOverflow reports that canonical raw hunk payload exceeded
	// its byte cap. Capture fails rather than downgrade to an estimate.
	ErrCanonicalPatchOverflow = errors.New("review: canonical raw hunk payload exceeds its byte cap")
	// ErrTooManyChangedPaths reports that the changed-path count exceeded its cap.
	ErrTooManyChangedPaths = errors.New("review: changed path count exceeds its cap")
	// ErrUnsupportedCaptureScope reports a capture request for a scope kind this
	// phase does not materialize. Committed range/commit materialization is a
	// later phase; CaptureOnce handles the current workspace.
	ErrUnsupportedCaptureScope = errors.New("review: capture supports only workspace scope in this phase")
	// ErrConcurrentModification reports that a workspace entry changed type or
	// vanished between enumeration and its rooted read. A replacement is never
	// followed; the capture fails so a later transaction can retry.
	ErrConcurrentModification = errors.New("review: workspace entry changed during capture")
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

// Capturer performs one deterministic raw change capture of a workspace scope.
// Root is the pinned workspace directory; every workspace-side read is opened
// relative to a descriptor for it with no-follow semantics. Runner is the
// sanitized Git surface. AfterFingerprint, when set, is invoked once after every
// rooted descriptor has been read and every fingerprint computed, so a test can
// force a concurrent type/content change and prove the capture is immutable.
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

// CaptureOnce performs a single immutable workspace capture: it enumerates
// tracked changes and non-ignored untracked paths, classifies every existing
// side with ThresholdTextV1, diffs text paths through attribute-free forced-text
// no-index diffs, and counts exact raw additions/deletions. It computes canonical
// path/content fingerprints, workspace blob OIDs without writing objects, and the
// WorkspaceTreeV1 head identity, then finalizes the scope digest. It never
// computes reviewable_churn; hunk reviewability sizing is a later phase.
//
// The complete two-capture/two-refresh retry transaction belongs to the plan
// service; CaptureOnce is one capture of that transaction.
func (c *Capturer) CaptureOnce(ctx context.Context, scope Scope) (Capture, error) {
	if scope.Kind != ScopeWorkspace {
		return Capture{}, fmt.Errorf("%w: %q", ErrUnsupportedCaptureScope, scope.Kind)
	}
	width, ok := oidWidthFor(scope.ObjectFormat)
	if !ok {
		return Capture{}, fmt.Errorf("%w: %q", ErrUnsupportedObjectFormat, scope.ObjectFormat)
	}
	root, err := os.OpenRoot(c.Root)
	if err != nil {
		return Capture{}, fmt.Errorf("review: open workspace root: %w", err)
	}
	defer root.Close()

	st := &captureState{c: c, ctx: ctx, scope: scope, width: width, root: root}
	if err := st.enumerate(); err != nil {
		return Capture{}, err
	}
	if err := st.readOldSides(); err != nil {
		return Capture{}, err
	}
	// Every rooted workspace descriptor is read here; nothing below re-reads the
	// filesystem, so a change made after this point cannot leak into the result.
	if err := st.buildRecords(); err != nil {
		return Capture{}, err
	}
	result, err := st.finalize()
	if err != nil {
		return Capture{}, err
	}
	if c.AfterFingerprint != nil {
		c.AfterFingerprint()
	}
	return result, nil
}

// captureState carries the per-capture accounting so the phase helpers stay
// small and share the byte budgets and computed sides.
type captureState struct {
	c     *Capturer
	ctx   context.Context
	scope Scope
	width int
	root  *os.Root

	tracked   []RawChange
	untracked []string
	oldBlobs  map[string]catBlob

	records     []RawPathRecord
	treeRecords []WorkspaceTreeRecord
	rawAdd      int
	rawDel      int

	totalRead      int64
	canonicalBytes int64
}

// catBlob is one resolved old-side blob: the full object id git echoed for a
// possibly abbreviated request, plus its exact bytes.
type catBlob struct {
	fullOID string
	bytes   []byte
}

// sideData is one fully-read revision side ready for classification and framing.
type sideData struct {
	present bool
	special bool // FIFO/socket/device/other: unreviewable, never opened
	gitlink bool // submodule: no worktree bytes are read
	kind    string
	mode    uint32
	bytes   []byte
	oidHex  string
}

func (d sideData) identity() SideIdentity {
	if !d.present || d.special || d.gitlink || d.oidHex == "" {
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

// enumerate collects tracked changes and untracked paths, validating UTF-8 path
// encoding and the changed-path cap before any content is read.
func (s *captureState) enumerate() error {
	rawOut, err := s.c.Runner.RawStatus(s.ctx, s.c.Root, captureGitOutputLimit, "HEAD")
	if err != nil {
		return err
	}
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

	untrackedOut, err := s.c.Runner.Output(s.ctx, s.c.Root, captureGitOutputLimit,
		"ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	paths, err := splitUntrackedZ(untrackedOut)
	if err != nil {
		return err
	}
	s.untracked = paths

	if total := len(s.tracked) + len(s.untracked); total > s.c.maxChangedPaths() {
		return fmt.Errorf("%w: %d paths", ErrTooManyChangedPaths, total)
	}
	return nil
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

// readOldSides resolves every tracked change's existing base blob in one
// cat-file batch, keyed by the (possibly abbreviated) object id git reported.
func (s *captureState) readOldSides() error {
	s.oldBlobs = map[string]catBlob{}
	var order []string
	seen := map[string]bool{}
	for _, ch := range s.tracked {
		if !hasOldBlob(ch) || seen[ch.OldOID] {
			continue
		}
		seen[ch.OldOID] = true
		order = append(order, ch.OldOID)
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
			return fmt.Errorf("review: base blob %s missing from the local object store", oid)
		}
		if o.Type != "blob" {
			return fmt.Errorf("review: base object %s is a %s, not a blob", oid, o.Type)
		}
		s.oldBlobs[oid] = catBlob{fullOID: o.OID, bytes: o.Data}
	}
	return nil
}

// hasOldBlob reports whether a change has an existing base blob whose bytes must
// be read: any non-addition whose base mode is a regular file or symlink.
func hasOldBlob(ch RawChange) bool {
	if strings.HasPrefix(ch.Status, "A") {
		return false
	}
	switch ch.OldMode {
	case "100644", "100755", "120000":
		return true
	}
	return false
}

// buildRecords reads every new side through rooted no-follow descriptors and
// assembles the canonical tracked and untracked path records in enumeration
// order. Records are sorted canonically when the digest is computed.
func (s *captureState) buildRecords() error {
	for _, ch := range s.tracked {
		rec, tree, err := s.trackedRecord(ch)
		if err != nil {
			return err
		}
		s.records = append(s.records, rec)
		if tree != nil {
			s.treeRecords = append(s.treeRecords, *tree)
		}
	}
	for _, p := range s.untracked {
		rec, tree, err := s.untrackedRecord(p)
		if err != nil {
			return err
		}
		s.records = append(s.records, rec)
		if tree != nil {
			s.treeRecords = append(s.treeRecords, *tree)
		}
	}
	return nil
}

// trackedRecord builds one canonical record for a tracked change, deriving the
// old/new paths from the status, reading the new side from the workspace, and
// classifying and diffing text sides.
func (s *captureState) trackedRecord(ch RawChange) (RawPathRecord, *WorkspaceTreeRecord, error) {
	var oldPath, newPath string
	switch {
	case ch.Status == "A":
		newPath = ch.Path
	case ch.Status == "D":
		oldPath = ch.Path
	case strings.HasPrefix(ch.Status, "R"), strings.HasPrefix(ch.Status, "C"):
		oldPath, newPath = ch.OldPath, ch.Path
	default:
		oldPath, newPath = ch.Path, ch.Path
	}

	old := s.oldSide(ch)
	var new sideData
	if newPath != "" {
		ns, err := s.readNewSide(newPath, ch.NewMode)
		if err != nil {
			return RawPathRecord{}, nil, err
		}
		new = ns
	}
	return s.assemble("tracked", ch.Status, oldPath, newPath, old, new)
}

// untrackedRecord builds one canonical all-addition record for an untracked
// path: an absent base and a new side read from the workspace.
func (s *captureState) untrackedRecord(path string) (RawPathRecord, *WorkspaceTreeRecord, error) {
	new, err := s.readNewSide(path, "")
	if err != nil {
		return RawPathRecord{}, nil, err
	}
	return s.assemble("untracked", "A", "", path, sideData{}, new)
}

// oldSide builds the base side from an already-resolved blob, or an absent side.
func (s *captureState) oldSide(ch RawChange) sideData {
	if ch.OldMode == "160000" {
		return sideData{present: true, gitlink: true, kind: "gitlink", mode: modeGitlink}
	}
	if !hasOldBlob(ch) {
		return sideData{}
	}
	blob := s.oldBlobs[ch.OldOID]
	side := sideData{present: true, bytes: blob.bytes, oidHex: blob.fullOID}
	switch ch.OldMode {
	case "120000":
		side.kind, side.mode = "symlink", modeSymlink
	case "100755":
		side.kind, side.mode = "regular", modeExecutable
	default:
		side.kind, side.mode = "regular", modeRegular
	}
	return side
}

// readNewSide inspects a workspace path with a rooted no-follow Lstat and reads
// only verified regular files (through OpenRegularNoFollow) and symlinks
// (through ReadlinkNoFollow). Gitlinks and special entries are recorded without
// being opened or followed. gitMode, when set from git's raw status, marks a
// tracked gitlink so a submodule directory is never treated as a regular tree.
func (s *captureState) readNewSide(path, gitMode string) (sideData, error) {
	if gitMode == "160000" {
		return sideData{present: true, gitlink: true, kind: "gitlink", mode: modeGitlink}, nil
	}
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
		if err := s.account(path, int64(len(b))); err != nil {
			return sideData{}, err
		}
		return sideData{present: true, kind: "symlink", mode: modeSymlink, bytes: b, oidHex: s.blobOID(b)}, nil
	case mode.IsRegular():
		return s.readRegular(path)
	default:
		// FIFO, socket, device, directory, or other special entry: unreviewable
		// and never opened or followed.
		return sideData{present: true, special: true, kind: "special"}, nil
	}
}

// readRegular reads a verified regular file through a no-follow descriptor,
// enforcing the exact per-file and per-plan byte bounds before and after the
// read so capture fails closed rather than estimating or truncating.
func (s *captureState) readRegular(path string) (sideData, error) {
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
	if size > s.c.maxFileBytes() {
		return sideData{}, fmt.Errorf("%w: %q is %d bytes", ErrCaptureFileTooLarge, path, size)
	}
	if s.totalRead+size > s.c.maxTotalBytes() {
		return sideData{}, fmt.Errorf("%w: %q would bring the total to %d bytes", ErrCaptureTotalTooLarge, path, s.totalRead+size)
	}
	b, err := boundedio.ReadAllContext(s.ctx, f, s.c.maxFileBytes())
	if err != nil {
		if errors.Is(err, boundedio.ErrTooLarge) {
			return sideData{}, fmt.Errorf("%w: %q grew past its bound during capture", ErrCaptureFileTooLarge, path)
		}
		return sideData{}, err
	}
	if err := s.account(path, int64(len(b))); err != nil {
		return sideData{}, err
	}
	mode := modeRegular
	if info.Mode()&0o111 != 0 {
		mode = modeExecutable
	}
	return sideData{present: true, kind: "regular", mode: mode, bytes: b, oidHex: s.blobOID(b)}, nil
}

// account adds n workspace bytes to the running total and fails when the exact
// per-plan accounting bound is exceeded.
func (s *captureState) account(path string, n int64) error {
	s.totalRead += n
	if s.totalRead > s.c.maxTotalBytes() {
		return fmt.Errorf("%w: %q brought the total to %d bytes", ErrCaptureTotalTooLarge, path, s.totalRead)
	}
	return nil
}

// assemble classifies a path from its two sides, diffs text paths, and produces
// the canonical record plus the workspace-tree record for its final state.
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
	unreviewable := old.special || new.special || old.gitlink || new.gitlink
	if unreviewable {
		rec.TextClass = string(TextClassBinary)
		return rec, treeRecordFor(newPath, new), nil
	}

	// A deletion classifies by its base path, so a removed recognized source is
	// still text even when its bytes carry NUL.
	classPath := newPath
	if classPath == "" {
		classPath = oldPath
	}
	class := ThresholdText(classPath, old.blobSide(), new.blobSide())
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

// finalize computes the canonical patch digest, the workspace head identity from
// the sorted final-state tree records, and the finalized scope digest, then
// returns the immutable capture. reviewable_churn is left zero for a later phase.
func (s *captureState) finalize() (Capture, error) {
	canonicalDigest := CanonicalPatchDigest(s.records)
	head := WorkspaceHeadIdentity(s.treeRecords)
	scope := s.scope
	scope.Head = head
	scope.Digest = ScopeDigest(scope.ObjectFormat, scope.Base, head, ScopeWorkspace, canonicalDigest)
	if err := scope.Validate(); err != nil {
		return Capture{}, err
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
