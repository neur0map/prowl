package cli

import (
	"context"

	"github.com/prowl-agent/prowl-agent/internal/setup"
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


// SetupAction remains the CLI's compatibility spelling for setup.Action.
type SetupAction = setup.Action

// SetupPlan carries the CLI-local project root while the domain plan remains
// path-safe and root-free for workbench use.
type SetupPlan struct {
	Root string
	setup.Plan
}

func DetectIntegrations(root string) []string {
	return setup.DetectIntegrations(root)
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
	return service.ApplyPlan(context.Background(), plan.Plan)
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
