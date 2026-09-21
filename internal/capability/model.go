package capability

// Manifest describes a discoverable workflow without embedding its full prompt
// or tool schemas in every agent turn.
type Manifest struct {
	Name        string   `yaml:"name" json:"name"`
	Title       string   `yaml:"title" json:"title"`
	Description string   `yaml:"description" json:"description"`
	Triggers    []string `yaml:"triggers" json:"triggers"`
	Requires    []string `yaml:"requires" json:"requires"`
	Outputs     []string `yaml:"outputs" json:"outputs"`
	Privacy     string   `yaml:"privacy" json:"privacy"`
	ReadOnly    bool     `yaml:"read_only" json:"read_only"`
	Version     string   `yaml:"version" json:"version"`
	Prompts     []string `yaml:"prompts" json:"prompts"`
	Resources   []string `yaml:"resources" json:"resources"`
	Tools       []string `yaml:"tools" json:"tools"`
	// Commands are the prowl CLI recipes that deliver this workflow. The
	// CLI is the primary agent surface, so discovery leads with these; Tools /
	// Resources / Prompts are the MCP projection of the same capability.
	Commands []string `yaml:"commands" json:"commands"`
}

// Summary is the token-lean discovery projection.
type Summary struct {
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Privacy     string   `json:"privacy"`
	Version     string   `json:"version"`
	ReadOnly    bool     `json:"read_only"`
	Triggers    []string `json:"triggers"`
	Commands    []string `json:"commands,omitempty"`
}

// Summary projects a manifest to its token-lean discovery view.
func (m Manifest) Summary() Summary {
	return Summary{
		Name: m.Name, Title: m.Title, Description: m.Description,
		Privacy: m.Privacy, Version: m.Version, ReadOnly: m.ReadOnly,
		Triggers: m.Triggers, Commands: m.Commands,
	}
}
