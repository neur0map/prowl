package parser

import (
	"testing"

	"github.com/neur0map/prowl/internal/graph"
)

func TestParseTypeScriptSymbols(t *testing.T) {
	src := `
import { db } from './db';
import { hash } from '../utils/hash';

export function handleLogin(req: Request, res: Response): Promise<void> {
  return db.query(req.body);
}

export class AuthService {
  validate(token: string): boolean {
    return hash(token) === stored;
  }
}

const SECRET = 'abc';
`
	result, err := ParseFile("src/auth.ts", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	// Check symbols
	if len(result.Symbols) < 2 {
		t.Fatalf("expected at least 2 symbols, got %d", len(result.Symbols))
	}

	foundFunc := false
	foundClass := false
	for _, s := range result.Symbols {
		if s.Name == "handleLogin" && s.Kind == "function" {
			foundFunc = true
			if !s.IsExported {
				t.Error("handleLogin should be exported")
			}
		}
		if s.Name == "AuthService" && s.Kind == "class" {
			foundClass = true
		}
	}
	if !foundFunc {
		t.Error("missing symbol: handleLogin")
	}
	if !foundClass {
		t.Error("missing symbol: AuthService")
	}

	// Check imports
	if len(result.Imports) != 2 {
		t.Fatalf("expected 2 imports, got %d: %v", len(result.Imports), result.Imports)
	}
}

func TestParseGoSymbols(t *testing.T) {
	src := `
package auth

import (
	"fmt"
	"github.com/example/db"
)

func HandleLogin(w http.ResponseWriter, r *http.Request) {
	fmt.Println("login")
}

type AuthService struct {
	DB *db.Client
}

func (s *AuthService) Validate(token string) bool {
	return true
}
`
	result, err := ParseFile("pkg/auth/auth.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Symbols) < 3 {
		t.Fatalf("expected at least 3 symbols, got %d", len(result.Symbols))
	}

	foundFunc := false
	foundStruct := false
	foundMethod := false
	for _, s := range result.Symbols {
		if s.Name == "HandleLogin" && s.Kind == "function" {
			foundFunc = true
			if !s.IsExported {
				t.Error("HandleLogin should be exported (uppercase)")
			}
		}
		if s.Name == "AuthService" && s.Kind == "struct" {
			foundStruct = true
		}
		if s.Name == "Validate" && s.Kind == "method" {
			foundMethod = true
		}
	}
	if !foundFunc {
		t.Error("missing symbol: HandleLogin")
	}
	if !foundStruct {
		t.Error("missing symbol: AuthService")
	}
	if !foundMethod {
		t.Error("missing symbol: Validate")
	}
}

func TestParseUnsupportedLanguage(t *testing.T) {
	result, err := ParseFile("readme.md", []byte("# Hello"))
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Error("expected nil result for unsupported language")
	}
}

// Ensure graph package is used (prevents "imported and not used" errors)
var _ graph.Symbol
