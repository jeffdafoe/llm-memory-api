package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// requestLogExcluded are path prefixes left out of request_log: dashboard
// polling endpoints, the admin static files and the health check.
var requestLogExcluded = []string{
	"/v1/admin/api-log",
	"/v1/admin/error-log",
	"/v1/admin/dashboard",
	"/v1/admin/notes/reindex-status",
	"/v1/admin/notes/reindex-clear",
	"/admin/",
	"/health",
}

// ClientAddress is the caller's address: the LAST X-Forwarded-For hop, else
// the TCP peer. nginx appends the address it actually saw after any hops the
// client sent, so earlier entries can be forged (scanners send
// "X-Forwarded-For: 127.0.0.1"). The server sits exactly one trusted proxy
// deep, so the last hop is the real peer.
func ClientAddress(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		hops := strings.Split(xff, ",")
		last := strings.TrimSpace(hops[len(hops)-1])
		if last != "" {
			return last
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RequestLog writes one request_log row per request after the response is
// sent. The insert runs in the background; a failed insert is dropped.
type RequestLog struct {
	DB db.DB
}

func (rl *RequestLog) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Express's originalUrl: the path plus the query string.
		path := r.URL.RequestURI()
		for _, prefix := range requestLogExcluded {
			if strings.HasPrefix(path, prefix) {
				next.ServeHTTP(w, r)
				return
			}
		}

		start := time.Now()
		st := StateOf(r)
		displayPath := path
		if path == "/mcp" {
			displayPath = mcpDisplayPath(st.Body, path)
		}

		var requestLength *int
		if header := r.Header.Get("Content-Length"); header != "" {
			n, err := strconv.Atoi(strings.TrimSpace(header))
			if err == nil {
				requestLength = &n
			}
		}

		rec := &recordingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		var actorID *int
		if st.ActorID != 0 {
			actorID = &st.ActorID
		} else if st.MCPActorID != 0 {
			actorID = &st.MCPActorID
		} else if st.AuthenticatedUser != nil {
			// An admin session with no actor id. The session middleware always
			// sets ActorID with the user, so this does not happen in practice;
			// it keeps the Node rule that admin traffic is not logged.
			return
		}

		var responseLength *int
		if header := rec.Header().Get("Content-Length"); header != "" {
			n, err := strconv.Atoi(header)
			if err == nil {
				responseLength = &n
			}
		}
		if responseLength == nil && rec.bytes > 0 {
			n := rec.bytes
			responseLength = &n
		}

		durationMS := int(time.Since(start) / time.Millisecond)
		method := r.Method
		status := rec.status
		ip := ClientAddress(r)
		db.Background(func(ctx context.Context) {
			rl.DB.Exec(ctx,
				`INSERT INTO request_log (method, path, status, duration_ms, actor_id, ip, request_length, response_length)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				method, displayPath, status, durationMS, actorID, ip, requestLength, responseLength)
		})
	})
}

// mcpDisplayPath names the JSON-RPC method of an MCP request, and the tool for
// tools/call, so request_log shows "/mcp → save_note" instead of "/mcp".
func mcpDisplayPath(body json.RawMessage, path string) string {
	if body == nil {
		return path
	}
	var rpc struct {
		Method json.RawMessage `json:"method"`
		Params struct {
			Name json.RawMessage `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &rpc) != nil {
		return path
	}
	method := jsText(rpc.Method)
	if method == "" {
		return path
	}
	if method == "tools/call" {
		name := jsText(rpc.Params.Name)
		if name != "" {
			return path + " → " + name
		}
	}
	return path + " → " + method
}

// jsText renders a JSON value the way JavaScript string concatenation would,
// and returns "" for the values JavaScript treats as false (missing, null,
// false, 0, "").
func jsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	text := string(raw)
	if text == "null" || text == "false" || text == "0" {
		return ""
	}
	return text
}

// recordingWriter captures the status and the number of body bytes written.
type recordingWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

// WriteHeader records the status the client gets. An informational status
// (1xx other than 101) is sent ahead of the final one, as net/http does, so
// it is not recorded; a final status after the first is ignored by net/http
// and here.
func (rw *recordingWriter) WriteHeader(status int) {
	if rw.wroteHeader {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		rw.ResponseWriter.WriteHeader(status)
		return
	}
	rw.status = status
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *recordingWriter) Write(b []byte) (int, error) {
	rw.wroteHeader = true
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += n
	return n, err
}

// Flush and Hijack pass through so streaming responses (SSE) and the admin
// WebSocket upgrade still work behind the request log. A flush sends the
// headers, so it commits the status as it stands (200 if none was set).
func (rw *recordingWriter) Flush() {
	rw.wroteHeader = true
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *recordingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	return h.Hijack()
}

func (rw *recordingWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
