package okfv01

import (
	"fmt"
	"strings"

	"github.com/neur0map/prowl/internal/knowledge"
)

// CaptureInput is a structured knowledge candidate. It lets a caller record a
// decision or claim without hand-authoring OKF frontmatter, which avoids the
// common mistake of nesting prowl fields at the top level. Anchors carry only a
// path plus a symbol or line range; propose fills content_hash from the current
// source.
type CaptureInput struct {
	Type        string
	Title       string
	Description string
	Resource    string
	Body        string
	Tags        []string
	Anchors     []knowledge.Anchor
}

// BuildCandidate assembles a valid OKF Markdown candidate from structured
// fields. The result is fed to ReviewInbox.Propose like any other candidate, so
// review, diffing, and anchor hashing are unchanged. It cannot misplace prowl
// fields, because the codec writes them.
func BuildCandidate(in CaptureInput) ([]byte, error) {
	if strings.TrimSpace(in.Type) == "" {
		return nil, fmt.Errorf("knowledge capture requires a type")
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, fmt.Errorf("knowledge capture requires a title")
	}
	body := in.Body
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	doc := &knowledge.Document{
		Path:        "candidate.md",
		Type:        strings.TrimSpace(in.Type),
		Title:       strings.TrimSpace(in.Title),
		Description: in.Description,
		Resource:    in.Resource,
		Tags:        in.Tags,
		Body:        []byte(body),
		Prowl:       knowledge.Metadata{Anchors: in.Anchors},
	}
	return Codec{}.Marshal(doc)
}
