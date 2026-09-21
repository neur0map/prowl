package provider

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestClassifyValidation(t *testing.T) {
	// Only 401/403 confirm a bad credential; the upstream reason is preserved.
	got := classifyValidation("Groq", 401, []byte(`{"error":{"message":"bad token"}}`))
	if !got.IsInvalid() {
		t.Fatalf("401 should be invalid, got %+v", got)
	}
	if want := "Groq key validation failed (HTTP 401): bad token"; got.Reason != want {
		t.Fatalf("reason = %q, want %q", got.Reason, want)
	}

	if !classifyValidation("X", 403, []byte(`{"detail":"nope"}`)).IsInvalid() {
		t.Fatal("403 should be invalid")
	}
	// Everything else is treated as live, including a 5xx - a validate probe is
	// not the place to bench a key.
	for _, status := range []int{200, 404, 429, 402, 500, 503} {
		if r := classifyValidation("X", status, nil); !r.IsValid() {
			t.Fatalf("HTTP %d should be valid, got %+v", status, r)
		}
	}
}

func TestClassifyValidationDetailOrder(t *testing.T) {
	// errors[0].message wins over message/detail/title when error.message absent.
	got := classifyValidation("P", 401, []byte(`{"errors":[{"message":"first"}],"message":"second"}`))
	if got.Reason != "P key validation failed (HTTP 401): first" {
		t.Fatalf("unexpected reason %q", got.Reason)
	}
	// No parseable detail falls back to the HTTP status text.
	got = classifyValidation("P", 401, []byte(`not json`))
	if got.Reason != "P key validation failed (HTTP 401): Unauthorized" {
		t.Fatalf("unexpected fallback reason %q", got.Reason)
	}
}

// TestClassifyValidationRedirectIsInconclusive proves a 3xx never reaches the
// authenticated endpoint, so it confirms neither a working nor a rejected key
// and must not be read as valid.
func TestClassifyValidationRedirectIsInconclusive(t *testing.T) {
	for _, status := range []int{300, 301, 302, 303, 307, 308} {
		r := classifyValidation("X", status, nil)
		if !r.IsInconclusive() {
			t.Fatalf("HTTP %d should be inconclusive, got %+v", status, r)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("5"); d != 5*time.Second {
		t.Fatalf("5 => %v", d)
	}
	if d := parseRetryAfter("100000000"); d != maxRetryAfter {
		t.Fatalf("huge value should clamp to %v, got %v", maxRetryAfter, d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Fatalf("empty => %v", d)
	}
	if d := parseRetryAfter("garbage"); d != 0 {
		t.Fatalf("garbage => %v", d)
	}
	when := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(when); d <= 0 || d > 15*time.Second {
		t.Fatalf("http-date => %v, want ~10s", d)
	}
}

func TestTriStateConstructors(t *testing.T) {
	if !Valid().IsValid() || Valid().IsInvalid() || Valid().IsInconclusive() {
		t.Fatal("Valid mislabelled")
	}
	if !Invalid("x").IsInvalid() || Invalid("x").Reason != "x" {
		t.Fatal("Invalid mislabelled")
	}
	if !Inconclusive("y").IsInconclusive() || Inconclusive("y").Reason != "y" {
		t.Fatal("Inconclusive mislabelled")
	}
}

func TestNormalizeChoicesJoinsArrayContent(t *testing.T) {
	resp := &ChatResponse{Choices: []Choice{{
		Message: RespMessage{Content: json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)},
	}}}
	normalizeChoices(resp)
	if got := string(resp.Choices[0].Message.Content); got != `"ab"` {
		t.Fatalf("array content => %s, want \"ab\"", got)
	}
}

func TestNormalizeChoicesFoldsReasoningOnlyWhenSafe(t *testing.T) {
	// Content empty and no tool calls: fold reasoning into content.
	resp := &ChatResponse{Choices: []Choice{{
		Message: RespMessage{Content: json.RawMessage(`""`), Reasoning: json.RawMessage(`"thought"`)},
	}}}
	normalizeChoices(resp)
	if got := string(resp.Choices[0].Message.Content); got != `"thought"` {
		t.Fatalf("reasoning fold => %s, want \"thought\"", got)
	}

	// Tool calls present: reasoning must NOT overwrite content, or a tool-call
	// turn would lose its structure.
	resp = &ChatResponse{Choices: []Choice{{
		Message: RespMessage{
			Content:   json.RawMessage(`""`),
			Reasoning: json.RawMessage(`"thought"`),
			ToolCalls: json.RawMessage(`[{"id":"1"}]`),
		},
	}}}
	normalizeChoices(resp)
	if got := string(resp.Choices[0].Message.Content); got != `""` {
		t.Fatalf("content with tool calls => %s, want empty (no fold)", got)
	}
}
