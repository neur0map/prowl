package review

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func textSide(b string) BlobSide { return BlobSide{Present: true, Bytes: []byte(b)} }
func absentSide() BlobSide       { return BlobSide{} }

func TestThresholdTextClassifiesSides(t *testing.T) {
	longNoNUL := strings.Repeat("a", thresholdTextPrefixV1+50)
	nulPastPrefix := strings.Repeat("a", thresholdTextPrefixV1) + "\x00tail"

	cases := []struct {
		name string
		path string
		old  BlobSide
		new  BlobSide
		want TextClass
	}{
		{"recognized source with NUL is text", "pkg/a.go", absentSide(), textSide("package p\n\x00\n"), TextClassText},
		{"unrecognized no-NUL text", "notes.txt", absentSide(), textSide("one\ntwo\n"), TextClassText},
		{"unrecognized with NUL is binary", "blob.bin", absentSide(), textSide("\x00\x01\x02"), TextClassBinary},
		{"empty added file is text", "empty.dat", absentSide(), textSide(""), TextClassText},
		{"symlink target with NUL is binary", "link", absentSide(), textSide("bad\x00target"), TextClassBinary},
		{"either side recognized wins", "x.unknown", textSide("\x00\x00"), textSide("plain text\n"), TextClassText},
		{"NUL only past the scan prefix stays text", "big.log", absentSide(), textSide(nulPastPrefix), TextClassText},
		{"prefix NUL within a long side is binary", "big.log", absentSide(), textSide("\x00" + longNoNUL), TextClassBinary},
		{"deletion classifies by old side", "old.bin", textSide("\x00data"), absentSide(), TextClassBinary},
		{"deletion of text classifies text", "old.txt", textSide("a\nb\n"), absentSide(), TextClassText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ThresholdText(tc.path, tc.old, tc.new); got != tc.want {
				t.Fatalf("ThresholdText(%q)=%q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestParseUnifiedPatchModifyCountsAndPayload(t *testing.T) {
	raw := []byte("diff --git a/old b/new\n" +
		"index de98044..7be73ce 100644\n" +
		"--- a/old\n+++ b/new\n" +
		"@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n")
	hunks, err := ParseUnifiedPatch(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hunks) != 1 {
		t.Fatalf("got %d hunks, want 1", len(hunks))
	}
	h := hunks[0]
	if h.Ordinal != 0 || h.OldStart != 1 || h.OldLines != 3 || h.NewStart != 1 || h.NewLines != 3 {
		t.Fatalf("coords=%+v", h)
	}
	if want := " a\n-b\n+B\n c\n"; string(h.Payload) != want {
		t.Fatalf("payload=%q, want %q", h.Payload, want)
	}
	if h.NoFinalNewlineOld || h.NoFinalNewlineNew {
		t.Fatalf("unexpected no-newline flags: %+v", h)
	}
	add, del := hunkPayloadChurn(h.Payload)
	if add != 1 || del != 1 {
		t.Fatalf("churn add=%d del=%d, want 1/1", add, del)
	}
}

func TestParseUnifiedPatchNoFinalNewlineExcludesMarker(t *testing.T) {
	raw := []byte("@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+bX\n\\ No newline at end of file\n")
	hunks, err := ParseUnifiedPatch(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	h := hunks[0]
	if want := " a\n-b\n+bX\n"; string(h.Payload) != want {
		t.Fatalf("payload=%q, want %q (marker must be excluded)", h.Payload, want)
	}
	if !h.NoFinalNewlineOld || !h.NoFinalNewlineNew {
		t.Fatalf("want both no-newline flags set, got %+v", h)
	}
	add, del := hunkPayloadChurn(h.Payload)
	if add != 1 || del != 1 {
		t.Fatalf("churn add=%d del=%d, want 1/1", add, del)
	}
}

func TestParseUnifiedPatchCRLFIsOneLine(t *testing.T) {
	raw := []byte("@@ -1,2 +1,2 @@\n a\r\n-b\r\n+B\r\n")
	hunks, err := ParseUnifiedPatch(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := " a\r\n-b\r\n+B\r\n"; string(hunks[0].Payload) != want {
		t.Fatalf("payload=%q, want the CR bytes preserved as %q", hunks[0].Payload, want)
	}
	add, del := hunkPayloadChurn(hunks[0].Payload)
	if add != 1 || del != 1 {
		t.Fatalf("CRLF line counted as add=%d del=%d, want 1/1", add, del)
	}
}

func TestParseUnifiedPatchAddAgainstEmpty(t *testing.T) {
	raw := []byte("@@ -0,0 +1,2 @@\n+x\n+y\n")
	hunks, err := ParseUnifiedPatch(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	h := hunks[0]
	if h.OldStart != 0 || h.OldLines != 0 || h.NewStart != 1 || h.NewLines != 2 {
		t.Fatalf("coords=%+v", h)
	}
	if !bytes.Equal(h.Payload, []byte("+x\n+y\n")) {
		t.Fatalf("payload=%q", h.Payload)
	}
	add, del := hunkPayloadChurn(h.Payload)
	if add != 2 || del != 0 {
		t.Fatalf("churn add=%d del=%d, want 2/0", add, del)
	}
}

func TestParseUnifiedPatchWholeHunksNotSplit(t *testing.T) {
	raw := []byte("@@ -1,1 +1,2 @@\n a\n+b\n@@ -10,2 +11,1 @@\n c\n-d\n")
	hunks, err := ParseUnifiedPatch(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hunks) != 2 {
		t.Fatalf("got %d hunks, want 2 whole hunks", len(hunks))
	}
	if hunks[0].Ordinal != 0 || hunks[1].Ordinal != 1 {
		t.Fatalf("ordinals=%d,%d, want 0,1", hunks[0].Ordinal, hunks[1].Ordinal)
	}
	if string(hunks[0].Payload) != " a\n+b\n" || string(hunks[1].Payload) != " c\n-d\n" {
		t.Fatalf("payloads=%q,%q", hunks[0].Payload, hunks[1].Payload)
	}
}

func TestParseUnifiedPatchRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"bad header", "@@ not a header @@\n a\n"},
		{"count underflow", "@@ -1,3 +1,3 @@\n a\n-b\n"},
		{"count overflow past range", "@@ -1,1 +1,1 @@\n a\n b\n"},
		{"unprefixed body line", "@@ -1,1 +1,1 @@\nxyz\n"},
		{"marker without preceding line", "@@ -1,1 +1,1 @@\n\\ No newline at end of file\n a\n"},
		{"content outside any hunk", "surprise\n@@ -1,1 +1,1 @@\n a\n"},
		{"extra added line", "@@ -1,1 +1,1 @@\n a\n+extra\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hunks, err := ParseUnifiedPatch([]byte(tc.raw))
			if !errors.Is(err, ErrMalformedPatch) {
				t.Fatalf("err=%v, want ErrMalformedPatch", err)
			}
			if hunks != nil {
				t.Fatalf("want no hunks on malformed input, got %d", len(hunks))
			}
		})
	}
}
