// Package discuss implements the discussion transport — a long-running
// process that relays messages between a Claude subagent and the memory
// API's chat system. It manages the full lifecycle: creating or joining
// a discussion, waiting for participants, running the message polling
// loop, convergence detection, and clean shutdown.
package discuss

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config holds all settings for a discussion transport session.
// Populated from CLI flags and .agent.json.
type Config struct {
	// Agent credentials (from .agent.json or CLI)
	Agent      string
	Passphrase string
	APIURL     string

	// Discussion parameters
	Topic                string
	Others               []string // required participants
	OptionalParticipants []string // optional participants
	Context              string   // discussion context text
	ContextFile          string   // path to context file (read at startup)
	Mode                 string   // "realtime" or "async"
	Initiator            bool     // true if this agent created the discussion

	// Limits
	MaxMessages    int
	TimeoutMinutes int
	JoinTimeout    int // seconds to wait for pending invitation
	MaxRounds      int // round-trips before convergence warning

	// Paths
	WorkDirBase string // base directory (discuss-<id> appended)
	WorkDir     string // full path including discuss-<id>

	// Transport tuning
	PollInterval      time.Duration
	SendDelay         time.Duration
	ResultTimeout     int // seconds to wait for result.md before shutdown
}

// DefaultConfig returns a Config with sensible defaults.
// Agent credentials must still be set from .agent.json or CLI flags.
func DefaultConfig() *Config {
	return &Config{
		Mode:           "realtime",
		MaxMessages:    200,
		TimeoutMinutes: 120,
		JoinTimeout:    300,
		MaxRounds:      20,
		PollInterval:   5 * time.Second,
		SendDelay:      3 * time.Second,
		ResultTimeout:  60,
	}
}

// LoadAgentConfig reads .agent.json and populates the Config's
// agent credentials and work directory (if not already set via CLI).
func (c *Config) LoadAgentConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config %s: %w", path, err)
	}

	var raw struct {
		Agent      string `json:"agent"`
		Passphrase string `json:"passphrase"`
		APIURL     string `json:"api_url"`
		WorkDir    string `json:"work_dir"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("failed to parse config %s: %w", path, err)
	}

	// CLI flags take precedence — only fill in blanks
	if c.Agent == "" {
		c.Agent = raw.Agent
	}
	if c.Passphrase == "" {
		c.Passphrase = raw.Passphrase
	}
	if c.APIURL == "" {
		c.APIURL = raw.APIURL
	}
	if c.WorkDirBase == "" && raw.WorkDir != "" {
		c.WorkDirBase = raw.WorkDir
	}

	return nil
}

// Validate checks that required fields are set.
func (c *Config) Validate() error {
	if c.Agent == "" {
		return fmt.Errorf("missing agent name (set in .agent.json or via --agent)")
	}
	if c.Passphrase == "" {
		return fmt.Errorf("missing passphrase (set in .agent.json or via --passphrase)")
	}
	if c.APIURL == "" {
		return fmt.Errorf("missing api_url (set in .agent.json or via --api-url)")
	}
	return nil
}

// SetWorkDir sets the full working directory path based on the discussion ID.
// Falls back to os.TempDir()/llm if no base was configured.
func (c *Config) SetWorkDir(discussionID int) {
	base := c.WorkDirBase
	if base == "" {
		base = filepath.Join(os.TempDir(), "llm")
	}
	c.WorkDir = filepath.Join(base, fmt.Sprintf("discuss-%d", discussionID))
}

// OtherAgents returns the combined list of required and optional participants
// (excluding self).
func (c *Config) OtherAgents() []string {
	result := make([]string, 0, len(c.Others)+len(c.OptionalParticipants))
	result = append(result, c.Others...)
	result = append(result, c.OptionalParticipants...)
	return result
}

// ApplyServerConfig fetches config from the API and applies values
// that weren't already set via CLI flags. This lets the server provide
// defaults for things like discussion timeouts and max rounds.
func ApplyServerConfig(client *APIClient, cfg *Config, logger *Logger) {
	var resp struct {
		Config map[string]string `json:"config"`
	}
	if err := client.Post("/agent/config", map[string]interface{}{}, &resp); err != nil {
		logger.Log("WARNING: Could not fetch server config: %s — using defaults", err)
		return
	}

	if v, ok := resp.Config["discussion_end_timeout_realtime"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ResultTimeout = n
			logger.Log("Server config: result timeout = %ds", n)
		}
	} else {
		logger.Log("WARNING: discussion_end_timeout_realtime not configured on server, using default %ds", cfg.ResultTimeout)
	}

	if v, ok := resp.Config["discussion_maxrounds"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxRounds = n
			logger.Log("Server config: max rounds = %d", n)
		}
	} else {
		logger.Log("WARNING: discussion_maxrounds not configured on server, using default %d", cfg.MaxRounds)
	}
}

// LoadContext reads the context file (if set) into c.Context.
func (c *Config) LoadContext() error {
	if c.ContextFile == "" {
		return nil
	}
	data, err := os.ReadFile(c.ContextFile)
	if err != nil {
		return fmt.Errorf("failed to read context file %s: %w", c.ContextFile, err)
	}
	c.Context = string(data)
	return nil
}
