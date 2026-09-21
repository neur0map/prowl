package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/doctor"
)

func TestRenderDoctorSARIF(t *testing.T) {
	rep := doctor.Report{
		Score: 70,
		Findings: []doctor.Finding{
			{Check: "dangling_reference", Severity: doctor.SevError, File: "a.go", Line: 12, Detail: "includes: missing"},
			{Check: "oversized_file", Severity: doctor.SevWarn, File: "b.go", Detail: "900 lines"},
			{Check: "dangling_reference", Severity: doctor.SevError, File: "c.go", Line: 3, Detail: "includes: gone"},
		},
	}
	out, err := renderDoctorSARIF(rep)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("SARIF not valid JSON: %v", err)
	}
	if doc["version"] != "2.1.0" {
		t.Errorf("version = %v", doc["version"])
	}
	run := doc["runs"].([]any)[0].(map[string]any)
	results := run["tool"].(map[string]any)["driver"].(map[string]any)["rules"].([]any)
	if len(results) != 2 { // deduped rule ids
		t.Errorf("rules = %d, want 2 (deduped)", len(results))
	}
	if !strings.Contains(out, "\"level\": \"error\"") || !strings.Contains(out, "\"level\": \"warning\"") {
		t.Errorf("levels not mapped:\n%s", out)
	}
	if !strings.Contains(out, "\"startLine\": 12") {
		t.Errorf("line location missing:\n%s", out)
	}
}

func TestRenderDoctorShields(t *testing.T) {
	cases := map[int]string{95: "brightgreen", 80: "green", 60: "yellow", 30: "orange", 10: "red"}
	for score, color := range cases {
		out, err := renderDoctorShields(doctor.Report{Score: score})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "\"color\":\""+color+"\"") {
			t.Errorf("score %d -> want color %s, got %s", score, color, out)
		}
		if !strings.Contains(out, itoa(score)+"/100") {
			t.Errorf("score %d message missing: %s", score, out)
		}
	}
}

func TestDeltaReport(t *testing.T) {
	base := doctor.Report{Findings: []doctor.Finding{
		{Check: "oversized_file", File: "old.go", Detail: "existing backlog"},
	}}
	rep := doctor.Report{Score: 88, Findings: []doctor.Finding{
		{Check: "oversized_file", File: "old.go", Detail: "existing backlog"}, // unchanged
		{Check: "dangling_reference", File: "new.go", Line: 4, Detail: "introduced"},
	}}
	d := deltaReport(rep, base)
	if len(d.Findings) != 1 || d.Findings[0].File != "new.go" {
		t.Fatalf("delta = %+v, want only the new finding", d.Findings)
	}
	if d.Score != 88 {
		t.Errorf("delta score = %d, want the whole-repo score 88", d.Score)
	}
	// Self vs self: no new findings, and a non-nil (empty) slice for clean JSON.
	self := deltaReport(rep, rep)
	if len(self.Findings) != 0 {
		t.Errorf("self-delta should be empty, got %+v", self.Findings)
	}
	b, _ := json.Marshal(self)
	if strings.Contains(string(b), "\"findings\":null") {
		t.Errorf("empty delta must marshal [] not null: %s", b)
	}
}
