package contract

import (
	"errors"
	"fmt"
	"testing"
)

func TestRetryExited(t *testing.T) {
	exited := fmt.Errorf("go did not start: %w: exit status 1", errExited)
	other := errors.New("no 200 from /health in time")
	for _, c := range []struct {
		name    string
		results []error
		calls   int
		want    error
	}{
		{"first start works", []error{nil}, 1, nil},
		{"lost the port once", []error{exited, nil}, 2, nil},
		{"never starts", []error{exited, exited, exited, nil}, 3, exited},
		{"timeout is not retried", []error{other, nil}, 1, other},
		{"exit then timeout", []error{exited, other, nil}, 2, other},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			got, err := retryExited(3, func() (int, error) {
				err := c.results[calls]
				calls++
				return calls, err
			})
			if calls != c.calls || !errors.Is(err, c.want) && err != c.want {
				t.Fatalf("calls %d err %v, want %d %v", calls, err, c.calls, c.want)
			}
			if err == nil && got != calls {
				t.Fatalf("value %d, want the last start's %d", got, calls)
			}
		})
	}
}
