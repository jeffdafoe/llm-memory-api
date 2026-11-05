package httpapi

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestErrorMessageUsesThePostgresMessage(t *testing.T) {
	pg := &pgconn.PgError{Severity: "ERROR", Code: "2201B", Message: "invalid regular expression: quantifier operand invalid"}
	if got := errorMessage(pg); got != pg.Message {
		t.Errorf("errorMessage(pg) = %q", got)
	}
	if got := errorMessage(fmt.Errorf("wrapped: %w", pg)); got != pg.Message {
		t.Errorf("a wrapped Postgres error: %q", got)
	}
	if got := errorMessage(errors.New("plain")); got != "plain" {
		t.Errorf("errorMessage(plain) = %q", got)
	}
}
