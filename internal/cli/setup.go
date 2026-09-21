package cli

import (
	"context"

	"github.com/neur0map/prowl/internal/setup"
)

const (
	IntegrationAgents   = setup.IntegrationAgents
	IntegrationGeneric  = setup.IntegrationGeneric
	IntegrationCursor   = setup.IntegrationCursor
	IntegrationVSCode   = setup.IntegrationVSCode
	IntegrationOMP      = setup.IntegrationOMP
	IntegrationFactory  = setup.IntegrationFactory
	IntegrationOpenCode = setup.IntegrationOpenCode
	IntegrationNeovim   = setup.IntegrationNeovim
	IntegrationHelix    = setup.IntegrationHelix
)

var allIntegrations = setup.AllIntegrations()

// completeIntegrations is the full registry -- project-level integrations plus
// the user-level-only harnesses init folds in from DetectInstalledHarnesses --
// so the interactive picker can display and preserve a detected user-only
// harness rather than silently dropping a pre-selected value it never rendered.
var completeIntegrations = setup.KnownIntegrations()

// SetupAction remains the CLI's compatibility spelling for setup.Action.
type SetupAction = setup.Action

// SetupPlan carries the CLI-local project root while the domain plan remains
// path-safe and root-free for workbench use.
type SetupPlan struct {
	Root string `json:"root"`
	setup.Plan
}

func DetectIntegrations(root string) []string {
	return setup.DetectIntegrations(root)
}

// DetectInstalledHarnesses reports harness integrations installed on this
// machine (omp, claude) so init can wire their native integration even in a
// repo that has no config directory for them yet.
func DetectInstalledHarnesses() []string {
	return setup.DetectInstalledHarnesses()
}

func ParseIntegrationSelection(value string, detected []string) ([]string, error) {
	return setup.ParseIntegrationSelection(value, detected)
}

func BuildSetupPlan(root string, integrations []string) (SetupPlan, error) {
	service, err := setup.NewService(root)
	if err != nil {
		return SetupPlan{}, err
	}
	plan, err := service.Plan(context.Background(), integrations)
	if err != nil {
		return SetupPlan{}, err
	}
	return SetupPlan{Root: root, Plan: plan}, nil
}

func ApplySetupPlan(plan SetupPlan) error {
	service, err := setup.NewService(plan.Root)
	if err != nil {
		return err
	}
	_, err = service.Apply(context.Background(), setup.ApplyRequest{
		Integrations: plan.Integrations, PlanHash: plan.Hash,
		ExpectedProjectConfigVersion: plan.ProjectConfigVersion,
		Approved:                     true, IdempotencyKey: "cli:" + plan.Hash,
	})
	return err
}

func VerifySetupPlan(plan SetupPlan) error {
	service, err := setup.NewService(plan.Root)
	if err != nil {
		return err
	}
	return service.Verify(context.Background(), plan.Plan)
}

func RemoveIntegrations(root string, integrations []string) error {
	return setup.RemoveIntegrations(root, integrations)
}
