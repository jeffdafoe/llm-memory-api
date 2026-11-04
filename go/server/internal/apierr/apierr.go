// Package apierr is the error type services return to carry an HTTP status.
// It has no dependencies so that every package can use it without import
// cycles. The Node services did the same by throwing an Error with a
// statusCode (and optionally a code) property.
package apierr

import (
	"errors"
	"net/http"
)

type Error struct {
	Status int
	// Code overrides the code derived from Status (see DefaultCode).
	Code    string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

func New(status int, message string) *Error {
	return &Error{Status: status, Message: message}
}

func WithCode(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

var defaultCodes = map[int]string{
	http.StatusBadRequest:      "BAD_REQUEST",
	http.StatusUnauthorized:    "UNAUTHORIZED",
	http.StatusForbidden:       "FORBIDDEN",
	http.StatusNotFound:        "NOT_FOUND",
	http.StatusConflict:        "CONFLICT",
	http.StatusTooManyRequests: "RATE_LIMITED",
}

// DefaultCode is the error code for a status when the error names none.
func DefaultCode(status int) string {
	code, ok := defaultCodes[status]
	if ok {
		return code
	}
	return "ERROR"
}

// StatusAndCode returns the status and code an error maps to. Errors that are
// not *Error are 500s.
func StatusAndCode(err error) (int, string) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		status := apiErr.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		code := apiErr.Code
		if code == "" {
			code = DefaultCode(status)
		}
		return status, code
	}
	return http.StatusInternalServerError, DefaultCode(http.StatusInternalServerError)
}
