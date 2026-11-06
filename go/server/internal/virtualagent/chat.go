package virtualagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// ChatSender sends a chat message (chat.Service.Send).
type ChatSender interface {
	Send(ctx context.Context, in chat.SendInput) (*chat.Sent, chat.PendingReply, error)
}

// statusError carries the status Node's error had (err.statusCode), which
// the call log stores, around the error the caller sees.
type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

// sendOpts are the chatSend options a virtual agent's reply or notice uses.
type sendOpts struct {
	sceneID, sceneStructure, conversationID *string
	toolCalls                               json.RawMessage
	isError, ackOnInsert                    bool
}

func (s *Service) chatSend(ctx context.Context, from, to, message string, o sendOpts) error {
	_, _, err := s.Chat.Send(ctx, chat.SendInput{
		FromAgent: from, ToAgents: chat.To(to), Message: &message,
		SceneID: o.sceneID, SceneStructure: o.sceneStructure, ConversationID: o.conversationID,
		ToolCalls: o.toolCalls, IsError: o.isError, AckOnInsert: o.ackOnInsert,
	})
	return err
}

// goDetached runs fn in the background with no time bound of its own: a
// learning extraction is a provider call, which bounds itself.
func goDetached(fn func(ctx context.Context)) {
	db.BackgroundUnbounded(fn)
}

// jsText is `${v}` for an optional string: undefined when absent.
func jsText(v *string) string {
	if v == nil {
		return "undefined"
	}
	return *v
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// offeredToolNames is offeredToolNames: the names of the tools offered on a
// tool-use call, for virtual_agent_calls.tool_names. An entry that is not an
// object with a string name is skipped. nil (NULL) when no tools were offered.
func offeredToolNames(toolsOffered any) []string {
	tools, ok := toolsOffered.([]any)
	if !ok || len(tools) == 0 {
		return nil
	}
	names := []string{}
	for _, t := range tools {
		o, ok := t.(*jsutil.Object)
		if !ok {
			continue
		}
		if v, ok := o.Get("name"); ok {
			if name, ok := v.(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// directChat is one direct chat turn's inputs, read from chat.DirectChat as
// Node read handleDirectChat's options.
type directChat struct {
	chat.DirectChat
	toolsOffered     any
	isToolUse        bool
	ephemeralContext string
	stableContext    string
}

// HandleDirectChat is handleDirectChat: a virtual agent's reply to a direct
// chat message. Every salem NPC turn comes through here (sender
// salem-engine). The reply is sent back as a chat message and returned as
// {text, tool_calls} for a wait=true caller; nil when the recipient is not
// a virtual agent. A failure after the agent loaded is recorded (an error
// usage row, the call log, error_log), answered with an "[Error] ... is
// unavailable" message, and returned; a rate limit or budget stop is
// *chat.RateLimitedError or *chat.OverBudgetError.
func (s *Service) HandleDirectChat(ctx context.Context, in chat.DirectChat) (json.RawMessage, error) {
	d := directChat{DirectChat: in}
	if len(in.ToolsOffered) > 0 {
		v, err := jsutil.Parse(in.ToolsOffered)
		if err != nil {
			return nil, err
		}
		d.toolsOffered = v
		d.isToolUse = nonEmptyArray(v)
	}
	d.ephemeralContext = deref(in.EphemeralContext)
	d.stableContext = deref(in.StableContext)

	agent, err := s.LoadAgent(ctx, in.VirtualAgent)
	if err != nil {
		return nil, err
	}
	if agent == nil || !agent.Virtual {
		return nil, nil
	}
	if str(agent.APIKey) == "" || str(agent.Provider) == "" || str(agent.Model) == "" {
		// An error, not a quiet nil: a wait=true caller (the salem engine)
		// gets a 502 and can tell a misconfigured agent from a silent one.
		s.logVA("direct-chat-skip", logger.Fields{{Key: "agent", Value: in.VirtualAgent}, {Key: "reason", Value: "missing config"}})
		if in.NotifyUnanswerable {
			notice := unanswerableNotice(in.VirtualAgent, "missing-config", missingConfigParts(agent))
			if err := s.chatSend(ctx, in.VirtualAgent, in.FromAgent, notice, sendOpts{isError: true}); err != nil {
				s.logVA("error-feedback-failed", logger.Fields{{Key: "agent", Value: in.VirtualAgent}, {Key: "error", Value: err.Error()}})
			}
		}
		return nil, errors.New("Agent " + in.VirtualAgent + " missing provider/model/api_key")
	}

	s.logVA("direct-chat-processing", logger.Fields{{Key: "agent", Value: in.VirtualAgent}, {Key: "from", Value: in.FromAgent}, {Key: "tool_use", Value: d.isToolUse}})

	reply, err := s.directChatTurn(ctx, agent, &d)
	if err == nil {
		return reply, nil
	}

	// Node's catch block. Its systemPrompt, loggedUserMessage and the call's
	// start time were declared inside the try block, so the catch never saw
	// them: the call row has no system prompt or duration, and the inbound
	// message as its user message.
	var usageID *int
	if id, uerr := s.recordUsage(ctx, agent.Name, str(agent.Provider), str(agent.Model), providers.Usage{}, "chat", "error", err.Error()); uerr == nil {
		usageID = &id
	}
	s.goLogCall(CallLog{
		ActorID: agent.ActorID, AgentName: in.VirtualAgent, Context: "chat",
		Provider: str(agent.Provider), Model: str(agent.Model),
		UserMessage: deref(in.Message), Err: err, UsageID: usageID,
		SceneID: deref(in.SceneID), ConversationID: deref(in.ConversationID),
		SimActorID: deref(in.SimActorID), SimActorName: deref(in.SimActorName),
		ToolNames: offeredToolNames(d.toolsOffered),
	})
	s.Log.LogError("virtual-agent", "direct-chat-error", logger.ErrorOpts{
		Agent: in.VirtualAgent, Context: "chat", Message: err.Error(), Detail: fmt.Sprintf("%+v", err), StatusCode: 500,
	})
	notice := "[Error] " + in.VirtualAgent + " is unavailable (" + err.Error() + ")."
	if sendErr := s.chatSend(ctx, in.VirtualAgent, in.FromAgent, notice, sendOpts{sceneID: in.SceneID, conversationID: in.ConversationID, isError: true}); sendErr != nil {
		s.logVA("error-feedback-failed", logger.Fields{{Key: "agent", Value: in.VirtualAgent}, {Key: "error", Value: sendErr.Error()}})
	}
	var se *statusError
	if errors.As(err, &se) {
		return nil, se.err
	}
	return nil, err
}

// loadHistory is the history a turn replays (LLM-687). A turn the caller
// scopes to its scene ("scene": the salem switchboard VAs, the stateless
// cascade VA, the court) reads only that scene's rows, and none when it has
// no scene, never the unscoped window, which would mix personas. Otherwise
// it is the agent-pair history, a stateful NPC's continuity channel.
func (s *Service) loadHistory(ctx context.Context, agent *Agent, d *directChat, historyCap int) ([]chatRow, error) {
	sceneScoped := d.HistoryScope != nil && *d.HistoryScope == "scene"
	var sceneFilter *string
	if sceneScoped {
		if d.SceneID == nil || *d.SceneID == "" {
			return []chatRow{}, nil
		}
		sceneFilter = d.SceneID
	}
	return s.loadDirectChatHistory(ctx, d.VirtualAgent, d.FromAgent, sceneFilter, historyCap)
}

// directChatTurn is the body of Node's try block.
func (s *Service) directChatTurn(ctx context.Context, agent *Agent, d *directChat) (json.RawMessage, error) {
	apiKey, err := s.Providers.DecryptAPIKey(*agent.APIKey)
	if err != nil {
		return nil, err
	}
	conf, err := BuildProviderConf(agent)
	if err != nil {
		return nil, err
	}

	historyCap := historyCapFor(agent.Name)
	history, err := s.loadHistory(ctx, agent, d, historyCap)
	if err != nil {
		return nil, err
	}

	// A sim turn comes from the salem village engine. It skips RAG (the
	// replayed history is its recall; a search on a perception finds
	// noise) and takes the Impressions counterparts from the people the
	// perception names around it, not from the sender.
	isSimChat := d.FromAgent == "salem-engine"
	isSimNpc := isSimChat
	var ragContext, peopleContext string
	if isSimNpc {
		// The people around the NPC are in the message or, since the engine
		// moved surroundings out of the stored perception, in
		// ephemeral_context, so both are read.
		source := deref(d.Message)
		if d.ephemeralContext != "" {
			source = jsText(d.Message) + "\n" + d.ephemeralContext
		}
		coLocated := dedupe(extractCoLocatedNames(source))
		sceneID := deref(d.SceneID)
		if d.IsToolResultCall {
			// A tool-result follow-up carries no perception: use the people
			// the scene's last fresh perception named.
			coLocated = s.recallSceneCoLocated(sceneID)
		} else if len(coLocated) > 0 {
			s.rememberSceneCoLocated(sceneID, coLocated)
		} else {
			// A fresh perception with nobody around clears the scene's entry.
			s.forgetSceneCoLocated(sceneID)
		}
		if len(coLocated) > 0 {
			peopleContext = s.loadPeopleContext(ctx, agent.Name, coLocated)
		}
	} else {
		if d.Message == nil {
			return nil, typeError("Cannot read properties of undefined (reading 'trim')")
		}
		ragQuery := *d.Message
		if len(jsutil.SplitSpace(jsutil.Trim(*d.Message))) < 5 && len(history) >= 2 {
			ragQuery = history[len(history)-2].messageText() + " " + *d.Message
		}
		ragContext = s.loadRAGContext(ctx, agent.Name, ragQuery)
		peopleContext = s.loadPeopleContext(ctx, agent.Name, []string{d.FromAgent})
	}
	soul := s.loadSoul(ctx, agent.Name)

	var systemPrompt providers.SystemPrompt
	switch {
	case isSimNpc && simCourtAgents[agent.Name]:
		systemPrompt = buildSimCourtSystemPrompt(agent, d.stableContext)
	case isSimNpc && d.isToolUse:
		// The system prompt is the provider-cached prefix of every request,
		// and a changed byte in it re-bills the whole replayed history
		// (LLM-501). So it holds only content stable within the day: the
		// Impressions, which change as people come and go, move to the
		// current turn, and the engine's daily stable_context goes here.
		systemPrompt, err = s.buildSimChatSystemPrompt(agent, ragContext, soul, "")
		if err != nil {
			return nil, err
		}
		if d.stableContext != "" {
			systemPrompt.Static += "\n\n" + wrapBlock("Standing", "standing-context", directiveStanding, d.stableContext)
		}
	case isSimNpc:
		systemPrompt, err = s.buildSimReflectionSystemPrompt(agent, ragContext, soul, peopleContext)
	default:
		systemPrompt, err = s.buildDirectChatSystemPrompt(agent, ragContext, soul, peopleContext)
	}
	if err != nil {
		return nil, err
	}

	var toolUseMessages []*jsutil.Object
	currentTurnContext := ""
	if d.isToolUse {
		toolUseHistory := history
		if isSimChat {
			// The head is quantized before the prune so the cap check sees
			// the fetched row count; both steps depend only on the rows, so
			// the replayed prefix stays the same between head jumps.
			toolUseHistory = pruneSimHistory(stabilizeHistoryWindow(history, historyCap))
		}
		toolUseMessages, err = buildToolUseMessages(toolUseHistory, d.VirtualAgent)
		if err != nil {
			return nil, err
		}
		toolUseMessages = trimHeadToUserMessage(toolUseMessages)
		if len(toolUseMessages) == 0 {
			// Anthropic refuses an empty conversation, so the inbound
			// message seeds it.
			seed := jsutil.NewObject()
			seed.Set("role", "user")
			if d.Message != nil && *d.Message != "" {
				seed.Set("content", *d.Message)
			} else {
				seed.Set("content", "Continue.")
			}
			toolUseMessages = []*jsutil.Object{seed}
		}
		// The volatile context goes to the provider apart from the history
		// (LLM-816), so Anthropic can put its cache breakpoint on the last
		// history block; other providers merge it into the messages.
		currentTurnContext = buildCurrentTurnContext(isSimNpc, peopleContext, d.ephemeralContext)
	}
	userMessage := ""
	if !d.isToolUse {
		userMessage = s.buildDirectChatUserMessage(history, d.FromAgent, jsText(d.Message))
	}

	// The call log shows what the model saw: the messages with the turn
	// context attached on the tool-use path, the user message otherwise.
	loggedUserMessage := userMessage
	if d.isToolUse {
		attached, err := providers.AttachTurnContext(toolUseMessages, currentTurnContext)
		if err != nil {
			return nil, err
		}
		loggedUserMessage, err = stringifyIndent(objects(attached))
		if err != nil {
			return nil, err
		}
	}

	// Only the engine's turns count once per scene (ZBBS-HOME-332): scene_id
	// comes from the client, so honouring it for any sender would let one
	// replay a scene to get past the limit.
	rateLimitScene := ""
	if isSimChat {
		rateLimitScene = deref(d.SceneID)
	}
	errorOpts := sendOpts{sceneID: d.SceneID, conversationID: d.ConversationID, isError: true}
	limited, err := s.IsRateLimited(ctx, agent.Name, rateLimitScene)
	if err != nil {
		return nil, err
	}
	if limited {
		s.logVA("direct-chat-rate-limited", logger.Fields{{Key: "agent", Value: d.VirtualAgent}, {Key: "from", Value: d.FromAgent}})
		if err := s.chatSend(ctx, d.VirtualAgent, d.FromAgent, "[Error] Rate limited — too many API calls. Please wait before trying again.", errorOpts); err != nil {
			return nil, err
		}
		// An error, not a nil reply (ZBBS-WORK-404): the wait=true route
		// answers 429, so the engine sees a cooldown, not a malformed reply.
		resume := s.RateLimitResumeSeconds(agent.Name)
		return nil, &statusError{429, &chat.RateLimitedError{Message: "Rate limited — too many API calls", ResumesInSeconds: &resume}}
	}

	reason, err := s.isOverCostLimit(ctx, agent)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		s.logVA("direct-chat-over-cost-limit", logger.Fields{{Key: "agent", Value: d.VirtualAgent}, {Key: "from", Value: d.FromAgent}, {Key: "reason", Value: reason}})
		if err := s.chatSend(ctx, d.VirtualAgent, d.FromAgent, "[Error] "+reason, errorOpts); err != nil {
			return nil, err
		}
		// A 402 on the wait=true route (LLM-513), so the engine raises its
		// budget alarm instead of booking a malformed reply.
		return nil, &statusError{402, &chat.OverBudgetError{Message: reason}}
	}

	call, err := s.Providers.CreateProvider(*agent.Provider, *agent.Model, apiKey, conf)
	if err != nil {
		return nil, err
	}
	if err := s.RecordCall(agent.Name, rateLimitScene); err != nil {
		return nil, err
	}
	start := s.now()
	raw := &providers.RawOpts{Cache: true}
	if d.isToolUse {
		raw.Tools = d.toolsOffered
		raw.Messages = objects(toolUseMessages)
		if currentTurnContext != "" {
			raw.TurnContext = currentTurnContext
		}
	}
	callProvider := func(ctx context.Context) (*providers.Result, error) {
		return WithActivity(ctx, s, agent.Name, func() (*providers.Result, error) {
			return call(ctx, systemPrompt, userMessage, raw)
		})
	}
	// No retry for the engine's wait=true tick: the engine gives up after
	// its 90 s timeout and ticks again, so a retry would bill a call nobody
	// reads and store an undispatched tool call in the NPC's history.
	var result *providers.Result
	if isSimChat && d.AckReplyOnInsert {
		result, err = callProvider(ctx)
	} else {
		result, err = retryWithBackoff(ctx, s, agent.Name, callProvider, func(err error, info RetryInfo) error {
			text := fmt.Sprintf("[Retrying] Initial attempt failed: %s. Retrying %d more time(s) over the next ~%s.",
				err.Error(), info.RetriesRemaining, formatDuration(info.TotalSeconds))
			return s.chatSend(ctx, d.VirtualAgent, d.FromAgent, text, errorOpts)
		})
	}
	if err != nil {
		return nil, err
	}

	var response any = ""
	if jsutil.Truthy(result.Text) {
		response = result.Text
	}
	usage := result.Usage
	toolCalls := result.ToolCalls
	if toolCalls == nil {
		toolCalls = []providers.ToolCall{}
	}
	duration := s.now().Sub(start)
	usageID, err := s.recordUsage(ctx, agent.Name, *agent.Provider, *agent.Model, usage, "chat", "", "")
	if err != nil {
		return nil, err
	}

	// On a tool-use turn the text is empty, so the call log and the
	// transcript show the tool calls.
	loggedResponse := response
	if len(toolCalls) > 0 {
		callsText, err := stringifyIndent(toolCallValues(toolCalls))
		if err != nil {
			return nil, err
		}
		callsText = "tool_calls:\n" + callsText
		if jsutil.Truthy(response) {
			text, err := jsutil.ValueString(response)
			if err != nil {
				return nil, err
			}
			loggedResponse = text + "\n\n" + callsText
		} else {
			loggedResponse = callsText
		}
	}

	cost, err := providers.CalculateCost(*agent.Provider, *agent.Model, usage)
	if err != nil {
		return nil, err
	}
	s.goLogCall(CallLog{
		ActorID: agent.ActorID, AgentName: agent.Name, Context: "chat",
		Provider: *agent.Provider, Model: *agent.Model,
		SystemPrompt: &systemPrompt, UserMessage: loggedUserMessage, Response: loggedResponse, Usage: &usage, Cost: cost,
		Duration: duration, UsageID: &usageID,
		SceneID: deref(d.SceneID), ConversationID: deref(d.ConversationID),
		SimActorID: deref(d.SimActorID), SimActorName: deref(d.SimActorName),
		ToolNames: offeredToolNames(d.toolsOffered),
	})

	// The reply row keeps the tool calls, so the next turn's history has
	// them, and the scene, so the admin view groups it with its perception.
	// A wait=true caller reads the reply inline and never acks it, so it is
	// acked as it is written.
	replyText, err := messageParam(response)
	if err != nil {
		return nil, err
	}
	replyOpts := sendOpts{sceneID: d.SceneID, sceneStructure: d.SceneStructure, conversationID: d.ConversationID, ackOnInsert: d.AckReplyOnInsert}
	if len(toolCalls) > 0 {
		replyOpts.toolCalls = json.RawMessage(jsutil.Marshal(toolCallValues(toolCalls)))
	}
	if err := s.chatSend(ctx, agent.Name, d.FromAgent, replyText, replyOpts); err != nil {
		return nil, err
	}

	// Every delivery row of the send is acked, not just the last: a
	// tool-results send has one row per result.
	var ackIDs []int
	for _, id := range d.MessageIDs {
		if id != 0 {
			ackIDs = append(ackIDs, id)
		}
	}
	if len(ackIDs) > 0 {
		if _, err := s.DB.Exec(ctx, "UPDATE chat_messages SET acked_at = NOW() WHERE id = ANY($1) AND to_actor_id = $2 AND acked_at IS NULL",
			ackIDs, agent.ActorID); err != nil {
			return nil, err
		}
	}

	responded := logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "to", Value: d.FromAgent}}
	if n, ok := jsLength(response); ok {
		responded = append(responded, logger.Field{Key: "responseLength", Value: n})
	}
	responded = append(responded, logger.Field{Key: "tool_calls", Value: len(toolCalls)})
	s.logVA("direct-chat-responded", responded)

	// Perceptions and tool results are not learning material.
	if !d.isToolUse {
		goDetached(func(bg context.Context) {
			if err := s.ExtractLearnings(bg, agent, systemPrompt, userMessage, response, "chat", d.FromAgent); err != nil {
				s.logVA("learning-extraction-failed", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "error", Value: err.Error()}})
			}
		})
	}
	goDetached(func(bg context.Context) {
		meta := TranscriptMeta{RequestingAgent: d.FromAgent, Model: *agent.Model}
		if err := s.LogTranscript(bg, agent.Name, systemPrompt, loggedUserMessage, loggedResponse, &usage, "chat", meta); err != nil {
			s.logVA("transcript-log-failed", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "error", Value: err.Error()}})
		}
	})

	out := jsutil.NewObject()
	out.Set("text", response)
	out.Set("tool_calls", toolCallValues(toolCalls))
	return json.RawMessage(jsutil.Marshal(out)), nil
}

// objects is a message list as a JavaScript array.
func objects(messages []*jsutil.Object) []any {
	out := make([]any, len(messages))
	for i, m := range messages {
		out[i] = m
	}
	return out
}

// toolCallValues is the neutral tool calls as JavaScript objects.
func toolCallValues(calls []providers.ToolCall) []any {
	out := make([]any, len(calls))
	for i, tc := range calls {
		b, _ := tc.MarshalJSON()
		out[i], _ = jsutil.Parse(b)
	}
	return out
}

// stringifyIndent is JSON.stringify(v, null, 2).
func stringifyIndent(v any) (string, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(jsutil.Marshal(v)), "", "  "); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// messageParam is the reply text as the chat_message_texts parameter: a
// string as is, anything else as node-postgres bound it.
func messageParam(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	text, ok := prepareValue(v)
	if !ok {
		return "", errors.New("reply text has no parameter form")
	}
	return text, nil
}

// jsLength is v.length: a string's UTF-16 length, an array's length;
// undefined (no field) for anything else.
func jsLength(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		return jsutil.UTF16Len(x), true
	case []any:
		return len(x), true
	case *jsutil.Object:
		if n, ok := x.Get("length"); ok {
			return logJS(n), true
		}
	}
	return nil, false
}
