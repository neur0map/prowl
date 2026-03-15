package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/neur0map/prowl/internal/config"
)

type wizardStep int

const (
	stepConfirmDir wizardStep = iota
	stepIgnorePatterns
	stepModelChoice
	stepMCPSetup
	stepIndexing
)

const totalSteps = 5

// Detected AI coder info
type coderInfo struct {
	name      string
	found     bool
	installed bool // prowl MCP already configured
	binPath   string
}

// WizardModel implements the multi-step setup wizard.
type WizardModel struct {
	step     wizardStep
	dir      string
	absDir   string
	cfg      *config.Config
	width    int
	height   int
	quitting bool

	// Step inputs
	ignoreInput textinput.Model
	modelChoice int // 0 = download, 1 = skip

	// Model detection
	modelInstalled bool

	// MCP setup
	coders         []coderInfo
	coderCursor    int
	coderSelected  []bool
	injectClaudeMD bool
	mcpInstalled   bool
	mcpMsg         string

	// Indexing
	indexModel IndexModel

	// For sending index progress back
	program *tea.Program
	done    bool
}

// NewWizardModel creates a wizard for the given project directory.
func NewWizardModel(dir string) WizardModel {
	absDir, _ := filepath.Abs(dir)

	ii := textinput.New()
	ii.Placeholder = "*.log, tmp/, dist/"
	ii.Width = 40
	ii.CharLimit = 200

	// Detect embedding model
	homeDir, _ := os.UserHomeDir()
	modelPath := filepath.Join(homeDir, ".prowl", "models", "Snowflake_snowflake-arctic-embed-s")
	_, modelErr := os.Stat(modelPath)
	modelInstalled := modelErr == nil

	// Detect AI coders
	coders := detectCoders()
	coderSelected := make([]bool, len(coders))

	return WizardModel{
		step:           stepConfirmDir,
		dir:            dir,
		absDir:         absDir,
		cfg:            &config.Config{},
		ignoreInput:    ii,
		modelInstalled: modelInstalled,
		coders:         coders,
		coderSelected:  coderSelected,
		indexModel:     NewIndexModel(absDir),
	}
}

func detectCoders() []coderInfo {
	coders := []coderInfo{
		{name: "Claude Code", binPath: "claude"},
		{name: "Codex", binPath: "codex"},
	}
	for i := range coders {
		if p, err := exec.LookPath(coders[i].binPath); err == nil {
			coders[i].found = true
			coders[i].binPath = p
			// Check if prowl MCP is already configured
			cmd := exec.Command(coders[i].binPath, "mcp", "get", "prowl")
			if err := cmd.Run(); err == nil {
				coders[i].installed = true
			}
		}
	}
	return coders
}

func (m WizardModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m WizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.indexModel.width = msg.Width

	case tea.KeyMsg:
		if m.step == stepIndexing {
			if msg.String() == "ctrl+c" {
				m.quitting = true
				return m, tea.Quit
			}
			break
		}

		switch msg.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "q":
			// Only quit on q if not in text input
			if m.step != stepIgnorePatterns {
				m.quitting = true
				return m, tea.Quit
			}
		case "enter":
			return m.advance()
		case "up", "k":
			if m.step == stepModelChoice && m.modelChoice > 0 {
				m.modelChoice--
			}
			if m.step == stepMCPSetup && m.coderCursor > 0 {
				m.coderCursor--
			}
		case "down", "j":
			if m.step == stepModelChoice && m.modelChoice < 1 {
				m.modelChoice++
			}
			if m.step == stepMCPSetup && m.coderCursor < len(m.coders) {
				m.coderCursor++ // last position = CLAUDE.md toggle
			}
		case " ":
			if m.step == stepMCPSetup {
				if m.coderCursor < len(m.coders) {
					if m.coders[m.coderCursor].found && !m.coders[m.coderCursor].installed {
						m.coderSelected[m.coderCursor] = !m.coderSelected[m.coderCursor]
					}
				} else if m.coderCursor == len(m.coders) {
					m.injectClaudeMD = !m.injectClaudeMD
				}
			}
		}

	case indexLogMsg:
		m.indexModel, _ = m.indexModel.Update(msg)
		return m, nil

	case indexDoneMsg:
		m.indexModel, _ = m.indexModel.Update(msg)
		m.done = true
		return m, nil

	case spinner.TickMsg:
		if m.step == stepIndexing {
			var cmd tea.Cmd
			m.indexModel, cmd = m.indexModel.Update(msg)
			return m, cmd
		}
	}

	// Update active text inputs
	var cmd tea.Cmd
	if m.step == stepIgnorePatterns {
		m.ignoreInput, cmd = m.ignoreInput.Update(msg)
	}
	return m, cmd
}

func (m WizardModel) advance() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepConfirmDir:
		m.step = stepIgnorePatterns
		m.ignoreInput.Focus()
		return m, textinput.Blink

	case stepIgnorePatterns:
		if v := m.ignoreInput.Value(); v != "" {
			for _, p := range strings.Split(v, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					m.cfg.IgnorePatterns = append(m.cfg.IgnorePatterns, p)
				}
			}
		}
		m.ignoreInput.Blur()
		m.step = stepModelChoice
		// If already installed, pre-select "already installed"
		if m.modelInstalled {
			m.modelChoice = 0
		}
		return m, nil

	case stepModelChoice:
		if m.modelInstalled {
			m.cfg.ModelChoice = "default"
		} else if m.modelChoice == 1 {
			m.cfg.ModelChoice = "skip"
		} else {
			m.cfg.ModelChoice = "default"
		}
		m.step = stepMCPSetup
		return m, nil

	case stepMCPSetup:
		// Save config
		m.cfg.Save()
		// Install MCP to selected coders
		m.installMCP()
		// Always install global skill for Claude Code
		installGlobalSkill()
		// Optionally inject prowl rules into project CLAUDE.md
		if m.injectClaudeMD {
			injectClaudeMDRules(m.absDir)
		}
		m.step = stepIndexing
		return m, m.indexModel.Init()
	}

	return m, nil
}

func (m *WizardModel) installMCP() {
	var installed []string
	prowlBin, _ := os.Executable()
	if prowlBin == "" {
		prowlBin = "prowl"
	}

	for i, c := range m.coders {
		if !c.found || c.installed || !m.coderSelected[i] {
			continue
		}
		switch c.name {
		case "Claude Code":
			cmd := exec.Command("claude", "mcp", "add", "-s", "user", "prowl", "--", prowlBin, "mcp", m.absDir)
			if err := cmd.Run(); err == nil {
				installed = append(installed, "Claude Code")
				m.coders[i].installed = true
			}
		case "Codex":
			cmd := exec.Command("codex", "mcp", "add", "prowl", "--", prowlBin, "mcp", m.absDir)
			if err := cmd.Run(); err == nil {
				installed = append(installed, "Codex")
				m.coders[i].installed = true
			}
		}
	}

	if len(installed) > 0 {
		m.mcpInstalled = true
		m.mcpMsg = "Installed to: " + strings.Join(installed, ", ")
	}
}

// SetProgram sets the tea.Program reference for async index messages.
func (m *WizardModel) SetProgram(p *tea.Program) {
	m.program = p
}

// StartIndexing begins the indexing pipeline.
func (m *WizardModel) StartIndexing() {
	if m.program != nil {
		startIndexBatchCmd(m.absDir, m.program)
	}
}

// Done returns true when the wizard has completed indexing.
func (m WizardModel) Done() bool {
	return m.done
}

func (m WizardModel) View() string {
	if m.quitting {
		return ""
	}

	contentWidth := min(m.width-4, 64)
	if contentWidth < 30 {
		contentWidth = 30
	}

	var content strings.Builder

	// Header
	content.WriteString(Logo())
	content.WriteString("\n\n")

	// Step indicator
	stepBar := m.renderStepBar(contentWidth)
	content.WriteString(stepBar)
	content.WriteString("\n\n")

	// Step content in a styled box
	var stepContent string
	switch m.step {
	case stepConfirmDir:
		stepContent = m.viewConfirmDir(contentWidth)
	case stepIgnorePatterns:
		stepContent = m.viewIgnorePatterns(contentWidth)
	case stepModelChoice:
		stepContent = m.viewModelChoice(contentWidth)
	case stepMCPSetup:
		stepContent = m.viewMCPSetup(contentWidth)
	case stepIndexing:
		stepContent = m.viewIndexing(contentWidth)
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Purple).
		Padding(1, 2).
		Width(contentWidth)

	content.WriteString(box.Render(stepContent))
	content.WriteString("\n")

	// Help line
	help := m.helpForStep()
	content.WriteString(HelpStyle.Render(help))

	// Center everything
	fullContent := content.String()
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			fullContent)
	}
	return fullContent
}

func (m WizardModel) renderStepBar(width int) string {
	steps := []string{"Dir", "Ignore", "Model", "MCP", "Index"}
	var parts []string
	for i, name := range steps {
		if wizardStep(i) < m.step {
			// Completed
			parts = append(parts, SuccessStyle.Render("✓ "+name))
		} else if wizardStep(i) == m.step {
			// Active
			parts = append(parts, AccentStyle.Render("● "+name))
		} else {
			// Pending
			parts = append(parts, MutedStyle.Render("○ "+name))
		}
	}
	return strings.Join(parts, MutedStyle.Render("  ─  "))
}

func (m WizardModel) viewConfirmDir(w int) string {
	var b strings.Builder
	b.WriteString(WizardStepStyle.Render("Project Directory") + "\n\n")
	b.WriteString("Index this directory?\n\n")

	dirBox := lipgloss.NewStyle().
		Background(lipgloss.Color("#1a1a2e")).
		Foreground(Cyan).
		Bold(true).
		Padding(0, 1)
	b.WriteString(dirBox.Render(m.absDir) + "\n")

	return b.String()
}

func (m WizardModel) viewIgnorePatterns(w int) string {
	var b strings.Builder
	b.WriteString(WizardStepStyle.Render("Ignore Patterns") + "\n\n")

	b.WriteString("Default ignores:\n")
	defaults := MutedStyle.Render("node_modules, .git, vendor, dist, build, __pycache__")
	b.WriteString(defaults + "\n\n")

	b.WriteString("Additional patterns " + MutedStyle.Render("(comma-separated, optional)") + "\n\n")
	b.WriteString(m.ignoreInput.View() + "\n")

	return b.String()
}

func (m WizardModel) viewModelChoice(w int) string {
	var b strings.Builder
	b.WriteString(WizardStepStyle.Render("Embedding Model") + "\n\n")

	modelName := AccentStyle.Render("snowflake-arctic-embed-s")
	b.WriteString("Model: " + modelName + " " + MutedStyle.Render("(384-dim, ~90MB)") + "\n")
	b.WriteString(MutedStyle.Render("Enables semantic search across your codebase.") + "\n\n")

	if m.modelInstalled {
		b.WriteString(SuccessStyle.Render("✓ Model already installed") + "\n")
	} else {
		choices := []string{
			"Download model (~90MB)",
			"Skip for now",
		}
		for i, c := range choices {
			if i == m.modelChoice {
				b.WriteString(SelectedStyle.Render("  ▸ "+c) + "\n")
			} else {
				b.WriteString(MutedStyle.Render("    "+c) + "\n")
			}
		}
	}

	return b.String()
}

func (m WizardModel) viewMCPSetup(w int) string {
	var b strings.Builder
	b.WriteString(WizardStepStyle.Render("MCP Integration") + "\n\n")

	// Show detected coders
	hasAny := false
	for _, c := range m.coders {
		if c.found {
			hasAny = true
			break
		}
	}

	if hasAny {
		b.WriteString("Detected AI coders:\n\n")
		for i, c := range m.coders {
			if !c.found {
				continue
			}
			if c.installed {
				b.WriteString("  " + SuccessStyle.Render("✓ "+c.name) + " " + MutedStyle.Render("(already configured)") + "\n")
				continue
			}
			cursor := "  "
			if i == m.coderCursor {
				cursor = "▸ "
			}
			check := "[ ]"
			if m.coderSelected[i] {
				check = "[✓]"
			}
			style := MutedStyle
			if i == m.coderCursor {
				style = SelectedStyle
			}
			b.WriteString(cursor + style.Render(check+" "+c.name) + "\n")
		}
		b.WriteString("\n" + MutedStyle.Render("Space to toggle, Enter to continue") + "\n")
	} else {
		b.WriteString(MutedStyle.Render("No AI coders detected in PATH.") + "\n")
	}

	b.WriteString("\n")

	// CLAUDE.md toggle
	claudeIdx := len(m.coders)
	claudeCursor := "  "
	if m.coderCursor == claudeIdx {
		claudeCursor = "▸ "
	}
	claudeCheck := "[ ]"
	if m.injectClaudeMD {
		claudeCheck = "[✓]"
	}
	claudeStyle := MutedStyle
	if m.coderCursor == claudeIdx {
		claudeStyle = SelectedStyle
	}
	b.WriteString(claudeCursor + claudeStyle.Render(claudeCheck+" Add prowl rules to CLAUDE.md") + "\n")
	b.WriteString("  " + MutedStyle.Render("  Injects workflow rules into project CLAUDE.md") + "\n")

	b.WriteString("\n")

	// Always show raw config for manual setup
	b.WriteString(MutedStyle.Render("Manual setup for Cursor / other IDEs:") + "\n\n")

	prowlBin, _ := os.Executable()
	if prowlBin == "" {
		prowlBin = "prowl"
	}
	snippet := fmt.Sprintf(`{
  "mcpServers": {
    "prowl": {
      "command": "%s",
      "args": ["mcp", "%s"]
    }
  }
}`, prowlBin, m.absDir)

	codeBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(BorderC).
		Foreground(White).
		Padding(0, 1)
	b.WriteString(codeBox.Render(snippet) + "\n")

	if m.mcpInstalled {
		b.WriteString("\n" + SuccessStyle.Render("✓ "+m.mcpMsg) + "\n")
	}

	return b.String()
}

func (m WizardModel) viewIndexing(w int) string {
	var b strings.Builder
	b.WriteString(WizardStepStyle.Render("Indexing") + "\n\n")
	b.WriteString(m.indexModel.View())

	if m.done {
		b.WriteString("\n\n" + SuccessStyle.Render("Press Enter to open dashboard →"))
	}

	return b.String()
}

// installGlobalSkill writes ~/.claude/skills/prowl/SKILL.md so the /prowl
// slash command is available across all projects and Claude auto-discovers
// prowl tools when relevant. This is the standard Claude Code skill format.
func installGlobalSkill() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}

	skillDir := filepath.Join(homeDir, ".claude", "skills", "prowl")
	os.MkdirAll(skillDir, 0o755)

	skillMD := `---
name: prowl
description: Use prowl MCP tools to explore and understand a codebase. Use when starting a new task, exploring code structure, finding relevant files, checking dependencies or blast radius, or searching by meaning. Replaces grep/find with a single ranked call.
---

# Prowl — Codebase Context Compiler

Prowl pre-indexes your codebase into a structured graph — symbols, call edges, import relationships, community clusters, and vector embeddings — and serves it over MCP. One tool call replaces the entire grep-read-grep exploration loop.

## When to Use Prowl Tools

| Situation | Tool | Why |
|-----------|------|-----|
| Starting any new task | ` + "`prowl_overview`" + ` | See the full map before touching anything |
| "Find the files I need for X" | ` + "`prowl_scope`" + ` | Ranked files with context in one call, not 15 greps |
| Need full context on one file | ` + "`prowl_file_context`" + ` | Exports, signatures, callers, imports, community |
| About to edit a file | ` + "`prowl_impact`" + ` | Know what breaks before you break it |
| Don't know the file path | ` + "`prowl_semantic_search`" + ` | Search by meaning, not keywords |
| Compare against another repo | ` + "`prowl_clone`" + ` | Clone, index, and query a GitHub repo |

## Tool Reference

### prowl_overview — Map the territory

**Call first on every task.** Returns the full project topology so you can orient without reading a single file.

` + "```" + `
prowl_overview()
` + "```" + `

Returns JSON with:
- ` + "`files`" + `, ` + "`symbols`" + `, ` + "`edges`" + `, ` + "`embeddings`" + ` — project scale at a glance
- ` + "`languages`" + ` — file counts per language (go, typescript, rust, python, etc.)
- ` + "`communities`" + ` — clusters of related files detected via Louvain algorithm. Each member is a glance digest: ` + "`path: parent_dir | N exports, N calls, N callers`" + ` (~15 tokens per file)
- ` + "`processes`" + ` — detected multi-step call chains from entry points (e.g. ` + "`handleRequest -> validate -> save -> notify`" + `)

Use the community list to understand the project's natural boundaries (auth, api, database, ui). Use processes to understand multi-step flows.

### prowl_scope — Find exactly what's needed

**The power tool.** Describe your task in natural language. Prowl combines semantic vector search with 1-hop graph expansion, ranks by relevance + community cohesion + session heat, then sorts by dependency depth.

` + "```" + `
prowl_scope({ task: "fix the authentication login flow", limit: 8 })
` + "```" + `

Returns an array of files, each with:
- ` + "`path`" + ` — project-relative file path
- ` + "`depth`" + ` — dependency depth within the result set (0 = leaf, read first)
- ` + "`reason`" + ` — why this file was included (` + "`search_hit`" + ` or ` + "`1-hop:called_by:...`" + `)
- ` + "`score`" + ` — semantic similarity (search hits only)
- ` + "`exports`" + `, ` + "`signatures`" + `, ` + "`calls`" + `, ` + "`callers`" + `, ` + "`imports`" + `, ` + "`upstream`" + `, ` + "`community`" + `

**How to read scope results:**
1. Results are sorted by depth ascending. Read depth 0 files first — they have no dependencies within the result set
2. Then depth 1, then depth 2. This prevents backtracking ("I should have read that file first")
3. ` + "`reason`" + ` tells you why a file was included — search hits are directly relevant, 1-hop files are structurally connected
4. Files in the same community as a search hit get a ranking boost — they're likely related even if keywords don't match

### prowl_file_context — Deep-dive one file

Full structural context for a single file. Use when you already know which file you need.

` + "```" + `
prowl_file_context({ path: "src/auth/login.ts" })
` + "```" + `

Returns:
- ` + "`exports`" + ` — what this file exposes (function names, classes, types)
- ` + "`signatures`" + ` — full function/method signatures with parameters and return types
- ` + "`calls`" + ` — files this file calls into (outgoing dependencies)
- ` + "`callers`" + ` — files that call into this file (incoming dependencies)
- ` + "`imports`" + ` — files this file imports
- ` + "`upstream`" + ` — files that import this file
- ` + "`community`" + ` — which cluster this file belongs to (e.g. "auth", "api")

### prowl_impact — Know what breaks

Blast radius analysis before making changes. **Always call before editing files with callers.**

` + "```" + `
prowl_impact({ path: "src/lib/terminal.ts" })
prowl_impact({ path: "src/lib/terminal.ts", symbol: "createTerminal" })
` + "```" + `

Returns:
- ` + "`direct_dependents`" + ` — files that directly call or import the target, with edge types (CALLS/IMPORTS) and their exports/signatures
- ` + "`transitive_dependents`" + ` — files that depend on the direct dependents (2nd-degree), with the ` + "`via`" + ` path showing the chain
- ` + "`affected_communities`" + ` — which community clusters are touched
- ` + "`cross_community`" + ` — true if the change ripples across community boundaries (higher risk)

The optional ` + "`symbol`" + ` parameter narrows analysis to a specific function/type — use it to reduce noise when a file exports many things.

### prowl_semantic_search — Find by meaning

Vector similarity search when you don't know the file path. Searches over embedded function signatures.

` + "```" + `
prowl_semantic_search({ query: "password hashing and validation", limit: 5 })
` + "```" + `

Returns ranked results with scores and signature previews. Use ` + "`prowl_scope`" + ` instead when you want full context and graph expansion — semantic_search is a lighter, targeted lookup.

### prowl_clone — Compare against another repo

Clone a GitHub repo, index it with the full pipeline, and query it using ` + "`project: \"comparison\"`" + ` on any other tool.

` + "```" + `
prowl_clone({ repo: "owner/repo" })
prowl_clone({ repo: "owner/repo", ref: "v2.0.0" })
prowl_clone({ repo: "owner/repo", token: "ghp_..." })  // private repos
` + "```" + `

After cloning, use ` + "`project: \"comparison\"`" + ` on any tool:
` + "```" + `
prowl_overview({ project: "comparison" })
prowl_scope({ task: "auth flow", project: "comparison" })
` + "```" + `

Use ` + "`prowl_clone_status`" + ` to check if a comparison repo is loaded. Use ` + "`prowl_clone_close`" + ` to free resources.

## Workflow

1. **Orient** — ` + "`prowl_overview()`" + ` → understand project structure, communities, processes
2. **Scope** — ` + "`prowl_scope({ task: \"your task\" })`" + ` → get exactly the files needed, ranked and contextualized
3. **Read in order** — start with depth 0 files, then depth 1, then depth 2
4. **Impact check** — ` + "`prowl_impact({ path: \"file-you-will-edit\" })`" + ` → see blast radius before editing
5. **Deep-dive** — ` + "`prowl_file_context({ path: \"specific-file\" })`" + ` → when you need full context on one file

## Rules

- **Always** call ` + "`prowl_overview`" + ` before exploring manually — it costs ~1,500 tokens for 100 files vs ~50,000 to read them
- **Prefer** ` + "`prowl_scope`" + ` over grep/find — one call returns ranked files with full structural context
- **Always** check ` + "`prowl_impact`" + ` before editing files that have callers or upstream dependents
- **Read depth 0 first** — files are sorted by dependency depth so you never backtrack
- **Trust communities** — files in the same community are structurally related even if naming doesn't suggest it
- **Don't re-explore** — prowl caches the full graph, you don't need to re-read files between tasks
`
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o644)
}

// injectClaudeMDRules appends prowl workflow rules to the project's CLAUDE.md.
// This is optional — only called when the user opts in during wizard setup.
func injectClaudeMDRules(projectDir string) {
	claudeMD := filepath.Join(projectDir, "CLAUDE.md")
	prowlSection := `
## Prowl — Structural Context

This project is indexed by [prowl](https://github.com/neur0map/prowl). Use prowl MCP tools instead of grep/find for code exploration.

### Rules
- Always call ` + "`prowl_overview`" + ` before exploring the codebase manually
- Prefer ` + "`prowl_scope`" + ` over grep/find — it returns ranked, contextual results in one call
- Check ` + "`prowl_impact`" + ` before modifying files with many dependents
- Read files in depth order (depth 0 first, then 1, then 2) to avoid backtracking
`

	existing, _ := os.ReadFile(claudeMD)
	if strings.Contains(string(existing), "## Prowl") {
		return
	}

	f, err := os.OpenFile(claudeMD, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(prowlSection)
}

func (m WizardModel) helpForStep() string {
	switch m.step {
	case stepConfirmDir:
		return "Enter: continue  ·  q: quit"
	case stepIgnorePatterns:
		return "Enter: continue  ·  ctrl+c: quit"
	case stepModelChoice:
		if m.modelInstalled {
			return "Enter: continue"
		}
		return "j/k: select  ·  Enter: confirm"
	case stepMCPSetup:
		return "Space: toggle  ·  j/k: navigate  ·  Enter: continue"
	case stepIndexing:
		if m.done {
			return "Enter: open dashboard"
		}
		return "Indexing in progress..."
	}
	return ""
}
