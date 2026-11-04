// Package signup holds the signup-funnel stamps (LLM-734) that the auth layer
// writes: an account's first MCP connection. The tool-call stamp and the
// admin funnel view move with the MCP endpoint and the admin API.
package signup

import (
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
)

// ConnectedSQL is the SET-clause fragment the MCP heartbeat folds into its
// UPDATE. $2 is TrackingSince(). A NULL $2 compares as NULL, so the CASE never
// stamps: an account older than the tracking start has an unknown true first
// use, and stamping its next use as "first" would read as fact.
const ConnectedSQL = `first_connected_at = COALESCE(first_connected_at,
                 CASE WHEN created_at >= $2::timestamptz THEN NOW() END)`

// dateLayouts are the forms of config.signup_funnel_since that JavaScript's
// Date.parse accepted and Postgres's timestamptz cast also reads.
var dateLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04",
	"2006-01-02",
}

// TrackingSince returns config.signup_funnel_since, or nil when it is blank or
// not a date (then no first_* stamp is written: fail closed). A missing key is
// an error, as config.get threw in Node.
func TrackingSince(store *config.Store) (*string, error) {
	value, err := store.Get("signup_funnel_since")
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil
	}
	for _, layout := range dateLayouts {
		if _, err := time.Parse(layout, value); err == nil {
			return &value, nil
		}
	}
	return nil, nil
}
