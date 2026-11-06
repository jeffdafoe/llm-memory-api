// Package activity clears stale activity indicators: an agent that has made
// no request for five minutes has gone away (closed its session, crashed),
// so its spinner is turned off. This is the minute timer in Node's
// server.js.
package activity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
)

// Interval is how often Run sweeps, as Node's setInterval did.
const Interval = time.Minute

const sweepTimeout = 30 * time.Second

const sweepSQL = `UPDATE actors SET active_since = NULL
                 WHERE active_since IS NOT NULL
                   AND last_seen < NOW() - INTERVAL '5 minutes'
                 RETURNING id, name`

// Sweeper clears the indicators and tells the admin dashboards.
type Sweeper struct {
	DB     db.DB
	Events events.Broadcaster
	Errorf func(format string, args ...any)
}

type activityEvent struct {
	Agent  string `json:"agent"`
	Active bool   `json:"active"`
}

// Sweep runs one pass: one UPDATE, then an agent_activity event per actor
// it cleared.
func (s *Sweeper) Sweep(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, sweepSQL)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range names {
		s.Events.Broadcast("agent_activity", activityEvent{Agent: name, Active: false})
	}
	return nil
}

// Run sweeps every Interval, the first one Interval after the start, until
// ctx is done. A sweep in progress finishes first: its statement runs on a
// context of its own, so shutdown does not turn it into a logged error.
func (s *Sweeper) Run(ctx context.Context) {
	tick := time.NewTicker(Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepTimeout)
		if err := s.Sweep(sweepCtx); err != nil {
			s.Errorf("Stale activity cleanup error: %s", message(err))
		}
		cancel()
	}
}

// message is err.message as node-postgres gave it: a Postgres error's own
// text, without pgx's "ERROR: " and SQLSTATE.
func message(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}
