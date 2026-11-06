package signup

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// Events the signup guide may send. The value names a client tab or a copy
// button; the page owns those names, so only their shape is checked.
var signupEvents = []string{"guide_shown", "client", "copy"}

var valuePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

const (
	// The guide shows once, right after signup. Past this window an event
	// is not a signup fact, so it is dropped rather than stored.
	signupWindowHours = 24
	// A page left open could send copy clicks without end; this bounds the
	// rows per account.
	maxEventsPerAccount = 100
	// First key of the two-key advisory lock that serializes one account's
	// event inserts (the second key is the actor id).
	eventLockClass = 734001
)

// EventResult is the /agent/signup-event answer.
type EventResult struct {
	Recorded bool   `json:"recorded"`
	Reason   string `json:"reason,omitempty"`
}

// jsonString is the value of a JSON string, or false for any other JSON
// value (Node compared with includes and typeof).
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// RecordEvent stores one signup-guide event for the actor (Node's
// recordSignupEvent). Past the account's first 24 hours, or over its cap,
// the event is dropped with recorded: false: the page cannot know either and
// must not show an error.
func RecordEvent(ctx context.Context, conn db.DB, actorID int, eventRaw, valueRaw json.RawMessage) (EventResult, error) {
	event, isString := jsonString(eventRaw)
	known := false
	for _, e := range signupEvents {
		if isString && event == e {
			known = true
		}
	}
	if !known {
		return EventResult{}, apierr.New(http.StatusBadRequest, "event must be one of: guide_shown, client, copy")
	}
	value, isString := jsonString(valueRaw)
	if !isString || !valuePattern.MatchString(value) {
		return EventResult{}, apierr.New(http.StatusBadRequest, "value must be 1-32 lowercase letters, digits, hyphens or underscores, starting with a letter")
	}

	// Concurrent inserts for one account would each count the same rows and
	// all pass the cap, so they are serialized per account by a
	// transaction-scoped advisory lock. The count-and-insert runs as a later
	// statement, so under READ COMMITTED it sees every row the previous lock
	// holder committed. Not FOR UPDATE on the actor row: the MCP heartbeat
	// updates that row on every request.
	tx, err := conn.Begin(ctx)
	if err != nil {
		return EventResult{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", eventLockClass, actorID); err != nil {
		return EventResult{}, err
	}
	rows, err := tx.Query(ctx, `INSERT INTO signup_events (actor_id, event, value)
             SELECT a.id, $2, $3
             FROM actors a
             WHERE a.id = $1
               AND a.created_at > NOW() - make_interval(hours => $4)
               AND (SELECT COUNT(*) FROM signup_events s WHERE s.actor_id = a.id) < $5
             RETURNING id`, actorID, event, value, signupWindowHours, maxEventsPerAccount)
	if err != nil {
		return EventResult{}, err
	}
	inserted := false
	for rows.Next() {
		inserted = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return EventResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EventResult{}, err
	}
	if !inserted {
		return EventResult{Recorded: false, Reason: "outside the signup window or over the event cap"}, nil
	}
	return EventResult{Recorded: true}, nil
}
