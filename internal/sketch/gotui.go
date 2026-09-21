package sketch

import (
	"fmt"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/neur0map/prowl/internal/parse"
)

// GoUI is the visual sketch of a Go terminal UI built with lipgloss: the color
// palette and the named styles with their attributes. A Go View() is imperative,
// so unlike a QML scene tree there is no reliable static layout; the palette and
// style catalog are the faithful, extractable visual identity.
type GoUI struct {
	File    string       `json:"file"`
	Palette []NamedColor `json:"palette,omitempty"`
	Styles  []NamedStyle `json:"styles,omitempty"`
}

// NamedColor is a palette entry: a variable bound to a lipgloss color literal.
type NamedColor struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NamedStyle is a variable bound to a lipgloss.NewStyle() chain and the visual
// attributes that chain sets.
type NamedStyle struct {
	Name  string `json:"name"`
	Attrs []Prop `json:"attrs,omitempty"`
}

// extractGo parses a Go file and pulls out its lipgloss palette and styles.
func extractGo(path string, src []byte) (*GoUI, error) {
	tree, err := parse.Parse("go", src)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	ui := &GoUI{File: baseName(path)}
	strConst := map[string]string{}
	walkDecls(tree.RootNode(), src, func(name string, val sitter.Node) {
		if val.Type() == "interpreted_string_literal" {
			strConst[name] = strings.Trim(val.Content(src), `"`)
			return
		}
		if val.Type() != "call_expression" {
			return
		}
		chain := unwindChain(val, src)
		if len(chain) == 0 {
			return
		}
		switch ctor := chain[0]; {
		case ctor.name == "NewStyle":
			ui.Styles = append(ui.Styles, NamedStyle{Name: name, Attrs: styleAttrs(chain[1:])})
		case ctor.name == "Color" || ctor.name == "AdaptiveColor" || ctor.name == "CompleteColor":
			v := strings.Trim(ctor.args, `"`)
			if hex, ok := strConst[v]; ok {
				v = hex
			}
			ui.Palette = append(ui.Palette, NamedColor{Name: name, Value: v})
		}
	})
	if len(ui.Palette) == 0 && len(ui.Styles) == 0 {
		return nil, fmt.Errorf("no lipgloss palette or styles found in %s", path)
	}
	return ui, nil
}

// mcall is one call in a method chain: a method name and its raw argument text.
type mcall struct {
	name string
	args string
}

// unwindChain unwinds a (possibly nested) call expression into constructor-first
// order: chain[0] is the innermost call (e.g. NewStyle or Color), the rest are
// the chained methods in source order.
func unwindChain(call sitter.Node, src []byte) []mcall {
	var calls []mcall
	for call.Type() == "call_expression" {
		fn := field(call, "function")
		if fn.IsNull() || fn.Type() != "selector_expression" {
			break
		}
		f := field(fn, "field")
		calls = append(calls, mcall{name: nodeText(f, src), args: argText(field(call, "arguments"), src)})
		operand := field(fn, "operand")
		if operand.IsNull() {
			break
		}
		if operand.Type() == "call_expression" {
			call = operand
			continue
		}
		break
	}
	// Reverse to constructor-first.
	for i, j := 0, len(calls)-1; i < j; i, j = i+1, j-1 {
		calls[i], calls[j] = calls[j], calls[i]
	}
	return calls
}

// styleAttrs maps lipgloss chain methods to compact visual attributes.
func styleAttrs(methods []mcall) []Prop {
	var attrs []Prop
	for _, m := range methods {
		switch m.name {
		case "Bold", "Italic", "Underline", "Strikethrough", "Faint", "Blink", "Reverse":
			if m.args == "false" {
				continue
			}
			attrs = append(attrs, Prop{Name: strings.ToLower(m.name)})
		case "Foreground":
			attrs = append(attrs, Prop{Name: "fg", Value: m.args})
		case "Background":
			attrs = append(attrs, Prop{Name: "bg", Value: m.args})
		case "BorderForeground":
			attrs = append(attrs, Prop{Name: "border.fg", Value: m.args})
		case "Border", "BorderStyle":
			attrs = append(attrs, Prop{Name: "border", Value: m.args})
		case "Padding":
			attrs = append(attrs, Prop{Name: "padding", Value: m.args})
		case "Margin":
			attrs = append(attrs, Prop{Name: "margin", Value: m.args})
		case "Width":
			attrs = append(attrs, Prop{Name: "width", Value: m.args})
		case "Height":
			attrs = append(attrs, Prop{Name: "height", Value: m.args})
		case "Align", "AlignHorizontal", "AlignVertical":
			attrs = append(attrs, Prop{Name: "align", Value: m.args})
		default:
			attrs = append(attrs, Prop{Name: m.name, Value: m.args})
		}
	}
	return attrs
}

// Text renders a Go UI sketch: the palette, then the styles with resolved colors.
func (ui *GoUI) Text() string {
	pal := map[string]string{}
	for _, c := range ui.Palette {
		pal[c.Name] = c.Value
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  ·  Go TUI (lipgloss)\n\n", ui.File)
	if len(ui.Palette) > 0 {
		b.WriteString("PALETTE\n")
		w := 0
		for _, c := range ui.Palette {
			w = max(w, len(c.Name))
		}
		for _, c := range ui.Palette {
			fmt.Fprintf(&b, "  %-*s  %s\n", w, c.Name, c.Value)
		}
		b.WriteString("\n")
	}
	if len(ui.Styles) > 0 {
		b.WriteString("STYLES\n")
		w := 0
		for _, s := range ui.Styles {
			w = max(w, len(s.Name))
		}
		for _, s := range ui.Styles {
			fmt.Fprintf(&b, "  %-*s  %s\n", w, s.Name, attrsText(s.Attrs, pal))
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// attrsText renders style attributes, resolving color references to the palette
// so a bare `cAccent` reads as `cAccent⟨#89b4fa⟩`.
func attrsText(attrs []Prop, pal map[string]string) string {
	var parts []string
	for _, a := range attrs {
		if a.Value == "" {
			parts = append(parts, a.Name)
			continue
		}
		v := a.Value
		if hex, ok := pal[v]; ok {
			v = fmt.Sprintf("%s⟨%s⟩", v, hex)
		}
		parts = append(parts, a.Name+"="+v)
	}
	return strings.Join(parts, " ")
}

// walkDecls visits var/const/short-var declarations, invoking fn(name, value)
// for each bound name and its value expression.
func walkDecls(n sitter.Node, src []byte, fn func(name string, val sitter.Node)) {
	switch n.Type() {
	case "var_spec", "const_spec":
		bindSpec(n, src, fn)
	case "short_var_declaration":
		bindPaired(field(n, "left"), field(n, "right"), src, fn)
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		walkDecls(n.NamedChild(i), src, fn)
	}
}

// bindSpec pairs the identifier names in a var/const spec with the values in its
// expression_list, positionally.
func bindSpec(spec sitter.Node, src []byte, fn func(string, sitter.Node)) {
	var names []string
	var vals sitter.Node
	for i := uint32(0); i < spec.NamedChildCount(); i++ {
		ch := spec.NamedChild(i)
		switch ch.Type() {
		case "identifier":
			names = append(names, nodeText(ch, src))
		case "expression_list":
			vals = ch
		}
	}
	if vals.IsNull() {
		return
	}
	for i, name := range names {
		if v := vals.NamedChild(uint32(i)); !v.IsNull() {
			fn(name, v)
		}
	}
}

// bindPaired pairs a left name list with a right value list positionally.
func bindPaired(left, right sitter.Node, src []byte, fn func(string, sitter.Node)) {
	if left.IsNull() || right.IsNull() {
		return
	}
	for i := uint32(0); i < left.NamedChildCount(); i++ {
		l := left.NamedChild(i)
		if l.Type() != "identifier" {
			continue
		}
		if v := right.NamedChild(i); !v.IsNull() {
			fn(nodeText(l, src), v)
		}
	}
}

func field(n sitter.Node, name string) sitter.Node { return n.ChildByFieldName(name) }

func nodeText(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	return n.Content(src)
}

// argText renders an argument list compactly, e.g. "0, 2" or "cAccent".
func argText(args sitter.Node, src []byte) string {
	if args.IsNull() {
		return ""
	}
	var parts []string
	for i := uint32(0); i < args.NamedChildCount(); i++ {
		parts = append(parts, collapse(args.NamedChild(i).Content(src)))
	}
	return clip(strings.Join(parts, ", "))
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}
