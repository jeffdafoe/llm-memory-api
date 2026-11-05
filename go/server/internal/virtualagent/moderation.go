package virtualagent

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// moderationAgent is the virtual agent that reviews proposed actor names.
const moderationAgent = "actor-name-check"

// errNoProviders stands in for the provider call until the providers are
// ported (step 8).
var errNoProviders = errors.New("virtual agent providers are not available in the Go server yet")

// Moderator is Node's moderateActorName: a proposed actor name goes to the
// actor-name-check virtual agent. Any failure approves the name, so
// registration is never blocked by moderation.
type Moderator struct {
	DB  db.DB
	Log *logger.Logger
}

// ModerateActorName returns approved, or false with the reason. With no
// actor-name-check virtual agent, or one without a provider, model or API
// key, the name is approved as in Node. A configured one would need the
// provider call, which is not ported yet; that is a moderation error, which
// approves, and the log says so.
func (m Moderator) ModerateActorName(ctx context.Context, name string) (approved bool, reason string) {
	var (
		virtual                 *bool
		provider, model, apiKey *string
	)
	err := m.DB.QueryRow(ctx,
		`SELECT agc.virtual, agc.provider, agc.model, agc.api_key
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE ac.name = $1`, moderationAgent).Scan(&virtual, &provider, &model, &apiKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, ""
	}
	if err != nil {
		m.Log.Log("actors", "moderation-error", logger.Fields{{Key: "name", Value: name}, {Key: "error", Value: err.Error()}})
		return true, ""
	}
	if virtual == nil || !*virtual {
		return true, ""
	}
	if empty(apiKey) || empty(provider) || empty(model) {
		return true, ""
	}
	m.Log.Log("actors", "moderation-error", logger.Fields{{Key: "name", Value: name}, {Key: "error", Value: errNoProviders.Error()}})
	return true, ""
}

func empty(s *string) bool {
	return s == nil || *s == ""
}
