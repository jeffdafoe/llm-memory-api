package events

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Path is the one request target server.js upgrades; any other upgrade
// request has its socket destroyed. It is compared with req.url, the target
// as sent, query string included.
const Path = "/admin/ws"

// IsUpgrade reports whether Node's http server hands r to its 'upgrade'
// listener instead of Express: an Upgrade header that is not empty and a
// Connection header with the "upgrade" token, whatever the method, path or
// HTTP version. server.js has an 'upgrade' listener, so such a request never
// reaches the JSON parser, the request log or any route.
func IsUpgrade(r *http.Request) bool {
	upgrade := false
	for _, v := range r.Header.Values("Upgrade") {
		if strings.TrimSpace(v) != "" {
			upgrade = true
		}
	}
	if !upgrade {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// nodeHeader is req.headers[name]: repeated header lines joined with ", ",
// and present false when there is none.
func nodeHeader(r *http.Request, name string) (value string, present bool) {
	values := r.Header.Values(name)
	if len(values) == 0 {
		return "", false
	}
	return strings.Join(values, ", "), true
}

// bearerToken is handleUpgrade's token: the browser sends
// new WebSocket(url, ['bearer', '<token>']), so the value after "bearer" in
// Sec-WebSocket-Protocol. Empty when there is none.
func bearerToken(r *http.Request) string {
	header, _ := nodeHeader(r, "Sec-Websocket-Protocol")
	parts := strings.Split(header, ",")
	for i, p := range parts {
		parts[i] = jsutil.Trim(p)
	}
	for i, p := range parts {
		if p == "bearer" {
			if i+1 < len(parts) {
				return parts[i+1]
			}
			return ""
		}
	}
	return ""
}

// ServeUpgrade answers an upgrade request (IsUpgrade) as server.js and
// events.js do: a target other than Path, or a token that is not a live admin
// web session, gets the socket closed (the token case after a bare
// "HTTP/1.1 401 Unauthorized"); then ws's handshake checks; then the
// handshake, with the first offered subprotocol echoed, and the welcome
// message.
func (h *Hub) ServeUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI != Path {
		destroy(w)
		return
	}

	username, ok := "", false
	if token := bearerToken(r); token != "" && h.Validate != nil {
		username, ok = h.Validate(r.Context(), token)
	}
	if !ok {
		writeRaw(w, "HTTP/1.1 401 Unauthorized\r\n\r\n")
		return
	}

	protocol, abort := checkHandshake(r)
	if abort != nil {
		writeRaw(w, abort.response())
		return
	}

	// ws accepts version 8 (the hybi-10 draft, the same frame format) and
	// 13; gorilla only 13, after the checks above have passed.
	r.Header.Set("Sec-Websocket-Version", "13")
	var header http.Header
	if protocol != "" {
		header = http.Header{"Sec-Websocket-Protocol": {protocol}}
	}
	upgrader := websocket.Upgrader{
		// ws has no origin check (no verifyClient), and the token is the
		// credential.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, header)
	if err != nil {
		// Upgrade has answered or closed the connection; the one case the
		// checks above do not exclude is a client that sent frames before
		// the 101.
		return
	}

	welcome, err := jsonutil.Marshal(envelope{Event: "connected", Data: welcomeData{User: username}})
	if err != nil {
		conn.Close()
		return
	}
	c := &client{conn: conn, send: make(chan []byte, sendQueue), alive: true, done: make(chan struct{})}
	if !h.add(c, welcome) {
		conn.Close()
		return
	}
	h.serve(c)
}

type welcomeData struct {
	User string `json:"user"`
}

// keyPattern is ws's keyRegex.
var keyPattern = regexp.MustCompile(`^[+/0-9A-Za-z]{22}==$`)

// handshakeAbort is a refusal ws writes with abortHandshake.
type handshakeAbort struct {
	code    int
	message string
	extra   [][2]string
}

// response is abortHandshake's bytes: the status line, Connection,
// Content-Type and Content-Length, any extra headers, and the message.
func (a *handshakeAbort) response() string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Type: text/html\r\nContent-Length: %d",
		a.code, http.StatusText(a.code), len(a.message))
	for _, h := range a.extra {
		fmt.Fprintf(&b, "\r\n%s: %s", h[0], h[1])
	}
	b.WriteString("\r\n\r\n")
	b.WriteString(a.message)
	return b.String()
}

// checkHandshake is the validation in ws 8.22.1's
// WebSocketServer.handleUpgrade, in its order, with perMessageDeflate off
// and no verifyClient (events.js's options). It returns the subprotocol to
// echo (the first offered) or the refusal.
func checkHandshake(r *http.Request) (protocol string, abort *handshakeAbort) {
	if r.Method != http.MethodGet {
		return "", &handshakeAbort{code: 405, message: "Invalid HTTP method"}
	}
	upgrade, ok := nodeHeader(r, "Upgrade")
	if !ok || jsutil.ToLower(upgrade) != "websocket" {
		return "", &handshakeAbort{code: 400, message: "Invalid Upgrade header"}
	}
	key, ok := nodeHeader(r, "Sec-Websocket-Key")
	if !ok || !keyPattern.MatchString(key) {
		return "", &handshakeAbort{code: 400, message: "Missing or invalid Sec-WebSocket-Key header"}
	}
	version := math.NaN()
	if v, ok := nodeHeader(r, "Sec-Websocket-Version"); ok {
		version = jsutil.StringToNumber(v)
	}
	if version != 13 && version != 8 {
		return "", &handshakeAbort{
			code: 400, message: "Missing or invalid Sec-WebSocket-Version header",
			extra: [][2]string{{"Sec-WebSocket-Version", "13, 8"}},
		}
	}
	if header, ok := nodeHeader(r, "Sec-Websocket-Protocol"); ok {
		protocols, err := parseSubprotocols(header)
		if err != nil {
			return "", &handshakeAbort{code: 400, message: "Invalid Sec-WebSocket-Protocol header"}
		}
		protocol = protocols[0]
	}
	return protocol, nil
}

// isTokenChar is ws's tokenChars table: the RFC 7230 tchar set.
func isTokenChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// parseSubprotocols is ws's subprotocol.parse: comma-separated tokens with
// optional spaces or tabs around them (none before the first), no empty or
// repeated name, nothing after the last. The names come back in order.
func parseSubprotocols(header string) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	start, end := -1, -1
	add := func(name string) error {
		if seen[name] {
			return fmt.Errorf("The %q subprotocol is duplicated", name)
		}
		seen[name] = true
		names = append(names, name)
		return nil
	}
	// ws walks UTF-16 code units; every byte that is not ASCII fails as a
	// non-token character either way, so bytes give the same verdict.
	i := 0
	for ; i < len(header); i++ {
		c := header[i]
		switch {
		case end == -1 && c < 0x80 && isTokenChar(c):
			if start == -1 {
				start = i
			}
		case i != 0 && (c == ' ' || c == '\t'):
			if end == -1 && start != -1 {
				end = i
			}
		case c == ',':
			if start == -1 {
				return nil, fmt.Errorf("Unexpected character at index %d", i)
			}
			if end == -1 {
				end = i
			}
			if err := add(header[start:end]); err != nil {
				return nil, err
			}
			start, end = -1, -1
		default:
			return nil, fmt.Errorf("Unexpected character at index %d", i)
		}
	}
	if start == -1 || end != -1 {
		return nil, fmt.Errorf("Unexpected end of input")
	}
	if err := add(header[start:i]); err != nil {
		return nil, err
	}
	return names, nil
}

// destroy is socket.destroy(): the connection closes with no response.
func destroy(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	conn.Close()
}

// writeRaw writes raw bytes to the socket and closes it, as socket.write or
// socket.end followed by destroy.
func writeRaw(w http.ResponseWriter, raw string) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	writeAndFlush(buf, raw)
}

func writeAndFlush(buf *bufio.ReadWriter, raw string) {
	if _, err := buf.WriteString(raw); err == nil {
		buf.Flush()
	}
}

// discard drains one incoming message.
func discard(r io.Reader) (int64, error) {
	return io.Copy(io.Discard, r)
}
