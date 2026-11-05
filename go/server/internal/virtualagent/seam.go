// Package virtualagent is the Go side of Node's services/virtual-agent.js.
// Seam carries the parts other services call before the virtual agents are
// ported (LLM-902 step 8).
package virtualagent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
)

// Seam is the virtual-agent surface mail, chat and the system handler call.
// UnanswerableNotice is Node's text; the handlers do nothing.
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

// HandleDirectChat is the virtual agent's reply to a direct chat message.
// The reply is what a wait=true sender gets as "reply"; nil is null.
func (Seam) HandleDirectChat(ctx context.Context, in chat.DirectChat) (json.RawMessage, error) {
	return nil, nil
}

// HandleVirtualAgent is the system handler for "virtual-agent" payloads:
// a discussion's virtual participants answering a message or a vote.
func (Seam) HandleVirtualAgent(ctx context.Context, payload json.RawMessage, meta system.Meta) error {
	return nil
}
