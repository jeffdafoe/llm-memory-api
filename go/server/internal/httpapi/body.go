package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// MaxBodyBytes is the JSON body limit ('5mb' in the Node server's
// express.json, which body-parser reads as 5 MiB).
const MaxBodyBytes = 5 * 1024 * 1024

// ParseJSONBody reads an application/json body into State.Body, with the same
// rules as Express's express.json():
//
//   - Only requests that carry a body (a Content-Length or Transfer-Encoding
//     header) and whose media type is application/json are read. Others pass
//     through with a nil Body.
//   - An empty body becomes {}.
//   - The top-level value must be an object or array ("strict" mode).
//   - Over 5 MiB is 413, a charset other than utf-* or a Content-Encoding
//     other than identity is 415, malformed JSON is 400.
//
// A rejected body is answered by WriteUnhandledError and never reaches the
// routes or the request log, as in Node, where the parser's error skipped
// every later non-error middleware.
//
// Unlike body-parser, gzip and deflate request bodies are refused rather than
// inflated; no client sends them.
func ParseJSONBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasBody(r) || !isJSONType(r.Header.Get("Content-Type")) {
			next.ServeHTTP(w, r)
			return
		}
		body, status, err := readJSONBody(r)
		if err != nil {
			WriteUnhandledError(w, status, err.Error())
			return
		}
		StateOf(r).Body = body
		next.ServeHTTP(w, r)
	})
}

// hasBody mirrors type-is's hasBody: a Transfer-Encoding header, or a
// Content-Length header that parses as a number (including "0").
func hasBody(r *http.Request) bool {
	if len(r.TransferEncoding) > 0 || r.Header.Get("Transfer-Encoding") != "" || r.ContentLength > 0 {
		return true
	}
	_, ok := r.Header["Content-Length"]
	return ok
}

func isJSONType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

func readJSONBody(r *http.Request) (json.RawMessage, int, error) {
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" {
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported content encoding \"%s\"", encoding)
	}
	_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	charset := strings.ToLower(params["charset"])
	if charset != "" && !strings.HasPrefix(charset, "utf-") {
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported charset \"%s\"", strings.ToUpper(charset))
	}

	if r.ContentLength > MaxBodyBytes {
		return nil, http.StatusRequestEntityTooLarge, errors.New("request entity too large")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("request aborted")
	}
	if len(raw) > MaxBodyBytes {
		return nil, http.StatusRequestEntityTooLarge, errors.New("request entity too large")
	}

	trimmed := bytes.TrimLeft(raw, " \t\n\r")
	if len(trimmed) == 0 {
		return json.RawMessage("{}"), 0, nil
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return nil, http.StatusBadRequest, fmt.Errorf("Unexpected token '%c', body is not a JSON object or array", trimmed[0])
	}
	if !json.Valid(raw) {
		var probe any
		parseErr := json.Unmarshal(raw, &probe)
		message := "Invalid JSON body"
		if parseErr != nil {
			message = parseErr.Error()
		}
		return nil, http.StatusBadRequest, errors.New(message)
	}
	return json.RawMessage(raw), 0, nil
}
