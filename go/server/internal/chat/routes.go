package chat

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// Routes are the /v1/chat routes (Node's routes/chat.js).
type Routes struct {
	Service *Service
	Router  *httpapi.Router
}

// Register adds the routes to the /v1 mux.
func (rt *Routes) Register(mux *httpapi.RawMux) {
	route := func(name string, fn httpapi.HandlerFunc) {
		mux.Handle(http.MethodPost, "/chat/"+name, rt.Router.Route("chat", name, fn))
	}
	route("send", rt.send)
	route("receive", rt.receive)
	route("ack", rt.ack)
	route("status", rt.status)
}

// simEngine is the only sender that may attach context the model sees but
// chat history does not keep (ephemeral_context, stable_context).
const simEngine = "salem-engine"

// maxContextChars bounds ephemeral_context and stable_context (UTF-16 units).
const maxContextChars = 65536

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// noBodyRead is V8's TypeError for `req.body.<field>` with no JSON body.
func noBodyRead(field string) error {
	return errors.New("Cannot read properties of undefined (reading '" + field + "')")
}

// messageField is message: nil when absent or null, else the sanitized
// string. Node stored any other type as text; the port refuses it.
func messageField(f httpapi.Fields) (*string, error) {
	if !present(f.Raw("message")) {
		return nil, nil
	}
	s, ok := f.String("message")
	if !ok {
		return nil, httpapi.NotString("message")
	}
	s = sanitize.Content(s)
	return &s, nil
}

// toAgentsField is to_agents. An array's names are sanitized except "*".
// Node passed a name of another JSON type on and answered 404 for it; the
// port refuses it.
func toAgentsField(f httpapi.Fields) (ToAgents, error) {
	if !f.Truthy("to_agents") {
		return ToAgents{}, nil
	}
	if s, ok := f.String("to_agents"); ok {
		if s == "*" {
			return ToAgents{Given: true, Names: []string{"*"}}, nil
		}
		return ToAgents{Given: true}, nil
	}
	list, isArray := f.Array("to_agents")
	if !isArray {
		return ToAgents{Given: true}, nil
	}
	names := make([]string, len(list))
	for i, raw := range list {
		var s string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
			return ToAgents{}, apierr.New(http.StatusBadRequest, "to_agents must be an array of strings")
		}
		if s != "*" {
			s = sanitize.AgentName(s)
		}
		names[i] = s
	}
	return ToAgents{Given: true, IsArray: true, Names: names}, nil
}

// trimmedOptional is the shape of conversation_id, sim_actor_id and
// sim_actor_name: absent or null is nil; a string is trimmed (JS trim) and
// "" becomes nil. lengthFirst checks the length before the trim.
func trimmedOptional(f httpapi.Fields, name string, max int, lengthFirst bool, message string) (*string, string) {
	if !present(f.Raw(name)) {
		return nil, ""
	}
	s, ok := f.String(name)
	if !ok || lengthFirst && jsutil.UTF16Len(s) > max {
		return nil, message
	}
	s = jsutil.Trim(s)
	if !lengthFirst && jsutil.UTF16Len(s) > max {
		return nil, message
	}
	if s == "" {
		return nil, ""
	}
	return &s, ""
}

// contextField is ephemeral_context or stable_context: only the sim engine
// may send one, as a string of at most 65536 UTF-16 units.
func contextField(w http.ResponseWriter, f httpapi.Fields, name, from string) (*string, bool) {
	if !present(f.Raw(name)) {
		return nil, false
	}
	if from != simEngine {
		httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", name+" is only supported for sim engine chat")
		return nil, true
	}
	s, ok := f.String(name)
	if !ok {
		httpapi.BadRequest(w, name+" must be a string")
		return nil, true
	}
	if jsutil.UTF16Len(s) > maxContextChars {
		httpapi.BadRequest(w, name+" exceeds "+strconv.Itoa(maxContextChars)+" chars")
		return nil, true
	}
	return &s, false
}

// toolCallResults reads tool_call_results; the message is the 400 to
// answer, "" when the value is valid.
func toolCallResults(raw json.RawMessage) ([]ToolCallResult, string) {
	list, isArray := elements(raw)
	if !isArray {
		return nil, "tool_call_results must be an array"
	}
	if len(list) == 0 {
		return nil, "tool_call_results must be non-empty when present"
	}
	out := make([]ToolCallResult, len(list))
	for i, e := range list {
		var obj map[string]json.RawMessage
		if len(e) == 0 || e[0] != '{' || json.Unmarshal(e, &obj) != nil {
			return nil, "tool_call_results entries must be { id: non-empty string, content: string }"
		}
		f := httpapi.Fields(obj)
		id, idOK := f.String("id")
		content, contentOK := f.String("content")
		if !idOK || id == "" || !contentOK {
			return nil, "tool_call_results entries must be { id: non-empty string, content: string }"
		}
		out[i] = ToolCallResult{ID: id, Content: content}
	}
	return out, ""
}

func isJSONTrue(raw json.RawMessage) bool {
	return string(raw) == "true"
}

func isArrayValue(raw json.RawMessage) bool {
	_, ok := elements(raw)
	return ok
}

func (rt *Routes) send(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("message")
	}
	from, done, err := httpapi.Identity(w, r, f, "from_agent", "from_agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	message, err := messageField(f)
	if err != nil {
		return err
	}
	toAgents, err := toAgentsField(f)
	if err != nil {
		return err
	}
	discussionID, err := ResolveDiscussionID(f.Raw("discussion_id"), f.Raw("channel"))
	if err != nil {
		return err
	}

	in := SendInput{FromAgent: from, ToAgents: toAgents, DiscussionID: discussionID, Message: message}

	if in.StableContext, done = contextField(w, f, "stable_context", from); done {
		return nil
	}
	if in.EphemeralContext, done = contextField(w, f, "ephemeral_context", from); done {
		return nil
	}
	toolCalls, toolsOffered, toolCallID := f.Raw("tool_calls"), f.Raw("tools_offered"), f.Raw("tool_call_id")
	if present(toolCalls) && !isArrayValue(toolCalls) {
		return httpapi.BadRequest(w, "tool_calls must be an array")
	}
	if present(toolsOffered) && !isArrayValue(toolsOffered) {
		return httpapi.BadRequest(w, "tools_offered must be an array")
	}
	if present(toolCallID) {
		id, isString := f.String("tool_call_id")
		if !isString {
			return httpapi.BadRequest(w, "tool_call_id must be a string")
		}
		in.ToolCallID = &id
	}
	if present(toolCalls) {
		in.ToolCalls = toolCalls
	}
	if present(toolsOffered) {
		in.ToolsOffered = toolsOffered
	}
	resultsRaw := f.Raw("tool_call_results")
	if present(resultsRaw) {
		results, msg := toolCallResults(resultsRaw)
		if msg != "" {
			return httpapi.BadRequest(w, msg)
		}
		if in.ToolCallID != nil && *in.ToolCallID != "" {
			return httpapi.BadRequest(w, "tool_call_results and tool_call_id are mutually exclusive")
		}
		if message != nil && *message != "" {
			return httpapi.BadRequest(w, "tool_call_results is mutually exclusive with message")
		}
		in.ToolCallResults = results
	}
	in.PersistOnly = isJSONTrue(f.Raw("persist_only"))
	if in.PersistOnly && !present(resultsRaw) {
		return httpapi.BadRequest(w, "persist_only requires tool_call_results")
	}

	if present(f.Raw("scene_id")) {
		id, isString := f.String("scene_id")
		if !isString || !uuidPattern.MatchString(id) {
			return httpapi.BadRequest(w, "scene_id must be a UUID string")
		}
		in.SceneID = &id
	}
	if present(f.Raw("scene_structure")) {
		structure, isString := f.String("scene_structure")
		if !isString || jsutil.UTF16Len(structure) > 100 {
			return httpapi.BadRequest(w, "scene_structure must be a string up to 100 chars")
		}
		// Stored as sent; only a blank value becomes NULL.
		if jsutil.Trim(structure) != "" {
			in.SceneStructure = &structure
		}
	}
	var msg string
	if in.ConversationID, msg = trimmedOptional(f, "conversation_id", 64, true, "conversation_id must be a string up to 64 chars"); msg != "" {
		return httpapi.BadRequest(w, msg)
	}
	if in.SimActorID, msg = trimmedOptional(f, "sim_actor_id", 100, false, "sim_actor_id must be a string up to 100 chars"); msg != "" {
		return httpapi.BadRequest(w, msg)
	}
	if in.SimActorName, msg = trimmedOptional(f, "sim_actor_name", 100, false, "sim_actor_name must be a string up to 100 chars"); msg != "" {
		return httpapi.BadRequest(w, msg)
	}
	if present(f.Raw("history_scope")) {
		scope, isString := f.String("history_scope")
		if !isString || scope != "scene" {
			return httpapi.BadRequest(w, `history_scope must be "scene" when present`)
		}
		in.HistoryScope = &scope
	}
	in.Wait = isJSONTrue(f.Raw("wait"))
	if in.PersistOnly && in.Wait {
		return httpapi.BadRequest(w, "persist_only is incompatible with wait=true (nothing to wait for)")
	}

	sent, pending, err := rt.Service.Send(r.Context(), in)
	if err != nil {
		return err
	}
	if !in.Wait {
		return httpapi.WriteJSON(w, http.StatusOK, sent)
	}
	if pending == nil {
		httpapi.WriteError(w, http.StatusBadRequest, "NO_REPLY_PENDING", "wait=true requires exactly one virtual-agent recipient on a non-discussion chat")
		return nil
	}
	// The reply goes on if the caller hangs up, as it did in Node.
	reply, err := pending(context.WithoutCancel(r.Context()))
	if err != nil {
		writeReplyError(w, err)
		return nil
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		*Sent
		Reply json.RawMessage `json:"reply"`
	}{sent, reply})
}

// writeReplyError answers a failed wait=true reply. Only the two typed
// failures pass their status through, so an internal error cannot widen the
// route's contract; everything else is 502 REPLY_FAILED.
func writeReplyError(w http.ResponseWriter, err error) {
	var limited *RateLimitedError
	if errors.As(err, &limited) {
		message := limited.Message
		if message == "" {
			message = "Rate limited"
		}
		body := struct {
			Code             string   `json:"code"`
			Message          string   `json:"message"`
			ResumesInSeconds *float64 `json:"resumes_in_seconds,omitempty"`
		}{"RATE_LIMITED", message, limited.ResumesInSeconds}
		httpapi.WriteJSON(w, http.StatusTooManyRequests, map[string]any{"error": body})
		return
	}
	var over *OverBudgetError
	if errors.As(err, &over) {
		message := over.Message
		if message == "" {
			message = "Cost budget exceeded"
		}
		httpapi.WriteError(w, http.StatusPaymentRequired, "OVER_BUDGET", message)
		return
	}
	message := err.Error()
	if message == "" {
		message = "virtual agent reply failed"
	}
	httpapi.WriteError(w, http.StatusBadGateway, "REPLY_FAILED", message)
}

// afterIDField is after_id: absent is nil; anything else must be a JSON
// integer of at least 0 (Number.isInteger, so null is refused).
func afterIDField(raw json.RawMessage) (*float64, bool) {
	if raw == nil {
		return nil, true
	}
	if len(raw) == 0 || !(raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9') {
		return nil, false
	}
	n, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsInf(n, 0) || n != math.Trunc(n) || n < 0 {
		return nil, false
	}
	return &n, true
}

func (rt *Routes) receive(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	agent, done, err := httpapi.Identity(w, r, f, "agent", "agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	from, err := httpapi.AgentField(f, "from_agent")
	if err != nil {
		return err
	}
	afterID, valid := afterIDField(f.Raw("after_id"))
	if !valid {
		return httpapi.BadRequest(w, "after_id must be a non-negative integer")
	}
	discussionID, err := ResolveDiscussionID(f.Raw("discussion_id"), f.Raw("channel"))
	if err != nil {
		return err
	}
	res, err := rt.Service.Receive(r.Context(), agent, discussionID, afterID, from)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) ack(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	// `ids` is canonical; `message_ids` is the legacy name.
	ids := f.Raw("ids")
	if !f.Truthy("ids") {
		ids = f.Raw("message_ids")
	}
	agent, done, err := httpapi.Identity(w, r, f, "agent", "agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	res, err := rt.Service.Ack(r.Context(), agent, ids)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) status(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	agent, done, err := httpapi.Identity(w, r, f, "agent", "agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	discussionID, err := ResolveDiscussionID(f.Raw("discussion_id"), f.Raw("channel"))
	if err != nil {
		return err
	}
	res, err := rt.Service.Status(r.Context(), agent, discussionID)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}
