package tui

import (
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/neur0map/prowl/internal/gateway"
)

const tutorialMarker = "console-tutorial-v1.seen"

func newToast(kind, text string) toastMsg {
	return toastMsg{Text: text, Kind: kind, Expires: time.Now().Add(8 * time.Second)}
}

func firstRunTutorial() tea.Cmd {
	return func() tea.Msg {
		path := filepath.Join(gateway.Dir(), tutorialMarker)
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, []byte("seen\n"), 0o600)
		return newToast("tip", "Welcome: use Tab / Shift+Tab to explore. Start with Projects; press ? for the full key guide.")
	}
}

func tipFor(tab Tab) string {
	switch tab {
	case TabProjects:
		return "Projects are local indexes. Run `prowl init` in any folder to add or refresh one."
	case TabProviders:
		return "Providers are service endpoints. Add an API key here, or connect a subscription under Accounts."
	case TabKeys:
		return "Credentials are individual keys. Disable one without deleting it; health and cooldowns affect routing."
	case TabLogins:
		return "Accounts connect through browser sign-in; compatible models are added to routing automatically."
	case TabRouting:
		return "Enter edits a model set, Space selects or activates, p chooses visible providers, and / searches everything."
	case TabUsage:
		return "Activity shows the provider, model, exact or estimated tokens, latency and result of every route."
	case TabToolkit:
		return "Toolkit maps every major Prowl function to a copyable command."
	case TabSetup:
		return "Setup writes only the selected harness integrations and records what it owns."
	default:
		return "Prowl combines local code intelligence with an optional smart model gateway."
	}
}
