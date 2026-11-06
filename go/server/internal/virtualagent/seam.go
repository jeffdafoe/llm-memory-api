// Package virtualagent is the Go side of Node's services/virtual-agent.js.
// Seam carries the parts other services call before the virtual agents are
// ported (LLM-902 step 8).
package virtualagent

import (
	"context"
	"encoding/json"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
)

// Seam is the virtual-agent surface the system handler calls: the
// discussion trigger, which does nothing yet. UnanswerableNotice is Node's
// text.
type Seam struct{}

// UnanswerableNotice is unanswerableNotice.
func (Seam) UnanswerableNotice(agentName, reason string, missing []string) string {
	return unanswerableNotice(agentName, reason, missing)
}

// HandleVirtualAgent is the system handler for "virtual-agent" payloads:
// a discussion's virtual participants answering a message or a vote.
func (Seam) HandleVirtualAgent(ctx context.Context, payload json.RawMessage, meta system.Meta) error {
	return nil
}
