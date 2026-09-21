package gateway

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"unicode"
)

// PromptDomain is the dominant kind of work inferred from the request itself.
// Classification is deliberately local and bounded: routing must not spend a
// second model call deciding which model should receive the first one.
type PromptDomain string

const (
	DomainGeneral    PromptDomain = "general"
	DomainCoding     PromptDomain = "coding"
	DomainAgentic    PromptDomain = "agentic"
	DomainReasoning  PromptDomain = "reasoning"
	DomainMath       PromptDomain = "math"
	DomainResearch   PromptDomain = "research"
	DomainWriting    PromptDomain = "writing"
	DomainExtraction PromptDomain = "extraction"
)

// PromptProfile is the cheap, request-scoped signal used by smart routing.
type PromptProfile struct {
	Domain     PromptDomain
	Complexity float64
	Stakes     float64
	Confidence float64
	UsesTools  bool
	HasCode    bool
	Reason     string
}

func (p PromptProfile) ComplexityLabel() string {
	switch {
	case p.Complexity >= 0.72:
		return "complex"
	case p.Complexity >= 0.38:
		return "moderate"
	default:
		return "simple"
	}
}

// ClassifyPrompt uses structural request facts and high-precision lexical cues.
// It reads only the latest user turn, bounded to 24 KiB. Harness instructions,
// accumulated history, and the mere availability of tools are capabilities of
// the client, not evidence that the user's task is difficult.
func ClassifyPrompt(messages []map[string]any, params map[string]any) PromptProfile {
	text := latestUserPromptText(messages, 24<<10)
	lower := strings.ToLower(text)
	usesTools := nonEmptyParam(params["tools"])

	coding := signalScore(lower, codingSignals)
	mathScore := signalScore(lower, mathSignals)
	research := signalScore(lower, researchSignals)
	writing := signalScore(lower, writingSignals)
	extraction := signalScore(lower, extractionSignals)
	reasoning := signalScore(lower, reasoningSignals)
	agentic := signalScore(lower, agenticSignals)
	if usesTools && agentic > 0 {
		agentic += 2
	}
	// An explicit request to research current material is stronger than a
	// coincidental repository/codebase noun. This is the common shape of a
	// request to investigate another project's implementation.
	if signalScore(lower, explicitResearchSignals) > 0 {
		research += 4
	}
	if strings.Contains(lower, "```") || strings.Contains(lower, "diff --git") || strings.Contains(lower, "stack trace") {
		coding += 4
	}

	type domainScore struct {
		domain PromptDomain
		score  int
	}
	scores := []domainScore{
		{DomainResearch, research}, {DomainCoding, coding}, {DomainAgentic, agentic},
		{DomainMath, mathScore}, {DomainWriting, writing},
		{DomainExtraction, extraction}, {DomainReasoning, reasoning},
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
	domain := DomainGeneral
	confidence := 0.35
	if scores[0].score > 0 {
		domain = scores[0].domain
		lead := scores[0].score - scores[1].score
		confidence = clamp01(0.45 + float64(lead)*0.12 + float64(scores[0].score)*0.04)
	}

	// Complexity follows the current task, not the full conversation token
	// count. A large static system prompt must not buy an expensive model.
	taskTokens := estimatedTextTokens(len(text))
	complexity := 0.12
	switch {
	case taskTokens >= 12000:
		complexity += 0.48
	case taskTokens >= 4000:
		complexity += 0.34
	case taskTokens >= 1200:
		complexity += 0.20
	case taskTokens >= 400:
		complexity += 0.08
	}
	if agentic > 0 {
		complexity += 0.16
	}
	if coding > 2 {
		complexity += 0.10
	}
	if reasoning > 2 || mathScore > 2 || research > 2 {
		complexity += 0.18
	}
	if signalScore(lower, complexTaskSignals) >= 3 {
		complexity += 0.30
	}
	switch constraints := countConstraints(lower); {
	case constraints >= 3:
		complexity += 0.12
	case constraints > 0:
		complexity += 0.05
	}
	stakes := float64(signalScore(lower, stakesSignals)) * 0.18
	stakes = clamp01(stakes)
	complexity = clamp01(complexity + stakes*0.12)

	profile := PromptProfile{
		Domain: domain, Complexity: complexity, Stakes: stakes,
		Confidence: confidence, UsesTools: usesTools, HasCode: coding > 0,
	}
	profile.Reason = string(profile.Domain) + " · " + profile.ComplexityLabel()
	if profile.Stakes >= 0.35 {
		profile.Reason += " · high stakes"
	}
	return profile
}

var codingSignals = []string{
	"compile", "compiler", "function", "method", "class ", "struct ", "interface ",
	"refactor", "bug", "debug", "stack trace", "exception", "repository", "codebase",
	"pull request", "git ", "test failure", "typescript", "javascript", "python", "golang",
	"rust ", "sql ", "api endpoint", "implement", ".go", ".ts", ".tsx", ".py", ".rs",
}

var agenticSignals = []string{
	"use the tool", "call the tool", "run the command", "open the file", "edit the file",
	"browse", "search the web", "terminal", "shell", "workflow", "step by step task",
	"implement the", "apply the fix", "make the change",
}

var mathSignals = []string{
	"calculate", "equation", "theorem", "proof", "derive", "integral", "derivative",
	"probability", "algebra", "geometry", "matrix", "optimize", "complexity analysis",
}

var researchSignals = []string{
	"research", "sources", "citations", "cite", "evidence", "compare studies", "literature",
	"latest", "current as of", "documentation", "investigate", "fact check", "verify claims",
}

var explicitResearchSignals = []string{
	"research", "search the web", "current as of", "latest documentation",
	"find sources", "with citations", "cite sources", "fact check",
}

var writingSignals = []string{
	"rewrite", "draft", "tone", "copy edit", "proofread", "email", "announcement", "essay",
	"blog post", "release notes", "make this clearer", "wording", "summarize for",
}

var extractionSignals = []string{
	"extract", "return json", "json schema", "classify", "label each", "parse", "convert to csv",
	"structured output", "fields from", "list every", "table of",
}

var reasoningSignals = []string{
	"analyze", "reason", "why", "tradeoff", "trade-off", "root cause", "architecture",
	"evaluate", "critique", "design", "plan", "explain", "constraints", "edge cases",
}

var complexTaskSignals = []string{
	"debug", "diagnose", "identify", "root cause", "implement", "migrate", "redesign",
	"across", "end to end", "without changing", "preserve", "verify", "acceptance",
}

var stakesSignals = []string{
	"security", "vulnerability", "credential", "financial", "legal", "medical", "production",
	"data loss", "destructive", "compliance", "authentication", "authorization", "privacy",
}

func signalScore(text string, signals []string) int {
	score := 0
	for _, signal := range signals {
		if strings.Contains(text, signal) {
			score++
		}
	}
	return score
}

func countConstraints(text string) int {
	count := 0
	for _, signal := range []string{" must ", " never ", " required", " without ", " only ", " do not ", " don't ", " acceptance"} {
		if strings.Contains(text, signal) {
			count++
		}
	}
	return count
}

func nonEmptyParam(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case []any:
		return len(typed) > 0
	case []map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

func latestUserPromptText(messages []map[string]any, limit int) string {
	for i := len(messages) - 1; i >= 0; i-- {
		role, _ := messages[i]["role"].(string)
		if role != "user" {
			continue
		}
		var builder strings.Builder
		builder.Grow(min(limit, 4096))
		appendPromptContent(&builder, messages[i]["content"], limit)
		return builder.String()
	}
	return ""
}

func estimatedTextTokens(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return max((bytes+3)/4, 1)
}

func appendPromptContent(builder *strings.Builder, value any, limit int) {
	appendText := func(text string) {
		remaining := limit - builder.Len()
		if remaining <= 0 {
			return
		}
		if len(text) > remaining {
			text = text[len(text)-remaining:]
		}
		builder.WriteString(text)
	}
	switch content := value.(type) {
	case string:
		appendText(content)
	case []any:
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			if text, _ := part["text"].(string); text != "" {
				appendText(text)
			}
		}
	default:
		if raw, err := json.Marshal(content); err == nil {
			appendText(string(raw))
		}
	}
}

// BenchmarkScores is the periodically refreshed capability vector for one local
// model. Scores are normalized to [0,1] when loaded.
type BenchmarkScores struct {
	Intelligence float64
	Coding       float64
	Agentic      float64
	Math         float64
	Multilingual float64
	Fresh        bool
	Source       string
}

// SmartScore is the explainable score attached to one smart-routing candidate.
type SmartScore struct {
	Capability  float64
	Reliability float64
	Speed       float64
	Economy     float64
	Headroom    float64
	RateLimit   float64
	Effective   float64
	Source      string
}

// OrderSmartChain combines request fit, current reliability, measured speed and
// quota guardrails. Operator enablement and weight overrides remain absolute;
// this function only orders candidates already admitted to the chain.
func OrderSmartChain(chain []ChainEntry, profile PromptProfile, benchmarks map[int64]BenchmarkScores, sampled bool, scorer AxisScorer) ([]ChainEntry, map[int64]SmartScore) {
	out := append([]ChainEntry(nil), chain...)
	explanations := make(map[int64]SmartScore, len(out))
	if len(out) == 0 {
		return out, explanations
	}

	composites := make([]float64, len(out))
	minimum, maximum := math.Inf(1), math.Inf(-1)
	for i := range out {
		composites[i] = IntelligenceComposite(out[i].Tier, out[i].IntelRank)
		minimum = math.Min(minimum, composites[i])
		maximum = math.Max(maximum, composites[i])
	}

	capabilityWeight := 0.30 + profile.Complexity*0.38 + profile.Stakes*0.08
	reliabilityWeight := 0.27 + profile.Stakes*0.12
	speedWeight := 0.14 + (1-profile.Complexity)*0.06
	economyWeight := 0.04 + math.Pow(1-profile.Complexity, 2)*0.28
	total := capabilityWeight + reliabilityWeight + speedWeight + economyWeight
	capabilityWeight, reliabilityWeight, speedWeight, economyWeight =
		capabilityWeight/total, reliabilityWeight/total, speedWeight/total, economyWeight/total

	minPrice, maxPrice := smartPriceRange(out)
	scores := make([]float64, len(out))
	for i := range out {
		axes := Axes{Reliability: 0.5, Speed: speedPrior, Headroom: 1, RateLimit: 1}
		if scorer != nil {
			axes = scorer.Axes(&out[i], sampled)
		}
		fallback := IntelligenceScore(composites[i], minimum, maximum)
		benchmark, found := benchmarks[out[i].ModelDBID]
		capability, source := promptCapability(out[i], profile, benchmark, found, fallback)
		economy := smartEconomyScore(out[i], minPrice, maxPrice)
		base := capabilityWeight*capability +
			reliabilityWeight*axes.Reliability +
			speedWeight*axes.Speed +
			economyWeight*economy
		effective := base * axes.Headroom * axes.RateLimit
		if out[i].WeightOverride != nil {
			effective *= *out[i].WeightOverride
		}
		scores[i] = effective
		explanations[out[i].ModelDBID] = SmartScore{
			Capability: capability, Reliability: axes.Reliability, Speed: axes.Speed, Economy: economy,
			Headroom: axes.Headroom, RateLimit: axes.RateLimit, Effective: effective, Source: source,
		}
	}

	indices := make([]int, len(out))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(a, b int) bool {
		left, right := indices[a], indices[b]
		if out[left].MatchTier != out[right].MatchTier {
			return out[left].MatchTier < out[right].MatchTier
		}
		if scores[left] != scores[right] {
			return scores[left] > scores[right]
		}
		return out[left].Priority < out[right].Priority
	})
	ordered := make([]ChainEntry, len(out))
	for i, index := range indices {
		ordered[i] = out[index]
	}
	return ordered, explanations
}

func smartPriceRange(entries []ChainEntry) (float64, float64) {
	minimum, maximum := math.Inf(1), math.Inf(-1)
	for _, entry := range entries {
		price, known := smartBlendedPrice(entry)
		if !known {
			continue
		}
		logPrice := math.Log1p(price)
		minimum = math.Min(minimum, logPrice)
		maximum = math.Max(maximum, logPrice)
	}
	return minimum, maximum
}

func smartEconomyScore(entry ChainEntry, minimum, maximum float64) float64 {
	price, known := smartBlendedPrice(entry)
	if !known {
		return 0.55
	}
	if price <= 0 {
		return 1
	}
	if math.IsInf(minimum, 1) || maximum <= minimum {
		return 0.5
	}
	return clamp01(1 - (math.Log1p(price)-minimum)/(maximum-minimum))
}

func smartBlendedPrice(entry ChainEntry) (float64, bool) {
	if entry.PaidInputPerM == nil && entry.PaidOutputPerM == nil {
		return 0, false
	}
	input, output := entry.PaidInputPerM, entry.PaidOutputPerM
	if input == nil {
		input = output
	}
	if output == nil {
		output = input
	}
	return 0.75*math.Max(0, *input) + 0.25*math.Max(0, *output), true
}

func promptCapability(entry ChainEntry, profile PromptProfile, score BenchmarkScores, found bool, fallback float64) (float64, string) {
	if !found {
		score = BenchmarkScores{Intelligence: fallback, Coding: fallback, Agentic: fallback, Math: fallback, Multilingual: fallback}
	}
	general := valueOr(score.Intelligence, fallback)
	coding := valueOr(score.Coding, general)
	agentic := valueOr(score.Agentic, general)
	mathScore := valueOr(score.Math, general)
	multilingual := valueOr(score.Multilingual, general)

	var capability float64
	switch profile.Domain {
	case DomainCoding:
		capability = 0.65*coding + 0.20*agentic + 0.15*general
	case DomainAgentic:
		capability = 0.60*agentic + 0.25*coding + 0.15*general
	case DomainMath:
		capability = 0.70*mathScore + 0.30*general
	case DomainReasoning:
		capability = general
	case DomainResearch:
		capability = 0.80*general + 0.20*multilingual
	case DomainWriting:
		capability = 0.75*general + 0.25*multilingual
	case DomainExtraction:
		capability = 0.70*general + 0.30*agentic
	default:
		capability = general
	}
	modelName := strings.ToLower(entry.ModelID + " " + entry.DisplayName)
	if profile.Domain == DomainCoding && containsCodeModelCue(modelName) {
		capability += 0.08
	}
	if profile.UsesTools && entry.SupportsTools {
		capability += 0.06
	}
	if (profile.Domain == DomainReasoning || profile.Domain == DomainMath || profile.Complexity >= 0.72) && entry.SupportsReasoning {
		capability += 0.06
	}
	source := "catalog prior"
	if found {
		source = score.Source
	}
	return clamp01(capability), source
}

func containsCodeModelCue(name string) bool {
	for _, cue := range []string{"codex", "coder", "code-", "codestral", "devstral", "deepseek-coder"} {
		if strings.Contains(name, cue) {
			return true
		}
	}
	return false
}

func valueOr(value, fallback float64) float64 {
	if value <= 0 {
		return fallback
	}
	return value
}

// normalizeModelName is shared with the benchmark matcher. It deliberately
// drops punctuation and provider decorations but keeps every letter and digit,
// so gpt-4 cannot accidentally match gpt-4o.
func normalizeModelName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if slash := strings.LastIndexByte(value, '/'); slash >= 0 {
		value = value[slash+1:]
	}
	if paren := strings.IndexByte(value, '('); paren >= 0 {
		value = value[:paren]
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}
