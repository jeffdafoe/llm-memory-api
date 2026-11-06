// Command memory-api is the llm-memory-api server.
//
// It reads the same environment as the Node server (/etc/memory-api/env):
// DATABASE_URL, PORT (default 3100), MEMORY_API_HOST (default 127.0.0.1),
// BASE_URL and LOG_DIR. It refuses to start when the config table cannot be
// read.
//
// `memory-api staleness-check <recipient>` instead runs the weekly staleness
// check once (staleness.go).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/activity"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apibase"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/cleanup"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/cron"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/server"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/watchdog"
)

const (
	shutdownGrace   = 10 * time.Second
	backgroundDrain = 5 * time.Second
	poolCloseWait   = 5 * time.Second
)

func main() {
	// The weekly staleness check is a root cron job, not part of the
	// server: it runs once and exits.
	if len(os.Args) > 1 && os.Args[1] == "staleness-check" {
		os.Exit(stalenessCheck(os.Args[2:]))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "memory-api:", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A bad *_API_BASE value stops the server here, with its name, instead
	// of failing every provider or embeddings call later.
	if err := apibase.Check(); err != nil {
		return err
	}

	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}

	store := config.NewStore()
	if err := store.Load(ctx, pool); err != nil {
		return fmt.Errorf("load config table: %w", err)
	}

	services := server.NewServices(pool, store, server.LogDir())
	// Started after the config load and before the listener, as in Node.
	if err := services.Agents.StartErrorPing(ctx); err != nil {
		return err
	}

	// The Node server failed to start when its cleanup scheduler threw (a
	// missing config key, a schedule with no time to run); so does this.
	cleanupTask, err := (&cleanup.Job{DB: pool, Config: store, Log: services.Log}).Start()
	if err != nil {
		pool.Close()
		return fmt.Errorf("cleanup scheduler: %w", err)
	}

	// Loopback by default: nginx and the salem engine reach the server on
	// 127.0.0.1. The login rate limits trust the X-Forwarded-For hop nginx
	// appends, so a request that reached this port directly could forge its
	// client address.
	host := envOr("MEMORY_API_HOST", "127.0.0.1")
	port := envOr("PORT", "3100")
	srv := &http.Server{
		Addr:              net.JoinHostPort(host, port),
		Handler:           server.Handler(services),
		ReadHeaderTimeout: 30 * time.Second,
	}

	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		stopJobs(cleanupTask)
		pool.Close()
		return err
	}
	// The bound port, not the setting: with PORT=0 the OS picks it, and the
	// contract harness reads it from this line.
	fmt.Printf("Memory API listening on %s:%d\n", host, listener.Addr().(*net.TCPAddr).Port)
	watchdog.Start(services.Log)

	sweeper := &activity.Sweeper{DB: pool, Events: services.Events, Errorf: services.Log.Errorf}
	// Run drains its own sweeps once ctx ends; sweptClean says whether
	// they all returned.
	sweeperDone := make(chan struct{})
	sweptClean := true
	go func() {
		sweptClean = sweeper.Run(ctx, backgroundDrain)
		close(sweeperDone)
	}()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		stopJobs(cleanupTask)
		pool.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// No new cleanup runs or sweeps from here; the stale-activity sweeper
	// drains its sweeps in progress on its own (2*backgroundDrain at most,
	// from now). Stop taking requests
	// and let the running ones finish. Past the grace period the remaining
	// connections are closed, which cancels their requests' contexts and so
	// their queries. Then a running cleanup pass gets backgroundDrain to
	// finish before its context is cancelled (each decay note and each
	// retention stage is one transaction, so a cancel rolls back whole
	// steps), and the background writes are drained. pool.Close waits for
	// every borrowed connection, so it gets a deadline too; past it the
	// process exits anyway, and Postgres drops the connections when the
	// process is gone. Total time is bounded by shutdownGrace +
	// 4*backgroundDrain + poolCloseWait.
	if cleanupTask != nil {
		cleanupTask.Stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	if err != nil {
		srv.Close()
	}
	if !stopJobs(cleanupTask) {
		fmt.Fprintln(os.Stderr, "memory-api: cleanup pass still running at shutdown")
	}
	<-sweeperDone
	if !sweptClean {
		fmt.Fprintln(os.Stderr, "memory-api: stale-activity sweep still running at shutdown")
	}
	if !db.DrainBackground(backgroundDrain) {
		fmt.Fprintln(os.Stderr, "memory-api: background writes still running at shutdown")
	}
	closed := make(chan struct{})
	go func() {
		pool.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(poolCloseWait):
		fmt.Fprintln(os.Stderr, "memory-api: database pool did not close in time")
	}
	return err
}

// stopJobs stops the cleanup schedule and drains a pass in progress. It
// reports whether nothing was left running.
func stopJobs(task *cron.Task) bool {
	if task == nil {
		return true
	}
	return task.Drain(backgroundDrain)
}
