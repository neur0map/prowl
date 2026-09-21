package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	contextpacket "github.com/neur0map/prowl/internal/context"
)

// samplingReranker orders context candidates using the connected MCP host model
// through sampling, so a client that advertises the capability gets
// semantic-quality relevance without a local inferencer. It is request-scoped:
// the tool call context and session are bound at construction because the
// SemanticReranker interface carries no context. Any failure returns an error so
// Service.Search keeps its deterministic ranking.
type samplingReranker struct {
	ctx     context.Context
	session *sdk.ServerSession
}

// sessionSupportsSampling reports whether the connected client advertised the
// sampling capability during initialization.
func sessionSupportsSampling(session *sdk.ServerSession) bool {
	if session == nil {
		return false
	}
	initialized := session.InitializeParams()
	return initialized != nil && initialized.Capabilities != nil && initialized.Capabilities.Sampling != nil
}

// Scores asks the host to order the candidates by relevance to the question and
// converts that order into descending semantic scores, mirroring the local
// AssistSemanticReranker. It never requires a local model.
func (reranker samplingReranker) Scores(question string, candidates []contextpacket.Candidate) (map[string]float64, error) {
	if !sessionSupportsSampling(reranker.session) {
		return nil, fmt.Errorf("host does not advertise sampling capability")
	}
	if len(candidates) == 0 {
		return map[string]float64{}, nil
	}
	var list strings.Builder
	for index, candidate := range candidates {
		list.WriteString(strconv.Itoa(index))
		list.WriteString(". ")
		list.WriteString(boundedText(candidate.Title, 160))
		if summary := boundedText(candidate.Summary, 200); summary != "" {
			list.WriteString(" | ")
			list.WriteString(summary)
		}
		list.WriteString("\n")
	}
	prompt := "Question: " + boundedText(question, 300) + "\n\nCandidates:\n" + list.String()
	result, err := reranker.session.CreateMessage(reranker.ctx, &sdk.CreateMessageParams{
		MaxTokens:    rerankMaxTokens(len(candidates)),
		SystemPrompt: "Rank the numbered candidates by relevance to the question. Reply with only the candidate numbers, most relevant first, separated by commas. Do not add any other text.",
		Messages:     []*sdk.SamplingMessage{{Role: sdk.Role("user"), Content: &sdk.TextContent{Text: prompt}}},
		Temperature:  0,
		ModelPreferences: &sdk.ModelPreferences{
			CostPriority: 0.5, SpeedPriority: 0.6, IntelligencePriority: 0.6,
		},
	})
	if err != nil {
		return nil, err
	}
	text, ok := result.Content.(*sdk.TextContent)
	if !ok || strings.TrimSpace(text.Text) == "" {
		return nil, fmt.Errorf("sampling reranker returned no text content")
	}
	order := parseIndexOrder(text.Text, len(candidates))
	if len(order) == 0 {
		return nil, fmt.Errorf("sampling reranker returned no usable candidate indices")
	}
	scores := make(map[string]float64, len(order))
	denominator := float64(len(order))
	for rank, index := range order {
		scores[candidates[index].ID] = float64(len(order)-rank) / denominator
	}
	return scores, nil
}

// rerankMaxTokens bounds the sampling response to a short list of indices.
func rerankMaxTokens(count int) int64 {
	tokens := int64(64 + 4*count)
	if tokens > 512 {
		tokens = 512
	}
	return tokens
}

// parseIndexOrder extracts the distinct in-range integer indices from the host
// reply in the order they appear, tolerating any surrounding punctuation or
// prose the model adds.
func parseIndexOrder(text string, count int) []int {
	var order []int
	seen := make(map[int]bool, count)
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		value := current.String()
		current.Reset()
		index, err := strconv.Atoi(value)
		if err != nil || index < 0 || index >= count || seen[index] {
			return
		}
		seen[index] = true
		order = append(order, index)
	}
	for _, r := range text {
		if r >= '0' && r <= '9' {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return order
}
