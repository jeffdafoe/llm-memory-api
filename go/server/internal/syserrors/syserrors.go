// Package syserrors records reported errors in system_errors (Node's
// services/error-handler.js handleError). The services call Record for
// failures in fire-and-forget work, so a broken background step is visible
// on the admin dashboard instead of being dropped. Report, behind
// POST /v1/system/error/report (routes.go), is handleError itself: it runs
// the handler for the report's "source:error_code", if there is one, then
// stores the row.
package syserrors

import (
	"context"
	"errors"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgjson"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
)

// Recorder writes system_errors rows.
type Recorder struct {
	DB     db.DB
	Actors *actors.Cache
	// Notifier sends the system messages Report's handlers send.
	Notifier *system.Notifier
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

// handler runs for one "source:error_code" and returns the handler_action
// text. details is the report's context object, nil when there was none.
type handler func(ctx context.Context, rec *Recorder, agent string, details *jsutil.Object) (string, error)

// handlers is Node's handler map. Every key has a ":", so no source and code
// can reach a member of Object.prototype in Node's lookup.
var handlers = map[string]handler{
	"discuss-transport:SUBAGENT_DEAD": subagentDead,
}

// subagentDead tells the agent its discuss subagent stopped answering.
func subagentDead(ctx context.Context, rec *Recorder, agent string, details *jsutil.Object) (string, error) {
	message := "Your subagent appears to have died"
	if details != nil {
		// Node: `context && context.discussion_id`, then a template string,
		// which is String() of any JSON value.
		if id := details.Value("discussion_id"); jsutil.Truthy(id) {
			s, err := jsutil.ValueString(id)
			if err != nil {
				return "", err
			}
			message += " in discussion #" + s
		}
	}
	message += ". The transport is still running. Relaunch the subagent to continue."

	if agent == "" {
		// A web-user session has no agent: Node's sendSystemMessage resolved
		// "system", then looked up undefined, which matches no actor.
		if _, err := rec.Actors.RequireByName(ctx, "system"); err != nil {
			return "", err
		}
		return "", errors.New(`Agent "undefined" is not registered`)
	}
	if _, err := rec.Notifier.SendSystemMessage(ctx, agent, message, nil); err != nil {
		return "", err
	}
	return "Sent system message to " + agent + `: "` + message + `"`, nil
}

// Report is Node's handleError: it runs the handler for source:code, if any,
// then inserts the row and returns its id, status, handler_action and
// reported_at as node-postgres read them. A handler that fails leaves the
// row unhandled with "Handler failed: <message>"; any other failure is
// returned. details is the parsed context object, nil for none; the row
// stores it as JSON.stringify wrote it.
func (rec *Recorder) Report(ctx context.Context, agent, source, code string, details *jsutil.Object) (pgjson.Row, error) {
	status := "unhandled"
	var handlerAction *string
	var resolvedAt *time.Time
	if h, ok := handlers[source+":"+code]; ok {
		action, err := h(ctx, rec, agent, details)
		if err != nil {
			action = "Handler failed: " + httpapi.ErrorMessage(err)
		} else {
			status = "auto_resolved"
			// new Date(): millisecond precision.
			now := time.Now().Truncate(time.Millisecond)
			resolvedAt = &now
		}
		handlerAction = &action
	}

	var actorID *int
	if agent != "" {
		a, ok, err := rec.Actors.ResolveByName(ctx, agent)
		if err != nil {
			return nil, err
		}
		if ok {
			actorID = &a.ID
		}
	}

	var detailJSON *string
	if details != nil {
		s := jsutil.Marshal(details)
		detailJSON = &s
	}
	row, _, err := pgjson.QueryOne(ctx, rec.DB,
		`INSERT INTO system_errors (actor_id, source, error_code, context, status, handler_action, resolved_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7)
         RETURNING id, status, handler_action, reported_at`,
		actorID, source, code, detailJSON, status, handlerAction, resolvedAt)
	return row, err
}
