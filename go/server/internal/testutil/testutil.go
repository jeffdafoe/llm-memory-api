// Package testutil has helpers shared by the server's unit tests.
package testutil

import (
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
)

// NewMock returns a pgxmock pool whose expectations may be met in any order.
// Background writes (request_log, error_log, last_seen) run on their own
// goroutines, so their order against the request's own queries is not fixed.
func NewMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	mock.MatchExpectationsInOrder(false)
	t.Cleanup(mock.Close)
	return mock
}

// WaitMet waits up to two seconds for every expectation to be met, for
// tests whose last query runs in the background.
func WaitMet(t *testing.T, mock pgxmock.PgxPoolIface) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := mock.ExpectationsWereMet()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expectations not met: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
