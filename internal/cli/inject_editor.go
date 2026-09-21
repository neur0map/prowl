package cli

import "github.com/neur0map/prowl/internal/setup"

// InjectEditor writes editor integration through the setup domain.
func InjectEditor(root string) error {
	return setup.InjectEditor(root)
}
