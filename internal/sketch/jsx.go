package sketch

import (
	"fmt"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/neur0map/prowl/internal/parse"
)

// maxJSXRoots bounds how many top-level render trees a React file contributes,
// so a file with many conditional returns stays token-lean.
const maxJSXRoots = 8

// extractJSX parses a React file (JSX/TSX) into a visual sketch: the returned
// element tree with each element's className/style/props and its handlers. A
// component with several conditional returns yields several trees; the largest
// (the main render) is primary, the rest are variants.
func extractJSX(path string, src []byte) (*Sketch, error) {
	lang := "javascript"
	if strings.HasSuffix(strings.ToLower(path), ".tsx") {
		lang = "tsx"
	}
	tree, err := parse.Parse(lang, src)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	var roots []sitter.Node
	collectTopJSX(tree.RootNode(), &roots)
	if len(roots) == 0 {
		return nil, fmt.Errorf("no JSX/React UI found in %s", path)
	}

	nodes := make([]*Node, 0, len(roots))
	for _, r := range roots {
		nodes = append(nodes, buildJSXNode(r, src))
	}
	if len(nodes) > maxJSXRoots {
		nodes = nodes[:maxJSXRoots]
	}
	// The main render is the tree with the most elements; other returns (loading,
	// empty, error states) become variants, in source order.
	primary := 0
	for i := range nodes {
		if countDesc(nodes[i]) > countDesc(nodes[primary]) {
			primary = i
		}
	}
	root := nodes[primary]
	var variants []*Node
	for i, n := range nodes {
		if i != primary {
			variants = append(variants, n)
		}
	}
	kind := strings.TrimSuffix(baseName(path), extOf(path))
	return &Sketch{File: baseName(path), Kind: kind + " (React)", Root: root, Variants: variants}, nil
}

// collectTopJSX gathers the outermost JSX trees: a jsx_element, self-closing
// element, or fragment stops the descent, so nested children are left to
// buildJSXNode. Sibling trees (a component's separate returns, or elements
// inside a `.map`) are each collected.
func collectTopJSX(n sitter.Node, out *[]sitter.Node) {
	switch n.Type() {
	case "jsx_element", "jsx_self_closing_element", "jsx_fragment":
		*out = append(*out, n)
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		collectTopJSX(n.NamedChild(i), out)
	}
}

// buildJSXNode turns a JSX element/fragment into a Node.
func buildJSXNode(n sitter.Node, src []byte) *Node {
	node := &Node{Line: int(n.StartPoint().Row) + 1}
	switch n.Type() {
	case "jsx_fragment":
		node.Kind = "<>"
		collectJSXChildren(node, n, src)
	case "jsx_self_closing_element":
		node.Kind = jsxTag(n, src)
		forEachAttr(node, n, src)
	default: // jsx_element
		opening := childType(n, "jsx_opening_element")
		node.Kind = jsxTag(opening, src)
		forEachAttr(node, opening, src)
		collectJSXChildren(node, n, src)
	}
	return node
}

// forEachAttr routes an opening element's attributes onto the node: id sets the
// id, event handlers (onClick, onChange) become behavior, the rest are props.
func forEachAttr(node *Node, opening sitter.Node, src []byte) {
	for i := uint32(0); i < opening.NamedChildCount(); i++ {
		a := opening.NamedChild(i)
		if a.Type() != "jsx_attribute" {
			continue
		}
		name, val, hasVal := jsxAttr(a, src)
		if name == "" {
			continue
		}
		if !hasVal {
			val = "true"
		}
		switch {
		case name == "id":
			node.ID = val
		case name == "key" || name == "ref":
			// framework bookkeeping, not visual
		case isHandler(name):
			node.Behavior = append(node.Behavior, name+"="+val)
		default:
			node.Props = append(node.Props, Prop{Name: name, Value: val, Group: "other"})
		}
	}
}

// collectJSXChildren appends child elements and folds text/expression content
// into a single `text` prop, so `<h2>{title}</h2>` reads as text={title}.
func collectJSXChildren(node *Node, n sitter.Node, src []byte) {
	var text []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		switch c.Type() {
		case "jsx_element", "jsx_self_closing_element", "jsx_fragment":
			node.Children = append(node.Children, buildJSXNode(c, src))
		case "jsx_expression":
			// An expression child may embed elements (a conditional or a .map);
			// surface those as children, otherwise treat it as dynamic text.
			var inner []sitter.Node
			collectTopJSX(c, &inner)
			if len(inner) > 0 {
				for _, e := range inner {
					node.Children = append(node.Children, buildJSXNode(e, src))
				}
			} else if s := collapse(c.Content(src)); s != "" && s != "{}" {
				text = append(text, s)
			}
		case "jsx_text":
			if s := collapse(c.Content(src)); s != "" {
				text = append(text, s)
			}
		}
	}
	if len(text) > 0 {
		node.Props = append(node.Props, Prop{Name: "text", Value: clip(strings.Join(text, " ")), Group: "text"})
	}
}

// jsxAttr returns an attribute's name, value, and whether a value was present
// (a bare boolean attribute like `disabled` has none).
func jsxAttr(a sitter.Node, src []byte) (name, val string, hasVal bool) {
	for i := uint32(0); i < a.NamedChildCount(); i++ {
		ch := a.NamedChild(i)
		if i == 0 && ch.Type() == "property_identifier" {
			name = ch.Content(src)
			continue
		}
		val, hasVal = jsxAttrValue(ch, src), true
	}
	return name, val, hasVal
}

// jsxAttrValue renders an attribute value: a string without quotes, or the
// inner text of a {expression}.
func jsxAttrValue(n sitter.Node, src []byte) string {
	switch n.Type() {
	case "string":
		if frag := childType(n, "string_fragment"); !frag.IsNull() {
			return frag.Content(src)
		}
		return strings.Trim(n.Content(src), `"'`)
	case "jsx_expression":
		if n.NamedChildCount() > 0 {
			return clip(collapse(n.NamedChild(0).Content(src)))
		}
		return ""
	default:
		return clip(collapse(n.Content(src)))
	}
}

// jsxTag returns an element's tag name (div, Button, motion.div).
func jsxTag(opening sitter.Node, src []byte) string {
	for i := uint32(0); i < opening.NamedChildCount(); i++ {
		switch ch := opening.NamedChild(i); ch.Type() {
		case "identifier", "nested_identifier", "member_expression":
			return ch.Content(src)
		}
	}
	return "?"
}

func childType(n sitter.Node, typ string) sitter.Node {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if ch := n.NamedChild(i); ch.Type() == typ {
			return ch
		}
	}
	return sitter.Node{}
}

func countDesc(n *Node) int {
	c := 1
	for _, ch := range n.Children {
		c += countDesc(ch)
	}
	return c
}

func extOf(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i:]
	}
	return ""
}
