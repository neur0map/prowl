// Package okfv01 implements the Open Knowledge Format v0.1 draft codec.
package okfv01

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/neur0map/prowl/internal/knowledge"
	"gopkg.in/yaml.v3"
)

// Codec parses and writes OKF v0.1 Markdown while retaining unknown YAML nodes.
type Codec struct{}

// Parse decodes one bundle-relative Markdown document.
func (Codec) Parse(path string, data []byte) (*knowledge.Document, error) {
	clean, err := safePath(path)
	if err != nil {
		return nil, err
	}
	front, body, hasFront, err := splitFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", clean, err)
	}
	var root yaml.Node
	if hasFront {
		if err := yaml.Unmarshal(front, &root); err != nil {
			return nil, fmt.Errorf("%s: invalid YAML frontmatter: %w", clean, err)
		}
	} else {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	mapping, err := mappingNode(&root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", clean, err)
	}
	if err := checkMisplacedProwlFields(mapping); err != nil {
		return nil, fmt.Errorf("%s: %w", clean, err)
	}
	doc := &knowledge.Document{
		Path: clean, Body: append([]byte(nil), body...), Frontmatter: cloneNode(&root),
		Type: scalar(mapping, "type"), Title: scalar(mapping, "title"),
		Description: scalar(mapping, "description"), Resource: scalar(mapping, "resource"),
		Timestamp: scalar(mapping, "timestamp"), Tags: stringSequence(mapping, "tags"),
	}
	if p := valueNode(mapping, "prowl"); p != nil && p.Kind == yaml.MappingNode {
		doc.Prowl = parseProwl(p)
	}
	base := strings.ToLower(filepath.Base(clean))
	if doc.Type == "" && base != "index.md" && base != "log.md" {
		return nil, fmt.Errorf("%s: required frontmatter field %q is missing", clean, "type")
	}
	return doc, nil
}

// Marshal encodes a document, updating recognized fields while preserving all
// unrecognized top-level and prowl-namespaced YAML fields.
func (Codec) Marshal(doc *knowledge.Document) ([]byte, error) {
	if doc == nil {
		return nil, errors.New("nil knowledge document")
	}
	if _, err := safePath(doc.Path); err != nil {
		return nil, err
	}
	var root *yaml.Node
	if doc.Frontmatter == nil {
		root = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	} else {
		root = cloneNode(doc.Frontmatter)
	}
	mapping, err := mappingNode(root)
	if err != nil {
		return nil, err
	}
	setScalar(mapping, "type", doc.Type)
	setScalar(mapping, "title", doc.Title)
	setScalar(mapping, "description", doc.Description)
	setScalar(mapping, "resource", doc.Resource)
	setScalar(mapping, "timestamp", doc.Timestamp)
	setStringSequence(mapping, "tags", doc.Tags)
	if hasProwl(doc.Prowl) || valueNode(mapping, "prowl") != nil {
		p := ensureMapping(mapping, "prowl")
		setScalar(p, "id", doc.Prowl.ID)
		setScalar(p, "status", doc.Prowl.Status)
		setScalar(p, "confidence", doc.Prowl.Confidence)
		setScalar(p, "valid_from", doc.Prowl.ValidFrom)
		setScalar(p, "valid_to", doc.Prowl.ValidTo)
		setScalar(p, "author", doc.Prowl.Author)
		setScalar(p, "source", doc.Prowl.Source)
		setStringSequence(p, "related", doc.Prowl.Related)
		existingAnchors := parseAnchors(valueNode(p, "anchors"))
		if !reflect.DeepEqual(existingAnchors, doc.Prowl.Anchors) {
			setAnchors(p, doc.Prowl.Anchors)
		}
	}
	base := strings.ToLower(filepath.Base(doc.Path))
	if scalar(mapping, "type") == "" && base != "index.md" && base != "log.md" {
		return nil, fmt.Errorf("%s: required frontmatter field %q is missing", doc.Path, "type")
	}
	encoded, err := yaml.Marshal(mapping)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(encoded)
	out.WriteString("---\n")
	out.Write(doc.Body)
	return out.Bytes(), nil
}

// prowlNamespacedFields are OKF fields that must live under `prowl:`. Finding one
// at the top level is almost always a nesting mistake that the codec would
// otherwise ignore silently, dropping the author's intent, so it is rejected.
var prowlNamespacedFields = map[string]bool{
	"anchors": true, "status": true, "confidence": true,
	"valid_from": true, "valid_to": true, "related": true,
}

func checkMisplacedProwlFields(mapping *yaml.Node) error {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if key := mapping.Content[i].Value; prowlNamespacedFields[key] {
			return fmt.Errorf("field %q must be nested under `prowl:`, not at the top level", key)
		}
	}
	return nil
}

func safePath(path string) (string, error) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" || filepath.IsAbs(path) {
		return "", fmt.Errorf("unsafe knowledge path %q", path)
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", fmt.Errorf("unsafe knowledge path %q", path)
	}
	return clean, nil
}

func splitFrontmatter(data []byte) (front, body []byte, found bool, err error) {
	if !bytes.HasPrefix(data, []byte("---\n")) && !bytes.HasPrefix(data, []byte("---\r\n")) {
		return nil, data, false, nil
	}
	firstEnd := bytes.IndexByte(data, '\n')
	start := firstEnd + 1
	for pos := start; pos <= len(data); {
		next := bytes.IndexByte(data[pos:], '\n')
		lineEnd := len(data)
		if next >= 0 {
			lineEnd = pos + next
		}
		line := bytes.TrimSuffix(data[pos:lineEnd], []byte("\r"))
		if bytes.Equal(line, []byte("---")) {
			bodyStart := lineEnd
			if next >= 0 {
				bodyStart++
			}
			return data[start:pos], data[bodyStart:], true, nil
		}
		if next < 0 {
			break
		}
		pos = lineEnd + 1
	}
	return nil, nil, true, errors.New("frontmatter closing delimiter is missing")
}

func mappingNode(root *yaml.Node) (*yaml.Node, error) {
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("frontmatter must be a YAML mapping")
	}
	return root.Content[0], nil
}

func valueNode(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func scalar(mapping *yaml.Node, key string) string {
	n := valueNode(mapping, key)
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func stringSequence(mapping *yaml.Node, key string) []string {
	n := valueNode(mapping, key)
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	if n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for _, item := range n.Content {
		if item.Kind == yaml.ScalarNode {
			out = append(out, item.Value)
		}
	}
	return out
}

func setScalar(mapping *yaml.Node, key, value string) {
	idx := keyIndex(mapping, key)
	if value == "" {
		if idx >= 0 {
			mapping.Content = append(mapping.Content[:idx], mapping.Content[idx+2:]...)
		}
		return
	}
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	if idx >= 0 {
		mapping.Content[idx+1] = n
		return
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, n)
}

func setStringSequence(mapping *yaml.Node, key string, values []string) {
	idx := keyIndex(mapping, key)
	if len(values) == 0 {
		if idx >= 0 {
			mapping.Content = append(mapping.Content[:idx], mapping.Content[idx+2:]...)
		}
		return
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, value := range values {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	if idx >= 0 {
		mapping.Content[idx+1] = seq
		return
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, seq)
}

// setIntScalar sets key to an unquoted integer scalar (or removes it when value
// is not positive), so line numbers round-trip as YAML ints, not quoted strings.
func setIntScalar(mapping *yaml.Node, key string, value int) {
	idx := keyIndex(mapping, key)
	if value <= 0 {
		if idx >= 0 {
			mapping.Content = append(mapping.Content[:idx], mapping.Content[idx+2:]...)
		}
		return
	}
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(value)}
	if idx >= 0 {
		mapping.Content[idx+1] = n
	} else {
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, n)
	}
}

func keyIndex(mapping *yaml.Node, key string) int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func ensureMapping(parent *yaml.Node, key string) *yaml.Node {
	if existing := valueNode(parent, key); existing != nil && existing.Kind == yaml.MappingNode {
		return existing
	}
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	idx := keyIndex(parent, key)
	if idx >= 0 {
		parent.Content[idx+1] = n
	} else {
		parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, n)
	}
	return n
}

func parseProwl(p *yaml.Node) knowledge.Metadata {
	return knowledge.Metadata{
		ID: scalar(p, "id"), Status: scalar(p, "status"), Confidence: scalar(p, "confidence"),
		ValidFrom: scalar(p, "valid_from"), ValidTo: scalar(p, "valid_to"),
		Author: scalar(p, "author"), Source: scalar(p, "source"),
		Related: stringSequence(p, "related"), Anchors: parseAnchors(valueNode(p, "anchors")),
	}
}

func parseAnchors(n *yaml.Node) []knowledge.Anchor {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]knowledge.Anchor, 0, len(n.Content))
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		out = append(out, knowledge.Anchor{
			Path: scalar(item, "path"), LineStart: intScalar(item, "line_start"),
			LineEnd: intScalar(item, "line_end"), ContentHash: scalar(item, "content_hash"),
			Symbol: scalar(item, "symbol"),
		})
	}
	return out
}

func intScalar(mapping *yaml.Node, key string) int {
	value := scalar(mapping, key)
	n, _ := strconv.Atoi(value)
	return n
}

func setAnchors(p *yaml.Node, anchors []knowledge.Anchor) {
	idx := keyIndex(p, "anchors")
	if len(anchors) == 0 {
		if idx >= 0 {
			p.Content = append(p.Content[:idx], p.Content[idx+2:]...)
		}
		return
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, anchor := range anchors {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setScalar(m, "path", anchor.Path)
		setIntScalar(m, "line_start", anchor.LineStart)
		setIntScalar(m, "line_end", anchor.LineEnd)
		setScalar(m, "content_hash", anchor.ContentHash)
		setScalar(m, "symbol", anchor.Symbol)
		seq.Content = append(seq.Content, m)
	}
	if idx >= 0 {
		p.Content[idx+1] = seq
	} else {
		p.Content = append(p.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "anchors"}, seq)
	}
}

func hasProwl(p knowledge.Metadata) bool {
	return p.ID != "" || p.Status != "" || p.Confidence != "" || p.ValidFrom != "" || p.ValidTo != "" || p.Author != "" || p.Source != "" || len(p.Related) > 0 || len(p.Anchors) > 0
}

func cloneNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	clone.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		clone.Content[i] = cloneNode(child)
	}
	return &clone
}
