package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// The protocol layer: what createMcpHandler (@modelcontextprotocol/server
// 2.4.0, legacy mode "stateless") and toNodeHandler (@modelcontextprotocol/
// node 2.1.2) answered for each request, ported from the SDK's behavior. The
// SDK's own texts, status codes and key orders are reproduced; the go-server
// note ("MCP endpoint") has the decision table.

const (
	// latestProtocolVersion is the SDK's LATEST_PROTOCOL_VERSION: what HEAD
	// advertises and what initialize answers for a version it does not know.
	latestProtocolVersion = "2025-11-25"
	modernVersion         = "2026-07-28"

	pvKey         = "io.modelcontextprotocol/protocolVersion"
	clientInfoKey = "io.modelcontextprotocol/clientInfo"
	ccKey         = "io.modelcontextprotocol/clientCapabilities"
	logLevelKey   = "io.modelcontextprotocol/logLevel"
	serverInfoKey = "io.modelcontextprotocol/serverInfo"
	subIDKey      = "io.modelcontextprotocol/subscriptionId"

	// instructions is deliberately only a pointer at read_instructions:
	// claude.ai drops this field (anthropics/claude-ai-mcp#93), so the
	// read_instructions tool description carries the same pointer.
	instructions = "At the start of every conversation, call the read_instructions tool to load your context and instructions. Follow whatever it returns."

	serverName    = "llm-memory"
	serverVersion = "2.0.0"

	maxBatchSize = 100
	// maxReadBody bounds a body toNodeHandler or the SDK read themselves
	// (one express.json did not parse): DEFAULT_MAX_REQUEST_BODY_SIZE.
	maxReadBody = 4 * 1024 * 1024

	keepAliveInterval = 15 * time.Second
	// toolTimeout bounds one request's work once it is detached from the
	// client: Node finished a handler whose client had gone, with no bound.
	toolTimeout = 5 * time.Minute
)

var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"}

var logLevels = []string{"debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"}

// ServeHEAD answers HEAD /mcp: protocol discovery, with no auth.
func ServeHEAD(w http.ResponseWriter) {
	w.Header().Set("Mcp-Protocol-Version", latestProtocolVersion)
	w.WriteHeader(http.StatusOK)
}

// ServeHTTP answers /mcp after auth.MCPAuth.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := httpapi.StateOf(r)
	c := caller{agent: st.MCPAgent, actorID: st.MCPActorID, namespace: st.MCPAgent, permissions: st.MCPPermissions}
	status := s.serve(w, r, c)

	// Connect side effects run only once the answer was 200, so a refused
	// handshake neither logs a connect nor lights the spinner.
	if status == http.StatusOK && r.Method == http.MethodPost && st.Body != nil {
		if connect, ok := describeConnect(st.Body); ok {
			s.logConnect(r, c, connect)
			s.activateOnConnect(c)
		}
	}
}

// --- responses ---

func newObject(kv ...any) *jsutil.Object {
	o := jsutil.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func errorObject(code int, message string, data any) *jsutil.Object {
	e := newObject("code", code, "message", message)
	if data != nil {
		e.Set("data", data)
	}
	return e
}

// entryError is an error built by the SDK's entry or transport:
// {"jsonrpc","error","id"}.
func entryError(code int, message string, data any, id any) *jsutil.Object {
	return newObject("jsonrpc", "2.0", "error", errorObject(code, message, data), "id", id)
}

// dispatchError is an error built by the protocol dispatcher:
// {"jsonrpc","id","error"}.
func dispatchError(id any, code int, message string, data any) *jsutil.Object {
	return newObject("jsonrpc", "2.0", "id", id, "error", errorObject(code, message, data))
}

func success(id any, result *jsutil.Object) *jsutil.Object {
	return newObject("result", result, "jsonrpc", "2.0", "id", id)
}

func writeJSONRPC(w http.ResponseWriter, status int, body *jsutil.Object) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, jsutil.Marshal(body))
	return status
}

func write405(w http.ResponseWriter) int {
	w.Header().Set("Allow", "POST")
	return writeJSONRPC(w, http.StatusMethodNotAllowed, entryError(-32000, "Method not allowed.", nil, nil))
}

// reportHandlerError is the handler's onerror: logError('mcp', 'handler').
func (s *Server) reportHandlerError(message string) {
	s.Log.LogError("mcp", "handler", logger.ErrorOpts{Message: message, Detail: "Error: " + message, StatusCode: 500})
}

// --- entry ---

func (s *Server) serve(w http.ResponseWriter, r *http.Request, c caller) int {
	// Every method but POST is a 2025-era session operation (GET, DELETE),
	// which a stateless server refuses.
	if r.Method != http.MethodPost {
		return write405(w)
	}

	st := httpapi.StateOf(r)
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		if st.Body == nil && bodyTooLarge(r) {
			return writeTooLarge(w)
		}
		message := "Unsupported Media Type: Content-Type must be application/json"
		s.reportHandlerError(message)
		return writeJSONRPC(w, http.StatusUnsupportedMediaType, entryError(-32000, message, nil, nil))
	}

	var body any
	if st.Body != nil {
		v, err := jsutil.Parse(st.Body)
		if err != nil {
			return writeJSONRPC(w, http.StatusBadRequest, entryError(-32700, "Parse error: Invalid JSON", nil, nil))
		}
		body = v
	} else {
		// A Content-Type express.json did not parse but the SDK reads as
		// JSON: toNodeHandler and the SDK read the body themselves. One that
		// is empty or not JSON goes to the 2025-era transport, which
		// refuses it.
		raw, tooLarge, err := readBody(r)
		if tooLarge {
			return writeTooLarge(w)
		}
		if err != nil {
			return writeJSONRPC(w, http.StatusBadRequest, entryError(-32700, "Parse error: the request body could not be read", nil, nil))
		}
		v, err := jsutil.Parse([]byte(httpapi.DecodeUTF8(raw)))
		if len(raw) == 0 || err != nil {
			if !acceptsBoth(r) {
				return writeNotAcceptable(w)
			}
			return writeJSONRPC(w, http.StatusBadRequest, entryError(-32700, "Parse error: Invalid JSON", nil, nil))
		}
		body = v
	}

	h := readHeaders(r)
	out := classify(body, h)
	switch out.kind {
	case outcomeReject:
		s.reportHandlerError("Rejected inbound request (" + out.cell + "): " + out.message)
		return writeJSONRPC(w, out.status, entryError(out.code, out.message, out.data, echoableID(body)))
	case outcomeModern:
		return s.serveModern(w, r, c, body, h, out)
	}
	return s.serveLegacy(w, r, c, body, h)
}

func bodyTooLarge(r *http.Request) bool {
	if r.ContentLength > maxReadBody {
		return true
	}
	_, tooLarge, _ := readBody(r)
	return tooLarge
}

func readBody(r *http.Request) ([]byte, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxReadBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(raw) > maxReadBody {
		return nil, true, nil
	}
	return raw, false, nil
}

func writeTooLarge(w http.ResponseWriter) int {
	w.Header().Set("Connection", "close")
	return writeJSONRPC(w, http.StatusRequestEntityTooLarge, entryError(-32000,
		fmt.Sprintf("Payload Too Large: Request body must not exceed %d bytes", maxReadBody), nil, nil))
}

// isJSONContentType is the SDK's isJsonContentType: the media type of the
// header is application/json, parameters ignored.
func isJSONContentType(ct string) bool {
	if ct == "application/json" {
		return true
	}
	essence, ok := mediaTypeEssence(ct)
	return ok && essence == "application/json"
}

// mediaTypeEssence is the SDK's mediaTypeEssence: the lower-cased type that
// content-type 1.0.5's parse reads. When parse refuses the header, the text
// before the first ";" counts, unless it is empty or the rest of the header
// holds a "," (two joined headers). The header is read as Node holds it, one
// character per byte.
func mediaTypeEssence(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	h := []rune{}
	for i := 0; i < len(header); i++ {
		h = append(h, rune(header[i]))
	}
	if essence, ok := parseMediaType(h); ok {
		return essence, true
	}
	first := h
	for i, r := range h {
		if r == ';' {
			first = h[:i]
			break
		}
	}
	essence := jsutil.ToLower(jsutil.Trim(string(first)))
	n := len([]rune(essence))
	if essence == "" || (n <= len(h) && strings.ContainsRune(string(h[n:]), ',')) {
		return "", false
	}
	return essence, true
}

func isTokenChar(r rune) bool {
	switch {
	case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		return true
	}
	return strings.ContainsRune("!#$%&'*+.^_`|~-", r)
}

// parseMediaType is content-type's parse: a type/subtype of token
// characters (trimmed), then only "; key=value" parameters to the end, a
// value being a token or a quoted string.
func parseMediaType(h []rune) (string, bool) {
	semi := -1
	for i, r := range h {
		if r == ';' {
			semi = i
			break
		}
	}
	typ := h
	if semi != -1 {
		typ = h[:semi]
	}
	t := []rune(jsutil.Trim(string(typ)))
	slash := -1
	for i, r := range t {
		if r == '/' {
			slash = i
			break
		}
	}
	if slash <= 0 || slash == len(t)-1 {
		return "", false
	}
	for i, r := range t {
		if i != slash && !isTokenChar(r) {
			return "", false
		}
	}
	if semi == -1 {
		return jsutil.ToLower(string(t)), true
	}
	pos := semi
	for pos < len(h) {
		next, ok := matchParam(h, pos)
		if !ok {
			return "", false
		}
		pos = next
	}
	return jsutil.ToLower(string(t)), true
}

// matchParam matches PARAM_REGEXP (`; *token *= *(quoted|token) *`) at pos.
func matchParam(h []rune, pos int) (int, bool) {
	if h[pos] != ';' {
		return 0, false
	}
	i := pos + 1
	spaces := func() {
		for i < len(h) && h[i] == ' ' {
			i++
		}
	}
	spaces()
	start := i
	for i < len(h) && isTokenChar(h[i]) {
		i++
	}
	if i == start {
		return 0, false
	}
	spaces()
	if i >= len(h) || h[i] != '=' {
		return 0, false
	}
	i++
	spaces()
	if i < len(h) && h[i] == '"' {
		i++
		for {
			if i >= len(h) {
				return 0, false
			}
			r := h[i]
			if r == '"' {
				i++
				break
			}
			if r == '\\' {
				if i+1 < len(h) && (h[i+1] == 0x0b || h[i+1] >= 0x20 && h[i+1] <= 0xff) {
					i += 2
					continue
				}
				return 0, false
			}
			if r == 0x0b || r == 0x20 || r == 0x21 || r >= 0x23 && r <= 0x5b || r >= 0x5d && r <= 0x7e || r >= 0x80 && r <= 0xff {
				i++
				continue
			}
			return 0, false
		}
	} else {
		start := i
		for i < len(h) && isTokenChar(h[i]) {
			i++
		}
		if i == start {
			return 0, false
		}
	}
	spaces()
	return i, true
}

// headers are the standard MCP request headers, OWS-stripped; a duplicate
// header is joined with ", " as Node joined it.
type headers struct {
	version, method, name string
	hasVersion, hasMethod bool
	hasName               bool
}

func headerValue(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	if len(values) == 0 {
		return "", false
	}
	return strings.Trim(strings.Join(values, ", "), " \t"), true
}

func readHeaders(r *http.Request) headers {
	var h headers
	h.version, h.hasVersion = headerValue(r, "Mcp-Protocol-Version")
	h.method, h.hasMethod = headerValue(r, "Mcp-Method")
	h.name, h.hasName = headerValue(r, "Mcp-Name")
	return h
}

// isModern is isModernProtocolVersion: a plain string comparison.
func isModern(v string) bool {
	return v >= modernVersion
}

// echoableID is the id an entry error carries: the body's id when the body
// is one object with a string method and a string or number id.
func echoableID(body any) any {
	o, ok := body.(*jsutil.Object)
	if !ok {
		return nil
	}
	if _, ok := o.Value("method").(string); !ok {
		return nil
	}
	switch id := o.Value("id").(type) {
	case string:
		return id
	case json.Number:
		return id
	}
	return nil
}

// --- connect log ---

type connectInfo struct {
	era, method     string
	protocolVersion any
	clientInfo      any
}

// describeConnect is Node's describeConnect: initialize opens a 2025-era
// connection, server/discover with the protocol-version claim a 2026-07-28
// one.
func describeConnect(raw json.RawMessage) (connectInfo, bool) {
	v, err := jsutil.Parse(raw)
	if err != nil {
		return connectInfo{}, false
	}
	body, ok := v.(*jsutil.Object)
	if !ok {
		return connectInfo{}, false
	}
	method := body.Value("method")
	params := body.Value("params")
	if !jsutil.Truthy(params) {
		params = jsutil.NewObject()
	}
	meta := member(params, "_meta")
	if !jsutil.Truthy(meta) {
		meta = jsutil.NewObject()
	}
	if method == "initialize" {
		info := member(params, "clientInfo")
		if !jsutil.Truthy(info) {
			info = jsutil.NewObject()
		}
		return connectInfo{era: "legacy", method: "initialize", protocolVersion: member(params, "protocolVersion"), clientInfo: info}, true
	}
	if method == "server/discover" && member(meta, pvKey) != jsutil.Undefined {
		info := member(meta, clientInfoKey)
		if !jsutil.Truthy(info) {
			info = jsutil.NewObject()
		}
		return connectInfo{era: "modern", method: "server/discover", protocolVersion: member(meta, pvKey), clientInfo: info}, true
	}
	return connectInfo{}, false
}

// member is v[k] for an object value, else undefined (a string's or
// number's own members are not modeled; none is read here).
func member(v any, k string) any {
	if o, ok := v.(*jsutil.Object); ok {
		return o.Value(k)
	}
	return jsutil.Undefined
}

// capString is a client-supplied string cut to 200 UTF-16 units, else null.
func capString(v any) any {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return jsutil.TruncateUTF16(s, 200)
}

// logConnect writes one line per client connect, to stdout only (Node's
// console.log).
func (s *Server) logConnect(r *http.Request, c caller, connect connectInfo) {
	line := newObject(
		"agent", c.agent,
		"ip", capString(httpapi.ClientAddress(r)),
		"era", connect.era,
		"method", connect.method,
		"protocolVersion", capString(connect.protocolVersion),
		"clientName", capString(member(connect.clientInfo, "name")),
		"clientVersion", capString(member(connect.clientInfo, "version")),
		"userAgent", capString(userAgent(r)),
	)
	fmt.Fprintln(s.stdout(), "[mcp] connect "+jsutil.Marshal(line))
}

func userAgent(r *http.Request) any {
	if v, ok := headerValue(r, "User-Agent"); ok {
		return v
	}
	return jsutil.Undefined
}

// activateOnConnect turns the activity spinner on for a client that connects,
// so one that never calls activity_start still shows as active.
func (s *Server) activateOnConnect(c caller) {
	s.refreshActivity(c)
}

// --- classification ---

type outcomeKind int

const (
	outcomeLegacy outcomeKind = iota
	outcomeModern
	outcomeReject
)

type outcome struct {
	kind outcomeKind
	// Reject: the cell (for the log line), HTTP status and JSON-RPC error.
	cell    string
	status  int
	code    int
	message string
	data    any
	// Modern: the revision the request or notification names.
	revision     string
	notification bool
}

func reject(cell string, status, code int, message string, data any) outcome {
	return outcome{kind: outcomeReject, cell: cell, status: status, code: code, message: message, data: data}
}

func mismatch(cell, header, detail string) outcome {
	return reject(cell, http.StatusBadRequest, -32020, "Bad Request: the request headers and body disagree: "+detail,
		newObject("mismatch", newObject("header", header, "body", detail)))
}

func onlyKeys(o *jsutil.Object, allowed ...string) bool {
	for _, k := range o.Keys() {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// isID is the RequestId schema: a string or a safe integer.
func isID(v any) bool {
	switch x := v.(type) {
	case string:
		return true
	case json.Number:
		f, ok := jsutil.AsFloat(x)
		return ok && f == float64(int64(f)) && f <= 9007199254740991 && f >= -9007199254740991
	}
	return false
}

// validParams: params, when present, is an object whose _meta, when
// present, is an object with a string or number progressToken.
func validParams(o *jsutil.Object) bool {
	p, has := o.Get("params")
	if !has {
		return true
	}
	po, ok := p.(*jsutil.Object)
	if !ok {
		return false
	}
	m, has := po.Get("_meta")
	if !has {
		return true
	}
	mo, ok := m.(*jsutil.Object)
	if !ok {
		return false
	}
	if t, has := mo.Get("progressToken"); has {
		switch t.(type) {
		case string, json.Number:
		default:
			return false
		}
	}
	return true
}

func isRequest(v any) bool {
	o, ok := v.(*jsutil.Object)
	if !ok || !onlyKeys(o, "jsonrpc", "id", "method", "params") || o.Value("jsonrpc") != "2.0" {
		return false
	}
	if _, ok := o.Value("method").(string); !ok {
		return false
	}
	return o.Has("id") && isID(o.Value("id")) && validParams(o)
}

func isNotification(v any) bool {
	o, ok := v.(*jsutil.Object)
	if !ok || !onlyKeys(o, "jsonrpc", "method", "params") || o.Value("jsonrpc") != "2.0" {
		return false
	}
	if _, ok := o.Value("method").(string); !ok {
		return false
	}
	return validParams(o)
}

func isResponse(v any) bool {
	o, ok := v.(*jsutil.Object)
	if !ok || o.Value("jsonrpc") != "2.0" {
		return false
	}
	if onlyKeys(o, "jsonrpc", "id", "result") && o.Has("result") {
		_, isObj := o.Value("result").(*jsutil.Object)
		return isObj && o.Has("id") && isID(o.Value("id"))
	}
	if onlyKeys(o, "jsonrpc", "id", "error") && o.Has("error") {
		if o.Has("id") && !isID(o.Value("id")) {
			return false
		}
		e, ok := o.Value("error").(*jsutil.Object)
		if !ok {
			return false
		}
		_, msgOK := e.Value("message").(string)
		return isID(e.Value("code")) && isIntegerCode(e.Value("code")) && msgOK
	}
	return false
}

func isIntegerCode(v any) bool {
	_, ok := v.(json.Number)
	return ok
}

// meta is params._meta when both are objects.
func meta(v any) *jsutil.Object {
	m, _ := member(member(v, "params"), "_meta").(*jsutil.Object)
	return m
}

func hasClaim(v any) bool {
	m := meta(v)
	return m != nil && m.Has(pvKey)
}

func classify(body any, h headers) outcome {
	if batch, ok := body.([]any); ok {
		if len(batch) == 0 {
			return reject("empty-batch", http.StatusBadRequest, -32600, "Bad Request: empty JSON-RPC batch", nil)
		}
		for _, m := range batch {
			if hasClaim(m) {
				return reject("batch-with-modern-element", http.StatusBadRequest, -32600,
					"Bad Request: JSON-RPC batches may not contain requests for protocol revision 2026-07-28 or later", nil)
			}
			if !isRequest(m) && !isNotification(m) && !isResponse(m) {
				return reject("batch-with-invalid-element", http.StatusBadRequest, -32600,
					"Bad Request: JSON-RPC batch contains an invalid message", nil)
			}
		}
		return outcome{kind: outcomeLegacy}
	}
	if isResponse(body) {
		return outcome{kind: outcomeLegacy}
	}
	if isRequest(body) {
		return classifyRequest(body.(*jsutil.Object), h)
	}
	if isNotification(body) {
		return classifyNotification(body.(*jsutil.Object), h)
	}
	return reject("invalid-json-rpc-body", http.StatusBadRequest, -32600, "Bad Request: the request body is not a valid JSON-RPC message", nil)
}

func classifyRequest(req *jsutil.Object, h headers) outcome {
	method := req.Value("method").(string)
	m := meta(req)
	claimed := m != nil && m.Has(pvKey)
	if method == "initialize" && !validModernClaim(m) {
		if h.hasVersion && isModern(h.version) {
			return mismatch("initialize-with-modern-header", h.version,
				"an initialize request (legacy handshake) was sent with a modern MCP-Protocol-Version header")
		}
		return outcome{kind: outcomeLegacy}
	}
	if claimed {
		if key, problem, bad := envelopeIssue(m); bad {
			return reject("envelope-invalid", http.StatusBadRequest, -32602,
				"Invalid _meta envelope for protocol revision 2026-07-28: "+key+": "+problem,
				newObject("envelope", newObject("key", key, "problem", problem)))
		}
		claim := m.Value(pvKey).(string)
		if h.hasVersion && h.version != claim {
			return mismatch("header-body-version-mismatch", h.version,
				"the body envelope names protocol version "+claim+" but the MCP-Protocol-Version header names "+h.version)
		}
		if h.hasMethod && h.method != method {
			return mismatch("method-header-mismatch", h.method,
				"the body names method "+method+" but the Mcp-Method header names "+h.method)
		}
		return outcome{kind: outcomeModern, revision: claim}
	}
	if h.hasVersion && isModern(h.version) {
		var missing []any
		if m == nil {
			missing = []any{"_meta"}
		} else {
			for _, k := range []string{pvKey, ccKey} {
				if !m.Has(k) {
					missing = append(missing, k)
				}
			}
			if len(missing) == 0 {
				missing = []any{pvKey}
			}
		}
		names := make([]string, len(missing))
		for i, k := range missing {
			names[i] = k.(string)
		}
		return reject("modern-header-without-claim", http.StatusBadRequest, -32602,
			"Invalid params: the MCP-Protocol-Version header names protocol revision "+h.version+
				", but the request is missing the required per-request envelope key(s): "+strings.Join(names, ", "),
			newObject("envelope", newObject("missing", missing)))
	}
	return outcome{kind: outcomeLegacy}
}

func classifyNotification(n *jsutil.Object, h headers) outcome {
	method := n.Value("method").(string)
	m := meta(n)
	if m != nil && m.Has(pvKey) {
		claim, ok := m.Value(pvKey).(string)
		if !ok {
			problem := issue{expected: "string", received: receivedName(m.Value(pvKey))}.message()
			return reject("notification-envelope-invalid", http.StatusBadRequest, -32602,
				"Invalid _meta envelope for protocol revision 2026-07-28: "+pvKey+": "+problem,
				newObject("envelope", newObject("key", pvKey, "problem", problem)))
		}
		if h.hasVersion && h.version != claim {
			return mismatch("notification-header-body-version-mismatch", h.version,
				"the notification envelope names protocol version "+claim+" but the MCP-Protocol-Version header names "+h.version)
		}
		if isModern(claim) && h.hasMethod && h.method != method {
			return mismatch("notification-method-header-mismatch", h.method,
				"the notification body names method "+method+" but the Mcp-Method header names "+h.method)
		}
		return outcome{kind: outcomeModern, revision: claim, notification: true}
	}
	if h.hasVersion && isModern(h.version) {
		if h.hasMethod && h.method != method {
			return mismatch("notification-method-header-mismatch", h.method,
				"the notification body names method "+method+" but the Mcp-Method header names "+h.method)
		}
		return outcome{kind: outcomeModern, revision: h.version, notification: true}
	}
	return outcome{kind: outcomeLegacy}
}

// validModernClaim: the claim names a modern version and the envelope has
// no issue.
func validModernClaim(m *jsutil.Object) bool {
	if m == nil || !m.Has(pvKey) {
		return false
	}
	v, ok := m.Value(pvKey).(string)
	if !ok || !isModern(v) {
		return false
	}
	_, _, bad := envelopeIssue(m)
	return !bad
}

// envelopeIssue is validateEnvelopeMeta's first issue: a missing required
// key, then the envelope schema's checks in its key order. Inside
// clientInfo and clientCapabilities only the members' JSON types are
// checked.
func envelopeIssue(m *jsutil.Object) (key, problem string, bad bool) {
	for _, k := range []string{pvKey, ccKey} {
		if !m.Has(k) {
			return k, "missing", true
		}
	}
	var issues []issue
	if t, has := m.Get("progressToken"); has {
		switch t.(type) {
		case string, json.Number:
		default:
			return "progressToken", "Invalid input", true
		}
	}
	if issues, _ = checkType(issues, m.Value(pvKey), "string", false, pvKey); len(issues) > 0 {
		return issues[0].path[0], issues[0].message(), true
	}
	if info := m.Value(clientInfoKey); info != jsutil.Undefined {
		if issues, _ = checkType(issues, info, "object", false, clientInfoKey); len(issues) > 0 {
			return clientInfoKey, issues[0].message(), true
		}
		if is := implementationIssues(info.(*jsutil.Object), clientInfoKey); len(is) > 0 {
			return strings.Join(is[0].path, "."), is[0].message(), true
		}
	}
	cc := m.Value(ccKey)
	if issues, _ = checkType(issues, cc, "object", false, ccKey); len(issues) > 0 {
		return ccKey, issues[0].message(), true
	}
	ccObj := cc.(*jsutil.Object)
	for _, field := range []string{"experimental", "sampling", "elicitation", "roots", "extensions"} {
		v := ccObj.Value(field)
		if issues, _ = checkType(issues, v, "object", true, ccKey, field); len(issues) > 0 {
			return strings.Join(issues[0].path, "."), issues[0].message(), true
		}
		if field == "experimental" || field == "extensions" {
			if o, ok := v.(*jsutil.Object); ok {
				for _, k := range o.Keys() {
					if issues, _ = checkType(issues, o.Value(k), "object", false, ccKey, field, k); len(issues) > 0 {
						return strings.Join(issues[0].path, "."), issues[0].message(), true
					}
				}
			}
		}
	}
	if level, has := m.Get(logLevelKey); has {
		s, _ := level.(string)
		valid := false
		for _, l := range logLevels {
			if s == l && level != nil {
				valid = true
			}
		}
		if !valid {
			quoted := make([]string, len(logLevels))
			for i, l := range logLevels {
				quoted[i] = `"` + l + `"`
			}
			return logLevelKey, "Invalid option: expected one of " + strings.Join(quoted, "|"), true
		}
	}
	return "", "", false
}

// implementationIssues checks an Implementation (clientInfo): name and
// version strings, title and websiteUrl optional strings, icons an optional
// array, in the schema's key order.
func implementationIssues(o *jsutil.Object, path ...string) []issue {
	var issues []issue
	at := func(k string) []string { return append(append([]string{}, path...), k) }
	issues, _ = checkType(issues, o.Value("name"), "string", false, at("name")...)
	issues, _ = checkType(issues, o.Value("title"), "string", true, at("title")...)
	issues, _ = checkType(issues, o.Value("icons"), "array", true, at("icons")...)
	issues, _ = checkType(issues, o.Value("version"), "string", false, at("version")...)
	issues, _ = checkType(issues, o.Value("websiteUrl"), "string", true, at("websiteUrl")...)
	issues, _ = checkType(issues, o.Value("description"), "string", true, at("description")...)
	return issues
}

// --- modern era (2026-07-28) ---

func (s *Server) serveModern(w http.ResponseWriter, r *http.Request, c caller, body any, h headers, out outcome) int {
	msg := body.(*jsutil.Object)
	id := echoableID(body)
	if out.revision != modernVersion {
		message := "Unsupported protocol version: " + out.revision
		s.reportHandlerError(message)
		return writeJSONRPC(w, http.StatusBadRequest, entryError(-32022, message,
			newObject("supported", []any{modernVersion}, "requested", out.revision), id))
	}
	if out.notification {
		w.WriteHeader(http.StatusAccepted)
		return http.StatusAccepted
	}

	method := msg.Value("method").(string)
	if bad, ok := standardHeaderRung(msg, method, h); ok {
		s.reportHandlerError("Rejected inbound request (" + bad.cell + "): " + bad.message)
		return writeJSONRPC(w, bad.status, entryError(bad.code, bad.message, bad.data, id))
	}

	if method == "subscriptions/listen" {
		return s.listen(w, msg, id)
	}

	ctx, cancel := detached(r)
	defer cancel()
	resp, status := s.dispatch(ctx, msg, c, true)
	return writeJSONRPC(w, status, resp)
}

// standardHeaderRung checks the headers a 2026-07-28 request must carry.
func standardHeaderRung(msg *jsutil.Object, method string, h headers) (outcome, bool) {
	if !h.hasVersion {
		claim, _ := meta(msg).Value(pvKey).(string)
		return mismatch("version-header-missing", "(missing)",
			"the body envelope names protocol version "+claim+" but the required MCP-Protocol-Version header is absent"), true
	}
	if !h.hasMethod {
		return mismatch("method-header-missing", "(missing)",
			"the body names method "+method+" but the required Mcp-Method header is absent"), true
	}
	field := ""
	switch method {
	case "tools/call", "prompts/get":
		field = "name"
	case "resources/read":
		field = "uri"
	case "tasks/get", "tasks/update", "tasks/cancel":
		field = "taskId"
	}
	if field == "" {
		return outcome{}, false
	}
	value, ok := member(msg.Value("params"), field).(string)
	if !ok {
		return outcome{}, false
	}
	if !h.hasName {
		return mismatch("name-header-missing", "(missing)",
			"the body carries params."+field+"=\""+value+"\" but the required Mcp-Name header is absent"), true
	}
	decoded, valid := decodeNameHeader(h.name)
	if !valid {
		return mismatch("name-header-invalid-encoding", h.name, "the Mcp-Name header carries an invalid Base64 sentinel value"), true
	}
	if decoded != value {
		return mismatch("name-header-mismatch", h.name,
			"the body carries params."+field+"=\""+value+"\" but the Mcp-Name header names \""+decoded+"\""), true
	}
	return outcome{}, false
}

// listen answers subscriptions/listen. The server's tools capability has no
// listChanged, so nothing a filter asks for is honored: the stream is the
// acknowledgement and an immediate close.
func (s *Server) listen(w http.ResponseWriter, msg *jsutil.Object, id any) int {
	filter, ok := member(msg.Value("params"), "notifications").(*jsutil.Object)
	if ok {
		for _, k := range []string{"toolsListChanged", "promptsListChanged", "resourcesListChanged"} {
			if v := filter.Value(k); v != jsutil.Undefined {
				if _, isBool := v.(bool); !isBool {
					ok = false
				}
			}
		}
		if v := filter.Value("resourceSubscriptions"); v != jsutil.Undefined {
			items, isArray := v.([]any)
			if !isArray {
				ok = false
			}
			for _, item := range items {
				if _, isString := item.(string); !isString {
					ok = false
				}
			}
		}
	}
	if !ok {
		return writeJSONRPC(w, http.StatusOK, entryError(-32602,
			"Invalid params: 'notifications' is required and must be a valid SubscriptionFilter", nil, id))
	}
	reqID := msg.Value("id")
	sse := newSSE(w)
	sse.frame(newObject("jsonrpc", "2.0", "method", "notifications/subscriptions/acknowledged",
		"params", newObject("notifications", jsutil.NewObject(), "_meta", newObject(subIDKey, reqID))))
	sse.frame(newObject("jsonrpc", "2.0", "id", reqID, "result", newObject("resultType", "complete",
		"_meta", newObject(subIDKey, reqID, serverInfoKey, serverInfo()))))
	return http.StatusOK
}

func serverInfo() *jsutil.Object {
	return newObject("name", serverName, "version", serverVersion)
}

// stampModern is the 2026-07-28 codec's result encoding: resultType, the
// cache fields for list and discover results, and the serverInfo in _meta.
func stampModern(method string, result *jsutil.Object) {
	if !result.Has("resultType") {
		result.Set("resultType", "complete")
	}
	if method == "tools/list" || method == "server/discover" {
		if !result.Has("ttlMs") {
			result.Set("ttlMs", 0)
		}
		if !result.Has("cacheScope") {
			result.Set("cacheScope", "private")
		}
	}
	if m, ok := result.Value("_meta").(*jsutil.Object); ok {
		if !m.Has(serverInfoKey) {
			m.Set(serverInfoKey, serverInfo())
		}
	} else {
		result.Set("_meta", newObject(serverInfoKey, serverInfo()))
	}
}

// --- legacy era (2025) ---

func acceptsBoth(r *http.Request) bool {
	accept, _ := headerValue(r, "Accept")
	return strings.Contains(accept, "application/json") && strings.Contains(accept, "text/event-stream")
}

func writeNotAcceptable(w http.ResponseWriter) int {
	return writeJSONRPC(w, http.StatusNotAcceptable, entryError(-32000,
		"Not Acceptable: Client must accept both application/json and text/event-stream", nil, nil))
}

func (s *Server) serveLegacy(w http.ResponseWriter, r *http.Request, c caller, body any, h headers) int {
	if !acceptsBoth(r) {
		return writeNotAcceptable(w)
	}
	messages, isBatch := body.([]any)
	if isBatch && len(messages) > maxBatchSize {
		return writeJSONRPC(w, http.StatusBadRequest, entryError(-32600,
			fmt.Sprintf("Invalid Request: Batch must not exceed %d messages", maxBatchSize), nil, nil))
	}
	if !isBatch {
		messages = []any{body}
	}

	hasInit := false
	for _, m := range messages {
		if isInitializeRequest(m) {
			hasInit = true
		}
	}
	if hasInit && len(messages) > 1 {
		return writeJSONRPC(w, http.StatusBadRequest, entryError(-32600, "Invalid Request: Only one initialization request is allowed", nil, nil))
	}
	if !hasInit && h.hasVersion && !contains(legacyVersions, h.version) {
		return writeJSONRPC(w, http.StatusBadRequest, entryError(-32000,
			"Bad Request: Unsupported protocol version: "+h.version+" (supported versions: "+strings.Join(legacyVersions, ", ")+")", nil, nil))
	}

	var requests []*jsutil.Object
	for _, m := range messages {
		if isRequest(m) {
			requests = append(requests, m.(*jsutil.Object))
		}
	}
	if len(requests) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return http.StatusAccepted
	}

	// One frame per answer, in the order Node wrote them: a method the
	// server does not handle is answered at once, during the pass over the
	// batch; the in-memory methods (ping, initialize, tools/list) resolve
	// next, in request order; tool calls, which wait on the database, as
	// they complete. The stream ends when every id has its answer; a later
	// answer for an id already answered is dropped, as the SDK keyed its
	// streams by id.
	pending := map[string]bool{}
	for _, req := range requests {
		pending[idKey(req.Value("id"))] = true
	}
	sse := newSSE(w)
	send := func(key string, resp *jsutil.Object) {
		if pending[key] {
			delete(pending, key)
			sse.frame(resp)
		}
	}
	for _, req := range requests {
		if !legacyMethods[req.Value("method").(string)] {
			resp, _ := s.dispatch(r.Context(), req, c, false)
			send(idKey(req.Value("id")), resp)
		}
	}
	for _, req := range requests {
		if m := req.Value("method").(string); legacyMethods[m] && m != "tools/call" {
			resp, _ := s.dispatch(r.Context(), req, c, false)
			send(idKey(req.Value("id")), resp)
		}
	}

	// The context is released only when every tool call has finished, so a
	// client that goes away does not cut the work short.
	ctx, cancel := detached(r)
	type answer struct {
		key  string
		resp *jsutil.Object
	}
	answers := make(chan answer, len(requests))
	var running sync.WaitGroup
	calls := 0
	for _, req := range requests {
		if req.Value("method") != "tools/call" {
			continue
		}
		calls++
		running.Add(1)
		go func(req *jsutil.Object) {
			defer running.Done()
			resp, _ := s.dispatch(ctx, req, c, false)
			answers <- answer{idKey(req.Value("id")), resp}
		}(req)
	}
	go func() {
		running.Wait()
		cancel()
		if s.callsDone != nil {
			s.callsDone()
		}
	}()

	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	for calls > 0 && len(pending) > 0 {
		select {
		case a := <-answers:
			calls--
			send(a.key, a.resp)
		case <-ticker.C:
			sse.comment("keepalive")
		case <-r.Context().Done():
			// The client went away; the calls still run to the end, as in
			// Node, but nothing more is written.
			return http.StatusOK
		}
	}
	return http.StatusOK
}

// legacyMethods are the methods a 2025-era server instance handles.
var legacyMethods = map[string]bool{"ping": true, "initialize": true, "tools/list": true, "tools/call": true}

func idKey(id any) string {
	if s, ok := id.(string); ok {
		return "s:" + s
	}
	f, _ := jsutil.AsFloat(id)
	// A Map key compares -0 and 0 as the same value.
	if f == 0 {
		return "n:0"
	}
	return "n:" + strconv.FormatFloat(f, 'g', -1, 64)
}

func contains(list []string, v string) bool {
	for _, l := range list {
		if l == v {
			return true
		}
	}
	return false
}

// isInitializeRequest: an initialize request whose params are valid
// initialize params.
func isInitializeRequest(m any) bool {
	if !isRequest(m) || m.(*jsutil.Object).Value("method") != "initialize" {
		return false
	}
	return len(initializeIssues(m.(*jsutil.Object))) == 0
}

func initializeIssues(msg *jsutil.Object) []issue {
	var issues []issue
	params, ok := msg.Value("params").(*jsutil.Object)
	if !ok {
		issues, _ = checkType(issues, msg.Value("params"), "object", false, "params")
		return issues
	}
	issues, _ = checkType(issues, params.Value("protocolVersion"), "string", false, "params", "protocolVersion")
	issues, _ = checkType(issues, params.Value("capabilities"), "object", false, "params", "capabilities")
	info := params.Value("clientInfo")
	var infoOK bool
	if issues, infoOK = checkType(issues, info, "object", false, "params", "clientInfo"); infoOK {
		issues = append(issues, implementationIssues(info.(*jsutil.Object), "params", "clientInfo")...)
	}
	return issues
}

// sse writes a text/event-stream response. Headers go out with the first
// frame, as Node sent them with the first body chunk.
type sse struct {
	w  http.ResponseWriter
	mu sync.Mutex
}

func newSSE(w http.ResponseWriter) *sse {
	h := w.Header()
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return &sse{w: w}
}

func (e *sse) frame(msg *jsutil.Object) {
	e.write("event: message\ndata: " + jsutil.Marshal(msg) + "\n\n")
}

func (e *sse) comment(text string) {
	e.write(": " + text + "\n\n")
}

func (e *sse) write(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	io.WriteString(e.w, text)
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
}

// detached is the request's context without the client's cancellation:
// Node ran a request's handler to its end after the client had gone.
func detached(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), toolTimeout)
}

// --- dispatch (both eras) ---

// dispatch answers one JSON-RPC request with the response object and the
// HTTP status a 2026-07-28 request gets for it: 404 for a method not found,
// else 200. modern selects the era's method registry and result encoding.
func (s *Server) dispatch(ctx context.Context, req *jsutil.Object, c caller, modern bool) (*jsutil.Object, int) {
	id := req.Value("id")
	method := req.Value("method").(string)
	notFound := func() (*jsutil.Object, int) {
		return dispatchError(id, -32601, "Method not found", nil), http.StatusNotFound
	}

	var result *jsutil.Object
	switch method {
	case "ping":
		if modern {
			return notFound()
		}
		result = jsutil.NewObject()
	case "initialize":
		if modern {
			return notFound()
		}
		if issues := initializeIssues(req); len(issues) > 0 {
			return dispatchError(id, -32603, zodText(issues), nil), http.StatusOK
		}
		requested, _ := member(req.Value("params"), "protocolVersion").(string)
		version := latestProtocolVersion
		if contains(legacyVersions, requested) {
			version = requested
		}
		result = newObject("protocolVersion", version, "capabilities", newObject("tools", jsutil.NewObject()),
			"serverInfo", serverInfo(), "instructions", instructions)
	case "server/discover":
		if !modern {
			return notFound()
		}
		result = newObject("supportedVersions", []any{modernVersion}, "capabilities", newObject("tools", jsutil.NewObject()),
			"instructions", instructions)
	case "tools/list":
		var issues []issue
		if params, ok := req.Value("params").(*jsutil.Object); ok {
			issues, _ = checkType(issues, params.Value("cursor"), "string", true, "params", "cursor")
		}
		if len(issues) > 0 {
			return dispatchError(id, -32603, zodText(issues), nil), http.StatusOK
		}
		result = newObject("tools", s.listTools(c))
	case "tools/call":
		var issues []issue
		params, ok := req.Value("params").(*jsutil.Object)
		if !ok {
			issues, _ = checkType(issues, req.Value("params"), "object", false, "params")
		} else {
			issues, _ = checkType(issues, params.Value("name"), "string", false, "params", "name")
			issues, _ = checkType(issues, params.Value("arguments"), "record", true, "params", "arguments")
		}
		if len(issues) > 0 {
			return dispatchError(id, -32602, "Invalid tools/call request: "+zodText(issues), nil), http.StatusOK
		}
		if state := params.Value("requestState"); state != jsutil.Undefined {
			if _, isString := state.(string); !isString {
				return dispatchError(id, -32602, "Invalid or expired requestState", newObject("reason", "invalid_request_state")), http.StatusOK
			}
		}
		res, err := s.callTool(ctx, c, params.Value("name").(string), params.Value("arguments"))
		if err != nil {
			return dispatchError(id, -32603, err.Error(), nil), http.StatusOK
		}
		result = newObject("content", []any{newObject("type", "text", "text", res.text)})
		if res.isError {
			result.Set("isError", true)
		}
	default:
		return notFound()
	}

	if modern {
		stampModern(method, result)
	}
	return success(id, result), http.StatusOK
}

// listTools is the tools/list handler: the tools the agent's permissions
// allow, in Node's order.
func (s *Server) listTools(c caller) []any {
	var tools []any
	for _, d := range toolDefs {
		perm, _ := toolPermission(d.name)
		if perm.held(c.permissions) {
			tools = append(tools, d.def)
		}
	}
	if tools == nil {
		tools = []any{}
	}
	return tools
}
