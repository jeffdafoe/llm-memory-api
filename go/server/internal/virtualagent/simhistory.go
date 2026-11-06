package virtualagent

import (
	"math"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// chatRow is one row of loadDirectChatHistory, oldest first. ToolCalls and
// ToolsOffered are the jsonb text (nil for NULL).
type chatRow struct {
	FromAgent    string
	ToAgent      string
	Message      *string
	SentAt       *time.Time
	ToolCalls    []byte
	ToolCallID   *string
	ToolsOffered []byte
}

// message is row.message || ”.
func (r chatRow) message() string {
	if r.Message == nil {
		return ""
	}
	return *r.Message
}

// messageText is `${row.message}`: NULL is "null".
func (r chatRow) messageText() string {
	if r.Message == nil {
		return "null"
	}
	return *r.Message
}

func (r chatRow) hasToolCallID() bool {
	return r.ToolCallID != nil && *r.ToolCallID != ""
}

// sentMs is new Date(sent_at).getTime(): node-postgres dropped the
// microseconds.
func sentMs(t time.Time) float64 {
	return float64(t.UnixMilli())
}

// isEnginePerception is a salem-engine row that is not a tool result: a
// perception, which a later perception in the same run replaces.
func isEnginePerception(r chatRow) bool {
	return jsutil.ToLower(r.FromAgent) == "salem-engine" && !r.hasToolCallID()
}

// pruneSimHistory keeps only the last perception of each run of
// consecutive engine perceptions; the latest subsumes the earlier ones.
// Sim mode only: a companion's consecutive messages are distinct.
func pruneSimHistory(history []chatRow) []chatRow {
	out := []chatRow{}
	var pending *chatRow
	flush := func() {
		if pending != nil {
			out = append(out, *pending)
			pending = nil
		}
	}
	for i := range history {
		if isEnginePerception(history[i]) {
			pending = &history[i]
			continue
		}
		flush()
		out = append(out, history[i])
	}
	flush()
	return out
}

// member is v[key] for a JavaScript value: an object's member, undefined on
// any other value. Callers handle null and undefined first where V8 threw.
func member(v any, key string) any {
	if o, ok := v.(*jsutil.Object); ok {
		return o.Value(key)
	}
	return jsutil.Undefined
}

func nonEmptyString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && s != ""
}

// paraphraseToolCall renders a replayed tool call as a short first-person
// line, so the model reads its own past actions as language rather than as
// tool arguments, and does not repeat them. It is "" for tools with nothing
// to say (done; look_around, whose result carries it).
func paraphraseToolCall(name, input any) (string, error) {
	inp := input
	if !jsutil.Truthy(inp) {
		inp = jsutil.NewObject()
	}
	n, _ := name.(string)
	switch n {
	case "speak":
		if text, ok := nonEmptyString(member(inp, "text")); ok {
			return "(I said aloud: " + jsutil.Marshal(text) + ")", nil
		}
		return "", nil
	case "pay_with_item":
		item, ok := nonEmptyString(member(inp, "item"))
		if !ok {
			return "", nil
		}
		seller := ""
		if s, ok := nonEmptyString(member(inp, "seller")); ok {
			seller = " from " + s
		}
		consumeNow := member(inp, "consume_now")
		eatsNow := consumeNow == true || consumeNow == "true"
		// A quote take settles at once, so it is a purchase; without a quote
		// it only places an offer the seller may never fill.
		quoteID, err := jsutil.ValueNumber(member(inp, "quote_id"))
		if err != nil {
			return "", err
		}
		if !math.IsNaN(quoteID) && !math.IsInf(quoteID, 0) && quoteID > 0 {
			ateNow := ""
			if eatsNow {
				ateNow = " and ate it on the spot"
			}
			return "(I bought " + item + seller + ateNow + ")", nil
		}
		eatNow := ""
		if eatsNow {
			eatNow = " to eat now"
		}
		return "(I offered to buy " + item + seller + eatNow + ")", nil
	case "consume":
		if item, ok := nonEmptyString(member(inp, "item")); ok {
			return "(I consumed " + item + ")", nil
		}
		return "", nil
	case "move_to":
		if place, ok := nonEmptyString(member(inp, "structure_name")); ok {
			return "(I set off toward " + place + ")", nil
		}
		if _, ok := nonEmptyString(member(inp, "structure_id")); ok {
			return "(I set off walking to a place I could see)", nil
		}
		return "", nil
	case "take_break":
		reason := ""
		if r, ok := nonEmptyString(member(inp, "reason")); ok {
			reason = ": " + r
		}
		return "(I stopped to take a break" + reason + ")", nil
	case "stop":
		return "(I stopped where I was)", nil
	case "chore":
		if t, ok := nonEmptyString(member(inp, "type")); ok {
			return "(I ran a chore: " + t + ")", nil
		}
		return "", nil
	}
	return "", nil
}

// formatGapMarker is the line put before a replayed perception after a
// pause: minutes (to 5) from 10 minutes, hours from 2 hours, else none. It
// depends only on the rows' stored times, so the replayed bytes stay the
// same between calls and the provider's prompt cache keeps working.
func formatGapMarker(gapMs float64) (string, bool) {
	const hourMs = 60 * 60 * 1000
	const minuteMs = 60 * 1000
	if gapMs >= 2*hourMs {
		return "--- gap: " + jsutil.NumberString(jsutil.Round(gapMs/hourMs)) + "h ---", true
	}
	if gapMs >= 10*minuteMs {
		return "--- gap: " + jsutil.NumberString(jsutil.Round(gapMs/(5*minuteMs))*5) + "m ---", true
	}
	return "", false
}

// buildCurrentTurnContext is the volatile context of the current sim turn:
// the Impressions block (a sim NPC's only) and the engine's
// ephemeral_context, which ends with its triage text and so stays last. It
// goes beside the history, never into it, so the cached prefix stays the
// same when someone walks in or out.
func buildCurrentTurnContext(isSimNpc bool, peopleContext string, ephemeralContext string) string {
	impressions := ""
	if isSimNpc {
		impressions = wrapBlock("Impressions", "private-relationship-notes", directiveImpressions, peopleContext)
	}
	switch {
	case impressions != "" && ephemeralContext != "":
		return impressions + "\n\n" + ephemeralContext
	case impressions != "":
		return impressions
	}
	return ephemeralContext
}

// typeError is a V8 TypeError message.
type typeError string

func (e typeError) Error() string { return string(e) }

// storedToolCalls reads a reply row's tool_calls as Node did: the parsed
// jsonb when it is truthy, parsed once more when it is a string. nil means
// the row has no tool calls. A string that is not JSON fails with Go's
// parser message, not V8's (Known differences).
func storedToolCalls(raw []byte) (any, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	v, err := jsutil.Parse(raw)
	if err != nil {
		return nil, false, err
	}
	if !jsutil.Truthy(v) {
		return nil, false, nil
	}
	if s, ok := v.(string); ok {
		v, err = jsutil.Parse([]byte(s))
		if err != nil {
			return nil, false, err
		}
	}
	return v, true, nil
}

// buildToolUseMessages turns the history into an OpenAI-shape conversation
// for the tool-use path: this agent's rows are assistant turns (with their
// tool calls in OpenAI's function shape), rows with a tool_call_id are tool
// results, and the rest are user turns, each led by a gap marker after a
// pause. Then parallel calls are split and each call's payload is mirrored
// into its turn's text.
func buildToolUseMessages(history []chatRow, npcAgentName string) ([]*jsutil.Object, error) {
	messages := []*jsutil.Object{}
	npcLower := jsutil.ToLower(npcAgentName)
	var prevSentAt *time.Time
	for _, row := range history {
		if jsutil.ToLower(row.FromAgent) == npcLower {
			msg := jsutil.NewObject()
			msg.Set("role", "assistant")
			msg.Set("content", row.message())
			raw, has, err := storedToolCalls(row.ToolCalls)
			if err != nil {
				return nil, err
			}
			if has {
				calls, err := openAIToolCalls(raw)
				if err != nil {
					return nil, err
				}
				msg.Set("tool_calls", calls)
			}
			messages = append(messages, msg)
		} else if row.hasToolCallID() {
			msg := jsutil.NewObject()
			msg.Set("role", "tool")
			msg.Set("tool_call_id", *row.ToolCallID)
			msg.Set("content", row.message())
			messages = append(messages, msg)
		} else {
			content := row.message()
			if row.SentAt != nil && prevSentAt != nil {
				if marker, ok := formatGapMarker(sentMs(*row.SentAt) - sentMs(*prevSentAt)); ok {
					content = marker + "\n" + content
				}
			}
			msg := jsutil.NewObject()
			msg.Set("role", "user")
			msg.Set("content", content)
			messages = append(messages, msg)
		}
		if row.SentAt != nil {
			prevSentAt = row.SentAt
		}
	}
	return mirrorToolCallSalience(splitParallelToolCallTurns(messages))
}

// openAIToolCalls maps stored {id, name, input} calls to OpenAI's
// {id, type: "function", function: {name, arguments}}.
func openAIToolCalls(raw any) ([]any, error) {
	if raw == nil {
		return nil, typeError("Cannot read properties of null (reading 'map')")
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, typeError("raw.map is not a function")
	}
	out := make([]any, len(list))
	for i, tc := range list {
		if tc == nil {
			return nil, typeError("Cannot read properties of null (reading 'id')")
		}
		input := member(tc, "input")
		var arguments any
		if s, ok := input.(string); ok {
			arguments = s
		} else {
			if !jsutil.Truthy(input) {
				input = jsutil.NewObject()
			}
			arguments = jsutil.Marshal(input)
		}
		fn := jsutil.NewObject()
		fn.Set("name", member(tc, "name"))
		fn.Set("arguments", arguments)
		call := jsutil.NewObject()
		call.Set("id", member(tc, "id"))
		call.Set("type", "function")
		call.Set("function", fn)
		out[i] = call
	}
	return out, nil
}

func toolCallsOf(msg *jsutil.Object) ([]any, bool) {
	calls, ok := msg.Value("tool_calls").([]any)
	return calls, ok
}

// orderedResults is a Map from tool_call_id to its result: a repeated id
// keeps its first place and its last result.
type orderedResults struct {
	keys   []string
	values map[string]*jsutil.Object
}

func (m *orderedResults) set(k string, v *jsutil.Object) {
	if _, ok := m.values[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.values[k] = v
}

func (m *orderedResults) take(id any) (*jsutil.Object, bool) {
	k, ok := id.(string)
	if !ok {
		return nil, false
	}
	v, ok := m.values[k]
	if !ok {
		return nil, false
	}
	delete(m.values, k)
	return v, true
}

// splitParallelToolCallTurns splits an assistant turn with several tool
// calls into one turn per call, each followed by its own result, because
// several upstreams behind OpenRouter refuse a history with parallel calls.
// A call with no result gets "[no result recorded]", since strict
// providers also refuse an unanswered call.
func splitParallelToolCallTurns(messages []*jsutil.Object) []*jsutil.Object {
	out := []*jsutil.Object{}
	i := 0
	for i < len(messages) {
		msg := messages[i]
		calls, isArray := toolCallsOf(msg)
		if !(msg.Value("role") == "assistant" && isArray && len(calls) > 1) {
			out = append(out, msg)
			i++
			continue
		}
		results := &orderedResults{values: map[string]*jsutil.Object{}}
		j := i + 1
		for j < len(messages) && messages[j].Value("role") == "tool" {
			id, _ := messages[j].Value("tool_call_id").(string)
			results.set(id, messages[j])
			j++
		}
		for index, call := range calls {
			piece := jsutil.NewObject()
			piece.Set("role", "assistant")
			piece.Set("content", "")
			piece.Set("tool_calls", []any{call})
			if index == 0 {
				// Text the model wrote belongs to the whole turn.
				content := msg.Value("content")
				if !jsutil.Truthy(content) {
					content = ""
				}
				piece.Set("content", content)
			}
			out = append(out, piece)
			id := member(call, "id")
			if result, ok := results.take(id); ok {
				out = append(out, result)
			} else {
				placeholder := jsutil.NewObject()
				placeholder.Set("role", "tool")
				placeholder.Set("tool_call_id", id)
				placeholder.Set("content", "[no result recorded]")
				out = append(out, placeholder)
			}
		}
		for _, k := range results.keys {
			if v, ok := results.values[k]; ok {
				out = append(out, v)
			}
		}
		i = j
	}
	return out
}

// mirrorToolCallSalience writes each assistant tool-call turn's paraphrase
// into its empty content, so a replayed speech or move reads as something
// said or done. Text the model wrote is never replaced.
func mirrorToolCallSalience(messages []*jsutil.Object) ([]*jsutil.Object, error) {
	for _, msg := range messages {
		calls, isArray := toolCallsOf(msg)
		if msg.Value("role") != "assistant" || !isArray || jsutil.Truthy(msg.Value("content")) {
			continue
		}
		var lines []string
		for _, call := range calls {
			fn := member(call, "function")
			paraphrase, err := paraphraseToolCall(member(fn, "name"), safeParseJSON(member(fn, "arguments")))
			if err != nil {
				return nil, err
			}
			if paraphrase != "" {
				lines = append(lines, paraphrase)
			}
		}
		if len(lines) > 0 {
			text := lines[0]
			for _, l := range lines[1:] {
				text += "\n" + l
			}
			msg.Set("content", text)
		}
	}
	return messages, nil
}

// safeParseJSON is JSON.parse, {} when it throws.
func safeParseJSON(v any) any {
	s, ok := v.(string)
	if !ok {
		return jsutil.NewObject()
	}
	parsed, err := jsutil.Parse([]byte(s))
	if err != nil {
		return jsutil.NewObject()
	}
	return parsed
}

// trimHeadToUserMessage drops leading turns until the first user turn: a
// history window cut mid-exchange can start with a tool result or an
// assistant turn, which Anthropic refuses.
func trimHeadToUserMessage(messages []*jsutil.Object) []*jsutil.Object {
	first := 0
	for first < len(messages) && messages[first].Value("role") != "user" {
		first++
	}
	return messages[first:]
}
