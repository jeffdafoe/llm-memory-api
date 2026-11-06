package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// HandlerFunc is a route handler that returns its error instead of writing
// the error response itself. Route turns the error into the response.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Router wraps route handlers with the shared error handling (Node's
// apiRoute in middleware/route-wrapper.js).
type Router struct {
	Log *logger.Logger
}

// Route wraps fn. Every error is logged to error_log with its status code,
// attributed to the authenticated agent when there is one, so the dashboard
// can tell client errors from server failures. Errors below 500 return their
// code and message; 500s return a generic message naming only the label.
// A panic in fn is handled as a 500.
func (rt *Router) Route(category, label string, fn HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := callRecovering(fn, w, r)
		if err == nil {
			return
		}
		status, code := apierr.StatusAndCode(err)
		rt.Log.LogError(category, label, logger.ErrorOpts{
			Agent:      StateOf(r).AuthenticatedAgent,
			Message:    errorMessage(err),
			Detail:     fmt.Sprintf("%+v", err),
			StatusCode: status,
		})
		if status < http.StatusInternalServerError {
			WriteError(w, status, code, err.Error())
			return
		}
		rt.Log.Errorf("%s %s error: %s", category, label, errorMessage(err))
		WriteError(w, http.StatusInternalServerError, "INTERNAL", "Failed: "+label)
	}
}

func callRecovering(fn HandlerFunc, w http.ResponseWriter, r *http.Request) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if http.ErrAbortHandler == p {
				panic(p)
			}
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn(w, r)
}

// ErrorMessage is err.message as Node had it: errorMessage below. Routes that
// put an error's text in a response or a log line of their own use it.
func ErrorMessage(err error) string {
	return errorMessage(err)
}

// errorMessage is the text Node logged for err. For a Postgres error that is
// the server's message alone, as node-postgres puts it in err.message; pgx's
// Error() adds "ERROR: " and the SQLSTATE.
func errorMessage(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}
