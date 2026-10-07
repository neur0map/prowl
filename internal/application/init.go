package application

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/setup"
	"github.com/neur0map/prowl/internal/workspace"
)

// InitOptions controls project initialization shared by the CLI and gateway.
type InitOptions struct {
	Root               string
	Tier               string
	AssistModel        string
	Provider           string
	AgentCommand       string
	Integrations       []string
	IntegrationsSet    bool
	Languages          []string
	LanguagesSet       bool
	EmbedProgress      func(embedded, remaining int)
	OnBlocked          func([]setup.BlockedAction)
	InferencerProvider InferencerProvider
	AfterSetup         func(*Project)
}

// InitializeProject creates the workspace, writes its config and rules, builds
// the first index, applies selected integrations, and registers the project.
func InitializeProject(ctx context.Context, opt InitOptions) (index.Summary, error) {
	root := opt.Root
	if root == "" {
		root, _ = os.Getwd()
	}
	ws, err := workspace.Create(root)
	if err != nil {
		return index.Summary{}, err
	}

	existed := false
	if _, statErr := os.Stat(filepath.Join(ws.Path, "config.toml")); statErr == nil {
		existed = true
	}

	cfg, err := config.Load(ws.Path)
	if err != nil {
		return index.Summary{}, fmt.Errorf("read existing config: %w", err)
	}
	global, _ := config.LoadGlobal()
	cfg.AI.Enabled = true

	tier := firstNonEmpty(opt.Tier, global.Tier, config.DefaultTier)
	switch {
	case opt.Tier != "":
		cfg.AI.AssistModel = config.PresetByName(opt.Tier).AssistModel
	case !existed:
		cfg.AI.AssistModel = firstNonEmpty(global.AssistModel, config.PresetByName(tier).AssistModel)
	}
	if opt.AssistModel != "" {
		cfg.AI.AssistModel = opt.AssistModel
	}
	if opt.Provider != "" {
		cfg.AI.Provider = opt.Provider
	}
	if opt.AgentCommand != "" {
		cfg.AI.AgentCommand = opt.AgentCommand
	}
	if opt.LanguagesSet {
		cfg.Languages = opt.Languages
	}

	if err := config.Save(ws.Path, cfg); err != nil {
		return index.Summary{}, err
	}
	if opt.Tier != "" || !existed {
		_ = config.SaveGlobal(config.GlobalConfig{
			AIEnabled:   true,
			Tier:        tier,
			AssistModel: cfg.AI.AssistModel,
		})
	}
	if _, statErr := os.Stat(filepath.Join(ws.Path, "rules.toml")); os.IsNotExist(statErr) {
		if err := config.SaveRules(ws.Path, config.DefaultRules()); err != nil {
			return index.Summary{}, err
		}
	}

	project, err := OpenProject(ctx, root, Options{
		EnableAI: cfg.AI.Enabled, InferencerProvider: opt.InferencerProvider,
	})
	if err != nil {
		return index.Summary{}, err
	}
	defer project.Close()

	summary := project.InitialRefresh.Summary
	if summary.Indexed == 0 {
		if status, statusErr := project.Query.Status(); statusErr == nil {
			summary.Indexed = status.Counts.Files
			summary.Symbols = status.Counts.Symbols
			summary.Edges = status.Counts.Edges
		}
	}
	var progress func(index.VectorPass)
	if opt.EmbedProgress != nil {
		progress = func(pass index.VectorPass) {
			opt.EmbedProgress(pass.Embedded, pass.Remaining)
		}
	}
	if _, embedErr := project.BuildSemanticIndex(ctx, progress); embedErr != nil {
		return summary, embedErr
	}

	integrations := setup.AllIntegrations()
	if opt.IntegrationsSet {
		integrations = append([]string(nil), opt.Integrations...)
	}
	service, err := setup.NewService(root)
	if err != nil {
		return summary, err
	}
	plan, err := service.Plan(ctx, integrations)
	if err != nil {
		return summary, err
	}
	if opt.OnBlocked != nil && len(plan.Blocked) > 0 {
		opt.OnBlocked(plan.Blocked)
	}
	if _, err := service.Apply(ctx, setup.ApplyRequest{
		Integrations:                 plan.Integrations,
		PlanHash:                     plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion,
		Approved:                     true,
		IdempotencyKey:               "cli:" + plan.Hash,
	}); err != nil {
		return summary, err
	}
	if opt.AfterSetup != nil {
		opt.AfterSetup(project)
	}
	if err := workspace.EnsureDerivedIgnored(root); err != nil {
		return summary, err
	}
	if err := workspace.Register(root, true); err != nil {
		return summary, err
	}
	return summary, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
