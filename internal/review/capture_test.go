//go:build unix

package review

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/boundedio"
)

// TestCaptureThresholdIgnoresGitAttributes proves threshold churn is computed
// from a forced-text diff and is not suppressed by a `-diff` gitattribute.
func TestCaptureThresholdIgnoresGitAttributes(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, ".gitattributes", "*.go -diff\n")
	repo.commitFile(t, "large.go", "package p\n")
	repo.write(t, "large.go", "package p\n"+strings.Repeat("var X = 1\n", 301))

	cap := captureWorkspace(t, repo)
	_, structuredRequired := ModeForChurn(cap.RawChurn, false)
	if cap.RawChurn != 301 || !structuredRequired {
		t.Fatalf("raw churn=%d structured=%v; want 301/true", cap.RawChurn, structuredRequired)
	}
	rec := recordByNewPath(t, cap, "large.go")
	if rec.Additions != 301 || rec.Deletions != 0 || rec.TextClass != string(TextClassText) {
		t.Fatalf("record=%+v", rec)
	}
}

// TestCaptureThreshold300IsNotStructured pins the strict boundary: exactly 300
// raw changed lines do not require structured review.
func TestCaptureThreshold300IsNotStructured(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	repo.write(t, "seed.go", "package p\n"+strings.Repeat("var X = 1\n", 300))

	cap := captureWorkspace(t, repo)
	if cap.RawChurn != 300 {
		t.Fatalf("raw churn=%d, want 300", cap.RawChurn)
	}
	if _, structured := ModeForChurn(cap.RawChurn, false); structured {
		t.Fatalf("300 lines must not require structured review")
	}
}

// TestCaptureByteSemantics exercises the exact per-record byte, class, status,
// and mode accounting across every changed-path shape the spec distinguishes.
func TestCaptureByteSemantics(t *testing.T) {
	cases := []struct {
		name                 string
		setup                func(t *testing.T, repo *gitFixture)
		verify               func(t *testing.T, cap Capture)
		readsWorktreeSymlink bool
	}{
		{
			name: "CRLF is one line",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "seed.go", "package p\n")
				repo.write(t, "crlf.txt", "a\r\nb\r\nc\r\n")
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "crlf.txt")
				assertRec(t, rec, "A", TextClassText, 3, 0)
				if rec.Kind != "untracked" {
					t.Fatalf("kind=%q, want untracked", rec.Kind)
				}
			},
		},
		{
			name: "empty added file has zero additions",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "seed.go", "package p\n")
				repo.write(t, "empty.txt", "")
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "empty.txt")
				assertRec(t, rec, "A", TextClassText, 0, 0)
				if rec.NewContentDigest == nil {
					t.Fatalf("empty file still has content; digest must be set")
				}
			},
		},
		{
			name: "missing final newline counts the last line",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "seed.go", "package p\n")
				repo.write(t, "nonl.txt", "a\nb\nc")
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "nonl.txt")
				assertRec(t, rec, "A", TextClassText, 3, 0)
				last := rec.Hunks[len(rec.Hunks)-1]
				if !last.NoFinalNewlineNew || last.NoFinalNewlineOld {
					t.Fatalf("no-newline flags=%+v", last)
				}
			},
		},
		{
			name: "recognized source with NUL is text",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "seed.go", "package p\n")
				repo.write(t, "src.go", "package p\nvar Z\x00 = 1\n")
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "src.go")
				assertRec(t, rec, "A", TextClassText, 2, 0)
			},
		},
		{
			name: "true binary has no churn but is a changed path",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "seed.go", "package p\n")
				repo.write(t, "blob.bin", "\x00\x01\x02\x03")
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "blob.bin")
				assertRec(t, rec, "A", TextClassBinary, 0, 0)
				if len(rec.Hunks) != 0 {
					t.Fatalf("binary must have no hunks, got %d", len(rec.Hunks))
				}
				if rec.NewContentDigest == nil {
					t.Fatalf("binary content digest must be set")
				}
			},
		},
		{
			name: "deletion-only hunk counts deletions",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "del.go", "a\nb\nc\n")
				if err := os.Remove(filepath.Join(repo.root, "del.go")); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByOldPath(t, cap, "del.go")
				assertRec(t, rec, "D", TextClassText, 0, 3)
				if rec.NewSide.Kind != SideAbsent {
					t.Fatalf("deleted path new side=%q, want absent", rec.NewSide.Kind)
				}
			},
		},
		{
			name: "mode-only change has no churn",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "s.sh", "echo hi\n")
				if err := os.Chmod(filepath.Join(repo.root, "s.sh"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "s.sh")
				assertRec(t, rec, "M", TextClassText, 0, 0)
				if rec.OldMode != modeRegular || rec.NewMode != modeExecutable {
					t.Fatalf("modes old=%o new=%o, want 100644->100755", rec.OldMode, rec.NewMode)
				}
			},
		},
		{
			name: "content-identical rename has no churn",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "orig.go", "l1\nl2\nl3\nl4\n")
				rawGit(t, repo.root, "mv", "orig.go", "renamed.go")
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "renamed.go")
				if !strings.HasPrefix(rec.Status, "R") {
					t.Fatalf("status=%q, want a rename", rec.Status)
				}
				if rec.OldPath != "orig.go" {
					t.Fatalf("old path=%q, want orig.go", rec.OldPath)
				}
				if rec.Additions != 0 || rec.Deletions != 0 {
					t.Fatalf("rename churn add=%d del=%d, want 0/0", rec.Additions, rec.Deletions)
				}
			},
		},
		{
			name: "copy is captured as an addition",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "base.go", "shared\ncontent\nhere\n")
				repo.write(t, "copy.go", "shared\ncontent\nhere\n")
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "copy.go")
				if rec.Status != "A" {
					t.Fatalf("status=%q, want A (copy detection disabled)", rec.Status)
				}
				assertRec(t, rec, "A", TextClassText, 3, 0)
			},
		},
		{
			name: "regular to symlink transition diffs raw blobs",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "conv", "hello\nworld\n")
				if err := os.Remove(filepath.Join(repo.root, "conv")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("some/target", filepath.Join(repo.root, "conv")); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "conv")
				assertRec(t, rec, "T", TextClassText, 1, 2)
				if rec.OldMode != modeRegular || rec.NewMode != modeSymlink {
					t.Fatalf("modes old=%o new=%o, want 100644->120000", rec.OldMode, rec.NewMode)
				}
			},
			readsWorktreeSymlink: true,
		},
		{
			name: "symlink to regular transition diffs raw blobs",
			setup: func(t *testing.T, repo *gitFixture) {
				if err := os.Symlink("orig/target", filepath.Join(repo.root, "conv2")); err != nil {
					t.Fatal(err)
				}
				repo.commit(t, "seed symlink")
				if err := os.Remove(filepath.Join(repo.root, "conv2")); err != nil {
					t.Fatal(err)
				}
				repo.write(t, "conv2", "now regular\n")
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByNewPath(t, cap, "conv2")
				assertRec(t, rec, "T", TextClassText, 1, 1)
				if rec.OldMode != modeSymlink || rec.NewMode != modeRegular {
					t.Fatalf("modes old=%o new=%o, want 120000->100644", rec.OldMode, rec.NewMode)
				}
			},
		},
		{
			name: "committed NUL symlink blob is binary",
			setup: func(t *testing.T, repo *gitFixture) {
				repo.commitFile(t, "seed.go", "package p\n")
				repo.commitBlobEntry(t, "120000", "nulink", "bad\x00target")
			},
			verify: func(t *testing.T, cap Capture) {
				rec := recordByOldPath(t, cap, "nulink")
				assertRec(t, rec, "D", TextClassBinary, 0, 0)
				if rec.OldMode != modeSymlink {
					t.Fatalf("old mode=%o, want 120000", rec.OldMode)
				}
				if rec.OldSide.Kind != SideGitOID {
					t.Fatalf("old side=%q, want git_oid", rec.OldSide.Kind)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newGitFixture(t)
			tc.setup(t, repo)
			// Reading a worktree symlink needs a descriptor-tied no-follow
			// readlink, which only Linux provides; elsewhere capture must fail
			// closed with the deliberate unsupported ruling rather than succeed.
			if tc.readsWorktreeSymlink && runtime.GOOS != "linux" {
				if _, err := runCapture(t, repo, &Capturer{}); !errors.Is(err, boundedio.ErrUnsupported) {
					t.Fatalf("err=%v, want boundedio.ErrUnsupported on %s", err, runtime.GOOS)
				}
				return
			}
			cap := captureWorkspace(t, repo)
			tc.verify(t, cap)
		})
	}
}

// assertRec checks a record's status, class, and exact churn.
func assertRec(t *testing.T, rec RawPathRecord, status string, class TextClass, add, del uint64) {
	t.Helper()
	if status != "" && rec.Status != status && !strings.HasPrefix(rec.Status, status) {
		t.Fatalf("status=%q, want %q", rec.Status, status)
	}
	if rec.TextClass != string(class) {
		t.Fatalf("text class=%q, want %q", rec.TextClass, class)
	}
	if rec.Additions != add || rec.Deletions != del {
		t.Fatalf("churn add=%d del=%d, want %d/%d", rec.Additions, rec.Deletions, add, del)
	}
}

// TestCaptureSpecialFileNotOpened proves a tracked path whose worktree entry is
// a FIFO is recorded as unreviewable without being opened or followed.
func TestCaptureSpecialFileNotOpened(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "pipe.txt", "hello\n")
	if err := os.Remove(filepath.Join(repo.root, "pipe.txt")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(repo.root, "pipe.txt"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	cap := captureWorkspace(t, repo)
	rec := recordByNewPath(t, cap, "pipe.txt")
	if rec.TextClass != string(TextClassBinary) {
		t.Fatalf("special file class=%q, want binary", rec.TextClass)
	}
	if len(rec.Hunks) != 0 || rec.Additions != 0 || rec.Deletions != 0 {
		t.Fatalf("special file must have no churn: %+v", rec)
	}
	if rec.NewContentDigest != nil {
		t.Fatalf("special file must not be read, so it carries no content digest")
	}
	if rec.NewSide.Kind != SideAbsent {
		t.Fatalf("special file new side=%q, want absent (never opened)", rec.NewSide.Kind)
	}
}

// TestCaptureRejectsInvalidPathEncoding proves tracked, renamed, and untracked
// paths whose raw bytes are not valid UTF-8 fail closed with the escaped bytes.
func TestCaptureRejectsInvalidPathEncoding(t *testing.T) {
	bad := "bad\xffname.txt"

	t.Run("tracked", func(t *testing.T) {
		repo := newGitFixture(t)
		writeRawNamed(t, repo, bad, "one\n")
		rawGit(t, repo.root, "add", "--", bad)
		rawGit(t, repo.root, "commit", "-qm", "add bad")
		writeRawNamed(t, repo, bad, "one\ntwo\n")
		if _, err := runCapture(t, repo, &Capturer{}); !errors.Is(err, ErrInvalidPathEncoding) {
			t.Fatalf("err=%v, want ErrInvalidPathEncoding", err)
		}
	})

	t.Run("renamed", func(t *testing.T) {
		repo := newGitFixture(t)
		repo.commitFile(t, "good.go", "l1\nl2\nl3\nl4\n")
		rawGit(t, repo.root, "mv", "good.go", bad)
		if _, err := runCapture(t, repo, &Capturer{}); !errors.Is(err, ErrInvalidPathEncoding) {
			t.Fatalf("err=%v, want ErrInvalidPathEncoding", err)
		}
	})

	t.Run("untracked", func(t *testing.T) {
		repo := newGitFixture(t)
		repo.commitFile(t, "seed.go", "package p\n")
		writeRawNamed(t, repo, bad, "content\n")
		if _, err := runCapture(t, repo, &Capturer{}); !errors.Is(err, ErrInvalidPathEncoding) {
			t.Fatalf("err=%v, want ErrInvalidPathEncoding", err)
		}
	})
}

// writeRawNamed writes body to a raw (possibly non-UTF-8) file name.
func writeRawNamed(t *testing.T, f *gitFixture, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCaptureUntrackedFileBoundFailsClosed proves a workspace file above the
// per-file accounting bound fails the capture rather than being estimated.
func TestCaptureUntrackedFileBoundFailsClosed(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	repo.write(t, "big.txt", strings.Repeat("x", 64))
	if _, err := runCapture(t, repo, &Capturer{MaxFileBytes: 32}); !errors.Is(err, ErrCaptureFileTooLarge) {
		t.Fatalf("err=%v, want ErrCaptureFileTooLarge", err)
	}
}

// TestCaptureUntrackedTotalBoundFailsClosed proves the per-plan total bound is
// enforced across multiple workspace files.
func TestCaptureUntrackedTotalBoundFailsClosed(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	repo.write(t, "a.txt", strings.Repeat("x", 40))
	repo.write(t, "b.txt", strings.Repeat("y", 40))
	if _, err := runCapture(t, repo, &Capturer{MaxFileBytes: 1000, MaxTotalBytes: 60}); !errors.Is(err, ErrCaptureTotalTooLarge) {
		t.Fatalf("err=%v, want ErrCaptureTotalTooLarge", err)
	}
}

// TestCaptureCanonicalPatchOverflowFailsClosed proves canonical raw hunk payload
// overflow fails the capture rather than downgrading to an estimate.
func TestCaptureCanonicalPatchOverflowFailsClosed(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "big.go", "package p\n")
	repo.write(t, "big.go", "package p\n"+strings.Repeat("var X = 1\n", 200))
	if _, err := runCapture(t, repo, &Capturer{MaxCanonicalBytes: 64}); !errors.Is(err, ErrCanonicalPatchOverflow) {
		t.Fatalf("err=%v, want ErrCanonicalPatchOverflow", err)
	}
}

// TestCaptureChangedPathCapFailsClosed proves the changed-path cap fails closed.
func TestCaptureChangedPathCapFailsClosed(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	repo.write(t, "a.txt", "1\n")
	repo.write(t, "b.txt", "2\n")
	repo.write(t, "c.txt", "3\n")
	if _, err := runCapture(t, repo, &Capturer{MaxChangedPaths: 2}); !errors.Is(err, ErrTooManyChangedPaths) {
		t.Fatalf("err=%v, want ErrTooManyChangedPaths", err)
	}
}

// TestCaptureIsDeterministic proves two captures of an unchanged workspace
// produce identical fingerprints and churn.
func TestCaptureIsDeterministic(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "keep.go", "package p\nfunc F() {}\n")
	repo.commitFile(t, "mod.go", "package p\nvar A = 1\n")
	repo.write(t, "mod.go", "package p\nvar A = 2\nvar B = 3\n")
	repo.write(t, "new.txt", "brand new\nlines\n")

	a := captureWorkspace(t, repo)
	b := captureWorkspace(t, repo)
	if !sameFingerprint(a, b) {
		t.Fatalf("captures differ:\n a=%s\n b=%s", fingerprint(a), fingerprint(b))
	}
}

// TestCaptureWorkspaceHeadIsFinalized proves the capture replaces the provisional
// zero head with a deterministic non-zero workspace_sha256 identity.
func TestCaptureWorkspaceHeadIsFinalized(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	repo.write(t, "seed.go", "package p\nvar X = 1\n")

	cap := captureWorkspace(t, repo)
	if cap.Scope.Head.Kind != SideWorkspaceSHA256 || len(cap.Scope.Head.Value) != 32 {
		t.Fatalf("head=%+v, want a 32-byte workspace_sha256", cap.Scope.Head)
	}
	if bytes.Equal(cap.Scope.Head.Value, make([]byte, 32)) {
		t.Fatalf("head is still the provisional all-zero placeholder")
	}
	if err := cap.Scope.Validate(); err != nil {
		t.Fatalf("finalized scope invalid: %v", err)
	}
}

// TestCaptureDoesNotComputeReviewableChurn proves capture leaves reviewable_churn
// unset; hunk reviewability sizing is a later phase.
func TestCaptureDoesNotComputeReviewableChurn(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	repo.write(t, "seed.go", "package p\n"+strings.Repeat("var X = 1\n", 10))

	cap := captureWorkspace(t, repo)
	if cap.RawChurn != 10 {
		t.Fatalf("raw churn=%d, want 10", cap.RawChurn)
	}
	if cap.ReviewableChurn != 0 {
		t.Fatalf("reviewable churn=%d, want 0 (set by a later phase)", cap.ReviewableChurn)
	}
}

// TestCaptureAfterFingerprintRaceIsImmutable proves a type/content change made
// after all rooted descriptors are read cannot leak into the capture, and that a
// fresh capture does observe it.
func TestCaptureAfterFingerprintRaceIsImmutable(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "race.go", "package p\n")
	repo.write(t, "race.go", "package p\nvar X = 1\n")

	clean := captureWorkspace(t, repo)

	raced := captureWorkspaceWith(t, repo, &Capturer{AfterFingerprint: func() {
		repo.write(t, "race.go", "package p\nvar TOTALLY = 1\nvar DIFFERENT = 2\nvar CONTENT = 3\n")
	}})
	if !sameFingerprint(clean, raced) {
		t.Fatalf("mid-flight mutation leaked into the capture:\n clean=%s\n raced=%s", fingerprint(clean), fingerprint(raced))
	}

	fresh := captureWorkspace(t, repo)
	if sameFingerprint(clean, fresh) {
		t.Fatalf("fresh capture did not observe the committed mutation")
	}
}

// sameFingerprint reports whether two captures agree on every immutable
// fingerprint: canonical patch digest, scope digest, workspace head, and churn.
func sameFingerprint(a, b Capture) bool {
	return a.CanonicalPatch == b.CanonicalPatch &&
		a.Scope.Digest == b.Scope.Digest &&
		a.Scope.Head.Kind == b.Scope.Head.Kind &&
		bytes.Equal(a.Scope.Head.Value, b.Scope.Head.Value) &&
		a.RawChurn == b.RawChurn &&
		a.ChangedPaths == b.ChangedPaths
}

func fingerprint(c Capture) string {
	return string(c.Scope.Head.Value) + "|" + string(c.CanonicalPatch[:]) + "|" +
		string(rune(c.RawChurn)) + "|" + string(rune(c.ChangedPaths))
}

// TestCaptureCommitScope proves a single-commit scope captures its diff against
// the first parent directly from Git objects with exact churn.
func TestCaptureCommitScope(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "a.go", "package p\n")
	repo.commitFile(t, "a.go", "package p\nvar X = 1\nvar Y = 2\n")

	cap := captureCommit(t, repo, repo.revParse(t, "HEAD"))
	if cap.Scope.Kind != ScopeCommit {
		t.Fatalf("scope kind=%q, want commit", cap.Scope.Kind)
	}
	if cap.RawChurn != 2 {
		t.Fatalf("raw churn=%d, want 2", cap.RawChurn)
	}
	rec := recordByNewPath(t, cap, "a.go")
	assertRec(t, rec, "M", TextClassText, 2, 0)
	if rec.OldSide.Kind != SideGitOID || rec.NewSide.Kind != SideGitOID {
		t.Fatalf("committed sides must both be git_oid: %+v", rec)
	}
}

// TestCaptureRangeScope proves a base..head range captures the merged diff of a
// linear range from Git objects with exact churn across multiple paths.
func TestCaptureRangeScope(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "base.go", "package p\n")
	base := repo.revParse(t, "HEAD")
	repo.commitFile(t, "base.go", "package p\nvar A = 1\n")
	repo.commitFile(t, "extra.go", "package q\nvar B = 2\n")
	head := repo.revParse(t, "HEAD")

	cap := captureRange(t, repo, base, head)
	if cap.Scope.Kind != ScopeRange {
		t.Fatalf("scope kind=%q, want range", cap.Scope.Kind)
	}
	if cap.RawChurn != 3 || cap.ChangedPaths != 2 {
		t.Fatalf("churn=%d paths=%d, want 3/2", cap.RawChurn, cap.ChangedPaths)
	}
	assertRec(t, recordByNewPath(t, cap, "base.go"), "M", TextClassText, 1, 0)
	assertRec(t, recordByNewPath(t, cap, "extra.go"), "A", TextClassText, 2, 0)
}

// TestCaptureCommittedGitlinkPreservesOIDs proves committed capture resolves full
// gitlink object ids into the side identities and that different submodule
// commits at the same path yield different capture fingerprints.
func TestCaptureCommittedGitlinkPreservesOIDs(t *testing.T) {
	oid1 := strings.Repeat("a", 40)
	oid2 := strings.Repeat("b", 40)
	oid3 := strings.Repeat("c", 40)

	capAB := gitlinkRangeCapture(t, oid1, oid2)
	rec := recordByNewPath(t, capAB, "sub")
	assertRec(t, rec, "M", TextClassBinary, 0, 0)
	if rec.OldMode != modeGitlink || rec.NewMode != modeGitlink {
		t.Fatalf("gitlink modes old=%o new=%o", rec.OldMode, rec.NewMode)
	}
	if hex.EncodeToString(rec.OldSide.Value) != oid1 || hex.EncodeToString(rec.NewSide.Value) != oid2 {
		t.Fatalf("gitlink sides old=%x new=%x, want %s/%s", rec.OldSide.Value, rec.NewSide.Value, oid1, oid2)
	}

	capAC := gitlinkRangeCapture(t, oid1, oid3)
	if capAB.CanonicalPatch == capAC.CanonicalPatch {
		t.Fatalf("different submodule commits must change the capture fingerprint")
	}
}

// gitlinkRangeCapture builds a fresh repo whose sub gitlink moves from -> to and
// captures that one-commit range.
func gitlinkRangeCapture(t *testing.T, from, to string) Capture {
	t.Helper()
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	repo.commitGitlink(t, "sub", from)
	g1 := repo.revParse(t, "HEAD")
	repo.commitGitlink(t, "sub", to)
	return captureRange(t, repo, g1, repo.revParse(t, "HEAD"))
}

// TestCaptureWorkspaceGitlinkUsesLiveSubmoduleHead proves a workspace gitlink is
// identified by the submodule worktree's live HEAD, not the stale index: an
// unstaged submodule advance changes the captured identity, and two distinct
// live commits produce distinct workspace fingerprints.
func TestCaptureWorkspaceGitlinkUsesLiveSubmoduleHead(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	indexOID := repo.initNestedGitlink(t, "sub", "one\n") // staged at c1
	repo.commit(t, "track sub")                           // super now tracks sub@c1

	live2 := repo.advanceNested(t, "sub", "two\n") // advance c1->c2, NOT staged in super
	capA := captureWorkspace(t, repo)
	recA := recordByNewPath(t, capA, "sub")
	assertRec(t, recA, "M", TextClassBinary, 0, 0)
	got := hex.EncodeToString(recA.NewSide.Value)
	if got != live2 {
		t.Fatalf("new gitlink side=%s, want live HEAD %s", got, live2)
	}
	if got == indexOID {
		t.Fatalf("new gitlink side is the stale index/base id %s, not the live HEAD", indexOID)
	}

	live3 := repo.advanceNested(t, "sub", "three\n") // advance c2->c3, still unstaged
	capB := captureWorkspace(t, repo)
	if hex.EncodeToString(recordByNewPath(t, capB, "sub").NewSide.Value) != live3 {
		t.Fatalf("second capture did not track the live HEAD %s", live3)
	}
	if sameFingerprint(capA, capB) {
		t.Fatalf("two distinct live submodule commits must change the workspace fingerprint")
	}
}

// TestCaptureRejectsMovedHead proves workspace capture is pinned to the resolved
// base OID: if HEAD advances after the scope is resolved, capture fails closed.
func TestCaptureRejectsMovedHead(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "a.go", "package p\n")
	ctx := context.Background()
	runner := execRunner(t)
	scope, err := ResolveScope(ctx, runner, repo.root, PlanRequest{})
	if err != nil {
		t.Fatalf("resolve scope: %v", err)
	}
	repo.commitFile(t, "b.go", "package q\n") // HEAD advances past the resolved base

	_, err = (&Capturer{Root: repo.root, Runner: runner}).CaptureOnce(ctx, scope)
	if !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("err=%v, want ErrHeadMoved", err)
	}
}

// TestCaptureRenameClassifiesByBasePath proves a rename to an unrecognized path
// stays text when its base path is a recognized source, even with NUL bytes.
func TestCaptureRenameClassifiesByBasePath(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "a.go", "package p\nvar Z\x00 = 1\n")
	rawGit(t, repo.root, "mv", "a.go", "b.unknown")

	cap := captureWorkspace(t, repo)
	rec := recordByNewPath(t, cap, "b.unknown")
	if !strings.HasPrefix(rec.Status, "R") || rec.OldPath != "a.go" {
		t.Fatalf("want rename from a.go, got status=%q old=%q", rec.Status, rec.OldPath)
	}
	if rec.TextClass != string(TextClassText) {
		t.Fatalf("class=%q, want text via the recognized base path", rec.TextClass)
	}
}

// TestCaptureTrackedFileExemptFromUntrackedCap proves the 64 MiB/512 MiB exact
// accounting caps apply only to untracked content: a tracked file above a tiny
// configured per-file cap is captured, while an untracked one fails.
func TestCaptureTrackedFileExemptFromUntrackedCap(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "big.txt", "small\n")
	repo.write(t, "big.txt", strings.Repeat("x", 200))

	cap := captureWorkspaceWith(t, repo, &Capturer{MaxFileBytes: 16, MaxTotalBytes: 16})
	rec := recordByNewPath(t, cap, "big.txt")
	if rec.Kind != "tracked" {
		t.Fatalf("kind=%q, want tracked", rec.Kind)
	}
	if rec.Additions != 1 || rec.Deletions != 1 {
		t.Fatalf("tracked churn add=%d del=%d, want 1/1", rec.Additions, rec.Deletions)
	}
}

// TestParseGitlinkLsTreeStrict proves the gitlink tree parser accepts exactly one
// well-formed 160000 commit record for the requested path and rejects every
// malformed shape.
func TestParseGitlinkLsTreeStrict(t *testing.T) {
	oid := strings.Repeat("a", 40)
	good := []byte("160000 commit " + oid + "\tsub\x00")
	got, err := parseGitlinkLsTree(good, "sub", 20)
	if err != nil || got != oid {
		t.Fatalf("good record: got %q err %v", got, err)
	}

	bad := []struct {
		name string
		out  string
	}{
		{"no records", ""},
		{"two records", "160000 commit " + oid + "\tsub\x00160000 commit " + strings.Repeat("b", 40) + "\tsubZ\x00"},
		{"wrong path", "160000 commit " + oid + "\tother\x00"},
		{"wrong mode", "100644 blob " + oid + "\tsub\x00"},
		{"wrong type", "160000 blob " + oid + "\tsub\x00"},
		{"short oid", "160000 commit " + strings.Repeat("a", 7) + "\tsub\x00"},
		{"non-hex oid", "160000 commit " + strings.Repeat("z", 40) + "\tsub\x00"},
		{"missing tab", "160000 commit " + oid + " sub\x00"},
		{"extra field", "160000 commit x " + oid + "\tsub\x00"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseGitlinkLsTree([]byte(tc.out), "sub", 20); err == nil {
				t.Fatalf("want error for %s", tc.name)
			}
		})
	}
}

// TestCaptureGitlinkLiteralPathspec proves a gitlink whose path contains glob
// magic is resolved by exact literal match, never a sibling entry, even when a
// glob-matching decoy exists at the same tree level.
func TestCaptureGitlinkLiteralPathspec(t *testing.T) {
	oidStar1 := strings.Repeat("a", 40)
	oidStar2 := strings.Repeat("b", 40)
	oidDecoy := strings.Repeat("c", 40)

	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	// A decoy sibling a glob "s*" would also match, and the real gitlink "s*".
	rawGit(t, repo.root, "update-index", "--add", "--cacheinfo", "160000,"+oidDecoy+",sZ")
	rawGit(t, repo.root, "update-index", "--add", "--cacheinfo", "160000,"+oidStar1+",s*")
	rawGit(t, repo.root, "commit", "-qm", "gitlinks")
	g1 := repo.revParse(t, "HEAD")
	rawGit(t, repo.root, "update-index", "--cacheinfo", "160000,"+oidStar2+",s*")
	rawGit(t, repo.root, "commit", "-qm", "move s*")
	g2 := repo.revParse(t, "HEAD")

	cap := captureRange(t, repo, g1, g2)
	rec := recordByNewPath(t, cap, "s*")
	assertRec(t, rec, "M", TextClassBinary, 0, 0)
	if hex.EncodeToString(rec.OldSide.Value) != oidStar1 || hex.EncodeToString(rec.NewSide.Value) != oidStar2 {
		t.Fatalf("literal match failed: old=%x new=%x, want %s/%s", rec.OldSide.Value, rec.NewSide.Value, oidStar1, oidStar2)
	}
}

// scriptedRunner is a CaptureRunner that intercepts specific git invocations by
// (root, subcommand) and returns scripted output, delegating everything else to
// a real ExecGit. It drives the fail-closed gitlink parsing paths.
type scriptedRunner struct {
	ExecGit
	intercept func(root string, args []string) ([]byte, bool, error)
}

func (r scriptedRunner) Output(ctx context.Context, root string, limit int64, args ...string) ([]byte, error) {
	if r.intercept != nil {
		if out, handled, err := r.intercept(root, args); handled {
			return out, err
		}
	}
	return r.ExecGit.Output(ctx, root, limit, args...)
}

// TestCaptureGitlinkRejectsMalformedTreeOutput proves committed gitlink capture
// fails closed when ls-tree returns malformed output.
func TestCaptureGitlinkRejectsMalformedTreeOutput(t *testing.T) {
	oid1 := strings.Repeat("a", 40)
	oid2 := strings.Repeat("b", 40)
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	repo.commitGitlink(t, "sub", oid1)
	g1 := repo.revParse(t, "HEAD")
	repo.commitGitlink(t, "sub", oid2)
	g2 := repo.revParse(t, "HEAD")

	ctx := context.Background()
	runner := scriptedRunner{
		ExecGit: execRunner(t),
		intercept: func(_ string, args []string) ([]byte, bool, error) {
			if len(args) > 0 && args[0] == "ls-tree" {
				return []byte("garbage-without-tab\x00"), true, nil
			}
			return nil, false, nil
		},
	}
	scope, err := ResolveScope(ctx, runner, repo.root, PlanRequest{Base: g1, Head: g2})
	if err != nil {
		t.Fatalf("resolve scope: %v", err)
	}
	_, err = (&Capturer{Root: repo.root, Runner: runner}).CaptureOnce(ctx, scope)
	if !errors.Is(err, ErrGitlinkUnresolved) {
		t.Fatalf("err=%v, want ErrGitlinkUnresolved", err)
	}
}

// TestCaptureWorkspaceGitlinkRejectsBadLiveHead proves a workspace gitlink fails
// closed when the live submodule rev-parse yields a non-OID.
func TestCaptureWorkspaceGitlinkRejectsBadLiveHead(t *testing.T) {
	repo := newGitFixture(t)
	repo.commitFile(t, "seed.go", "package p\n")
	repo.initNestedGitlink(t, "sub", "one\n")
	repo.commit(t, "track sub")
	repo.advanceNested(t, "sub", "two\n") // unstaged advance so the new side is a live gitlink

	sub := filepath.Join(repo.root, "sub")
	ctx := context.Background()
	runner := scriptedRunner{
		ExecGit: execRunner(t),
		intercept: func(root string, args []string) ([]byte, bool, error) {
			if root == sub && len(args) > 0 && args[0] == "rev-parse" {
				return []byte("not-a-valid-object-id\n"), true, nil
			}
			return nil, false, nil
		},
	}
	scope, err := ResolveScope(ctx, runner, repo.root, PlanRequest{})
	if err != nil {
		t.Fatalf("resolve scope: %v", err)
	}
	_, err = (&Capturer{Root: repo.root, Runner: runner}).CaptureOnce(ctx, scope)
	if !errors.Is(err, ErrGitlinkUnresolved) {
		t.Fatalf("err=%v, want ErrGitlinkUnresolved", err)
	}
}
