package contract

import "errors"

// startAttempts bounds the retries of a server that exits before /health
// answers. freePort closes its listener before the server binds the port,
// so a server of a parallel case can take the port in between; the loser
// then exits (Go with "address already in use"; Node prints its listening
// line anyway, since server.js does not check the error Express 5 hands its
// listen callback, and exits 0). A fresh port fixes that, and a server that
// really cannot start fails every attempt.
const startAttempts = 3

// errExited marks a start that failed because the server process ended.
var errExited = errors.New("exited")

// retryExited runs start up to attempts times, retrying only a start whose
// error wraps errExited.
func retryExited[T any](attempts int, start func() (T, error)) (T, error) {
	var v T
	var err error
	for i := 0; i < attempts; i++ {
		v, err = start()
		if err == nil || !errors.Is(err, errExited) {
			return v, err
		}
	}
	return v, err
}
