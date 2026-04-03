package discuss

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GeneratePrompt takes the template content (typically embedded in the
// binary), performs variable substitution, writes the result to prompt.txt,
// and returns the content.
func GeneratePrompt(cfg *Config, paths *Paths, discussionID int, proxyPort int, logger *Logger, templateContent string) (string, error) {

	// Build human-readable participant list
	others := cfg.OtherAgents()
	var otherAgentsStr string
	if len(others) == 1 {
		otherAgentsStr = fmt.Sprintf("the \"%s\" agent", others[0])
	} else if len(others) > 1 {
		quoted := make([]string, len(others))
		for i, a := range others {
			quoted[i] = fmt.Sprintf("\"%s\"", a)
		}
		otherAgentsStr = "agents " + strings.Join(quoted, ", ")
	} else {
		otherAgentsStr = "other agents"
	}

	// Build first-contact text based on role (creator vs joiner)
	var firstContact string
	if cfg.Initiator {
		firstContact = `## First Contact

You are the CREATOR of this discussion. Send your opening message immediately after completing
your setup steps (defining helper functions). Do not wait for the other side to message first.
Your opening message should introduce the topic and your initial position.

If no reply arrives within 90 seconds of your opening message, write a diagnostic to outbox:
"Sent opening message but no reply yet. Is the other participant's transport running?"

If no contact from the other side after 5 minutes total, write "No contact after 5 minutes.
Exiting." to outbox and exit.`
	} else {
		firstContact = `## First Contact

You are JOINING this discussion. After completing your setup steps (defining helper functions),
begin your polling loop. Your inbox may be empty initially — this is normal, the creator may
still be launching.

If no message arrives within 90 seconds of your first poll, write a diagnostic to outbox:
"Joined and waiting — is the creator side running?"

If no contact after 5 minutes total, write "No contact after 5 minutes. Exiting." to outbox
and exit.`
	}

	// Context text
	contextStr := cfg.Context
	if contextStr == "" {
		contextStr = "(none)"
	}

	// Use forward slashes for bash compatibility
	workDir := filepath.ToSlash(cfg.WorkDir)

	replacements := map[string]string{
		"[OTHER_AGENTS]":   otherAgentsStr,
		"[TOPIC]":          cfg.Topic,
		"[WORK_DIR]":       workDir,
		"[DISCUSSION_ID]":  fmt.Sprintf("%d", discussionID),
		"[API_URL]":        fmt.Sprintf("http://127.0.0.1:%d", proxyPort),
		"[API_KEY]":        "", // proxy handles auth
		"[MY_AGENT]":       cfg.Agent,
		"[CONTEXT]":        contextStr,
		"[GUIDELINES]":     "", // not loading from repo — subagent gets guidelines via its own bootstrap
		"[FIRST_CONTACT]":  firstContact,
	}

	prompt := templateContent
	for placeholder, value := range replacements {
		prompt = strings.ReplaceAll(prompt, placeholder, value)
	}

	if err := os.WriteFile(paths.PromptFile, []byte(prompt), 0644); err != nil {
		return "", fmt.Errorf("write prompt: %w", err)
	}
	logger.Log("Prompt written to %s", paths.PromptFile)

	return prompt, nil
}
