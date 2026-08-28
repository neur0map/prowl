package review

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"strings"

	"github.com/prowl-agent/prowl-agent/internal/parse"
)

// thresholdTextPrefixV1 is the NUL-scan prefix ThresholdTextV1 inspects on each
// side. A NUL anywhere in the first min(8000, size) bytes of an unrecognized
// path marks that side as non-text; the rest of the blob is never scanned.
const thresholdTextPrefixV1 = 8000

// ErrMalformedPatch reports an unparseable unified patch. ParseUnifiedPatch is a
// strict whole-hunk parser: any header, framing, count, or prefix violation
// yields this error and no hunks, never a partial or split parse.
var ErrMalformedPatch = errors.New("review: malformed unified patch")

// TextClass is a changed path's threshold classification under ThresholdTextV1.
// It is independent of repository-controlled diff attributes.
type TextClass string

const (
	// TextClassText marks a path whose churn is counted from a forced-text diff.
	TextClassText TextClass = "text"
	// TextClassBinary marks a path with no line churn.
	TextClassBinary TextClass = "binary"
)

// BlobSide is one raw revision side considered by ThresholdTextV1. Bytes holds a
// regular file's exact content or a symbolic link's exact target bytes,
// including a committed link blob containing NUL. An absent side is not present.
type BlobSide struct {
	Present bool
	Bytes   []byte
}

// ThresholdText classifies a changed path under the ThresholdTextV1 rule,
// independent of any repository diff attribute. A side is recognized text when
// Prowl recognizes its path as an indexed text language/config format or its
// first min(8000, size) bytes contain no NUL. A path is text when either side
// is recognized text; otherwise it is binary, because every present side of a
// non-text path necessarily carries a NUL in that prefix. An empty side has an
// empty prefix and is therefore always recognized text.
func ThresholdText(path string, old, new BlobSide) TextClass {
	if recognizedTextSide(path, old) || recognizedTextSide(path, new) {
		return TextClassText
	}
	return TextClassBinary
}

// recognizedTextSide reports whether a present side is recognized text: its path
// is a detected indexed format, or its inspected prefix contains no NUL. Path
// recognition uses only the path string (no shebang sniffing), so a recognized
// source file classifies as text even when its bytes contain NUL.
func recognizedTextSide(path string, side BlobSide) bool {
	if !side.Present {
		return false
	}
	if parse.Detect(path, nil) != "" {
		return true
	}
	prefix := side.Bytes
	if len(prefix) > thresholdTextPrefixV1 {
		prefix = prefix[:thresholdTextPrefixV1]
	}
	return bytes.IndexByte(prefix, 0) < 0
}

// ParseUnifiedPatch parses a unified diff into canonical raw hunk records. Every
// hunk is captured whole: its declared old/new ranges must be exactly accounted
// by its body, every body line must carry a ' ', '+', '-', or '\' prefix, and
// the exact context/add/delete payload bytes are bound verbatim. A hunk is never
// split and a malformed patch never yields a partial result.
//
// Payload is the exact concatenation of the hunk's context, addition, and
// deletion lines, each terminated by the single '\n' git emits; the localized
// "\ No newline at end of file" marker is excluded from the payload and instead
// recorded in NoFinalNewlineOld/NoFinalNewlineNew, so payloads never carry
// locale-dependent bytes. Ordinal is the zero-based hunk index within the patch.
func ParseUnifiedPatch(raw []byte) ([]RawHunk, error) {
	lines := strings.Split(string(raw), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var (
		hunks        []RawHunk
		cur          *RawHunk
		payload      []byte
		oldRemaining int
		newRemaining int
		lastPrefix   byte
	)
	flush := func() error {
		if cur == nil {
			return nil
		}
		if oldRemaining != 0 || newRemaining != 0 {
			return fmt.Errorf("%w: hunk ended with %d old / %d new lines unaccounted", ErrMalformedPatch, oldRemaining, newRemaining)
		}
		cur.Payload = payload
		hunks = append(hunks, *cur)
		return nil
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			if err := flush(); err != nil {
				return nil, err
			}
			m := hunkHeaderRE.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("%w: bad hunk header %q", ErrMalformedPatch, line)
			}
			oldStart, e1 := parseHunkNum(m[1], 0)
			oldLines, e2 := parseHunkNum(m[2], 1)
			newStart, e3 := parseHunkNum(m[3], 0)
			newLines, e4 := parseHunkNum(m[4], 1)
			if err := cmp.Or(e1, e2, e3, e4); err != nil {
				return nil, fmt.Errorf("%w: hunk header %q: %v", ErrMalformedPatch, line, err)
			}
			if (oldLines > 0 && oldStart < 1) || (newLines > 0 && newStart < 1) {
				return nil, fmt.Errorf("%w: hunk header %q has a nonzero count starting at line 0", ErrMalformedPatch, line)
			}
			cur = &RawHunk{
				Ordinal:  uint64(len(hunks)),
				OldStart: uint64(oldStart),
				OldLines: uint64(oldLines),
				NewStart: uint64(newStart),
				NewLines: uint64(newLines),
			}
			payload = nil
			oldRemaining, newRemaining = oldLines, newLines
			lastPrefix = 0
			continue
		}
		if cur == nil {
			if isDiffPreambleLine(line) {
				continue
			}
			return nil, fmt.Errorf("%w: unexpected line %q outside any hunk", ErrMalformedPatch, line)
		}
		// A no-newline marker can trail the final line of either side even after
		// both counters are exhausted, so it is handled before the range checks.
		if len(line) > 0 && line[0] == '\\' {
			switch lastPrefix {
			case '-':
				cur.NoFinalNewlineOld = true
			case '+':
				cur.NoFinalNewlineNew = true
			case ' ':
				cur.NoFinalNewlineOld = true
				cur.NoFinalNewlineNew = true
			default:
				return nil, fmt.Errorf("%w: no-newline marker without a preceding payload line", ErrMalformedPatch)
			}
			lastPrefix = 0
			continue
		}
		if oldRemaining == 0 && newRemaining == 0 {
			if isDiffPreambleLine(line) {
				if err := flush(); err != nil {
					return nil, err
				}
				cur = nil
				continue
			}
			return nil, fmt.Errorf("%w: payload line %q past declared hunk range", ErrMalformedPatch, line)
		}
		if line == "" {
			return nil, fmt.Errorf("%w: empty payload line inside hunk", ErrMalformedPatch)
		}
		switch line[0] {
		case '+':
			if newRemaining == 0 {
				return nil, fmt.Errorf("%w: extra added line %q", ErrMalformedPatch, line)
			}
			newRemaining--
		case '-':
			if oldRemaining == 0 {
				return nil, fmt.Errorf("%w: extra deleted line %q", ErrMalformedPatch, line)
			}
			oldRemaining--
		case ' ':
			if oldRemaining == 0 || newRemaining == 0 {
				return nil, fmt.Errorf("%w: extra context line %q", ErrMalformedPatch, line)
			}
			oldRemaining--
			newRemaining--
		default:
			return nil, fmt.Errorf("%w: unprefixed hunk line %q", ErrMalformedPatch, line)
		}
		payload = append(payload, line...)
		payload = append(payload, '\n')
		lastPrefix = line[0]
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return hunks, nil
}

// hunkPayloadChurn counts the '+' and '-' payload lines in a canonical hunk
// payload. Context lines and the excluded no-newline marker never contribute.
func hunkPayloadChurn(payload []byte) (additions, deletions int) {
	rest := payload
	for len(rest) > 0 {
		var line []byte
		if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
			line, rest = rest[:nl], rest[nl+1:]
		} else {
			line, rest = rest, nil
		}
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case '+':
			additions++
		case '-':
			deletions++
		}
	}
	return additions, deletions
}
