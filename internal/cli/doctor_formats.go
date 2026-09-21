package cli

import (
	"encoding/json"
	"os"

	"github.com/neur0map/prowl/internal/doctor"
)

// sarifLevel maps a Prowl severity to a SARIF result level.
func sarifLevel(s doctor.Severity) string {
	switch s {
	case doctor.SevError:
		return "error"
	case doctor.SevWarn:
		return "warning"
	default:
		return "note"
	}
}

// renderDoctorSARIF renders a report as SARIF 2.1.0 so it can be uploaded to
// GitHub code scanning (and read by VS Code and other static-analysis tooling),
// turning Prowl's repo-health findings into inline PR annotations.
func renderDoctorSARIF(rep doctor.Report) (string, error) {
	type artifactLoc struct {
		URI string `json:"uri"`
	}
	type region struct {
		StartLine int `json:"startLine,omitempty"`
	}
	type physLoc struct {
		ArtifactLocation artifactLoc `json:"artifactLocation"`
		Region           *region     `json:"region,omitempty"`
	}
	type location struct {
		PhysicalLocation physLoc `json:"physicalLocation"`
	}
	type message struct {
		Text string `json:"text"`
	}
	type result struct {
		RuleID    string     `json:"ruleId"`
		Level     string     `json:"level"`
		Message   message    `json:"message"`
		Locations []location `json:"locations,omitempty"`
	}
	type rule struct {
		ID string `json:"id"`
	}
	type driver struct {
		Name           string `json:"name"`
		InformationURI string `json:"informationUri"`
		Rules          []rule `json:"rules"`
	}
	type tool struct {
		Driver driver `json:"driver"`
	}
	type run struct {
		Tool    tool     `json:"tool"`
		Results []result `json:"results"`
	}
	type sarif struct {
		Schema  string `json:"$schema"`
		Version string `json:"version"`
		Runs    []run  `json:"runs"`
	}

	seenRule := map[string]bool{}
	var rules []rule
	results := make([]result, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		if !seenRule[f.Check] {
			seenRule[f.Check] = true
			rules = append(rules, rule{ID: f.Check})
		}
		res := result{RuleID: f.Check, Level: sarifLevel(f.Severity), Message: message{Text: f.Detail}}
		if f.File != "" {
			pl := physLoc{ArtifactLocation: artifactLoc{URI: f.File}}
			if f.Line > 0 {
				pl.Region = &region{StartLine: f.Line}
			}
			res.Locations = []location{{PhysicalLocation: pl}}
		}
		results = append(results, res)
	}
	doc := sarif{
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Version: "2.1.0",
		Runs: []run{{
			Tool:    tool{Driver: driver{Name: "prowl", InformationURI: "https://github.com/neur0map/prowl", Rules: rules}},
			Results: results,
		}},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// renderDoctorShields renders a shields.io endpoint badge for the health score.
func renderDoctorShields(rep doctor.Report) (string, error) {
	color := "red"
	switch {
	case rep.Score >= 90:
		color = "brightgreen"
	case rep.Score >= 75:
		color = "green"
	case rep.Score >= 50:
		color = "yellow"
	case rep.Score >= 25:
		color = "orange"
	}
	b, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"label":         "prowl health",
		"message":       itoa(rep.Score) + "/100",
		"color":         color,
	})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// findingKey identifies a finding for baseline comparison, independent of
// ordering. Line is included so a moved/new issue is distinguished from an
// existing one at another location.
func findingKey(f doctor.Finding) string {
	return f.Check + "\x00" + f.File + "\x00" + itoa(f.Line) + "\x00" + f.Detail
}

// deltaReport keeps only findings absent from the baseline report: the issues a
// change introduced. Score stays the whole-repo score; Summary recounts the new
// findings so a PR comment reads "N new issues" without the existing backlog.
func deltaReport(rep, baseline doctor.Report) doctor.Report {
	base := make(map[string]bool, len(baseline.Findings))
	for _, f := range baseline.Findings {
		base[findingKey(f)] = true
	}
	out := doctor.Report{Score: rep.Score, Summary: map[string]int{}, Findings: []doctor.Finding{}}
	for _, f := range rep.Findings {
		if base[findingKey(f)] {
			continue
		}
		out.Findings = append(out.Findings, f)
		out.Summary[f.Check]++
	}
	return out
}

// loadDoctorBaseline reads a prior --format json doctor report.
func loadDoctorBaseline(path string) (doctor.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return doctor.Report{}, err
	}
	var rep doctor.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return doctor.Report{}, err
	}
	return rep, nil
}
