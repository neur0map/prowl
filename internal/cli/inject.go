package cli

import "github.com/prowl-agent/prowl-agent/internal/setup"

// Inject writes MCP server configs and AGENTS.md through the setup domain.
func Inject(root string) error {
	return setup.Inject(root)
}
