// Package activity clears stale activity indicators: an agent that has made
// no request for five minutes has gone away (closed its session, crashed),
// so its spinner is turned off. This is the minute timer in Node's
// server.js.
package activity

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
)

// Interval is how often Run sweeps, as Node's setInterval did.
const Interval = time.Minute

const sweepSQL = `UPDATE actors SET active_since = NULL
                 WHERE active_since IS NOT NULL
                   AND last_seen < NOW() - INTERVAL '5 minutes'
                 RETURNING id, name`

// Sweeper clears the indicators and tells the admin dashboards.
type Sweeper struct {
	DB     db.DB
	Events events.Broadcaster
	Errorf func(format string, args ...any)
	// Every replaces Interval, for tests.
	Every time.Duration
	// ticks replaces the ticker, for tests.
	ticks <-chan time.Time
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

// Run starts a sweep every Interval, the first one Interval after the
// start, until ctx is done. As with Node's setInterval, a sweep does not
// wait for an earlier one still running and has no deadline of its own.
// When ctx ends no sweep starts; Run waits up to drain for the ones in
// progress, cancels them, waits up to drain again, and reports whether they
// all returned.
func (s *Sweeper) Run(ctx context.Context, drain time.Duration) bool {
	every := s.Every
	if every <= 0 {
		every = Interval
	}
	sweepCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	var running sync.WaitGroup
	ticks := s.ticks
	if ticks == nil {
		tick := time.NewTicker(every)
		defer tick.Stop()
		ticks = tick.C
	}
	for {
		select {
		case <-ctx.Done():
			done := make(chan struct{})
			go func() {
				running.Wait()
				close(done)
			}()
			select {
			case <-done:
				return true
			case <-time.After(drain):
			}
			cancel()
			select {
			case <-done:
				return true
			case <-time.After(drain):
				return false
			}
		case <-ticks:
		}
		// select picks at random when the tick and the cancel are both
		// ready; a tick after the cancel starts nothing.
		if ctx.Err() != nil {
			continue
		}
		running.Add(1)
		go func() {
			defer running.Done()
			if err := s.Sweep(sweepCtx); err != nil {
				s.Errorf("Stale activity cleanup error: %s", message(err))
			}
		}()
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
