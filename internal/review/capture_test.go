//go:build unix

package review

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
		name   string
		setup  func(t *testing.T, repo *gitFixture)
		verify func(t *testing.T, cap Capture)
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
