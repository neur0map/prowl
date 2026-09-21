package sketch

import (
	"fmt"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/neur0map/prowl/internal/parse"
)

// maxCSSRules bounds how many rules a sheet lists, keeping large stylesheets
// token-lean.
const maxCSSRules = 80

// CSSSheet is the visual sketch of a CSS/SCSS file: the design tokens it defines
// (custom properties and SCSS variables) and its rules. Tokens are a stylesheet's
// palette and metrics, the same role the Go palette or a QML token singleton
// plays.
type CSSSheet struct {
	File   string    `json:"file"`
	Tokens []CSSVar  `json:"tokens,omitempty"`
	Rules  []CSSRule `json:"rules,omitempty"`
}

// CSSVar is a name/value pair: a design token, or one declaration in a rule.
type CSSVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CSSRule is a selector and its non-token declarations.
type CSSRule struct {
	Selector string   `json:"selector"`
	Decls    []CSSVar `json:"decls,omitempty"`
}

// extractCSS parses a CSS/SCSS file into its tokens and rules.
func extractCSS(path string, src []byte) (*CSSSheet, error) {
	lang := "css"
	if strings.HasSuffix(strings.ToLower(path), ".scss") {
		lang = "scss"
	}
	tree, err := parse.Parse(lang, src)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	sheet := &CSSSheet{File: baseName(path)}
	root := tree.RootNode()

	// Tokens: every custom-property / SCSS-variable declaration anywhere in the
	// sheet, including top-level SCSS vars that sit outside any rule.
	var decls []sitter.Node
	collectDeclarations(root, &decls)
	seen := map[string]bool{}
	for _, d := range decls {
		name, value := cssDecl(d, src)
		if name != "" && isCSSToken(name) && !seen[name] {
			seen[name] = true
			sheet.Tokens = append(sheet.Tokens, CSSVar{Name: name, Value: value})
		}
	}

	// Rules: each rule_set's selector and its non-token declarations.
	var rules []sitter.Node
	collectRuleSets(root, &rules)
	for _, rs := range rules {
		selector := collapse(nodeContentOfType(rs, "selectors", src))
		block := childType(rs, "block")
		if block.IsNull() {
			continue
		}
		var ruleDecls []CSSVar
		for i := uint32(0); i < block.NamedChildCount(); i++ {
			d := block.NamedChild(i)
			if d.Type() != "declaration" {
				continue
			}
			if name, value := cssDecl(d, src); name != "" && !isCSSToken(name) {
				ruleDecls = append(ruleDecls, CSSVar{Name: name, Value: value})
			}
		}
		if len(ruleDecls) > 0 {
			sheet.Rules = append(sheet.Rules, CSSRule{Selector: selector, Decls: ruleDecls})
		}
	}
	if len(sheet.Tokens) == 0 && len(sheet.Rules) == 0 {
		return nil, fmt.Errorf("no CSS rules or tokens found in %s", path)
	}
	return sheet, nil
}

// Text renders the tokens as a palette and the rules as compact selector lines.
func (s *CSSSheet) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  ·  CSS\n\n", s.File)
	if len(s.Tokens) > 0 {
		b.WriteString("TOKENS\n")
		w := 0
		for _, t := range s.Tokens {
			w = max(w, len(t.Name))
		}
		for _, t := range s.Tokens {
			fmt.Fprintf(&b, "  %-*s  %s\n", w, t.Name, t.Value)
		}
		b.WriteString("\n")
	}
	if len(s.Rules) > 0 {
		b.WriteString("RULES\n")
		for i, r := range s.Rules {
			if i >= maxCSSRules {
				fmt.Fprintf(&b, "  … %d more rules\n", len(s.Rules)-maxCSSRules)
				break
			}
			var parts []string
			for _, d := range r.Decls {
				parts = append(parts, d.Name+"="+d.Value)
			}
			fmt.Fprintf(&b, "  %s  %s\n", r.Selector, clip(strings.Join(parts, "  ")))
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// collectRuleSets gathers every rule_set node, including SCSS-nested ones.
func collectRuleSets(n sitter.Node, out *[]sitter.Node) {
	if n.Type() == "rule_set" {
		*out = append(*out, n)
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		collectRuleSets(n.NamedChild(i), out)
	}
}

// collectDeclarations gathers every declaration node in the tree.
func collectDeclarations(n sitter.Node, out *[]sitter.Node) {
	if n.Type() == "declaration" {
		*out = append(*out, n)
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		collectDeclarations(n.NamedChild(i), out)
	}
}

// cssDecl splits a declaration node into its property name and value.
func cssDecl(d sitter.Node, src []byte) (name, value string) {
	full := d.Content(src)
	i := strings.IndexByte(full, ':')
	if i < 0 {
		return "", ""
	}
	name = strings.TrimSpace(full[:i])
	value = collapse(strings.TrimRight(strings.TrimSpace(full[i+1:]), ";"))
	return name, value
}

// isCSSToken reports whether a declaration name is a design token: a CSS custom
// property (--x) or an SCSS variable ($x).
func isCSSToken(name string) bool {
	return strings.HasPrefix(name, "--") || strings.HasPrefix(name, "$")
}

// nodeContentOfType returns the source of n's first named child of typ.
func nodeContentOfType(n sitter.Node, typ string, src []byte) string {
	if ch := childType(n, typ); !ch.IsNull() {
		return ch.Content(src)
	}
	return ""
}
