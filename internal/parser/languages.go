package parser

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// Lang identifies a supported programming language.
type Lang string

const (
	LangTypeScript Lang = "typescript"
	LangGo         Lang = "go"
	LangRust       Lang = "rust"
)

// DetectLanguage returns the language for a file path, or "" if unsupported.
func DetectLanguage(path string) Lang {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".tsx"), strings.HasSuffix(lower, ".ts"):
		return LangTypeScript
	case strings.HasSuffix(lower, ".go"):
		return LangGo
	case strings.HasSuffix(lower, ".rs"):
		return LangRust
	default:
		return ""
	}
}

// GetLanguage returns the tree-sitter Language for a Lang.
func GetLanguage(lang Lang) *sitter.Language {
	switch lang {
	case LangTypeScript:
		return typescript.GetLanguage()
	case LangGo:
		return golang.GetLanguage()
	case LangRust:
		return rust.GetLanguage()
	default:
		return nil
	}
}
