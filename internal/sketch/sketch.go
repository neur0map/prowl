// Package sketch derives a compact, structured "visual sketch" of a UI from
// source, so an AI agent (or a human) can understand how a UI looks and behaves
// without running it or reading the whole file. Declarative UI source is close
// to a scene graph; sketch extracts the element tree, the visual properties on
// each element, and the behavior (handlers, animations, conditional rendering),
// then renders it as an indented, token-lean tree. It is deterministic and needs
// no runtime, model, or display.
//
// Supported dialects, one extractor file each:
//
//	qml.go      Qt Quick / QML       scene tree + properties + behavior + tokens
//	swiftui.go  SwiftUI (swift)      view tree + modifiers + state + handlers
//	jsx.go      React (JSX / TSX)    element tree + className/style + handlers
//	gotui.go    Go + lipgloss (TUI)  color palette + named styles
//	css.go      CSS / SCSS           design tokens + rules
//
// Node-tree dialects (QML, SwiftUI, React) share the model in model.go and the renderer
// in render.go; catalog dialects (Go, CSS) carry their own small model and Text.
package sketch

import (
	"fmt"
	"strings"

	"github.com/neur0map/prowl/internal/parse"
)

// Model is a renderable visual sketch. Each supported dialect produces its own
// concrete model, all of which render to the same token-lean text and marshal
// to JSON.
type Model interface {
	Text() string
}

// Of extracts the structured visual sketch of the UI defined in src. It
// dispatches on the detected language; unsupported files return an error.
func Of(path string, src []byte) (Model, error) {
	head := src
	if len(head) > 512 {
		head = head[:512]
	}
	switch parse.Detect(path, head) {
	case "qml":
		return extractQML(path, src)
	case "tsx", "javascript":
		return extractJSX(path, src)
	case "go":
		return extractGo(path, src)
	case "css", "scss":
		return extractCSS(path, src)
	case "swift":
		return extractSwiftUI(path, src)
	default:
		return nil, fmt.Errorf("visual sketch supports QML, SwiftUI (swift), React (jsx/tsx), Go/lipgloss, and CSS files; %s is none of these", path)
	}
}

// Render produces the text visual sketch of the UI defined in src.
func Render(path string, src []byte) (string, error) {
	sk, err := Of(path, src)
	if err != nil {
		return "", err
	}
	return sk.Text(), nil
}

// in reports whether s equals any value in set.
func in(s string, set ...string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

// collapse flattens runs of whitespace (including newlines) to single spaces.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
