package config

// CodexQuotaProbe controls the minimal probe sent when a Codex reset window is reached.
type CodexQuotaProbe struct {
	Model  string `yaml:"model" json:"model"`
	Prompt string `yaml:"prompt" json:"prompt"`
}
