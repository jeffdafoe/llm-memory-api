//go:build contract

// Command runjob runs one pass of a Go server background job against
// DATABASE_URL, for the contract harness's job steps; runjob.js beside it
// does the same with the Node server's code. It is built with the contract
// tag only, so it is never part of the server binary.
//
//	runjob cleanup
//	runjob dream
//
// The pass logs as the server would (stdout, and LOG_DIR/activity.log), and
// the background error_log writes are drained before it exits.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/cleanup"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/dream"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runjob:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: runjob <job>")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	store := config.NewStore()
	if err := store.Load(ctx, pool); err != nil {
		return err
	}
	log := logger.New(pool, actors.NewCache(pool), os.Getenv("LOG_DIR"))

	switch os.Args[1] {
	case "cleanup":
		// As the server starts it: the scheduler's own log line, then the
		// pass its timer would run.
		job := &cleanup.Job{DB: pool, Config: store, Log: log}
		task, err := job.Start()
		if err != nil {
			return err
		}
		if task == nil {
			return errors.New("the cleanup scheduler did not start")
		}
		task.Drain(time.Second)
		schedule, _ := store.Get("cleanup_cron_schedule")
		job.Pass(ctx, schedule)
	case "dream":
		// The server's services, as main wires the job.
		services := server.NewServices(pool, store, os.Getenv("LOG_DIR"))
		job := &dream.Job{
			DB: pool, Conns: dream.Pool{Pool: pool}, Config: store, Log: services.Log,
			Notes: services.Notes, Agents: services.Agents, Trust: services.Admin,
		}
		task, err := job.Start()
		if err != nil {
			return err
		}
		if task == nil {
			return errors.New("the dream scheduler did not start")
		}
		task.Drain(time.Second)
		schedule, _ := store.Get("dream_cron_schedule")
		job.Pass(ctx, schedule)
	default:
		return fmt.Errorf("unknown job %q", os.Args[1])
	}
	if !db.DrainBackground(10 * time.Second) {
		return errors.New("background writes did not finish")
	}
	return nil
}
