package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/mail"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/server"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/staleness"
)

// envFile is where the service's environment lives; the root cron job that
// runs the staleness check has none of it.
const envFile = "/etc/memory-api/env"

// stalenessCheck is `memory-api staleness-check <recipient>`: the weekly
// report of pending apt upgrades, runtimes near end of life, a pending
// reboot and TLS certificates near expiry, mailed to <recipient> (an actor)
// from `system` through the mail service. It never installs anything. It
// runs as root (apt-get update, /etc/letsencrypt), as Node's
// scripts/staleness-check.js did. The exit status is 1 when the run failed.
func stalenessCheck(args []string) int {
	if len(args) < 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "Usage: memory-api staleness-check <recipient>")
		return 1
	}
	recipient := args[0]
	ctx := context.Background()

	var check *staleness.Checker
	err := func() error {
		url, err := databaseURL()
		if err != nil {
			return err
		}
		// No ping: as with node-postgres, an unreachable database shows up
		// as a failed PostgreSQL check in the report, and then as the
		// failed send.
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			return err
		}
		defer pool.Close()
		actorCache := actors.NewCache(pool)
		log := logger.New(pool, actorCache, server.LogDir())
		// The report is mailed from "system", which never triggers a
		// virtual agent, so mail needs none.
		mailer := &mail.Service{DB: pool, Actors: actorCache, Log: log}
		check = staleness.New(
			func(ctx context.Context) (string, error) {
				var num string
				err := pool.QueryRow(ctx, "SHOW server_version_num").Scan(&num)
				return num, err
			},
			func(ctx context.Context, to, subject, body string) (string, error) {
				sent, err := mailer.Send(ctx, to, "system", subject, body, "")
				if err != nil {
					return "", err
				}
				return sent.ID, nil
			},
		)
		defer db.DrainBackground(5 * time.Second)
		return check.Check(ctx, recipient)
	}()
	if err != nil {
		if check == nil {
			check = staleness.New(nil, nil)
		}
		check.Stamp("FAILED: " + err.Error())
		return 1
	}
	return 0
}

var quotedValue = regexp.MustCompile(`^(["'])(.*)(["'])$`)

// databaseURL is DATABASE_URL, else the DATABASE_URL= line of the service's
// env file. Ansible writes a bare value; one surrounding quote pair is also
// stripped, and escapes inside a value are not handled.
func databaseURL() (string, error) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url, nil
	}
	return databaseURLFrom(envFile)
}

// databaseURLFrom reads the DATABASE_URL line of the env file at path; a
// read error has Node's fs text.
func databaseURLFrom(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New(staleness.FSMessage(err, "open", path))
	}
	return urlFromEnvFile(string(data))
}

// urlFromEnvFile reads the DATABASE_URL= line of an env file.
func urlFromEnvFile(data string) (string, error) {
	for _, line := range strings.Split(data, "\n") {
		if value, ok := strings.CutPrefix(line, "DATABASE_URL="); ok {
			value = jsutil.Trim(value)
			if m := quotedValue.FindStringSubmatch(value); m != nil && m[1] == m[3] {
				value = m[2]
			}
			return value, nil
		}
	}
	return "", fmt.Errorf("DATABASE_URL not found in %s", envFile)
}
