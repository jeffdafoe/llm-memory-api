package httpapi

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
)

// WriteUnhandledError is the Node server's last-resort error handler
// (server.js): a body-parser failure or an error no route caught. 500 gets
// INTERNAL_ERROR, every other status BAD_REQUEST.
func WriteUnhandledError(w http.ResponseWriter, status int, message string) {
	code := "BAD_REQUEST"
	if status == http.StatusInternalServerError {
		code = "INTERNAL_ERROR"
	}
	if message == "" {
		message = "An internal error occurred"
	}
	WriteError(w, status, code, message)
}

// Recover turns a panic outside a Route into the last-resort 500 response,
// the way an uncaught error reached Express's error handler.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			fmt.Fprintf(os.Stderr, "Unhandled error: %v\n%s\n", p, debug.Stack())
			WriteUnhandledError(w, http.StatusInternalServerError, fmt.Sprint(p))
		}()
		next.ServeHTTP(w, r)
	})
}

// NotFound answers an unmatched route with the page Express's finalhandler
// sent, so clients see the same 404 the Node server gave.
func NotFound(w http.ResponseWriter, r *http.Request) {
	body := "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Error</title>\n</head>\n<body>\n<pre>" +
		"Cannot " + html.EscapeString(r.Method) + " " + html.EscapeString(r.URL.EscapedPath()) +
		"</pre>\n</body>\n</html>\n"
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(body))
}
