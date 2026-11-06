// Command memory-api is the llm-memory-api server.
//
// It reads the same environment as the Node server (/etc/memory-api/env):
// DATABASE_URL, PORT (default 3100), MEMORY_API_HOST (default 127.0.0.1),
// BASE_URL and LOG_DIR. It refuses to start when the config table cannot be
// read.
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

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apibase"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
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
	// server.js starts the stale-vote scanner after the config load.
	services.Discussions.StartStaleVoteScanner(ctx)

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
		return err
	}
	fmt.Printf("Memory API listening on %s:%s\n", host, port)
	watchdog.Start(services.Log)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		pool.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// Stop taking requests and let the running ones finish. Past the grace
	// period the remaining connections are closed, which cancels their
	// requests' contexts and so their queries. Then the background writes are
	// drained. pool.Close waits for every borrowed connection, so it gets a
	// deadline too; past it the process exits anyway, and Postgres drops the
	// connections when the process is gone. Total time is bounded by
	// shutdownGrace + 2*backgroundDrain + poolCloseWait.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	if err != nil {
		srv.Close()
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
