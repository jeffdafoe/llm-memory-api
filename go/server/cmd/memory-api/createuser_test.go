package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
)

const (
	selectActor = `SELECT id, password_hash FROM actors WHERE name = \$1`
	insertActor = `INSERT INTO actors \(name, password_hash, password_salt\) VALUES \(\$1, \$2, \$3\)`
	updateActor = `UPDATE actors SET password_hash = \$1, password_salt = \$2 WHERE id = \$3`
)

// captured is a pgxmock argument that matches any string and keeps it, so a
// test can check the hash and salt that were written.
type captured struct{ value string }

func (c *captured) Match(v any) bool {
	s, ok := v.(string)
	c.value = s
	return ok
}

func newMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock
}

// checkWritten fails unless the written hash is what a web login with the
// password checks against. auth.TestHashMatchesNode pins auth.Hash to Node's
// hashing.js, so the Node server also accepts it.
func checkWritten(t *testing.T, password string, hash, salt *captured) {
	t.Helper()
	if len(salt.value) != 64 {
		t.Fatalf("salt %q: want 64 hex characters", salt.value)
	}
	if !auth.Verify(password, salt.value, hash.value) {
		t.Fatal("the written hash does not verify with the password")
	}
	if auth.Verify("wrong", salt.value, hash.value) {
		t.Fatal("the written hash verifies with a wrong password")
	}
}

func TestCreateUserNew(t *testing.T) {
	mock := newMock(t)
	hash, salt := &captured{}, &captured{}
	mock.ExpectQuery(selectActor).WithArgs("admin").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(insertActor).WithArgs("admin", hash, salt).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	var stdout, stderr bytes.Buffer
	if err := createUser(context.Background(), mock, "admin", "s3cret", &stdout, &stderr); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if got, want := stdout.String(), "User \"admin\" created.\n"; got != want {
		t.Errorf("stdout %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr %q, want empty", stderr.String())
	}
	checkWritten(t, "s3cret", hash, salt)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateUserUpdatesWebUser(t *testing.T) {
	mock := newMock(t)
	hash, salt := &captured{}, &captured{}
	mock.ExpectQuery(selectActor).WithArgs("admin").
		WillReturnRows(pgxmock.NewRows([]string{"id", "password_hash"}).AddRow(int64(7), new("oldhash")))
	mock.ExpectExec(updateActor).WithArgs(hash, salt, int64(7)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	var stdout, stderr bytes.Buffer
	if err := createUser(context.Background(), mock, "admin", "n3w", &stdout, &stderr); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if got, want := stdout.String(), "User \"admin\" password updated.\n"; got != want {
		t.Errorf("stdout %q, want %q", got, want)
	}
	checkWritten(t, "n3w", hash, salt)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateUserRefusesAgentOnly(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(selectActor).WithArgs("home").
		WillReturnRows(pgxmock.NewRows([]string{"id", "password_hash"}).AddRow(int64(3), (*string)(nil)))

	var stdout, stderr bytes.Buffer
	err := createUser(context.Background(), mock, "home", "x", &stdout, &stderr)
	if !errors.Is(err, errAgentOnly) {
		t.Fatalf("err %v, want errAgentOnly", err)
	}
	want := "Error: Actor \"home\" exists as an agent without web credentials.\n" +
		"Refusing to add web credentials. To grant dual identity, use the admin UI.\n"
	if stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want empty", stdout.String())
	}
	// No write was expected, so a write would fail this.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateUserDatabaseError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(selectActor).WithArgs("admin").WillReturnError(errors.New("connection refused"))

	var stdout, stderr bytes.Buffer
	err := createUser(context.Background(), mock, "admin", "x", &stdout, &stderr)
	if err == nil || err.Error() != "connection refused" {
		t.Fatalf("err %v, want connection refused", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("output %q %q, want none (the command prints the error)", stdout.String(), stderr.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateUserCommandUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"admin"}, {"", "x"}, {"admin", ""}} {
		if code := createUserCommand(args); code != 1 {
			t.Errorf("createUserCommand(%q) = %d, want 1", args, code)
		}
	}
}
