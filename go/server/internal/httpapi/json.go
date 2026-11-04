// Package httpapi holds the HTTP plumbing every route shares: JSON responses,
// the route error wrapper, the per-request auth state, and the middleware
// that runs ahead of the routes.
package httpapi

import (
	"net/http"
	"strconv"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
)

// WriteJSON sends v with the headers Express sets for res.json.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := jsonutil.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"INTERNAL_ERROR","message":"An internal error occurred"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
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

// WriteError sends {"error": {"code": code, "message": message}}.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}
