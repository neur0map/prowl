package sketch

import (
	"fmt"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/neur0map/prowl/internal/parse"
)

// extractSwiftUI derives the visual sketch of a SwiftUI View from Swift source.
// It finds the primary view struct (the one whose `body` computed property
// returns a view), then turns that body expression into an element tree:
//   - view calls (VStack, Text, StatCardView) become nodes;
//   - view modifiers (.padding, .font, .background) become visual props;
//   - container trailing closures become children;
//   - control actions (Button { ... }) and .on* handlers become behavior notes;
//   - @State/@Binding/@StateObject declarations become the view's state surface.
//
// It never runs the UI: it reads the declarative tree, the SwiftUI analog of
// what sketch already does for QML and React.
func extractSwiftUI(path string, src []byte) (*Sketch, error) {
	tree, err := parse.Parse("swift", src)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	name, body, decls, ok := swiftPrimaryView(tree.RootNode(), src)
	if !ok {
		return nil, fmt.Errorf("no SwiftUI View (a struct with a `body` view) found in %s", path)
	}
	root := swiftViewNode(body, src)
	if root == nil {
		return nil, fmt.Errorf("could not read the body view of %s", path)
	}
	root.Decls = append(decls, root.Decls...)

	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return &Sketch{File: base, Kind: name, Root: root}, nil
}

// swiftPrimaryView finds the first struct/class that is a SwiftUI view (has a
// `body` computed property), returning its type name, the body's root view
// expression, and its state declarations.
func swiftPrimaryView(root sitter.Node, src []byte) (string, sitter.Node, []Decl, bool) {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		c := root.NamedChild(i)
		if c.Type() != "class_declaration" {
			continue
		}
		nameNode, body := c.ChildByFieldName("name"), c.ChildByFieldName("body")
		if nameNode.IsNull() || body.IsNull() {
			continue
		}
		var bodyExpr sitter.Node
		var decls []Decl
		found := false
		for j := uint32(0); j < body.NamedChildCount(); j++ {
			m := body.NamedChild(j)
			if m.Type() != "property_declaration" {
				continue
			}
			if propBoundName(m, src) == "body" {
				if cv := m.ChildByFieldName("computed_value"); !cv.IsNull() {
					if e, ok := firstViewExpr(cv); ok {
						bodyExpr, found = e, true
					}
				}
			} else if d, ok := stateDecl(m, src); ok {
				decls = append(decls, d)
			}
		}
		if found {
			return typeName(nameNode, src), bodyExpr, decls, true
		}
	}
	return "", sitter.Node{}, nil, false
}

// firstViewExpr returns the first view-shaped expression inside a computed
// property's statement list (the body's returned view).
func firstViewExpr(computed sitter.Node) (sitter.Node, bool) {
	stmts := namedChildOfType(computed, "statements")
	if stmts.IsNull() {
		return sitter.Node{}, false
	}
	for i := uint32(0); i < stmts.NamedChildCount(); i++ {
		s := stmts.NamedChild(i)
		if isViewExpr(s.Type()) {
			return s, true
		}
		// A `return <view>` wraps the expression one level down.
		for j := uint32(0); j < s.NamedChildCount(); j++ {
			if isViewExpr(s.NamedChild(j).Type()) {
				return s.NamedChild(j), true
			}
		}
	}
	return sitter.Node{}, false
}

// swiftViewNode turns a view expression into a Node, unwinding modifier chains
// and descending into container closures.
func swiftViewNode(n sitter.Node, src []byte) *Node {
	switch n.Type() {
	case "call_expression":
		callee := n.NamedChild(0)
		if callee.IsNull() {
			return nil
		}
		switch callee.Type() {
		case "navigation_expression":
			// `<target>.<modifier>(args)` -- a modifier on the inner view.
			inner := swiftViewNode(callee.ChildByFieldName("target"), src)
			if inner == nil {
				return nil
			}
			applyModifier(inner, navSuffixName(callee, src), callArgs(n, src))
			return inner
		case "simple_identifier":
			node := &Node{Kind: callee.Content(src), Line: nodeLine(n)}
			if cs := namedChildOfType(n, "call_suffix"); !cs.IsNull() {
				fillCallSuffix(node, cs, src)
			}
			return node
		default:
			return &Node{Kind: clip(collapse(n.Content(src))), Line: nodeLine(n)}
		}
	case "navigation_expression", "simple_identifier":
		return &Node{Kind: clip(collapse(n.Content(src))), Line: nodeLine(n)}
	case "if_statement", "if_expression", "guard_statement":
		return swiftIfNode(n, src)
	}
	// Unwrap a wrapper (e.g. a return statement) to its inner view expression.
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); isViewExpr(c.Type()) {
			return swiftViewNode(c, src)
		}
	}
	return nil
}

// fillCallSuffix reads a view call's arguments (a compact label) and its
// trailing closure (container children, or a control action as behavior).
func fillCallSuffix(node *Node, cs sitter.Node, src []byte) {
	if va := namedChildOfType(cs, "value_arguments"); !va.IsNull() {
		if s := clip(collapse(va.Content(src))); s != "" && s != "()" {
			node.Props = append(node.Props, Prop{Name: "args", Value: strings.Trim(s, "()"), Group: "other"})
		}
	}
	if lam := namedChildOfType(cs, "lambda_literal"); !lam.IsNull() {
		addClosureChildren(node, lam, src)
	}
}

// addClosureChildren interprets a trailing closure: statements that build a view
// become children; anything else (a control's action) becomes a behavior note.
func addClosureChildren(node *Node, lam sitter.Node, src []byte) {
	stmts := namedChildOfType(lam, "statements")
	if stmts.IsNull() {
		return
	}
	for i := uint32(0); i < stmts.NamedChildCount(); i++ {
		s := stmts.NamedChild(i)
		if child := swiftViewNode(s, src); child != nil && child.Kind != "" && upper(child.Kind) {
			node.Children = append(node.Children, child)
		} else {
			if b := clip(collapse(s.Content(src))); b != "" {
				node.Behavior = append(node.Behavior, b)
			}
		}
	}
}

// swiftIfNode renders a ViewBuilder conditional as an "If" node whose children
// are the branch's views and whose behavior note is the condition.
func swiftIfNode(n sitter.Node, src []byte) *Node {
	node := &Node{Kind: "If", Line: nodeLine(n)}
	if cond := n.ChildByFieldName("condition"); !cond.IsNull() {
		node.Behavior = append(node.Behavior, clip(collapse(cond.Content(src))))
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c.Type() == "statements" {
			for j := uint32(0); j < c.NamedChildCount(); j++ {
				if child := swiftViewNode(c.NamedChild(j), src); child != nil && upper(child.Kind) {
					node.Children = append(node.Children, child)
				}
			}
		}
	}
	return node
}

// applyModifier records a modifier: .on*/gesture/task/presentation modifiers are
// behavior; the rest are visual props grouped for readable rendering.
func applyModifier(node *Node, name, args string) {
	if name == "" {
		return
	}
	if swiftHandler(name) {
		call := name
		if args != "" && args != "()" {
			call += "(" + strings.Trim(clip(args), "()") + ")"
		}
		node.Behavior = append(node.Behavior, call)
		return
	}
	node.Props = append(node.Props, Prop{Name: name, Value: strings.Trim(clip(args), "()"), Group: swiftModifierGroup(name)})
}

// navSuffixName returns the modifier name from a `target.suffix` navigation.
func navSuffixName(nav sitter.Node, src []byte) string {
	suffix := nav.ChildByFieldName("suffix")
	if suffix.IsNull() {
		return ""
	}
	if id := suffix.ChildByFieldName("suffix"); !id.IsNull() {
		return id.Content(src)
	}
	return clip(collapse(suffix.Content(src)))
}

// callArgs returns the value_arguments text of a call, for a modifier's args.
func callArgs(call sitter.Node, src []byte) string {
	if cs := namedChildOfType(call, "call_suffix"); !cs.IsNull() {
		if va := namedChildOfType(cs, "value_arguments"); !va.IsNull() {
			return collapse(va.Content(src))
		}
	}
	return ""
}

// propBoundName returns a property_declaration's bound identifier name.
func propBoundName(prop sitter.Node, src []byte) string {
	name := prop.ChildByFieldName("name")
	if name.IsNull() {
		return ""
	}
	if bi := name.ChildByFieldName("bound_identifier"); !bi.IsNull() {
		return bi.Content(src)
	}
	return collapse(name.Content(src))
}

// stateDecl records a property carrying a SwiftUI state attribute
// (@State/@Binding/@StateObject/@ObservedObject/@Published/@Environment...).
func stateDecl(prop sitter.Node, src []byte) (Decl, bool) {
	mods := namedChildOfType(prop, "modifiers")
	if mods.IsNull() {
		return Decl{}, false
	}
	for i := uint32(0); i < mods.NamedChildCount(); i++ {
		a := mods.NamedChild(i)
		if a.Type() != "attribute" {
			continue
		}
		attr := clip(collapse(a.Content(src)))
		if attr == "" || !swiftStateAttr(strings.TrimPrefix(attr, "@")) {
			continue
		}
		return Decl{Name: propBoundName(prop, src), Type: attr}, true
	}
	return Decl{}, false
}

// typeName returns a declaration's type name, digging through a user_type when
// present (extensions), else the node's own text.
func typeName(n sitter.Node, src []byte) string {
	if n.Type() == "user_type" {
		if id := namedChildOfType(n, "type_identifier"); !id.IsNull() {
			return id.Content(src)
		}
	}
	return n.Content(src)
}

// namedChildOfType returns the first named child of n with the given type, or a
// null node.
func namedChildOfType(n sitter.Node, typ string) sitter.Node {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c.Type() == typ {
			return c
		}
	}
	return sitter.Node{}
}

func nodeLine(n sitter.Node) int { return int(n.StartPoint().Row) + 1 }

func upper(s string) bool { return s != "" && s[0] >= 'A' && s[0] <= 'Z' }

// isViewExpr reports whether a node type can hold a SwiftUI view expression.
func isViewExpr(typ string) bool {
	switch typ {
	case "call_expression", "navigation_expression", "if_statement", "if_expression",
		"guard_statement", "ternary_expression", "simple_identifier":
		return true
	}
	return false
}

func swiftHandler(name string) bool {
	if len(name) > 2 && strings.HasPrefix(name, "on") && name[2] >= 'A' && name[2] <= 'Z' {
		return true
	}
	switch name {
	case "task", "gesture", "refreshable", "sheet", "alert", "popover", "fullScreenCover",
		"confirmationDialog", "contextMenu", "focused", "animation":
		return true
	}
	return false
}

func swiftStateAttr(name string) bool {
	switch name {
	case "State", "Binding", "StateObject", "ObservedObject", "EnvironmentObject",
		"Environment", "Bindable", "Published", "AppStorage", "SceneStorage",
		"FocusState", "Namespace", "Query":
		return true
	}
	return false
}

// swiftModifierGroup buckets a SwiftUI modifier into a visual group so geometry
// and layout render before paint and text.
func swiftModifierGroup(name string) string {
	switch name {
	case "frame", "padding", "offset", "position", "fixedSize", "layoutPriority",
		"edgesIgnoringSafeArea", "safeAreaInset", "zIndex", "spacing", "aspectRatio":
		return "layout"
	case "foregroundColor", "foregroundStyle", "background", "backgroundStyle", "tint",
		"accentColor", "opacity", "shadow", "border", "cornerRadius", "clipShape",
		"clipped", "overlay", "blur", "brightness", "contrast", "saturation", "mask", "colorScheme":
		return "paint"
	case "font", "bold", "italic", "fontWeight", "fontDesign", "lineLimit", "lineSpacing",
		"multilineTextAlignment", "kerning", "tracking", "textCase", "monospaced", "underline":
		return "text"
	case "width", "height":
		return "geom"
	}
	return "other"
}
