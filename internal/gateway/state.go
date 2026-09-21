package gateway

import (
	"fmt"
	"os"
	"path/filepath"
)

// Dir returns the gateway's state directory:
// $XDG_DATA_HOME/prowl-agent/gateway (or ~/.local/share/... ). It holds
// gateway.db, master.key, the local token, settings.json, the OAuth login
// vault and the request trail - everything at 0700, because another user on
// the machine has no business listing it, let alone reading it.
func Dir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			base = filepath.Join(home, ".local", "share")
		} else {
			base = ".prowl-agent-data"
		}
	}
	return filepath.Join(base, "prowl-agent", "gateway")
}

// EnsureLocalIdentity creates just the state a client needs to address the
// gateway: its directory and local token. It deliberately does not open the
// key store or the database, so an installer can register the provider
// without starting a gateway.
func EnsureLocalIdentity(dir string) (baseURL, token string, err error) {
	if dir == "" {
		dir = Dir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create gateway dir: %w", err)
	}
	token, err = loadOrCreateToken(dir)
	if err != nil {
		return "", "", err
	}
	return DefaultBaseURL(), token, nil
}

// DefaultBaseURL is where a gateway started with default flags listens.
func DefaultBaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", DefaultPort)
}
