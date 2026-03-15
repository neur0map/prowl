package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/neur0map/prowl/internal/embed"
	"github.com/neur0map/prowl/internal/store"
)

// searchResultMsg carries results from an async search.
type searchResultMsg struct {
	results []store.SearchResult
	err     error
}

// SearchModel provides semantic search with async results.
type SearchModel struct {
	input    textinput.Model
	results  []store.SearchResult
	err      error
	loading  bool
	cursor   int
	store    *store.Store
	embedder *embed.Embedder
	width    int
}

// NewSearchModel creates a search tab model.
func NewSearchModel(st *store.Store, emb *embed.Embedder) SearchModel {
	ti := textinput.New()
	ti.Placeholder = "Search codebase..."
	ti.Width = 50
	ti.Focus()

	return SearchModel{
		input:    ti,
		store:    st,
		embedder: emb,
	}
}

func (m SearchModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m SearchModel) Update(msg tea.Msg) (SearchModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			query := m.input.Value()
			if query != "" && m.embedder != nil {
				m.loading = true
				m.err = nil
				return m, m.searchCmd(query)
			}
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.results)-1 {
				m.cursor++
			}
		case "esc":
			m.input.SetValue("")
			m.results = nil
			m.cursor = 0
			m.err = nil
		}
	case searchResultMsg:
		m.loading = false
		m.results = msg.results
		m.err = msg.err
		m.cursor = 0
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m SearchModel) searchCmd(query string) tea.Cmd {
	return func() tea.Msg {
		vecs, err := m.embedder.Encode([]string{query})
		if err != nil {
			return searchResultMsg{err: fmt.Errorf("encode: %w", err)}
		}
		results, err := m.store.SearchSimilar(vecs[0], 10)
		return searchResultMsg{results: results, err: err}
	}
}

func (m SearchModel) View() string {
	var b strings.Builder

	b.WriteString("  " + m.input.View() + "\n\n")

	if m.loading {
		b.WriteString("  " + SpinnerStyle.Render("⠋") + " Searching...\n")
		return b.String()
	}

	if m.err != nil {
		b.WriteString("  " + ErrorStyle.Render("Error: "+m.err.Error()) + "\n")
		return b.String()
	}

	if m.embedder == nil {
		b.WriteString("  " + MutedStyle.Render("No embedding model loaded. Search unavailable.") + "\n")
		return b.String()
	}

	if len(m.results) == 0 && m.input.Value() != "" {
		b.WriteString("  " + MutedStyle.Render("No results. Try a different query.") + "\n")
		return b.String()
	}

	for i, r := range m.results {
		cursor := "  "
		style := MutedStyle
		if i == m.cursor {
			cursor = "▸ "
			style = SelectedStyle
		}

		score := fmt.Sprintf("%.3f", r.Score)
		line := fmt.Sprintf("%s%s  %s", cursor, style.Render(r.FilePath), MutedStyle.Render(score))
		b.WriteString(line + "\n")

		// Show first 2 signature lines for selected result
		if i == m.cursor && r.Signatures != "" {
			sigLines := strings.SplitN(r.Signatures, "\n", 3)
			for _, sl := range sigLines[:min(len(sigLines), 2)] {
				b.WriteString("    " + MutedStyle.Render(sl) + "\n")
			}
		}
	}

	return b.String()
}
