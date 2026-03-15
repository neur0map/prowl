package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds global prowl settings stored at ~/.prowl/config.json.
type Config struct {
	GitHubToken    string   `json:"github_token,omitempty"`
	ModelChoice    string   `json:"model_choice,omitempty"`
	IgnorePatterns []string `json:"ignore_patterns,omitempty"`
}

// ConfigDir returns the global config directory (~/.prowl/).
func ConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".prowl")
}

func configPath() string {
	return filepath.Join(ConfigDir(), "config.json")
}

// Load reads ~/.prowl/config.json. Returns an empty Config if the file doesn't exist.
func Load() (*Config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Save writes the config to ~/.prowl/config.json.
func (c *Config) Save() error {
	if err := os.MkdirAll(ConfigDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0o644)
}
