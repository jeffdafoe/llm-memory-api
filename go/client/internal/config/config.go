// Package config reads .agent.json files for authentication credentials
// and API configuration. The agent config file is the shared credential
// store used by all client tools (memory-sync, discuss, etc.).
package config

import (
    "encoding/json"
    "fmt"
    "os"
)

// AgentConfig holds the fields from .agent.json.
type AgentConfig struct {
    Agent      string `json:"agent"`
    Passphrase string `json:"passphrase"`
    APIURL     string `json:"api_url"`
    WorkDir    string `json:"work_dir"`
}

// Load reads and parses an .agent.json file from the given path.
// Returns an error if the file can't be read or is missing required fields.
func Load(path string) (*AgentConfig, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
    }

    var cfg AgentConfig
    if err := json.Unmarshal(data, &cfg); err != nil {
        return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
    }

    if cfg.Agent == "" || cfg.Passphrase == "" || cfg.APIURL == "" {
        return nil, fmt.Errorf(".agent.json must contain: agent, passphrase, api_url")
    }

    return &cfg, nil
}
