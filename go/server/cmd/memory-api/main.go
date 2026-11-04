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

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/server"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/watchdog"
)

const shutdownGrace = 10 * time.Second

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

	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()

	store := config.NewStore()
	if err := store.Load(ctx, pool); err != nil {
		return fmt.Errorf("load config table: %w", err)
	}

	services := server.NewServices(pool, store, server.LogDir())

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
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
