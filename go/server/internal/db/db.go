// Package db opens the shared Postgres pool and defines the narrow interface
// every service takes, so tests can substitute pgxmock for the real pool.
package db

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// DB is the subset of *pgxpool.Pool the server uses. pgxmock's pool
// implements it as well.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// maxConns matches node-postgres's default pool size (10), which the Node
// server ran with.
const maxConns = 10

// backgroundTimeout bounds fire-and-forget writes (request_log, error_log,
// last_seen) so a stuck database cannot pile up goroutines without limit.
const backgroundTimeout = 30 * time.Second

// Open connects to Postgres and registers the pgvector types on every new
// connection, so vector columns scan into pgvector.Vector values.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// runner tracks fire-and-forget jobs so shutdown can wait for them.
type runner struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

func newRunner() *runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &runner{ctx: ctx, cancel: cancel}
}

var jobs = newRunner()

// Background runs fn on its own goroutine with a fresh, bounded context. Use it
// for writes whose failure must never affect the request that caused them.
// The Node server did these as un-awaited promises with `.catch(() => {})`.
// Once DrainBackground has started, new jobs are dropped; started reports
// whether fn was scheduled.
func Background(fn func(ctx context.Context)) (started bool) {
	return jobs.after(nil, fn)
}

// BackgroundAfter is Background for a job that must wait for an earlier one:
// it is tracked from now, so a drain waits for it, but it starts, and its
// bounded context is made, only when prev is closed.
func BackgroundAfter(prev <-chan struct{}, fn func(ctx context.Context)) (started bool) {
	return jobs.after(prev, fn)
}

func (r *runner) after(prev <-chan struct{}, fn func(ctx context.Context)) bool {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		if prev != nil {
			<-prev
		}
		ctx, cancel := context.WithTimeout(r.ctx, backgroundTimeout)
		defer cancel()
		fn(ctx)
	}()
	return true
}

// DrainBackground stops new background jobs, waits up to timeout for the
// running ones, then cancels them and waits up to timeout again. It reports
// whether every job returned. Call it at shutdown, before the pool closes.
func DrainBackground(timeout time.Duration) bool {
	return jobs.drain(timeout)
}

func (r *runner) drain(timeout time.Duration) bool {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
	}
	r.cancel()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
