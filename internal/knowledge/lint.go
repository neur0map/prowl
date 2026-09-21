package knowledge

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

// Finding is a stable, namespaced knowledge health result.
type Finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

// Lint validates a bundle permissively. Malformed documents and stale evidence
// become findings; one bad concept does not make the rest unreadable.
func (r *Repository) Lint(sourceRoot string, resolve SymbolResolver) ([]Finding, error) {
	type parsed struct {
		doc   *Document
		links []string
	}
	var documents []parsed
	var findings []Finding
	err := filepath.WalkDir(r.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		base := strings.ToLower(entry.Name())
		if base == "index.md" || base == "log.md" {
			return nil
		}
		rel, err := filepath.Rel(r.Root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := readRootFile(r.Root, rel, MaxDocumentBytes)
		if err != nil {
			return err
		}
		doc, err := r.Codec.Parse(rel, data)
		if err != nil {
			findings = append(findings, Finding{Code: "knowledge.invalid_document", Severity: "error", Path: rel, Message: err.Error()})
			return nil
		}
		links := localLinks(doc.Body)
		documents = append(documents, parsed{doc: doc, links: links})
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	byID := map[string]*Document{}
	inbound := map[string]int{}
	for _, item := range documents {
		doc := item.doc
		if doc.Prowl.ID != "" {
			if previous := byID[doc.Prowl.ID]; previous != nil {
				findings = append(findings, Finding{Code: "knowledge.duplicate_id", Severity: "error", Path: doc.Path, Message: fmt.Sprintf("stable id %q is also used by %s", doc.Prowl.ID, previous.Path)})
				if previous.Prowl.Status != doc.Prowl.Status || !stringBytesEqual(previous.Body, doc.Body) {
					findings = append(findings, Finding{Code: "knowledge.contradiction", Severity: "warning", Path: doc.Path, Message: fmt.Sprintf("documents sharing id %q disagree", doc.Prowl.ID)})
				}
			} else {
				byID[doc.Prowl.ID] = doc
			}
		}
		if !validTemporal(doc.Prowl.ValidFrom, doc.Prowl.ValidTo) {
			findings = append(findings, Finding{Code: "knowledge.invalid_temporal_range", Severity: "error", Path: doc.Path, Message: "valid_from must not be after valid_to and both values must be ISO dates"})
		}
		if doc.Resource != "" {
			parsedURI, err := url.Parse(doc.Resource)
			if err != nil || parsedURI.Scheme == "" {
				findings = append(findings, Finding{Code: "knowledge.invalid_resource", Severity: "warning", Path: doc.Path, Message: fmt.Sprintf("resource is not an absolute URI: %q", doc.Resource)})
			}
		}
		for _, link := range item.links {
			var target string
			if strings.HasPrefix(link, "/") {
				target = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(link))), "/")
			} else {
				target = filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(doc.Path), filepath.FromSlash(link))))
			}
			if target == ".." || strings.HasPrefix(target, "../") {
				findings = append(findings, Finding{Code: "knowledge.broken_link", Severity: "warning", Path: doc.Path, Message: fmt.Sprintf("linked document escapes the bundle: %s", link)})
				continue
			}
			if _, err := r.ReadBundleFile(target); err != nil {
				findings = append(findings, Finding{Code: "knowledge.broken_link", Severity: "warning", Path: doc.Path, Message: fmt.Sprintf("linked document does not exist: %s", link)})
			} else {
				inbound[target]++
			}
		}
		for _, anchor := range doc.Prowl.Anchors {
			check := CheckAnchorResolved(sourceRoot, anchor, resolve)
			switch check.Status {
			case AnchorStale:
				where := fmt.Sprintf("%s:%d-%d", anchor.Path, anchor.LineStart, anchor.LineEnd)
				if anchor.Symbol != "" {
					where = fmt.Sprintf("%s (symbol %s)", anchor.Path, anchor.Symbol)
				}
				findings = append(findings, Finding{Code: "knowledge.stale_anchor", Severity: "warning", Path: doc.Path, Message: where + " changed"})
			case AnchorMoved:
				findings = append(findings, Finding{
					Code: "knowledge.moved_anchor", Severity: "info", Path: doc.Path,
					Message: fmt.Sprintf("%s:%d-%d is intact at lines %d-%d; run `prowl knowledge lint --repair` to update the anchor",
						anchor.Path, anchor.LineStart, anchor.LineEnd, check.MovedStart, check.MovedEnd),
				})
			case AnchorMissing:
				findings = append(findings, Finding{Code: "knowledge.missing_anchor", Severity: "warning", Path: doc.Path, Message: fmt.Sprintf("source is missing: %s", anchor.Path)})
			case AnchorInvalid:
				findings = append(findings, Finding{Code: "knowledge.invalid_anchor", Severity: "error", Path: doc.Path, Message: check.Message})
			}
		}
		if strings.EqualFold(doc.Prowl.Status, "accepted") && doc.Resource == "" && len(doc.Prowl.Anchors) == 0 {
			findings = append(findings, Finding{Code: "knowledge.missing_evidence", Severity: "warning", Path: doc.Path, Message: "accepted knowledge has no resource or source anchor"})
		}
	}
	if len(documents) > 1 {
		for _, item := range documents {
			if len(item.links) == 0 && len(item.doc.Prowl.Related) == 0 && inbound[item.doc.Path] == 0 {
				findings = append(findings, Finding{Code: "knowledge.orphan", Severity: "info", Path: item.doc.Path, Message: "concept has no local links or related concepts"})
			}
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path == findings[j].Path {
			return findings[i].Code < findings[j].Code
		}
		return findings[i].Path < findings[j].Path
	})
	return findings, nil
}

func localLinks(body []byte) []string {
	var links []string
	for _, match := range markdownLink.FindAllSubmatch(body, -1) {
		raw := strings.TrimSpace(string(match[1]))
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "" || u.Host != "" {
			continue
		}
		path, err := url.PathUnescape(u.Path)
		if err == nil && strings.EqualFold(filepath.Ext(path), ".md") {
			links = append(links, filepath.ToSlash(path))
		}
	}
	return links
}

func validTemporal(from, to string) bool {
	if from == "" || to == "" {
		return validDate(from) && validDate(to)
	}
	fromTime, fromOK := parseDate(from)
	toTime, toOK := parseDate(to)
	return fromOK && toOK && !fromTime.After(toTime)
}

func validDate(value string) bool {
	if value == "" {
		return true
	}
	_, ok := parseDate(value)
	return ok
}

func parseDate(value string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func stringBytesEqual(a, b []byte) bool { return string(a) == string(b) }
