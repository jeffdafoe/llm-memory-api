// Package httpapi holds the HTTP plumbing every route shares: JSON responses,
// the route error wrapper, the per-request auth state, and the middleware
// that runs ahead of the routes.
package httpapi

import (
	"net/http"
	"strconv"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
)

// WriteJSON sends v with the headers Express sets for res.json. When v cannot
// be encoded nothing is written and the error is returned, so a route that
// returns it gets the usual 500 from Route, as a throwing res.json did in
// Node.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	body, err := jsonutil.Marshal(v)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
	return nil
}

// ErrorBody is the {"error": {"code", "message"}} shape used by almost every
// non-OAuth error response.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError sends {"error": {"code": code, "message": message}}. Two strings
// always encode, so it cannot fail.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}
