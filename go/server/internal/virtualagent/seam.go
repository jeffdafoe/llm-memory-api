// Package virtualagent is the Go side of Node's services/virtual-agent.js.
// Seam carries the parts other services call before the virtual agents are
// ported (LLM-902 step 8).
package virtualagent

import (
	"context"
	"strings"
)

// Seam is the virtual-agent surface mail calls. UnanswerableNotice is
// Node's text; HandleDirectMail does nothing until step 8.
type Seam struct{}

// UnanswerableNotice is the message a virtual agent sends back when it will
// not reply: the sender has no access to it ("no-access"), or it lacks the
// named settings.
func (Seam) UnanswerableNotice(agentName, reason string, missing []string) string {
	if reason == "no-access" {
		return "[Error] You do not have access to " + agentName + ", so it will not reply. " +
			"Its owner can grant access in the dashboard (Agents → " + agentName + " → Permissions)."
	}
	pronoun := "them"
	if len(missing) == 1 {
		pronoun = "it"
	}
	return "[Error] " + agentName + " cannot reply: it has no " + strings.Join(missing, ", ") + " set. " +
		"Its owner can add " + pronoun + " in the dashboard (Agents → " + agentName + ")."
}

// HandleDirectMail is the virtual agent's reply to direct mail.
func (Seam) HandleDirectMail(ctx context.Context, virtualAgent, fromAgent, mailID string) error {
	return nil
}
