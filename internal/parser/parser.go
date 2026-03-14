package parser

import (
	"context"
	"embed"
	"strings"
	"unicode"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/neur0map/prowl/internal/graph"
)

//go:embed queries/*.scm
var queryFS embed.FS

// ParseResult holds the extracted symbols and raw import specifiers for one file.
type ParseResult struct {
	Symbols  []graph.Symbol
	Imports  []string            // raw specifier strings (e.g. "./db", "fmt")
	Calls    []graph.CallRef     // function/method calls found
	Heritage []graph.HeritageRef // extends/implements relationships
}

// captureToKind maps tree-sitter capture names to symbol kinds.
var captureToKind = map[string]string{
	"definition.function":  "function",
	"definition.class":     "class",
	"definition.interface": "interface",
	"definition.method":    "method",
	"definition.struct":    "struct",
	"definition.enum":      "enum",
	"definition.const":     "const",
	"definition.type":      "type",
}

// ParseFile extracts symbols and imports from a single source file.
// Returns nil (no error) for unsupported languages.
func ParseFile(filePath string, source []byte) (*ParseResult, error) {
	lang := DetectLanguage(filePath)
	if lang == "" {
		return nil, nil
	}

	tsLang := GetLanguage(lang)
	if tsLang == nil {
		return nil, nil
	}

	// Load the query file
	queryFile := "queries/" + string(lang) + ".scm"
	queryBytes, err := queryFS.ReadFile(queryFile)
	if err != nil {
		return nil, nil // no query for this language
	}

	// Parse the source
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(tsLang)

	tree, err := parser.ParseCtx(context.Background(), nil, source)
	if err != nil {
		return nil, err
	}
	root := tree.RootNode()

	// Execute the query
	q, err := sitter.NewQuery(queryBytes, tsLang)
	if err != nil {
		return nil, err
	}
	defer q.Close()

	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(q, root)

	result := &ParseResult{}
	// seenIdx tracks the index of each symbol in result.Symbols by name, for dedup/upgrade.
	seenIdx := make(map[string]int)
	// kindPriority: higher = more specific; used to upgrade "type" -> "struct"/"interface"
	kindPriority := map[string]int{
		"type":      1,
		"const":     2,
		"function":  3,
		"method":    3,
		"class":     3,
		"enum":      3,
		"struct":    4,
		"interface": 4,
	}

	for {
		match, ok := qc.NextMatch()
		if !ok {
			break
		}
		match = qc.FilterPredicates(match, source)

		// Build capture map
		caps := make(map[string]*sitter.Node)
		for _, c := range match.Captures {
			name := q.CaptureNameForId(c.Index)
			caps[name] = c.Node
		}

		// Handle call captures
		if _, isCall := caps["call"]; isCall {
			if nameNode, ok := caps["call.name"]; ok {
				calleeName := nameNode.Content(source)
				// Skip runtime/builtin names
				if !isRuntimeName(calleeName) {
					result.Calls = append(result.Calls, graph.CallRef{
						CalleeName: calleeName,
						Line:       int(nameNode.StartPoint().Row) + 1,
					})
				}
			}
			continue
		}

		// Handle heritage captures
		if _, isHeritage := caps["heritage"]; isHeritage {
			classNode := caps["heritage.class"]
			extendsNode := caps["heritage.extends"]
			if classNode != nil && extendsNode != nil {
				result.Heritage = append(result.Heritage, graph.HeritageRef{
					ChildName:  classNode.Content(source),
					ParentName: extendsNode.Content(source),
					Type:       "extends",
				})
			}
			continue
		}
		if _, isImpl := caps["heritage.impl"]; isImpl {
			classNode := caps["heritage.class"]
			implNode := caps["heritage.implements"]
			if classNode != nil && implNode != nil {
				result.Heritage = append(result.Heritage, graph.HeritageRef{
					ChildName:  classNode.Content(source),
					ParentName: implNode.Content(source),
					Type:       "implements",
				})
			}
			continue
		}

		// Handle import captures
		if _, isImport := caps["import"]; isImport {
			if srcNode, ok := caps["import.source"]; ok {
				raw := srcNode.Content(source)
				// Strip quotes
				raw = strings.Trim(raw, "'\"")
				result.Imports = append(result.Imports, raw)
			}
			continue
		}

		// Handle definition captures
		nameNode, hasName := caps["name"]
		if !hasName {
			continue
		}

		name := nameNode.Content(source)

		// Determine kind from capture names
		kind := "unknown"
		for capName, k := range captureToKind {
			if _, ok := caps[capName]; ok {
				kind = k
				break
			}
		}
		if kind == "unknown" {
			continue
		}

		// Check if we've already seen this name
		if idx, alreadySeen := seenIdx[name]; alreadySeen {
			// Upgrade kind if this match is more specific (e.g. "type" -> "struct")
			existingKind := result.Symbols[idx].Kind
			if kindPriority[kind] > kindPriority[existingKind] {
				result.Symbols[idx].Kind = kind
				// Also update signature with the more specific match
				sig := extractSignature(match, q, source)
				if sig != "" {
					result.Symbols[idx].Signature = sig
				}
			}
			continue
		}
		seenIdx[name] = len(result.Symbols)

		// Determine export status
		exported := isExported(nameNode, name, lang, source)

		// Extract signature: the full text of the definition capture node,
		// but only the first line (signature, not body)
		sig := extractSignature(match, q, source)

		sym := graph.Symbol{
			Name:       name,
			Kind:       kind,
			FilePath:   filePath,
			StartLine:  int(nameNode.StartPoint().Row) + 1,
			EndLine:    int(nameNode.EndPoint().Row) + 1,
			IsExported: exported,
			Signature:  sig,
		}
		result.Symbols = append(result.Symbols, sym)
	}

	return result, nil
}

// isExported determines whether a symbol is publicly visible.
func isExported(nameNode *sitter.Node, name string, lang Lang, source []byte) bool {
	switch lang {
	case LangGo:
		if len(name) == 0 {
			return false
		}
		return unicode.IsUpper(rune(name[0]))
	case LangTypeScript:
		// Walk up to check for export_statement ancestor
		node := nameNode.Parent()
		for node != nil {
			if node.Type() == "export_statement" {
				return true
			}
			node = node.Parent()
		}
		return false
	case LangRust:
		// Walk up to find a parent with a visibility_modifier child containing "pub"
		node := nameNode.Parent()
		for node != nil {
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				if child.Type() == "visibility_modifier" {
					return true
				}
			}
			// Stop at the nearest declaration boundary
			nt := node.Type()
			if nt == "function_item" || nt == "struct_item" || nt == "enum_item" ||
				nt == "trait_item" || nt == "impl_item" || nt == "type_item" ||
				nt == "const_item" || nt == "static_item" || nt == "mod_item" {
				break
			}
			node = node.Parent()
		}
		return false
	default:
		return false
	}
}

// isRuntimeName returns true for built-in/runtime function names that should not produce CALLS edges.
var runtimeNames = map[string]bool{
	// Console
	"log": true, "warn": true, "error": true, "info": true, "debug": true,
	"trace": true, "dir": true, "table": true, "assert": true,
	// Timers
	"setTimeout": true, "setInterval": true, "clearTimeout": true, "clearInterval": true,
	"requestAnimationFrame": true,
	// JSON/Object
	"stringify": true, "parse": true, "keys": true, "values": true, "entries": true,
	"assign": true, "freeze": true, "create": true, "defineProperty": true,
	// Array methods
	"push": true, "pop": true, "shift": true, "unshift": true, "splice": true,
	"slice": true, "map": true, "filter": true, "reduce": true, "forEach": true,
	"find": true, "findIndex": true, "some": true, "every": true, "includes": true,
	"sort": true, "reverse": true, "concat": true, "join": true, "flat": true, "flatMap": true,
	// Promise
	"then": true, "catch": true, "finally": true, "resolve": true, "reject": true, "all": true,
	// Type checks
	"typeof": true, "instanceof": true,
	// Go builtins
	"make": true, "len": true, "cap": true, "append": true, "copy": true, "delete": true,
	"close": true, "panic": true, "recover": true, "print": true, "println": true,
	"new": true, "string": true, "int": true, "float64": true, "bool": true,
	// Python builtins
	"range": true, "enumerate": true, "zip": true, "type": true, "super": true,
	"isinstance": true, "issubclass": true, "getattr": true, "setattr": true, "hasattr": true,
	// Rust builtins/macros (call name without !)
	"vec":     true,
	"format":  true,
	"write":   true,
	"writeln": true,
	"todo":    true,
	"unimplemented": true,
	"unreachable":   true,
	"unwrap":  true,
	"expect":  true,
	"clone":   true,
	"into":    true,
	"from":    true,
	"as_ref":  true,
	"as_mut":  true,
	"iter":    true,
	"collect": true,
	"ok_or":   true,
	"map_err": true,
	// Common utility
	"require": true, "import": true, "console": true, "fmt": true,
}

func isRuntimeName(name string) bool {
	return runtimeNames[name]
}

// extractSignature pulls the definition text, taking only up to the opening brace.
func extractSignature(match *sitter.QueryMatch, q *sitter.Query, source []byte) string {
	// Find the outermost definition capture node
	for _, c := range match.Captures {
		capName := q.CaptureNameForId(c.Index)
		if strings.HasPrefix(capName, "definition.") {
			text := c.Node.Content(source)
			// Truncate at opening brace to get just the signature
			if idx := strings.Index(text, "{"); idx > 0 {
				return strings.TrimSpace(text[:idx])
			}
			// For single-line definitions without braces
			if idx := strings.Index(text, "\n"); idx > 0 {
				return strings.TrimSpace(text[:idx])
			}
			if len(text) > 200 {
				return text[:200]
			}
			return strings.TrimSpace(text)
		}
	}
	return ""
}
