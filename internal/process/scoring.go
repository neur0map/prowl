package process

import (
	"regexp"
	"sort"
	"strings"

	"github.com/neur0map/prowl/internal/graph"
)

// ScoredEntry represents a symbol scored as a potential entry point.
type ScoredEntry struct {
	Symbol   graph.Symbol
	FilePath string
	Score    float64
}

// scorableKinds lists the symbol kinds eligible for entry point scoring.
var scorableKinds = map[string]bool{
	"function": true,
	"method":   true,
	"class":    true,
}

// Name patterns for demoting utility-like functions.
var demotePattern = regexp.MustCompile(
	`^(get|set|is|has|format|log|to|from|parse|stringify|validate|sanitize|normalize|encode|decode)[A-Z]`,
)

// Name patterns for promoting entry-point-like functions.
var promoteExact = regexp.MustCompile(
	`^(main|init|start|run|execute|handle|on|process|dispatch|serve|listen|boot|setup|configure|register)$`,
)
var promotePrefix = regexp.MustCompile(
	`^(main|init|start|run|execute|handle|on|process|dispatch|serve|listen|boot|setup|configure|register)[A-Z]`,
)
var promoteSuffix = regexp.MustCompile(
	`(Handler|Controller|Middleware|Router|Server)$`,
)

// Framework-level path patterns that boost entry point likelihood.
var fwPathPatterns = []string{
	"/routes/",
	"/controllers/",
	"/handlers/",
	"/cmd/",
	"/api/",
}

var fwFileNames = map[string]bool{
	"main.go":   true,
	"server.go": true,
	"app.go":    true,
}

// ScoreEntryPoints ranks symbols by their likelihood of being entry points.
// Returns scored entries sorted by score descending.
func ScoreEntryPoints(g *graph.Graph) []ScoredEntry {
	// Build per-file callee and caller counts from CALLS edges.
	calleeCount := make(map[string]map[string]bool) // file -> set of unique target files
	callerCount := make(map[string]map[string]bool) // file -> set of unique source files

	for _, e := range g.EdgesOfType("CALLS") {
		if e.SourcePath == e.TargetPath {
			continue // skip self-calls
		}
		if calleeCount[e.SourcePath] == nil {
			calleeCount[e.SourcePath] = make(map[string]bool)
		}
		calleeCount[e.SourcePath][e.TargetPath] = true

		if callerCount[e.TargetPath] == nil {
			callerCount[e.TargetPath] = make(map[string]bool)
		}
		callerCount[e.TargetPath][e.SourcePath] = true
	}

	// Compute per-file ratio: calleeCount / (callerCount + 1)
	fileRatio := make(map[string]float64)
	// Collect all files that participate in CALLS edges.
	allFiles := make(map[string]bool)
	for f := range calleeCount {
		allFiles[f] = true
	}
	for f := range callerCount {
		allFiles[f] = true
	}
	for f := range allFiles {
		cc := float64(len(calleeCount[f]))
		cr := float64(len(callerCount[f]))
		fileRatio[f] = cc / (cr + 1.0)
	}

	// Score each eligible symbol.
	var entries []ScoredEntry
	for _, sym := range g.AllSymbols() {
		if !scorableKinds[sym.Kind] {
			continue
		}

		ratio := fileRatio[sym.FilePath] // 0 if file has no CALLS edges

		visFactor := 1.0
		if sym.IsExported {
			visFactor = 1.5
		}

		nameFactor := computeNameFactor(sym.Name)
		fwFactor := computeFWFactor(sym.FilePath)

		score := ratio * visFactor * nameFactor * fwFactor
		entries = append(entries, ScoredEntry{
			Symbol:   sym,
			FilePath: sym.FilePath,
			Score:    score,
		})
	}

	// Sort by score descending.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Score > entries[j].Score
	})

	return entries
}

// computeNameFactor returns the name-based scoring factor.
func computeNameFactor(name string) float64 {
	if demotePattern.MatchString(name) {
		return 0.3
	}
	if promoteExact.MatchString(name) || promotePrefix.MatchString(name) || promoteSuffix.MatchString(name) {
		return 2.0
	}
	return 1.0
}

// computeFWFactor returns the framework-path scoring factor.
func computeFWFactor(filePath string) float64 {
	for _, pat := range fwPathPatterns {
		if strings.Contains(filePath, pat) {
			return 2.0
		}
	}
	// Check filename at end of path.
	parts := strings.Split(filePath, "/")
	if len(parts) > 0 {
		fname := parts[len(parts)-1]
		if fwFileNames[fname] {
			return 2.0
		}
	}
	return 1.0
}
