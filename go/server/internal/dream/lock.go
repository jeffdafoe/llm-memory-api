package dream

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// runLockKey is the dream run's advisory lock key (LLM-532). Keys follow
// <ticket number>001 and are never reused; this is the app's only one.
const runLockKey = 532001

// lockTimeout bounds the lock's own statements: the unlock runs when the
// run's context may already be cancelled.
const lockTimeout = 5 * time.Second

// Conn is one connection held for the whole run: advisory locks belong to
// the Postgres session, so the lock must live on a connection checked out
// of the pool, not on pool queries that return their connection at once.
type Conn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	// Ping reports whether the session is still alive.
	Ping(ctx context.Context) error
	// Release returns the connection, or, with destroy, closes it: after a
	// failed statement its session state is unknown, and ending the
	// session releases any lock it held.
	Release(destroy bool)
}

// Connector checks a connection out of the pool.
type Connector interface {
	Connect(ctx context.Context) (Conn, error)
}

// Pool is the Connector over the server's pgxpool.
type Pool struct{ Pool *pgxpool.Pool }

func (p Pool) Connect(ctx context.Context) (Conn, error) {
	c, err := p.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return poolConn{c}, nil
}

type poolConn struct{ c *pgxpool.Conn }

func (p poolConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.c.QueryRow(ctx, sql, args...)
}

func (p poolConn) Ping(ctx context.Context) error { return p.c.Ping(ctx) }

func (p poolConn) Release(destroy bool) {
	if !destroy {
		p.c.Release()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()
	_ = p.c.Hijack().Close(ctx)
}

// errRunLockLost aborts the whole run. Every loop of the run catches
// per-item errors and goes on, which is right for a bad row and wrong for
// a lost lock, so each of those catches passes this one on.
var errRunLockLost = errors.New("dream run lock lost: its Postgres session ended mid-run, so another run may already hold the lock")

// runLock is a held dream-run lock.
type runLock struct {
	j        *Job
	conn     Conn
	lost     bool
	released bool
}

// acquireRunLock takes the lock, or returns nil when another run holds it.
// Two overlapping runs would read the same last_dream_at cursors, plan
// overlapping windows, and the later writer would move a cursor past days
// the other never dreamed. The guard must work across processes (operator
// runs are separate processes), and an advisory lock ends with its session,
// so a crashed run leaves nothing stuck.
func (j *Job) acquireRunLock(ctx context.Context) (*runLock, error) {
	conn, err := j.Conns.Connect(ctx)
	if err != nil {
		return nil, err
	}
	// No row reads as not granted, as rows[0] undefined did in Node.
	var ok *bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1) AS ok", runLockKey).Scan(&ok); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// The connection is suspect: destroy it rather than hand it on.
		conn.Release(true)
		return nil, err
	}
	if ok == nil || !*ok {
		conn.Release(false)
		return nil, nil
	}
	return &runLock{j: j, conn: conn}, nil
}

// check returns errRunLockLost when the lock's session has died. Node
// heard of a dead session from the client's error event; Go asks the
// session at each check, so the loss is logged here, at the first check
// after it (Known differences). It cannot stop a statement in flight,
// only the next unit of work or cursor write.
func (l *runLock) check(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if !l.lost {
		pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockTimeout)
		err := l.conn.Ping(pingCtx)
		cancel()
		if err != nil {
			l.lost = true
			l.j.log("lock-connection-error", logger.Fields{{Key: "error", Value: message(err)}})
		}
	}
	if l.lost {
		return errRunLockLost
	}
	return nil
}

// release unlocks and returns the connection; a second call does nothing.
// An unlock that says the session did not hold the lock (it was lost) is
// logged. A failed unlock destroys the connection, which ends the session
// and so releases the lock.
func (l *runLock) release(ctx context.Context) {
	if l.released {
		return
	}
	l.released = true
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockTimeout)
	defer cancel()
	var ok *bool
	err := l.conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1) AS ok", runLockKey).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if err != nil {
		l.j.log("lock-release-error", logger.Fields{{Key: "error", Value: message(err)}})
	} else if ok == nil || !*ok {
		l.lost = true
		l.j.log("lock-release-not-held", logger.Fields{})
	}
	l.conn.Release(err != nil)
}
