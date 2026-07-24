// Package setup owns Prowl's project-local integration setup semantics.
package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	IntegrationAgents   = "agents"
	IntegrationGeneric  = "generic-mcp"
	IntegrationCursor   = "cursor"
	IntegrationVSCode   = "vscode"
	IntegrationOMP      = "omp"
	IntegrationFactory  = "factory"
	IntegrationOpenCode = "opencode"
	IntegrationNeovim   = "neovim"
	IntegrationHelix    = "helix"

	maxIdempotencyKeyBytes = 128
)

var allIntegrations = []string{
	IntegrationAgents, IntegrationCursor, IntegrationFactory, IntegrationGeneric,
	IntegrationHelix, IntegrationNeovim, IntegrationOMP, IntegrationOpenCode, IntegrationVSCode,
}

// AllIntegrations returns the canonical integration names in deterministic order.
func AllIntegrations() []string {
	return append([]string(nil), allIntegrations...)
}

var (
	ErrApprovalRequired = errors.New("setup approval is required")
	ErrPlanConflict     = errors.New("setup plan conflicts with current project state")
)

// Action identifies one safe, project-relative integration destination.
type Action struct {
	Integration string `json:"integration"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// Plan is a deterministic, reviewable setup mutation. It intentionally contains
// no project root, configuration contents, or credential values.
type Plan struct {
	Integrations          []string `json:"integrations"`
	Actions               []Action `json:"actions"`
	ProjectConfigVersion  string   `json:"project_config_version"`
	Hash                  string   `json:"hash"`
}

// DetectResult reports the current safe setup surface.
type DetectResult struct {
	Integrations         []string `json:"integrations"`
	ProjectConfigVersion string   `json:"project_config_version"`
}

// RollbackItem reports only metadata needed to understand an apply backup.
type RollbackItem struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
}

// ApplyRequest binds an approved, reviewed plan to a project config version.
type ApplyRequest struct {
	Integrations                 []string `json:"integrations"`
	PlanHash                     string   `json:"plan_hash"`
	ExpectedProjectConfigVersion string   `json:"expected_project_config_version"`
	Approved                     bool     `json:"approved"`
	IdempotencyKey               string   `json:"idempotency_key"`
}

// ApplyOutcome is durable, replay-safe metadata for an applied setup plan.
type ApplyOutcome struct {
	PlanHash             string         `json:"plan_hash"`
	ProjectConfigVersion string         `json:"project_config_version"`
	IdempotencyKey       string         `json:"idempotency_key"`
	RollbackManifest     []RollbackItem `json:"rollback_manifest"`
	Verified             bool           `json:"verified"`
}

func sameRequest(left, right ApplyRequest) bool {
	if left.PlanHash != right.PlanHash ||
		left.ExpectedProjectConfigVersion != right.ExpectedProjectConfigVersion ||
		left.Approved != right.Approved ||
		left.IdempotencyKey != right.IdempotencyKey ||
		len(left.Integrations) != len(right.Integrations) {
		return false
	}
	for index := range left.Integrations {
		if left.Integrations[index] != right.Integrations[index] {
			return false
		}
	}
	return true
}

type replayRecord struct {
	Request ApplyRequest `json:"request"`
	Outcome ApplyOutcome `json:"outcome"`
}

// Service owns setup reads and mutations for one project root.
type Service struct {
	root string
	mu   sync.Mutex
}

func NewService(root string) (*Service, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Service{root: absolute}, nil
}

// Detect performs filesystem reads only.
func (service *Service) Detect(ctx context.Context) (DetectResult, error) {
	if err := ctx.Err(); err != nil {
		return DetectResult{}, err
	}
	version, err := service.projectConfigVersion()
	if err != nil {
		return DetectResult{}, safeError(err)
	}
	return DetectResult{Integrations: DetectIntegrations(service.root), ProjectConfigVersion: version}, nil
}

// Plan calculates a deterministic reviewable plan without persisting state.
func (service *Service) Plan(ctx context.Context, integrations []string) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	integrations, err := NormalizeIntegrations(integrations)
	if err != nil {
		return Plan{}, err
	}
	version, err := service.projectConfigVersion()
	if err != nil {
		return Plan{}, safeError(err)
	}
	actions := actionsFor(integrations)
	plan := Plan{Integrations: integrations, Actions: actions, ProjectConfigVersion: version}
	plan.Hash = planHash(plan)
	return plan, nil
}

// Apply validates the supplied review state before the first write, snapshots all
// destinations, and restores them if mutation or verification fails.
func (service *Service) Apply(ctx context.Context, request ApplyRequest) (ApplyOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ApplyOutcome{}, err
	}
	if !request.Approved {
		return ApplyOutcome{}, ErrApprovalRequired
	}
	if len(request.IdempotencyKey) == 0 || len(request.IdempotencyKey) > maxIdempotencyKeyBytes {
		return ApplyOutcome{}, errors.New("invalid setup idempotency key")
	}
	service.mu.Lock()
	defer service.mu.Unlock()

	replays, err := service.loadReplays()
	if err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	if previous, ok := replays[request.IdempotencyKey]; ok {
		if !sameRequest(previous.Request, request) {
			return ApplyOutcome{}, ErrPlanConflict
		}
		return previous.Outcome, nil
	}

	plan, err := service.Plan(ctx, request.Integrations)
	if err != nil {
		return ApplyOutcome{}, err
	}
	if request.ExpectedProjectConfigVersion != plan.ProjectConfigVersion || request.PlanHash != plan.Hash {
		return ApplyOutcome{}, ErrPlanConflict
	}

	snapshots, err := service.snapshots(plan.Actions, replayPath)
	if err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	if err := service.applyActions(plan.Actions); err != nil {
		service.restore(snapshots)
		return ApplyOutcome{}, safeError(err)
	}
	if err := service.Verify(ctx, plan); err != nil {
		service.restore(snapshots)
		return ApplyOutcome{}, safeError(err)
	}
	outcome := ApplyOutcome{
		PlanHash: plan.Hash, ProjectConfigVersion: plan.ProjectConfigVersion,
		IdempotencyKey: request.IdempotencyKey, RollbackManifest: manifest(snapshots), Verified: true,
	}
	replays[request.IdempotencyKey] = replayRecord{Request: request, Outcome: outcome}
	if err := service.saveReplays(replays); err != nil {
		service.restore(snapshots)
		return ApplyOutcome{}, safeError(err)
	}
	return outcome, nil
}

// ApplyPlan preserves the CLI's established direct setup behavior. Workbench
// callers must use Apply, which enforces review/version/idempotency guards.
func (service *Service) ApplyPlan(ctx context.Context, plan Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.Hash != planHash(plan) {
		return ErrPlanConflict
	}
	snapshots, err := service.snapshots(plan.Actions)
	if err != nil {
		return safeError(err)
	}
	if err := service.applyActions(plan.Actions); err != nil {
		service.restore(snapshots)
		return safeError(err)
	}
	if err := service.Verify(ctx, plan); err != nil {
		service.restore(snapshots)
		return safeError(err)
	}
	return nil
}

// Verify reads only the selected destinations and confirms Prowl ownership.
func (service *Service) Verify(ctx context.Context, plan Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, action := range plan.Actions {
		if !safeRelativePath(action.Path) {
			return errors.New("invalid setup action path")
		}
		data, err := os.ReadFile(filepath.Join(service.root, filepath.FromSlash(action.Path)))
		if err != nil || (!strings.Contains(string(data), "prowl-agent") && !strings.Contains(string(data), "prowl_agent")) {
			return errors.New("setup verification failed")
		}
	}
	return nil
}

func (service *Service) projectConfigVersion() (string, error) {
	data, err := os.ReadFile(filepath.Join(service.root, ".prowl", "config.toml"))
	if os.IsNotExist(err) {
		return digest([]byte("absent")), nil
	}
	if err != nil {
		return "", err
	}
	return digest(append([]byte("present\x00"), data...)), nil
}

func planHash(plan Plan) string {
	canonical := struct {
		Integrations         []string `json:"integrations"`
		Actions              []Action `json:"actions"`
		ProjectConfigVersion string   `json:"project_config_version"`
	}{plan.Integrations, plan.Actions, plan.ProjectConfigVersion}
	data, _ := json.Marshal(canonical)
	return digest(data)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func actionsFor(integrations []string) []Action {
	actions := make([]Action, 0, len(integrations))
	for _, integration := range integrations {
		path := map[string]string{
			IntegrationAgents: "AGENTS.md", IntegrationGeneric: ".mcp.json", IntegrationCursor: ".cursor/mcp.json",
			IntegrationVSCode: ".vscode/mcp.json", IntegrationOMP: ".omp/mcp.json", IntegrationFactory: ".factory/mcp.json",
			IntegrationOpenCode: "opencode.json", IntegrationNeovim: ".prowl/editor/nvim.lua", IntegrationHelix: ".helix/languages.toml",
		}[integration]
		actions = append(actions, Action{Integration: integration, Path: path, Description: "merge Prowl integration without replacing existing settings"})
	}
	return actions
}

func (service *Service) applyActions(actions []Action) error {
	for _, action := range actions {
		if !safeRelativePath(action.Path) {
			return errors.New("invalid setup action path")
		}
		var err error
		switch action.Integration {
		case IntegrationAgents:
			err = ensureAgentsBlock(filepath.Join(service.root, action.Path))
		case IntegrationGeneric, IntegrationCursor, IntegrationOMP, IntegrationFactory:
			err = mergeMCPConfig(filepath.Join(service.root, filepath.FromSlash(action.Path)), "mcpServers")
		case IntegrationVSCode:
			err = mergeMCPConfig(filepath.Join(service.root, filepath.FromSlash(action.Path)), "servers")
		case IntegrationOpenCode:
			err = mergeOpenCode(filepath.Join(service.root, action.Path))
		case IntegrationNeovim:
			err = injectNeovim(service.root)
		case IntegrationHelix:
			err = injectHelix(service.root)
		default:
			err = errors.New("unknown setup integration")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type snapshot struct {
	path    string
	rel     string
	data    []byte
	mode    os.FileMode
	existed bool
}

const replayPath = ".prowl/setup-applies.json"

func (service *Service) snapshots(actions []Action, extra ...string) ([]snapshot, error) {
	paths := make([]string, 0, len(actions)+len(extra))
	for _, action := range actions { paths = append(paths, action.Path) }
	paths = append(paths, extra...)
	snapshots := make([]snapshot, 0, len(paths))
	for _, rel := range paths {
		if !safeRelativePath(rel) { return nil, errors.New("invalid setup action path") }
		path := filepath.Join(service.root, filepath.FromSlash(rel))
		info, err := os.Stat(path)
		if os.IsNotExist(err) { snapshots = append(snapshots, snapshot{path: path, rel: rel}); continue }
		if err != nil { return nil, err }
		data, err := os.ReadFile(path)
		if err != nil { return nil, err }
		snapshots = append(snapshots, snapshot{path: path, rel: rel, data: data, mode: info.Mode(), existed: true})
	}
	return snapshots, nil
}

func manifest(snapshots []snapshot) []RollbackItem {
	out := make([]RollbackItem, 0, len(snapshots))
	for _, item := range snapshots {
		if item.rel == replayPath { continue }
		out = append(out, RollbackItem{Path: item.rel, Existed: item.existed})
	}
	return out
}

func (service *Service) restore(snapshots []snapshot) {
	for _, before := range snapshots {
		if before.existed {
			_ = os.MkdirAll(filepath.Dir(before.path), 0o755)
			_ = os.WriteFile(before.path, before.data, before.mode.Perm())
		} else {
			_ = os.Remove(before.path)
		}
	}
}

func (service *Service) loadReplays() (map[string]replayRecord, error) {
	data, err := os.ReadFile(filepath.Join(service.root, replayPath))
	if os.IsNotExist(err) { return map[string]replayRecord{}, nil }
	if err != nil { return nil, err }
	out := map[string]replayRecord{}
	if err := json.Unmarshal(data, &out); err != nil { return nil, errors.New("invalid setup replay state") }
	return out, nil
}

func (service *Service) saveReplays(replays map[string]replayRecord) error {
	data, err := json.Marshal(replays)
	if err != nil { return err }
	path := filepath.Join(service.root, replayPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { return err }
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func DetectIntegrations(root string) []string {
	checks := []struct{ name, path string }{
		{IntegrationAgents, "AGENTS.md"}, {IntegrationGeneric, ".mcp.json"}, {IntegrationCursor, ".cursor"},
		{IntegrationVSCode, ".vscode"}, {IntegrationOMP, ".omp"}, {IntegrationFactory, ".factory"},
		{IntegrationOpenCode, "opencode.json"}, {IntegrationNeovim, ".nvim"}, {IntegrationHelix, ".helix"},
	}
	out := make([]string, 0, len(checks))
	for _, check := range checks { if _, err := os.Stat(filepath.Join(root, check.path)); err == nil { out = append(out, check.name) } }
	sort.Strings(out)
	return out
}

func ParseIntegrationSelection(value string, detected []string) ([]string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "auto" { return NormalizeIntegrations(detected) }
	if value == "none" { return []string{}, nil }
	if value == "all" { return append([]string(nil), allIntegrations...), nil }
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		switch parts[i] { case "mcp", "generic": parts[i] = IntegrationGeneric; case "oh-my-pi": parts[i] = IntegrationOMP; case "vs-code": parts[i] = IntegrationVSCode }
	}
	return NormalizeIntegrations(parts)
}

func NormalizeIntegrations(values []string) ([]string, error) {
	known := make(map[string]bool, len(allIntegrations))
	for _, name := range allIntegrations { known[name] = true }
	set := map[string]bool{}
	for _, name := range values {
		if name == "" { continue }
		if !known[name] { return nil, fmt.Errorf("unknown integration %q", name) }
		set[name] = true
	}
	out := make([]string, 0, len(set))
	for name := range set { out = append(out, name) }
	sort.Strings(out)
	return out, nil
}

func RemoveIntegrations(root string, integrations []string) error {
	integrations, err := NormalizeIntegrations(integrations)
	if err != nil { return err }
	for _, name := range integrations {
		var err error
		switch name {
		case IntegrationAgents: err = removeAgentsBlock(filepath.Join(root, "AGENTS.md"))
		case IntegrationGeneric: err = removeMCPConfig(filepath.Join(root, ".mcp.json"), "mcpServers")
		case IntegrationCursor: err = removeMCPConfig(filepath.Join(root, ".cursor", "mcp.json"), "mcpServers")
		case IntegrationVSCode: err = removeMCPConfig(filepath.Join(root, ".vscode", "mcp.json"), "servers")
		case IntegrationOMP: err = removeMCPConfig(filepath.Join(root, ".omp", "mcp.json"), "mcpServers")
		case IntegrationFactory: err = removeMCPConfig(filepath.Join(root, ".factory", "mcp.json"), "mcpServers")
		case IntegrationOpenCode: err = removeOpenCode(filepath.Join(root, "opencode.json"))
		case IntegrationNeovim: err = removeOwnedFile(filepath.Join(root, ".prowl", "editor", "nvim.lua"), nvimConfig)
		case IntegrationHelix: err = removeOwnedFile(filepath.Join(root, ".helix", "languages.toml"), helixConfig())
		}
		if err != nil { return safeError(err) }
	}
	return nil
}

func Inject(root string) error {
	service, err := NewService(root)
	if err != nil { return err }
	plan, err := service.Plan(context.Background(), []string{IntegrationAgents, IntegrationGeneric, IntegrationCursor, IntegrationVSCode, IntegrationOMP, IntegrationFactory, IntegrationOpenCode})
	if err != nil { return err }
	return service.ApplyPlan(context.Background(), plan)
}

func InjectEditor(root string) error {
	dir := filepath.Join(root, ".prowl", "editor")
	if err := os.MkdirAll(dir, 0o755); err != nil { return err }
	files := map[string]string{"nvim.lua": nvimConfig, "helix-languages.toml": helixConfig(), "SETUP.md": editorSetupDoc}
	for name, content := range files { if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil { return err } }
	helix := filepath.Join(root, ".helix", "languages.toml")
	if _, err := os.Stat(helix); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(helix), 0o755); err != nil { return err }
		if err := os.WriteFile(helix, []byte(helixConfig()), 0o644); err != nil { return err }
	}
	return nil
}

const agentsMarker = "<!-- prowl-agent -->"
const agentsEndMarker = "<!-- /prowl-agent -->"

const AgentsMarker = agentsMarker
const AgentsEndMarker = agentsEndMarker
const agentsBlock = agentsMarker + `
## Prowl project context

Use the local Prowl index before broad file reads. Results cite file and line ranges;
the index refreshes automatically.

- Start: ` + "`prowl-agent overview`" + `
- Locate code or knowledge: ` + "`prowl-agent find <name>`" + ` and ` + "`prowl-agent search <text>`" + `
- Understand dependencies: ` + "`prowl-agent callers|callees|relations <path>`" + `
- Before editing: ` + "`prowl-agent impact <path>`" + ` and ` + "`prowl-agent references <symbol_id>`" + `
- After editing: ` + "`prowl-agent changed`" + ` and ` + "`prowl-agent doctor`" + `
- Dotfile/desktop checks: ` + "`prowl-agent doctor --profile rice`" + `

Use ` + "`--format human|toon|json|markdown`" + ` as needed; non-terminal output
remains token-lean TOON by default. MCP clients can launch ` + "`prowl-agent serve`" + `.
<!-- /prowl-agent -->`

type mcpServer struct { Type string `json:"type"`; Command string `json:"command"`; Args []string `json:"args"` }

func mergeMCPConfig(path, key string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { return err }
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil { if err := json.Unmarshal(data, &doc); err != nil { return errors.New("invalid existing setup JSON") } } else if !os.IsNotExist(err) { return err }
	servers, _ := doc[key].(map[string]any); if servers == nil { servers = map[string]any{} }
	servers["prowl-agent"] = mcpServer{Type: "stdio", Command: "prowl-agent", Args: []string{"serve"}}
	doc[key] = servers
	return writeJSON(path, doc)
}

func mergeOpenCode(path string) error {
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil { if err := json.Unmarshal(data, &doc); err != nil { return errors.New("invalid existing setup JSON") } } else if !os.IsNotExist(err) { return err }
	if _, ok := doc["$schema"]; !ok { doc["$schema"] = "https://opencode.ai/config.json" }
	mcp, _ := doc["mcp"].(map[string]any); if mcp == nil { mcp = map[string]any{} }
	mcp["prowl-agent"] = map[string]any{"type":"local", "command":[]string{"prowl-agent", "serve"}, "enabled":true}
	doc["mcp"] = mcp
	return writeJSON(path, doc)
}

func ensureAgentsBlock(path string) error {
	data, err := os.ReadFile(path); if err != nil && !os.IsNotExist(err) { return err }
	content := string(data)
	if start := strings.Index(content, agentsMarker); start >= 0 {
		end := len(content)
		if offset := strings.Index(content[start:], agentsEndMarker); offset >= 0 { end = start + offset + len(agentsEndMarker) } else if offset := strings.IndexByte(content[start:], '\n'); offset >= 0 { end = start + offset }
		updated := content[:start] + agentsBlock + content[end:]
		if updated == content { return nil }
		return os.WriteFile(path, []byte(updated), 0o644)
	}
	if content != "" && !strings.HasSuffix(content, "\n") { content += "\n" }
	if content != "" { content += "\n" }
	return os.WriteFile(path, []byte(content+agentsBlock+"\n"), 0o644)
}

// EnsureAgentsBlock updates only Prowl's marked block in AGENTS.md.
func EnsureAgentsBlock(path string) error {
	return ensureAgentsBlock(path)
}

func removeMCPConfig(path, key string) error { data, err := os.ReadFile(path); if os.IsNotExist(err) { return nil }; if err != nil { return err }; doc := map[string]any{}; if err := json.Unmarshal(data, &doc); err != nil { return errors.New("invalid existing setup JSON") }; servers, _ := doc[key].(map[string]any); if servers == nil { return nil }; if _, ok := servers["prowl-agent"]; !ok { return nil }; delete(servers, "prowl-agent"); doc[key] = servers; return writeJSON(path, doc) }
func removeOpenCode(path string) error { data, err := os.ReadFile(path); if os.IsNotExist(err) { return nil }; if err != nil { return err }; doc := map[string]any{}; if err := json.Unmarshal(data, &doc); err != nil { return errors.New("invalid existing setup JSON") }; mcp, _ := doc["mcp"].(map[string]any); if mcp == nil { return nil }; delete(mcp, "prowl-agent"); doc["mcp"] = mcp; return writeJSON(path, doc) }
func writeJSON(path string, doc map[string]any) error { out, err := json.MarshalIndent(doc, "", "  "); if err != nil { return err }; return os.WriteFile(path, append(out, '\n'), 0o644) }
func removeAgentsBlock(path string) error { data, err := os.ReadFile(path); if os.IsNotExist(err) { return nil }; if err != nil { return err }; content := string(data); start := strings.Index(content, agentsMarker); if start < 0 { return nil }; offset := strings.Index(content[start:], agentsEndMarker); if offset < 0 { return errors.New("malformed setup marker") }; updated := strings.TrimSpace(content[:start] + content[start+offset+len(agentsEndMarker):]); if updated == "" { return os.Remove(path) }; return os.WriteFile(path, []byte(updated+"\n"), 0o644) }
func removeOwnedFile(path, expected string) error { data, err := os.ReadFile(path); if os.IsNotExist(err) { return nil }; if err != nil { return err }; if string(data) != expected { return errors.New("refusing to remove modified setup file") }; return os.Remove(path) }

const nvimConfig = `-- prowl-agent language server (Neovim 0.11+).
-- Source this file from your config: dofile(vim.fn.getcwd() .. "/.prowl/editor/nvim.lua")
-- or copy the block into your own config.
vim.lsp.config("prowl_agent", {
  cmd = { "prowl-agent", "lsp" },
  filetypes = {
    "conf", "config", "dosini", "toml", "yaml", "json", "jsonc",
    "css", "scss", "lua", "python", "sh", "bash", "fish", "qml", "hyprlang",
  },
  root_markers = { ".prowl", ".git" },
})
vim.lsp.enable("prowl_agent")
`

func helixConfig() string { langs := []string{"hyprlang", "toml", "ini", "css", "scss", "json", "yaml", "qml"}; var b strings.Builder; b.WriteString("[language-server.prowl-agent]\ncommand = \"prowl-agent\"\nargs = [\"lsp\"]\n\n# Helix replaces the per-language server list, so add your existing servers\n# back to any language below if you rely on them.\n"); for _, lang := range langs { fmt.Fprintf(&b, "\n[[language]]\nname = \"%s\"\nlanguage-servers = [\"prowl-agent\"]\n", lang) }; return b.String() }
func injectNeovim(root string) error { dir := filepath.Join(root, ".prowl", "editor"); if err := os.MkdirAll(dir, 0o755); err != nil { return err }; return os.WriteFile(filepath.Join(dir, "nvim.lua"), []byte(nvimConfig), 0o644) }
func injectHelix(root string) error { path := filepath.Join(root, ".helix", "languages.toml"); if _, err := os.Stat(path); err == nil { return nil }; if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { return err }; return os.WriteFile(path, []byte(helixConfig()), 0o644) }

const editorSetupDoc = "# Editor setup (prowl-agent LSP)\n\nGenerated project-local editor integration.\n"

func safeRelativePath(value string) bool { return value != "" && filepath.IsLocal(filepath.FromSlash(value)) && !filepath.IsAbs(value) && !strings.Contains(value, "\\") }
func safeError(err error) error { if err == nil { return nil }; return errors.New("setup operation failed") }
