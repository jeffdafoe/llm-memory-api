// Package syserrors records reported errors in system_errors (Node's
// services/error-handler.js handleError). The services call it for failures
// in fire-and-forget work, so a broken background step is visible on the
// admin dashboard instead of being dropped.
//
// Node's handler map has one entry, for discuss-transport SUBAGENT_DEAD
// (it sends the agent a system message). Errors from any other source are
// stored as unhandled, which is all this package does.
package syserrors

import (
	"context"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
)

// Recorder writes system_errors rows.
type Recorder struct {
	DB     db.DB
	Actors *actors.Cache
	// Errorf writes to stderr when the row cannot be written; nil drops it.
	Errorf func(format string, args ...any)
}

// Record inserts one unhandled error. agent "" stores no actor. context is
// marshaled to JSON (jsonb, so key order does not matter); nil stores NULL.
// The caller is already off the request path; a failure here is only logged,
// as Node's .catch(() => {}) dropped it.
func (rec *Recorder) Record(ctx context.Context, agent, source, code string, details any) {
	var actorID *int
	if agent != "" && rec.Actors != nil {
		if a, ok, err := rec.Actors.ResolveByName(ctx, agent); err == nil && ok {
			actorID = &a.ID
		}
	}
	var detailJSON *string
	if details != nil {
		b, err := jsonutil.Marshal(details)
		if err == nil {
			s := string(b)
			detailJSON = &s
		}
	}
	if _, err := rec.DB.Exec(ctx,
		`INSERT INTO system_errors (actor_id, source, error_code, context, status, handler_action, resolved_at)
         VALUES ($1, $2, $3, $4, 'unhandled', NULL, NULL)`,
		actorID, source, code, detailJSON); err != nil && rec.Errorf != nil {
		rec.Errorf("system_errors %s %s not recorded: %s", source, code, err)
	}
}
