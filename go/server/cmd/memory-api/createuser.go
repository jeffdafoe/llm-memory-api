package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
)

// userStore is the part of a pool createUser uses, so the tests can hand it
// a pgxmock pool.
type userStore interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// errAgentOnly ends a run that would give an agent-only actor web
// credentials. Its message is already printed, so the caller adds nothing.
var errAgentOnly = errors.New("agent-only actor")

// createUserCommand is `memory-api create-user <username> <password>`: it
// creates an admin web user, or sets a new password on one that exists. It
// replaces scripts/create-user.js, which infrastructure/tasks/create-admin.yml
// ran; the messages and exit codes are the same. An actor that exists
// without a password_hash is an agent with no web login, and the command
// refuses to add one, because that would open the dashboard to it.
func createUserCommand(args []string) int {
	if len(args) < 2 || args[0] == "" || args[1] == "" {
		fmt.Fprintln(os.Stderr, "Usage: memory-api create-user <username> <password>")
		return 1
	}
	ctx := context.Background()
	err := func() error {
		url, err := databaseURL()
		if err != nil {
			return err
		}
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			return err
		}
		defer pool.Close()
		return createUser(ctx, pool, args[0], args[1], os.Stdout, os.Stderr)
	}()
	if err != nil {
		if !errors.Is(err, errAgentOnly) {
			fmt.Fprintln(os.Stderr, "Failed:", err.Error())
		}
		return 1
	}
	return 0
}

func createUser(ctx context.Context, store userStore, username, password string, stdout, stderr io.Writer) error {
	salt := auth.GenerateSalt()
	passwordHash, err := auth.Hash(password, salt)
	if err != nil {
		return err
	}

	var id int64
	var existingHash *string
	err = store.QueryRow(ctx, "SELECT id, password_hash FROM actors WHERE name = $1", username).Scan(&id, &existingHash)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = store.Exec(ctx,
			"INSERT INTO actors (name, password_hash, password_salt) VALUES ($1, $2, $3)",
			username, passwordHash, salt)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "User \"%s\" created.\n", username)
		return nil
	}
	if err != nil {
		return err
	}
	if existingHash == nil {
		fmt.Fprintf(stderr, "Error: Actor \"%s\" exists as an agent without web credentials.\n", username)
		fmt.Fprintln(stderr, "Refusing to add web credentials. To grant dual identity, use the admin UI.")
		return errAgentOnly
	}
	_, err = store.Exec(ctx,
		"UPDATE actors SET password_hash = $1, password_salt = $2 WHERE id = $3",
		passwordHash, salt, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "User \"%s\" password updated.\n", username)
	return nil
}
