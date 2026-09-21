package sketch

import (
	"fmt"
	"sort"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/neur0map/prowl/internal/parse"
)

// extractQML parses a QML file into its structured visual sketch.
func extractQML(path string, src []byte) (*Sketch, error) {
	tree, err := parse.Parse("qml", src)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	prog := tree.RootNode()
	var desc []string
	var root *Node
	for i := uint32(0); i < prog.NamedChildCount(); i++ {
		c := prog.NamedChild(i)
		switch c.Type() {
		case "comment":
			if t := cleanComment(c.Content(src)); t != "" {
				desc = append(desc, t)
			}
		case "ui_object_definition":
			root = buildQMLNode(c, src, "")
		}
		if root != nil {
			break
		}
	}
	if root == nil {
		return nil, fmt.Errorf("no QML component found in %s", path)
	}

	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return &Sketch{File: base, Kind: root.Kind, Desc: collapse(strings.Join(desc, " ")), Root: root}, nil
}

// buildQMLNode turns a ui_object_definition into a Node. slot is the parent
// property this object fills (e.g. delegate), or "" for a plain child element.
func buildQMLNode(n sitter.Node, src []byte, slot string) *Node {
	node := &Node{Slot: slot, Line: int(n.StartPoint().Row) + 1}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		ch := n.NamedChild(i)
		switch ch.Type() {
		case "identifier", "nested_identifier":
			if node.Kind == "" {
				node.Kind = ch.Content(src)
			}
		case "ui_object_initializer":
			parseInitializer(node, ch, src)
		}
	}
	return node
}

func parseInitializer(node *Node, init sitter.Node, src []byte) {
	for i := uint32(0); i < init.NamedChildCount(); i++ {
		ch := init.NamedChild(i)
		switch ch.Type() {
		case "ui_binding":
			handleBinding(node, ch, src)
		case "ui_object_definition":
			child := buildQMLNode(ch, src, "")
			if behaviorKinds[child.Kind] {
				node.Behavior = append(node.Behavior, behaviorLine(child))
			} else {
				node.Children = append(node.Children, child)
			}
		case "ui_object_definition_binding":
			node.Behavior = append(node.Behavior, objDefBindingLine(ch, src))
		case "ui_property":
			handleProperty(node, ch, src)
		}
	}
	sort.SliceStable(node.Props, func(i, j int) bool {
		return groupOrder[node.Props[i].Group] < groupOrder[node.Props[j].Group]
	})
}

func handleBinding(node *Node, b sitter.Node, src []byte) {
	var name string
	var val sitter.Node
	haveVal := false
	for i := uint32(0); i < b.NamedChildCount(); i++ {
		ch := b.NamedChild(i)
		if i == 0 {
			name = ch.Content(src)
			continue
		}
		val, haveVal = ch, true
	}
	if name == "" {
		return
	}
	// A property bound to an object (delegate: Rectangle { ... }) is a child.
	if haveVal && val.Type() == "ui_object_definition" {
		child := buildQMLNode(val, src, name)
		if behaviorKinds[child.Kind] {
			node.Behavior = append(node.Behavior, name+": "+behaviorLine(child))
		} else {
			node.Children = append(node.Children, child)
		}
		return
	}
	value := ""
	if haveVal {
		value = clip(collapse(stmtValue(val, src)))
	}
	switch {
	case name == "id":
		node.ID = value
	case isHandler(name), name == "states", name == "transitions":
		node.Behavior = append(node.Behavior, name+": "+value)
	default:
		node.Props = append(node.Props, Prop{Name: name, Value: value, Group: classify(name)})
	}
}

// handleProperty records a `property T name: value` declaration: the component's
// own API, or a theme/token singleton's exposed palette and metrics.
func handleProperty(node *Node, p sitter.Node, src []byte) {
	var typ, name, value string
	for i := uint32(0); i < p.NamedChildCount(); i++ {
		ch := p.NamedChild(i)
		switch ch.Type() {
		case "ui_property_modifier":
			// readonly / required / default: not needed for the sketch.
		case "type_identifier":
			typ = ch.Content(src)
		case "identifier":
			name = ch.Content(src)
		case "expression_statement":
			value = clip(collapse(stmtValue(ch, src)))
		}
	}
	if name == "" {
		return
	}
	node.Decls = append(node.Decls, Decl{Name: name, Type: typ, Value: value})
}

// stmtValue returns the source of an expression_statement's inner expression,
// which excludes any trailing line comment the grammar attaches to the
// statement node. For other node kinds it returns the node's own source.
func stmtValue(n sitter.Node, src []byte) string {
	if n.Type() == "expression_statement" && n.NamedChildCount() > 0 {
		return n.NamedChild(0).Content(src)
	}
	return n.Content(src)
}

// behaviorLine renders a behavior/animation element compactly on one line.
func behaviorLine(n *Node) string {
	var parts []string
	for _, p := range n.Props {
		parts = append(parts, p.Name+"="+p.Value)
	}
	inner := ""
	for _, c := range n.Children {
		inner += " " + behaviorLine(c)
	}
	for _, bh := range n.Behavior {
		inner += " " + bh
	}
	s := n.Kind
	if len(parts) > 0 {
		s += "(" + strings.Join(parts, ", ") + ")"
	}
	return strings.TrimSpace(s + inner)
}

// objDefBindingLine renders a `Behavior on width { ... }` style binding, where
// the element applies to a named target property.
func objDefBindingLine(n sitter.Node, src []byte) string {
	var kind, target string
	var init sitter.Node
	haveInit, ids := false, 0
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		ch := n.NamedChild(i)
		switch ch.Type() {
		case "identifier", "nested_identifier":
			if ids == 0 {
				kind = ch.Content(src)
			} else if ids == 1 {
				target = ch.Content(src)
			}
			ids++
		case "ui_object_initializer":
			init, haveInit = ch, true
		}
	}
	tmp := &Node{Kind: kind}
	if haveInit {
		parseInitializer(tmp, init, src)
	}
	line := behaviorLine(tmp)
	if target != "" {
		line = kind + " on " + target + strings.TrimPrefix(line, kind)
	}
	return line
}

// clip trims a captured value: drops trailing separators and caps its length so
// a long expression stays a hint, not a wall of text.
func clip(s string) string {
	s = strings.TrimRight(s, "; ")
	const max = 60
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}

// cleanComment strips comment markers and surrounding whitespace, including the
// per-line leading `*` of a block comment.
func cleanComment(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "//")
	s = strings.TrimPrefix(s, "/*")
	s = strings.TrimSuffix(s, "*/")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		ln = strings.TrimSpace(ln)
		ln = strings.TrimPrefix(ln, "*")
		lines[i] = strings.TrimSpace(ln)
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

// groupOrder weights property groups when rendering, so geometry and layout
// read before paint and text.
var groupOrder = map[string]int{"geom": 0, "layout": 1, "paint": 2, "text": 3, "other": 4}

// classify buckets a QML property name into a visual group.
func classify(name string) string {
	switch {
	case in(name, "width", "height", "implicitWidth", "implicitHeight", "radius", "x", "y", "z"):
		return "geom"
	case strings.HasPrefix(name, "anchors") || strings.HasPrefix(name, "Layout.") ||
		strings.HasSuffix(name, "Margin") || strings.HasSuffix(name, "Padding") ||
		in(name, "spacing", "margins", "padding", "orientation", "columns", "rows", "fill", "alignment"):
		return "layout"
	case strings.HasPrefix(name, "font") || name == "text" ||
		in(name, "elide", "wrapMode", "horizontalAlignment", "verticalAlignment"):
		return "text"
	case strings.HasPrefix(name, "border") ||
		in(name, "color", "opacity", "visible", "antialiasing", "source", "gradient", "clip", "smooth"):
		return "paint"
	default:
		return "other"
	}
}

// isHandler reports whether a binding name is a signal handler (onClicked, onAct).
func isHandler(name string) bool {
	return len(name) > 2 && strings.HasPrefix(name, "on") && name[2] >= 'A' && name[2] <= 'Z'
}

// behaviorKinds are QML element types that describe behavior/animation rather
// than a visible box, so they render as behavior notes on their parent.
var behaviorKinds = map[string]bool{
	"Behavior": true, "NumberAnimation": true, "ColorAnimation": true, "PropertyAnimation": true,
	"SequentialAnimation": true, "ParallelAnimation": true, "SpringAnimation": true,
	"SmoothedAnimation": true, "PauseAnimation": true, "Transition": true, "State": true,
	"PropertyChanges": true, "Connections": true, "Timer": true,
}
