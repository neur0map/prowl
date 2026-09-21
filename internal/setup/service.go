// Package setup owns Prowl's project-local integration setup semantics.
package setup

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/neur0map/prowl/skills"
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
	// IntegrationAgentSkills installs prowl's skills into `.agents/skills/`, the
	// harness-agnostic Agent Skills standard location (agentskills.io) that Prime
	// Agent and other standard-compliant harnesses discover, not a per-client dir.
	IntegrationAgentSkills = "agent-skills"
	IntegrationClaude      = "claude"
	// IntegrationHermes is a user-level-only client (Hermes reads user-level
	// skills, so it needs no project integration). Its user assets mirror
	// Claude's, installed under ~/.hermes/skills/prowl.
	IntegrationHermes = "hermes"
	// IntegrationProwl is the retired Prowl agent harness, preserved as the
	// `prowl-legacy` binary after the current product took the `prowl` name. It is
	// a user-level-only client that reads user-level skills, so it needs no
	// project integration. It ships its own native skill tree (resolved from the
	// embedded "prowl" native assets via nativeAssetClient) installed under its
	// existing ~/.config/prowl/skills/prowl root. The id is explicit
	// (`prowl-legacy`, not `prowl`) so the current `prowl` binary is never
	// mistaken for a skill harness that would install to a root nothing reads.
	IntegrationProwl = "prowl-legacy"

	// integrationSkill marks an Action that installs one embedded agent skill.
	integrationSkill = "skill"
	// integrationRules marks an Action that installs Prowl's sticky RULES.md
	// block. omp re-injects RULES.md near every turn, so the "reach for Prowl
	// before grep" directive keeps its hold instead of drifting up the transcript
	// the way AGENTS.md does.
	integrationRules = "rules"
	// integrationAgent marks an Action that installs a prowl-aware task-agent
	// definition (the scout override), so delegated exploration uses Prowl
	// instead of grepping the whole tree.
	integrationAgent = "agent"
	// integrationLegacySkill marks an Action that migrates a repository off a
	// retired skill: it removes an exact Prowl-owned copy at the old install path
	// and, when the copy was edited locally, leaves it in place as a conflict.
	integrationLegacySkill = "legacy-skill"

	maxIdempotencyKeyBytes = 128
	replayPath             = ".prowl/setup-applies.json"
	transactionPath        = ".prowl-setup-transaction.json"
	legacyTransactionPath  = ".prowl/setup-transaction.json"
	setupLockPath          = ".prowl-setup.lock"
	transactionSchema      = 1
	setupLockTimeout       = 5 * time.Second
)

var allIntegrations = []string{
	IntegrationAgents, IntegrationAgentSkills, IntegrationClaude, IntegrationCursor, IntegrationFactory, IntegrationGeneric,
	IntegrationHelix, IntegrationNeovim, IntegrationOMP, IntegrationOpenCode, IntegrationVSCode,
}

// AllIntegrations returns the canonical integration names in deterministic order.
func AllIntegrations() []string {
	return append([]string(nil), allIntegrations...)
}

// KnownIntegrations returns the complete integration registry: every project-
// level integration plus the user-level-only harnesses (Hermes, Prowl Legacy,
// Pi, OpenClaw) init folds in from DetectInstalledHarnesses. It is the single
// source of truth for "which names are valid", so the interactive init picker
// can offer and preserve a detected user-only harness instead of dropping it.
func KnownIntegrations() []string {
	return knownIntegrations()
}

// knownIntegrations is every name NormalizeIntegrations may receive: the
// project-level integrations plus user-level-only harnesses that init folds in
// from DetectInstalledHarnesses. Hermes, Prowl, Pi, and OpenClaw have no
// project-level action (their assets are user-level, installed by `prowl
// skills`), but they must survive normalization so a detected ~/.hermes,
// ~/.config/prowl, ~/.pi, or ~/.openclaw does not fail `init`.
func knownIntegrations() []string {
	return append(append([]string(nil), allIntegrations...),
		IntegrationHermes, IntegrationProwl, IntegrationPi, IntegrationOpenClaw)
}

var (
	ErrApprovalRequired = errors.New("setup approval is required")
	ErrPlanConflict     = errors.New("setup plan conflicts with current project state")
	ErrRecoveryRequired = errors.New("an interrupted setup transaction was rolled back; review and retry")

	// errSymlinkDestination and errUnsafeActionPath name their cause in
	// project-relative terms, so they are safe to report verbatim.
	errSymlinkDestination = errors.New("is a symbolic link; Prowl does not write through symlinks")
	errUnsafeActionPath   = errors.New("invalid setup action path")
)

// Action identifies one safe, project-relative integration destination.
type Action struct {
	Integration string `json:"integration"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// BlockedAction is a destination Prowl will not write, with a project-relative
// reason. Setup degrades one integration instead of failing outright, because a
// single unwritable path is a normal repository shape, not a broken install.
type BlockedAction struct {
	Integration string `json:"integration"`
	Path        string `json:"path"`
	Reason      string `json:"reason"`
}

// Plan is a deterministic, reviewable setup mutation. It intentionally contains
// no project root, configuration contents, or credential values.
type Plan struct {
	Integrations         []string        `json:"integrations"`
	Actions              []Action        `json:"actions"`
	Blocked              []BlockedAction `json:"blocked,omitempty"`
	ProjectConfigVersion string          `json:"project_config_version"`
	Hash                 string          `json:"hash"`
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

type transactionSnapshot struct {
	Path    string `json:"path"`
	Data    []byte `json:"data"`
	Mode    uint32 `json:"mode"`
	Existed bool   `json:"existed"`
}

type transactionDirectory struct {
	Path          string `json:"path"`
	Created       bool   `json:"created"`
	LegacyExisted *bool  `json:"existed,omitempty"`
}

type transaction struct {
	SchemaVersion int                    `json:"schema_version"`
	Request       ApplyRequest           `json:"request"`
	Snapshots     []transactionSnapshot  `json:"snapshots"`
	Directories   []transactionDirectory `json:"directories"`
	journalPath   string
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
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("setup root is not a directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	return &Service{root: canonical}, nil
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

// Plan calculates a deterministic reviewable plan without persisting state. The
// preview is root-aware so a destination Prowl cannot write shows up as blocked
// here rather than as a failure halfway through apply.
func (service *Service) Plan(ctx context.Context, integrations []string) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	root, err := os.OpenRoot(service.root)
	if err != nil {
		return Plan{}, safeError(err)
	}
	defer root.Close()
	return service.plan(root, ctx, integrations)
}

// Apply validates the supplied review state, serializes project mutations,
// durably records a recoverable transaction before the first write, and
// verifies or rolls it back.
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
	root, err := os.OpenRoot(service.root)
	if err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	defer root.Close()
	pending, hasPending, err := loadTransactionInRoot(root)
	if err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	replays, err := loadReplaysInRoot(root)
	if err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	if !hasPending {
		if previous, ok := replays[request.IdempotencyKey]; ok {
			if !sameRequest(previous.Request, request) {
				return ApplyOutcome{}, ErrPlanConflict
			}
			return previous.Outcome, nil
		}
		initialPlan, err := service.plan(root, ctx, request.Integrations)
		if err != nil {
			return ApplyOutcome{}, err
		}
		if request.ExpectedProjectConfigVersion != initialPlan.ProjectConfigVersion || request.PlanHash != initialPlan.Hash {
			return ApplyOutcome{}, ErrPlanConflict
		}
	}

	service.mu.Lock()
	defer service.mu.Unlock()
	unlock, err := service.lock(ctx, root)
	if err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	defer unlock()
	if err := service.recoverInterruptedTransactionInRoot(root); err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	replays, err = loadReplaysInRoot(root)
	if err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	if previous, ok := replays[request.IdempotencyKey]; ok {
		if !sameRequest(previous.Request, request) {
			return ApplyOutcome{}, ErrPlanConflict
		}
		return previous.Outcome, nil
	}
	plan, err := service.plan(root, ctx, request.Integrations)
	if err != nil {
		return ApplyOutcome{}, err
	}
	if request.ExpectedProjectConfigVersion != plan.ProjectConfigVersion || request.PlanHash != plan.Hash {
		return ApplyOutcome{}, ErrPlanConflict
	}
	snapshots, directories, err := snapshotsInRoot(root, plan.Actions, replayPath)
	if err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	outcome := ApplyOutcome{
		PlanHash: plan.Hash, ProjectConfigVersion: plan.ProjectConfigVersion,
		IdempotencyKey: request.IdempotencyKey, RollbackManifest: manifest(snapshots), Verified: true,
	}
	pending = transaction{
		SchemaVersion: transactionSchema,
		Request:       request,
		Snapshots:     transactionSnapshots(snapshots),
		Directories:   directories,
		journalPath:   transactionPath,
	}
	if err := saveTransactionInRoot(root, pending); err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	if err := service.applyActionsInRoot(root, plan.Actions, &pending); err != nil {
		if rollbackErr := service.rollbackTransactionInRoot(root, pending); rollbackErr != nil {
			return ApplyOutcome{}, safeError(rollbackErr)
		}
		return ApplyOutcome{}, safeError(err)
	}
	if err := service.verifyInRoot(root, ctx, plan); err != nil {
		if rollbackErr := service.rollbackTransactionInRoot(root, pending); rollbackErr != nil {
			return ApplyOutcome{}, safeError(rollbackErr)
		}
		return ApplyOutcome{}, safeError(err)
	}
	replays[request.IdempotencyKey] = replayRecord{Request: request, Outcome: outcome}
	if err := service.ensureParentDirectories(root, replayPath, &pending); err != nil {
		if rollbackErr := service.rollbackTransactionInRoot(root, pending); rollbackErr != nil {
			return ApplyOutcome{}, safeError(rollbackErr)
		}
		return ApplyOutcome{}, safeError(err)
	}
	if err := saveReplaysInRoot(root, replays); err != nil {
		if rollbackErr := service.rollbackTransactionInRoot(root, pending); rollbackErr != nil {
			return ApplyOutcome{}, safeError(rollbackErr)
		}
		return ApplyOutcome{}, safeError(err)
	}
	if err := removeTransactionInRoot(root, pending.journalPath); err != nil {
		return ApplyOutcome{}, safeError(err)
	}
	return outcome, nil
}

// Verify reads only selected destinations and confirms Prowl ownership.
func (service *Service) Verify(ctx context.Context, plan Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(service.root)
	if err != nil {
		return safeError(err)
	}
	defer root.Close()
	return service.verifyInRoot(root, ctx, plan)
}

func (service *Service) verifyInRoot(root *os.Root, ctx context.Context, plan Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, action := range plan.Actions {
		if action.Integration == integrationLegacySkill {
			// A removal is verified only when the retired file is actually gone.
			// A still-present copy (a pending or rematerialized migration) is a
			// failure, not a success; other read errors -- including symlinked or
			// unsafe paths -- propagate their project-relative cause.
			switch _, err := readRootFile(root, action.Path); {
			case os.IsNotExist(err):
				continue
			case err != nil:
				return err
			default:
				return errors.New("setup verification failed")
			}
		}
		data, err := readRootFile(root, action.Path)
		if err != nil {
			return errors.New("setup verification failed")
		}
		switch action.Integration {
		case IntegrationNeovim:
			// Prowl owns nvim.lua and rewrites it on every apply, so it must be
			// the current body. A legacy prowl-agent body (which still contains
			// the "prowl" substring) means the rewrite silently did not land.
			if string(data) != nvimConfig {
				return errors.New("setup verification failed")
			}
		case IntegrationHelix:
			// Apply either wrote/migrated the current body or intentionally left
			// a foreign config in place. A body still equal to the retired
			// prowl-agent generated body means the migration did not run -- the
			// broad "contains prowl" check passed it falsely.
			if string(data) == helixConfigLegacy() {
				return errors.New("setup verification failed")
			}
		default:
			if !strings.Contains(string(data), "prowl") {
				return errors.New("setup verification failed")
			}
		}
	}
	return nil
}

func (service *Service) projectConfigVersion() (string, error) {
	root, err := os.OpenRoot(service.root)
	if err != nil {
		return "", err
	}
	defer root.Close()
	return projectConfigVersionInRoot(root)
}

func projectConfigVersionInRoot(root *os.Root) (string, error) {
	data, err := readRootFile(root, ".prowl/config.toml")
	if os.IsNotExist(err) {
		return digest([]byte("absent")), nil
	}
	if err != nil {
		return "", err
	}
	return digest(append([]byte("present\x00"), data...)), nil
}

func (service *Service) plan(root *os.Root, ctx context.Context, integrations []string) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	normalized, err := NormalizeIntegrations(integrations)
	if err != nil {
		return Plan{}, err
	}
	version, err := projectConfigVersionInRoot(root)
	if err != nil {
		return Plan{}, safeError(err)
	}
	actions, blocked, err := partitionActions(root, actionsFor(normalized))
	if err != nil {
		return Plan{}, safeError(err)
	}
	plan := Plan{Integrations: normalized, Actions: actions, Blocked: blocked, ProjectConfigVersion: version}
	plan.Hash = planHash(plan)
	return plan, nil
}

// partitionActions splits planned destinations into the ones Prowl can write and
// the ones it refuses to. A symlinked destination is the common case -- repos
// that support several agent harnesses routinely point AGENTS.md at CLAUDE.md --
// and writing through it could escape the project root, so the integration is
// dropped with a reason instead of taking the whole setup down with it.
func partitionActions(root *os.Root, actions []Action) ([]Action, []BlockedAction, error) {
	writable := make([]Action, 0, len(actions))
	var blocked []BlockedAction
	for _, action := range actions {
		if action.Integration == integrationLegacySkill {
			keep, block, err := planLegacyRemoval(root, action)
			if err != nil {
				return nil, nil, err
			}
			switch {
			case block != nil:
				blocked = append(blocked, *block)
			case keep:
				writable = append(writable, action)
			}
			continue
		}
		switch _, err := validateRootPath(root, action.Path); {
		case err == nil:
			writable = append(writable, action)
		case errors.Is(err, errSymlinkDestination):
			blocked = append(blocked, BlockedAction{Integration: action.Integration, Path: action.Path, Reason: err.Error()})
		default:
			return nil, nil, err
		}
	}
	return writable, blocked, nil
}

func planHash(plan Plan) string {
	canonical := struct {
		Integrations         []string        `json:"integrations"`
		Actions              []Action        `json:"actions"`
		Blocked              []BlockedAction `json:"blocked"`
		ProjectConfigVersion string          `json:"project_config_version"`
	}{plan.Integrations, plan.Actions, plan.Blocked, plan.ProjectConfigVersion}
	data, _ := json.Marshal(canonical)
	return digest(data)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func actionsFor(integrations []string) []Action {
	base := map[string]string{
		IntegrationAgents: "AGENTS.md", IntegrationGeneric: ".mcp.json", IntegrationCursor: ".cursor/mcp.json",
		IntegrationVSCode: ".vscode/mcp.json", IntegrationOMP: ".omp/mcp.json", IntegrationFactory: ".factory/mcp.json",
		IntegrationOpenCode: "opencode.json", IntegrationNeovim: ".prowl/editor/nvim.lua", IntegrationHelix: ".helix/languages.toml",
	}
	skillRoots := map[string]string{
		IntegrationOMP:         ".omp/skills",
		IntegrationClaude:      ".claude/skills",
		IntegrationCursor:      ".cursor/skills",
		IntegrationOpenCode:    ".opencode/skills",
		IntegrationAgentSkills: ".agents/skills",
	}
	actions := make([]Action, 0, len(integrations))
	for _, integration := range integrations {
		if path, ok := base[integration]; ok {
			actions = append(actions, Action{Integration: integration, Path: path, Description: "merge Prowl integration without replacing existing settings"})
		}
		if dir, ok := skillRoots[integration]; ok {
			actions = append(actions, skillActions(dir)...)
			actions = append(actions, legacySkillActions(dir)...)
		}
		if integration == IntegrationOMP {
			actions = append(actions, Action{Integration: integrationRules, Path: ".omp/RULES.md", Description: "install prowl sticky rule (re-injected each turn)"})
			actions = append(actions, Action{Integration: integrationAgent, Path: ".omp/agents/scout.md", Description: "install prowl-aware scout subagent"})
		}
	}
	return actions
}

// skillActions emits one install Action per embedded skill under dir. The order
// follows skills.All (sorted by name) so plans stay deterministic.
func skillActions(dir string) []Action {
	all := skills.All()
	actions := make([]Action, 0, len(all))
	for _, skill := range all {
		actions = append(actions, Action{
			Integration: integrationSkill,
			Path:        dir + "/" + skill.Name + "/SKILL.md",
			Description: "install prowl agent skill",
		})
	}
	return actions
}

// legacySkillNames lists retired skills that setup migrates off the install
// path. prowl-repo-exploration was superseded by the CLI-first code-search skill.
var legacySkillNames = []string{"prowl-repo-exploration"}

// legacySkillActions emits a removal Action for each retired skill under dir, so
// planning can migrate an old Prowl-owned copy off the install path. Whether a
// copy is actually removed, preserved, or absent is decided during planning
// against the real file bytes (see planLegacyRemoval); actionsFor cannot read
// the filesystem, so it emits the candidates unconditionally.
func legacySkillActions(dir string) []Action {
	actions := make([]Action, 0, len(legacySkillNames))
	for _, name := range legacySkillNames {
		actions = append(actions, Action{
			Integration: integrationLegacySkill,
			Path:        dir + "/" + name + "/SKILL.md",
			Description: "remove superseded prowl agent skill",
		})
	}
	return actions
}

// planLegacyRemoval decides how a retired skill copy at action.Path is handled.
// A missing file is nothing to migrate (omitted). An exact Prowl-owned legacy
// body is safe to remove (kept as an action). Anything else -- a locally edited
// copy, a symlink, or a path that cannot be verified -- is preserved and
// reported as blocked, so setup never deletes user work.
func planLegacyRemoval(root *os.Root, action Action) (keep bool, blocked *BlockedAction, err error) {
	legacy, ok := skills.Legacy(skillNameFromPath(action.Path))
	if !ok {
		return false, nil, nil
	}
	data, readErr := readRootFile(root, action.Path)
	switch {
	case os.IsNotExist(readErr):
		return false, nil, nil
	case errors.Is(readErr, errSymlinkDestination):
		return false, &BlockedAction{Integration: action.Integration, Path: action.Path, Reason: readErr.Error()}, nil
	case readErr != nil:
		return false, &BlockedAction{Integration: action.Integration, Path: action.Path, Reason: "cannot verify retired skill ownership; left in place"}, nil
	case string(data) == legacy.Content:
		return true, nil, nil
	default:
		return false, &BlockedAction{Integration: action.Integration, Path: action.Path, Reason: "locally modified; left in place"}, nil
	}
}

// removeLegacySkill deletes a retired skill copy only when its bytes still match
// the exact body Prowl once installed, so a user edit is never destroyed.
func removeLegacySkill(root *os.Root, rel string) error {
	legacy, ok := skills.Legacy(skillNameFromPath(rel))
	if !ok {
		return nil
	}
	return removeOwnedFile(root, rel, legacy.Content)
}

// skillContent returns the embedded body for the named skill.
func skillContent(name string) (string, bool) {
	for _, skill := range skills.All() {
		if skill.Name == name {
			return skill.Content, true
		}
	}
	return "", false
}

// skillNameFromPath recovers a skill name from its install path
// (".../skills/<name>/SKILL.md").
func skillNameFromPath(rel string) string {
	return filepath.Base(filepath.Dir(rel))
}

// installSkill writes one embedded skill body to its install path. Skills are
// fully prowl-owned, so a whole-file overwrite is correct.
func installSkill(root *os.Root, rel string) error {
	content, ok := skillContent(skillNameFromPath(rel))
	if !ok {
		return errors.New("unknown prowl skill")
	}
	return writeRootFile(root, rel, []byte(content), 0o644)
}

// removeSkill deletes a prowl-owned skill file, refusing when a user edited it.
func removeSkill(root *os.Root, rel string) error {
	content, ok := skillContent(skillNameFromPath(rel))
	if !ok {
		return nil
	}
	return removeOwnedFile(root, rel, content)
}

func (service *Service) applyActionsInRoot(root *os.Root, actions []Action, pending *transaction) error {
	for _, action := range actions {
		if err := service.ensureParentDirectories(root, action.Path, pending); err != nil {
			return err
		}
		switch action.Integration {
		case IntegrationAgents:
			if err := ensureAgentsBlock(root, action.Path); err != nil {
				return err
			}
		case IntegrationGeneric, IntegrationCursor, IntegrationOMP, IntegrationFactory:
			if err := mergeMCPConfig(root, action.Path, "mcpServers"); err != nil {
				return err
			}
		case IntegrationVSCode:
			if err := mergeMCPConfig(root, action.Path, "servers"); err != nil {
				return err
			}
		case IntegrationOpenCode:
			if err := mergeOpenCode(root, action.Path); err != nil {
				return err
			}
		case IntegrationNeovim:
			if err := injectNeovim(root, action.Path); err != nil {
				return err
			}
		case IntegrationHelix:
			if err := injectHelix(root, action.Path); err != nil {
				return err
			}
		case integrationSkill:
			if err := installSkill(root, action.Path); err != nil {
				return err
			}
		case integrationRules:
			if err := ensureRulesBlock(root, action.Path); err != nil {
				return err
			}
		case integrationAgent:
			if err := writeRootFile(root, action.Path, []byte(ompScoutAgent), 0o644); err != nil {
				return err
			}
		case integrationLegacySkill:
			if err := removeLegacySkill(root, action.Path); err != nil {
				return err
			}
		default:
			return errors.New("unknown setup integration")
		}
	}
	return nil
}

type snapshot struct {
	rel     string
	data    []byte
	mode    os.FileMode
	existed bool
}

func snapshotsInRoot(root *os.Root, actions []Action, extra ...string) ([]snapshot, []transactionDirectory, error) {
	paths := make([]string, 0, len(actions)+len(extra))
	for _, action := range actions {
		paths = append(paths, action.Path)
	}
	paths = append(paths, extra...)
	seen := make(map[string]struct{}, len(paths))
	snapshots := make([]snapshot, 0, len(paths))
	for _, rel := range paths {
		clean, err := validateRootPath(root, rel)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		info, err := root.Stat(clean)
		if os.IsNotExist(err) {
			snapshots = append(snapshots, snapshot{rel: clean})
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		data, err := root.ReadFile(clean)
		if err != nil {
			return nil, nil, err
		}
		snapshots = append(snapshots, snapshot{rel: clean, data: data, mode: info.Mode(), existed: true})
	}
	return snapshots, transactionDirectories(paths), nil
}

func manifest(snapshots []snapshot) []RollbackItem {
	out := make([]RollbackItem, 0, len(snapshots))
	for _, item := range snapshots {
		if item.rel == replayPath {
			continue
		}
		out = append(out, RollbackItem{Path: item.rel, Existed: item.existed})
	}
	return out
}

func transactionSnapshots(snapshots []snapshot) []transactionSnapshot {
	out := make([]transactionSnapshot, 0, len(snapshots))
	for _, item := range snapshots {
		out = append(out, transactionSnapshot{
			Path: item.rel, Data: append([]byte(nil), item.data...), Mode: uint32(item.mode), Existed: item.existed,
		})
	}
	return out
}

func transactionDirectories(paths []string) []transactionDirectory {
	seen := map[string]struct{}{}
	var directories []transactionDirectory
	for _, rel := range paths {
		for dir := filepath.Dir(filepath.FromSlash(rel)); dir != "."; dir = filepath.Dir(dir) {
			clean := filepath.ToSlash(dir)
			if _, ok := seen[clean]; ok {
				continue
			}
			seen[clean] = struct{}{}
			directories = append(directories, transactionDirectory{Path: clean})
		}
	}
	sort.Slice(directories, func(left, right int) bool {
		return directories[left].Path < directories[right].Path
	})
	return directories
}

func (service *Service) ensureParentDirectories(root *os.Root, rel string, pending *transaction) error {
	clean, err := validateRootPath(root, rel)
	if err != nil {
		return err
	}
	dir := filepath.Dir(clean)
	current := ""
	for _, component := range strings.Split(filepath.ToSlash(dir), "/") {
		if component == "." || component == "" {
			continue
		}
		if current == "" {
			current = component
		} else {
			current = filepath.ToSlash(filepath.Join(current, component))
		}
		info, err := root.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("setup destination must be a directory")
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := root.Mkdir(current, 0o755); err != nil {
			if !os.IsExist(err) {
				return err
			}
			info, err := root.Lstat(current)
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("setup destination must be a directory")
			}
			continue
		}
		if pending == nil {
			continue
		}
		for index := range pending.Directories {
			if pending.Directories[index].Path == current {
				pending.Directories[index].Created = true
				break
			}
		}
		if err := saveTransactionInRoot(root, *pending); err != nil {
			return err
		}
	}
	return nil
}

func restoreInRoot(root *os.Root, snapshots []transactionSnapshot, journalPath string) error {
	journal, err := validateRootPath(root, journalPath)
	if err != nil {
		return err
	}
	for _, before := range snapshots {
		clean, err := validateRootPath(root, before.Path)
		if err != nil {
			return err
		}
		if clean == journal {
			continue
		}
		if before.Existed {
			if err := writeAtomicInRoot(root, clean, before.Data, os.FileMode(before.Mode).Perm()); err != nil {
				return err
			}
			continue
		}
		if err := root.Remove(clean); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (service *Service) rollbackTransaction(pending transaction) error {
	root, err := os.OpenRoot(service.root)
	if err != nil {
		return err
	}
	defer root.Close()
	return service.rollbackTransactionInRoot(root, pending)
}

func (service *Service) rollbackTransactionInRoot(root *os.Root, pending transaction) error {
	if err := restoreInRoot(root, pending.Snapshots, journalPathFor(pending)); err != nil {
		return err
	}
	journalDirectory := filepath.ToSlash(filepath.Dir(journalPathFor(pending)))
	directories := append([]transactionDirectory(nil), pending.Directories...)
	sort.Slice(directories, func(left, right int) bool {
		return strings.Count(directories[left].Path, "/") > strings.Count(directories[right].Path, "/")
	})
	for _, directory := range directories {
		if !directory.Created || directory.Path == journalDirectory {
			continue
		}
		clean, err := validateRootPath(root, directory.Path)
		if err != nil {
			return err
		}
		if err := root.Remove(clean); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return removeTransactionInRoot(root, journalPathFor(pending))
}

func (service *Service) recoverInterruptedTransactionInRoot(root *os.Root) error {
	pending, found, err := loadTransactionInRoot(root)
	if err != nil || !found {
		return err
	}
	replays, err := loadReplaysInRoot(root)
	if err != nil {
		return err
	}
	if replay, ok := replays[pending.Request.IdempotencyKey]; ok && sameRequest(replay.Request, pending.Request) && replay.Outcome.Verified {
		return removeTransactionInRoot(root, journalPathFor(pending))
	}
	if err := service.rollbackTransactionInRoot(root, pending); err != nil {
		return err
	}
	return ErrRecoveryRequired
}

func (service *Service) loadTransaction() (transaction, bool, error) {
	root, err := os.OpenRoot(service.root)
	if err != nil {
		return transaction{}, false, err
	}
	defer root.Close()
	return loadTransactionInRoot(root)
}

func loadTransactionInRoot(root *os.Root) (transaction, bool, error) {
	for _, path := range []string{transactionPath, legacyTransactionPath} {
		data, err := readRootFile(root, path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return transaction{}, false, err
		}
		var pending transaction
		if err := json.Unmarshal(data, &pending); err != nil {
			return transaction{}, false, errors.New("invalid setup transaction")
		}
		if pending.SchemaVersion != transactionSchema || len(pending.Snapshots) == 0 {
			return transaction{}, false, errors.New("invalid setup transaction")
		}
		pending.journalPath = path
		if path == legacyTransactionPath {
			for index := range pending.Directories {
				if pending.Directories[index].LegacyExisted != nil {
					pending.Directories[index].Created = !*pending.Directories[index].LegacyExisted
				}
			}
		}
		return pending, true, nil
	}
	return transaction{}, false, nil
}

func (service *Service) saveTransaction(pending transaction) error {
	root, err := os.OpenRoot(service.root)
	if err != nil {
		return err
	}
	defer root.Close()
	return saveTransactionInRoot(root, pending)
}

func saveTransactionInRoot(root *os.Root, pending transaction) error {
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	return writeAtomicInRoot(root, journalPathFor(pending), append(data, '\n'), 0o600)
}

func journalPathFor(pending transaction) string {
	if pending.journalPath != "" {
		return pending.journalPath
	}
	return transactionPath
}

func removeTransactionInRoot(root *os.Root, path string) error {
	clean, err := validateRootPath(root, path)
	if err != nil {
		return err
	}
	if err := root.Remove(clean); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func loadReplaysInRoot(root *os.Root) (map[string]replayRecord, error) {
	data, err := readRootFile(root, replayPath)
	if os.IsNotExist(err) {
		return map[string]replayRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]replayRecord{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, errors.New("invalid setup replay state")
	}
	return out, nil
}

func saveReplaysInRoot(root *os.Root, replays map[string]replayRecord) error {
	data, err := json.Marshal(replays)
	if err != nil {
		return err
	}
	return writeAtomicInRoot(root, replayPath, append(data, '\n'), 0o600)
}

func (service *Service) lock(ctx context.Context, root *os.Root) (func(), error) {
	clean, err := validateRootPath(root, setupLockPath)
	if err != nil {
		return nil, err
	}
	if err := root.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return nil, err
	}
	file, err := root.OpenFile(clean, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return lockSetupFile(ctx, file, setupLockTimeout)
}

func validateRootPath(root *os.Root, rel string) (string, error) {
	if !safeRelativePath(rel) {
		return "", errUnsafeActionPath
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	current := ""
	for _, component := range strings.Split(clean, "/") {
		if component == "." || component == "" {
			continue
		}
		if current == "" {
			current = component
		} else {
			current = filepath.ToSlash(filepath.Join(current, component))
		}
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s %w", current, errSymlinkDestination)
		}
	}
	return clean, nil
}

func readRootFile(root *os.Root, rel string) ([]byte, error) {
	clean, err := validateRootPath(root, rel)
	if err != nil {
		return nil, err
	}
	return root.ReadFile(clean)
}

func writeAtomicInRoot(root *os.Root, rel string, data []byte, mode os.FileMode) error {
	clean, err := validateRootPath(root, rel)
	if err != nil {
		return err
	}
	dir := filepath.Dir(clean)
	var random [12]byte
	if _, err := cryptorand.Read(random[:]); err != nil {
		return err
	}
	tmp := filepath.ToSlash(filepath.Join(dir, ".prowl-write-"+hex.EncodeToString(random[:])))
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if _, err := validateRootPath(root, clean); err != nil {
		return err
	}
	if err := root.Rename(tmp, clean); err != nil {
		return err
	}
	directory, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return syncSetupDirectory(directory)
}

func DetectIntegrations(root string) []string {
	checks := []struct{ name, path string }{
		{IntegrationAgents, "AGENTS.md"}, {IntegrationGeneric, ".mcp.json"}, {IntegrationCursor, ".cursor"},
		{IntegrationVSCode, ".vscode"}, {IntegrationOMP, ".omp"}, {IntegrationFactory, ".factory"},
		{IntegrationOpenCode, "opencode.json"}, {IntegrationNeovim, ".nvim"}, {IntegrationHelix, ".helix"}, {IntegrationClaude, ".claude"},
		{IntegrationAgentSkills, ".agents"},
	}
	out := make([]string, 0, len(checks))
	for _, check := range checks {
		if _, err := os.Stat(filepath.Join(root, check.path)); err == nil {
			out = append(out, check.name)
		}
	}
	sort.Strings(out)
	return out
}

func ParseIntegrationSelection(value string, detected []string) ([]string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "auto" {
		// AGENTS.md is the client-agnostic guidance almost every coding agent
		// reads; the client-agnostic `.mcp.json` registers Prowl's MCP server so
		// the tools appear in the agent's own tool list. Guidance an agent never
		// sees cannot make it reach for Prowl, and prose alone is unreliable for
		// models with strong native grep/read priors (notably Anthropic's), which
		// under-weight an instruction to shell out to an unfamiliar CLI -- a
		// visible tool is what actually gets called. Always include both in auto,
		// even on a repo that has neither yet, so a bare `init` both tells agents
		// to prefer Prowl and gives them the tools to do it. Both are reversible
		// (`--remove-integrations`): the AGENTS.md block is marker-bounded and the
		// `.mcp.json` entry is a single server key.
		return NormalizeIntegrations(append(append([]string(nil), detected...), IntegrationAgents, IntegrationGeneric))
	}
	if value == "none" {
		return []string{}, nil
	}
	if value == "all" {
		return append([]string(nil), allIntegrations...), nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		switch parts[i] {
		case "mcp", "generic":
			parts[i] = IntegrationGeneric
		case "oh-my-pi":
			parts[i] = IntegrationOMP
		case "vs-code":
			parts[i] = IntegrationVSCode
		}
	}
	return NormalizeIntegrations(parts)
}

func NormalizeIntegrations(values []string) ([]string, error) {
	names := knownIntegrations()
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[name] = true
	}
	set := map[string]bool{}
	for _, name := range values {
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, fmt.Errorf("unknown integration %q", name)
		}
		set[name] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func RemoveIntegrations(root string, integrations []string) error {
	service, err := NewService(root)
	if err != nil {
		return err
	}
	return service.removeIntegrations(integrations)
}

func (service *Service) removeIntegrations(integrations []string) error {
	integrations, err := NormalizeIntegrations(integrations)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(service.root)
	if err != nil {
		return safeError(err)
	}
	defer root.Close()
	for _, action := range actionsFor(integrations) {
		switch action.Integration {
		case IntegrationAgents:
			err = removeAgentsBlock(root, action.Path)
		case IntegrationGeneric, IntegrationCursor, IntegrationOMP, IntegrationFactory:
			err = removeMCPConfig(root, action.Path, "mcpServers")
		case IntegrationVSCode:
			err = removeMCPConfig(root, action.Path, "servers")
		case IntegrationOpenCode:
			err = removeOpenCode(root, action.Path)
		case IntegrationNeovim:
			err = removeOwnedFile(root, action.Path, nvimConfig, nvimConfigLegacy)
		case IntegrationHelix:
			err = removeOwnedFile(root, action.Path, helixConfig(), helixConfigLegacy())
		case integrationSkill:
			err = removeSkill(root, action.Path)
		case integrationRules:
			err = removeAgentsBlock(root, action.Path)
		case integrationAgent:
			err = removeOwnedFile(root, action.Path, ompScoutAgent)
		case integrationLegacySkill:
			err = removeLegacySkill(root, action.Path)
		}
		if err != nil {
			return safeError(err)
		}
	}
	return nil
}

func (service *Service) applyReviewedPlan(ctx context.Context, plan Plan, idempotencyKey string) error {
	_, err := service.Apply(ctx, ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion, Approved: true, IdempotencyKey: idempotencyKey,
	})
	return err
}

func Inject(root string) error {
	service, err := NewService(root)
	if err != nil {
		return err
	}
	plan, err := service.Plan(context.Background(), []string{
		IntegrationAgents, IntegrationGeneric, IntegrationCursor, IntegrationVSCode,
		IntegrationOMP, IntegrationFactory, IntegrationOpenCode,
	})
	if err != nil {
		return err
	}
	return service.applyReviewedPlan(context.Background(), plan, "inject:"+plan.Hash)
}

func InjectEditor(root string) error {
	service, err := NewService(root)
	if err != nil {
		return err
	}
	plan, err := service.Plan(context.Background(), []string{IntegrationNeovim, IntegrationHelix})
	if err != nil {
		return err
	}
	return service.applyReviewedPlan(context.Background(), plan, "inject-editor:"+plan.Hash)
}

const agentsMarker = "<!-- prowl-agent -->"
const agentsEndMarker = "<!-- /prowl-agent -->"

const AgentsMarker = agentsMarker
const AgentsEndMarker = agentsEndMarker

// The map block is written by the CLI (overview/init) but removed here, so its
// markers are canonical in the setup package.
const AgentsMapMarker = "<!-- prowl-agent:map -->"
const AgentsMapEndMarker = "<!-- /prowl-agent:map -->"
const agentsBlock = agentsMarker + `
## Prowl project context

This repo has a Prowl index of its files, symbols, and how they connect. For any
semantic or structural question -- where code is, what it does, who calls it, or
what a change touches -- **run the read-only prowl CLI first**; do not grep or
read whole files just to locate things. Prowl reindexes what changed before each
query, so answers stay current and are cited to file:line, returned in one call
instead of a grep hit list you then open files to disambiguate.

| Question | First command |
|---|---|
| Map the repository | ` + "`prowl overview`" + ` |
| Locate a feature or concept | ` + "`prowl search \"<question>\"`" + ` |
| Locate a named symbol | ` + "`prowl find <name>`" + ` |
| Read one symbol's source | ` + "`prowl def <name-or-id>`" + ` |
| Inspect a file's structure | ` + "`prowl outline <path>`" + ` |
| Trace who uses a symbol | ` + "`prowl references <name-or-id>`" + ` |
| Size a change's blast radius | ` + "`prowl impact <path>`" + ` |
| Inspect uncommitted work | ` + "`prowl wip`" + ` / ` + "`prowl changed`" + ` |
| Read a located line range | ` + "`prowl peek <file:start-end>`" + ` |

Keep grep for exact literal or regex text and glob for filename patterns. CLI
output is token-lean TOON by default; add --format human|toon|json|markdown. If
your harness also wires Prowl as an MCP server, the same index is reachable
there; the CLI needs no server and is the first choice.
<!-- /prowl-agent -->`

// rulesBlock is Prowl's sticky rule, installed as .omp/RULES.md for the omp
// integration. omp re-injects RULES.md near every turn (unlike AGENTS.md, which
// drifts up the transcript), so this keeps the "reach for Prowl before grep" hold
// even in long sessions. It shares the AGENTS.md markers so the same ensure/strip
// helpers manage it and it merges with any user-authored RULES.md.
const rulesBlock = agentsMarker + `
Prowl indexes this repo. For any semantic or structural question -- where a
symbol, setting, or component is defined, who calls or imports it, how a feature
works, or a change's blast radius -- run the read-only Prowl CLI first:
` + "`prowl overview`" + `, ` + "`prowl find <name>`" + `, ` + "`prowl def <name-or-id>`" + `,
` + "`prowl outline <path>`" + `, ` + "`prowl references <name-or-id>`" + `, ` + "`prowl impact <path>`" + `,
` + "`prowl search \"<question>\"`" + `, or ` + "`prowl peek <file:start-end>`" + `. It answers
from a cited index (reindexed before each query) in one call. Use grep only for
exact literal or regex text and glob only for filename patterns.
` + agentsEndMarker

type mcpServer struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func mergeMCPConfig(root *os.Root, rel, key string) error {
	doc := map[string]any{}
	if data, err := readRootFile(root, rel); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return errors.New("invalid existing setup JSON")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	servers, _ := doc[key].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	delete(servers, "prowl-agent")
	servers["prowl"] = mcpServer{Type: "stdio", Command: "prowl", Args: []string{"serve", "--mcp-surface", "core"}}
	doc[key] = servers
	return writeJSON(root, rel, doc)
}

func mergeOpenCode(root *os.Root, rel string) error {
	doc := map[string]any{}
	if data, err := readRootFile(root, rel); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return errors.New("invalid existing setup JSON")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, ok := doc["$schema"]; !ok {
		doc["$schema"] = "https://opencode.ai/config.json"
	}
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	delete(mcp, "prowl-agent")
	mcp["prowl"] = map[string]any{"type": "local", "command": []string{"prowl", "serve", "--mcp-surface", "core"}, "enabled": true}
	doc["mcp"] = mcp
	return writeJSON(root, rel, doc)
}

func ensureAgentsBlock(root *os.Root, rel string) error {
	return ensureMarkedBlock(root, rel, agentsBlock)
}

// ensureRulesBlock installs Prowl's sticky rule into rel (.omp/RULES.md).
func ensureRulesBlock(root *os.Root, rel string) error {
	return ensureMarkedBlock(root, rel, rulesBlock)
}

// ensureMarkedBlock writes block into rel, replacing an existing prowl-marked
// region in place (so a user's surrounding content survives) or appending it.
func ensureMarkedBlock(root *os.Root, rel, block string) error {
	data, err := readRootFile(root, rel)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(data)
	if start := strings.Index(content, agentsMarker); start >= 0 {
		end := len(content)
		if offset := strings.Index(content[start:], agentsEndMarker); offset >= 0 {
			end = start + offset + len(agentsEndMarker)
		} else if offset := strings.IndexByte(content[start:], '\n'); offset >= 0 {
			end = start + offset
		}
		updated := content[:start] + block + content[end:]
		if updated == content {
			return nil
		}
		return writeRootFile(root, rel, []byte(updated), 0o644)
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if content != "" {
		content += "\n"
	}
	return writeRootFile(root, rel, []byte(content+block+"\n"), 0o644)
}

// EnsureAgentsBlock updates only Prowl's marked block in an explicit file.
func EnsureAgentsBlock(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	defer root.Close()
	return ensureAgentsBlock(root, filepath.Base(absolute))
}

func removeMCPConfig(root *os.Root, rel, key string) error {
	data, err := readRootFile(root, rel)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return errors.New("invalid existing setup JSON")
	}
	servers, _ := doc[key].(map[string]any)
	if servers == nil {
		return nil
	}
	if _, current := servers["prowl"]; !current {
		if _, legacy := servers["prowl-agent"]; !legacy {
			return nil
		}
	}
	delete(servers, "prowl")
	delete(servers, "prowl-agent")
	doc[key] = servers
	return writeJSON(root, rel, doc)
}

func removeOpenCode(root *os.Root, rel string) error {
	data, err := readRootFile(root, rel)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return errors.New("invalid existing setup JSON")
	}
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		return nil
	}
	delete(mcp, "prowl")
	delete(mcp, "prowl-agent")
	doc["mcp"] = mcp
	return writeJSON(root, rel, doc)
}

func writeJSON(root *os.Root, rel string, doc map[string]any) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeRootFile(root, rel, append(out, '\n'), 0o644)
}

func writeRootFile(root *os.Root, rel string, data []byte, fallback os.FileMode) error {
	clean, err := validateRootPath(root, rel)
	if err != nil {
		return err
	}
	mode := fallback
	if info, err := root.Stat(clean); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeAtomicInRoot(root, clean, data, mode)
}

func removeAgentsBlock(root *os.Root, rel string) error {
	data, err := readRootFile(root, rel)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	content := string(data)
	if !strings.Contains(content, agentsMarker) && !strings.Contains(content, AgentsMapMarker) {
		return nil
	}
	content = stripMarkedRegion(content, agentsMarker, agentsEndMarker)
	content = stripMarkedRegion(content, AgentsMapMarker, AgentsMapEndMarker)
	updated := strings.TrimSpace(content)
	if updated == "" {
		clean, err := validateRootPath(root, rel)
		if err != nil {
			return err
		}
		return root.Remove(clean)
	}
	return writeRootFile(root, rel, []byte(updated+"\n"), 0o644)
}

// stripMarkedRegion removes every start..end region (inclusive) from content.
func stripMarkedRegion(content, start, end string) string {
	for {
		s := strings.Index(content, start)
		if s < 0 {
			return content
		}
		off := strings.Index(content[s:], end)
		if off < 0 {
			return content
		}
		content = content[:s] + content[s+off+len(end):]
	}
}

// removeOwnedFile deletes a setup destination only when its bytes exactly match
// one of the Prowl-owned bodies passed in -- the current body plus any retired
// generated body from an earlier release. Matching an explicit, bounded set
// (never a substring or a loose "contains prowl" test) means removal recognizes
// a legacy generated config without ever accepting an arbitrary user file.
func removeOwnedFile(root *os.Root, rel string, expected ...string) error {
	data, err := readRootFile(root, rel)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	owned := false
	for _, want := range expected {
		if string(data) == want {
			owned = true
			break
		}
	}
	if !owned {
		return errors.New("refusing to remove modified setup file")
	}
	clean, err := validateRootPath(root, rel)
	if err != nil {
		return err
	}
	return root.Remove(clean)
}

const nvimConfig = `-- Prowl language server (Neovim 0.11+).
-- Source this file from your config: dofile(vim.fn.getcwd() .. "/.prowl/editor/nvim.lua")
-- or copy the block into your own config.
vim.lsp.config("prowl", {
  cmd = { "prowl", "lsp" },
  filetypes = {
    "conf", "config", "dosini", "toml", "yaml", "json", "jsonc",
    "css", "scss", "lua", "python", "sh", "bash", "fish", "qml", "hyprlang",
  },
  root_markers = { ".prowl", ".git" },
})
vim.lsp.enable("prowl")
`

// nvimConfigLegacy is the exact Neovim body earlier releases generated under the
// retired prowl-agent product name. It is a frozen artifact kept only so re-setup
// can migrate it to nvimConfig and removal can recognize it as Prowl-owned.
const nvimConfigLegacy = `-- prowl-agent language server (Neovim 0.11+).
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

func helixConfig() string {
	langs := []string{"hyprlang", "toml", "ini", "css", "scss", "json", "yaml", "qml"}
	var b strings.Builder
	b.WriteString("[language-server.prowl]\ncommand = \"prowl\"\nargs = [\"lsp\"]\n\n# Helix replaces the per-language server list, so add your existing servers\n# back to any language below if you rely on them.\n")
	for _, lang := range langs {
		fmt.Fprintf(&b, "\n[[language]]\nname = \"%s\"\nlanguage-servers = [\"prowl\"]\n", lang)
	}
	return b.String()
}

// helixConfigLegacy is the exact Helix body earlier releases generated under the
// retired prowl-agent product name (its language-server id and command were both
// prowl-agent). Kept only so re-setup can migrate it to helixConfig and removal
// can recognize it as Prowl-owned.
func helixConfigLegacy() string {
	langs := []string{"hyprlang", "toml", "ini", "css", "scss", "json", "yaml", "qml"}
	var b strings.Builder
	b.WriteString("[language-server.prowl-agent]\ncommand = \"prowl-agent\"\nargs = [\"lsp\"]\n\n# Helix replaces the per-language server list, so add your existing servers\n# back to any language below if you rely on them.\n")
	for _, lang := range langs {
		fmt.Fprintf(&b, "\n[[language]]\nname = \"%s\"\nlanguage-servers = [\"prowl-agent\"]\n", lang)
	}
	return b.String()
}

// injectNeovim owns .prowl/editor/nvim.lua outright (it lives inside Prowl's own
// workspace dir), so it rewrites the current body unconditionally -- that both
// installs it and migrates any legacy prowl-agent body to the prowl invocation.
func injectNeovim(root *os.Root, rel string) error {
	return writeRootFile(root, rel, []byte(nvimConfig), 0o644)
}

// injectHelix writes .helix/languages.toml, a file shared with the user's own
// Helix config. It creates the current body when absent and migrates a
// Prowl-generated legacy body (still invoking the retired prowl-agent binary) to
// the current prowl invocation, but never overwrites a foreign config -- so a
// stale generated config is not left silently invoking a binary that no longer
// exists, and a hand-written config is left untouched.
func injectHelix(root *os.Root, rel string) error {
	data, err := readRootFile(root, rel)
	switch {
	case os.IsNotExist(err):
		return writeRootFile(root, rel, []byte(helixConfig()), 0o644)
	case err != nil:
		return err
	}
	switch string(data) {
	case helixConfig():
		return nil // already the current body
	case helixConfigLegacy():
		return writeRootFile(root, rel, []byte(helixConfig()), 0o644)
	default:
		return nil // a foreign Helix config: never overwrite the user's own file
	}
}

func safeRelativePath(value string) bool {
	return value != "" && filepath.IsLocal(filepath.FromSlash(value)) && !filepath.IsAbs(value) && !strings.Contains(value, "\\")
}

// safeError keeps a setup failure diagnosable without leaking the project root,
// file contents, or OS internals. Prowl's own reasons are already
// project-relative, so they pass through; anything else is reduced to its
// classification, which is the part a caller can actually act on. Collapsing
// every failure to one opaque string, as this once did, left `init` reporting
// nothing a user or agent could use.
func safeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errSymlinkDestination), errors.Is(err, errUnsafeActionPath):
		return err
	case errors.Is(err, fs.ErrPermission):
		return errors.New("setup operation failed: permission denied")
	case errors.Is(err, fs.ErrExist):
		return errors.New("setup operation failed: destination already exists")
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("setup operation failed: a required path is missing")
	case errors.Is(err, fs.ErrInvalid):
		return errors.New("setup operation failed: invalid path")
	}
	return errors.New("setup operation failed")
}
