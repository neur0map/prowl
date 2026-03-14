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

func TestParseTypeScriptCalls(t *testing.T) {
	src := `
import { db } from './db';

export function handleLogin(req: Request): void {
  const user = db.findUser(req.body.email);
  validatePassword(user, req.body.password);
  sendWelcomeEmail(user.email);
}

function validatePassword(user: User, password: string): boolean {
  return hash(password) === user.passwordHash;
}
`
	result, err := ParseFile("src/auth.ts", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Calls) < 2 {
		t.Fatalf("expected at least 2 calls, got %d: %v", len(result.Calls), result.Calls)
	}

	foundFindUser := false
	foundValidate := false
	for _, c := range result.Calls {
		if c.CalleeName == "findUser" {
			foundFindUser = true
		}
		if c.CalleeName == "validatePassword" {
			foundValidate = true
		}
	}
	if !foundFindUser {
		t.Errorf("missing call: findUser. Got calls: %v", result.Calls)
	}
	if !foundValidate {
		t.Errorf("missing call: validatePassword. Got calls: %v", result.Calls)
	}
}

func TestParseTypeScriptHeritage(t *testing.T) {
	src := `
export class Animal {
  name: string;
}

export class Dog extends Animal {
  bark(): void {}
}

export interface Serializable {
  serialize(): string;
}

export class Cat extends Animal {
  meow(): void {}
}
`
	result, err := ParseFile("src/animals.ts", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Heritage) < 2 {
		t.Fatalf("expected at least 2 heritage refs, got %d: %v", len(result.Heritage), result.Heritage)
	}

	foundDogExtends := false
	foundCatExtends := false
	for _, h := range result.Heritage {
		if h.ChildName == "Dog" && h.ParentName == "Animal" && h.Type == "extends" {
			foundDogExtends = true
		}
		if h.ChildName == "Cat" && h.ParentName == "Animal" && h.Type == "extends" {
			foundCatExtends = true
		}
	}
	if !foundDogExtends {
		t.Errorf("missing heritage: Dog extends Animal. Got: %v", result.Heritage)
	}
	if !foundCatExtends {
		t.Errorf("missing heritage: Cat extends Animal. Got: %v", result.Heritage)
	}
}

func TestParseGoCalls(t *testing.T) {
	src := `
package auth

import "fmt"

func HandleLogin(w http.ResponseWriter, r *http.Request) {
	user := FindUser(r.FormValue("email"))
	ValidateToken(user.Token)
	fmt.Println("done")
}

func FindUser(email string) *User {
	return nil
}
`
	result, err := ParseFile("pkg/auth/auth.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Calls) < 2 {
		t.Fatalf("expected at least 2 calls, got %d: %v", len(result.Calls), result.Calls)
	}

	foundFindUser := false
	foundValidate := false
	for _, c := range result.Calls {
		if c.CalleeName == "FindUser" {
			foundFindUser = true
		}
		if c.CalleeName == "ValidateToken" {
			foundValidate = true
		}
	}
	if !foundFindUser {
		t.Errorf("missing call: FindUser. Got: %v", result.Calls)
	}
	if !foundValidate {
		t.Errorf("missing call: ValidateToken. Got: %v", result.Calls)
	}
}

// Ensure graph package is used (prevents "imported and not used" errors)
var _ graph.Symbol
