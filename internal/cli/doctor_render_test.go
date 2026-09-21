package cli

import (
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/doctor"
)

func TestRenderDoctorPlain(t *testing.T) {
	rep := doctor.Report{
		Score:   82,
		Summary: map[string]int{"duplicate_keybind": 1},
		Findings: []doctor.Finding{
			{Check: "duplicate_keybind", Severity: doctor.SevWarn, File: "hypr/x.conf", Line: 3, Detail: "dup SUPER+T"},
		},
	}
	out := renderDoctorPlain(rep)
	for _, want := range []string{"82", "warn", "duplicate_keybind", "hypr/x.conf:3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plain render missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderDoctorPlainClean(t *testing.T) {
	out := renderDoctorPlain(doctor.Report{Score: 100})
	if !strings.Contains(out, "100") || !strings.Contains(out, "No issues found") {
		t.Fatalf("clean render unexpected:\n%s", out)
	}
}
