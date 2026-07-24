package cli

import "github.com/prowl-agent/prowl-agent/internal/setup"

// InjectEditor writes editor integration through the setup domain.
func InjectEditor(root string) error {
	return setup.InjectEditor(root)
}
