package virtualagent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/discussion"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsregex"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
)

// Discussions is the discussion service a virtual participant calls
// (discussion.Service): a ballot's evaluation, and leaving after a failure.
type Discussions interface {
	EvaluateVote(ctx context.Context, voteID discussion.ID) error
	Leave(ctx context.Context, discussionID discussion.ID, agent string) (*discussion.LeaveResult, error)
}

const (
	directiveDiscussion   = "The discussion you are participating in. Stable for the life of the discussion."
	directiveConversation = "The live conversation, oldest first. The last entry is the message you are replying to. Each entry has the shape {\"sender\": <who>, \"content\": <what they said>}."
	directiveOutput       = "Reply with your own message content only — plain text, no JSON wrapper, no sender labels, no additional entries, no continuation of other participants' messages. End when your own message is complete."
	directiveVote         = "A vote has been proposed. Reply with ONLY a JSON object: {\"choice\": 1, \"reason\": \"...\"} to approve or {\"choice\": 2, \"reason\": \"...\"} to reject."
)

// discussionStop is the one stop sequence a discussion call sends: the model
// starting another {"sender": ...} history entry, whoever it names.
var discussionStop = []any{"\n{\"sender\":\""}

// discussionRow is the part of the discussions row the trigger reads.
type discussionRow struct {
	ID      int
	Topic   string
	Status  string
	Mode    string
	Context *string
}

type participant struct {
	Agent  string
	Status string
	Role   string
}

// historyMessage is a discussion chat message (loadChatHistory's row).
type historyMessage struct {
	ID        int
	FromAgent string
	Message   *string
	SentAt    time.Time
}

// loadDiscussion is the discussion and its participants, or nil when there
// is no such discussion. Node read `SELECT *`; only these columns are used.
func (s *Service) loadDiscussion(ctx context.Context, id discussion.ID) (*discussionRow, []participant, error) {
	var d discussionRow
	err := s.DB.QueryRow(ctx, "SELECT id, topic, status, mode, context FROM discussions WHERE id = $1", id.Param()).
		Scan(&d.ID, &d.Topic, &d.Status, &d.Mode, &d.Context)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.DB.Query(ctx,
		`SELECT ac.name AS agent, dp.status, dp.role
         FROM discussion_participants dp
         JOIN actors ac ON ac.id = dp.actor_id
         WHERE dp.discussion_id = $1`, id.Param())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	parts := []participant{}
	for rows.Next() {
		var p participant
		if err := rows.Scan(&p.Agent, &p.Status, &p.Role); err != nil {
			return nil, nil, err
		}
		parts = append(parts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return &d, parts, nil
}

// loadChatHistory is the discussion's newest 50 messages, oldest first,
// without system's and without error rows (the [Retrying] / [Error]
// breadcrumbs, so they are not replayed to the model).
func (s *Service) loadChatHistory(ctx context.Context, id discussion.ID) ([]historyMessage, error) {
	rows, err := s.DB.Query(ctx,
		`SELECT DISTINCT ON (cmt.id) cmt.id, fa.name AS from_agent, cmt.message, cmt.sent_at
         FROM chat_message_texts cmt
         JOIN actors fa ON fa.id = cmt.from_actor_id
         WHERE cmt.discussion_id = $1 AND NOT (fa.name = 'system')
           AND NOT cmt.is_error
         ORDER BY cmt.id DESC LIMIT $2`, id.Param(), 50)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []historyMessage
	for rows.Next() {
		var m historyMessage
		var sentAt *time.Time
		if err := rows.Scan(&m.ID, &m.FromAgent, &m.Message, &sentAt); err != nil {
			return nil, err
		}
		if sentAt != nil {
			m.SentAt = *sentAt
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func renderDiscussionContext(agentName string, d *discussionRow) string {
	lines := []string{
		"You are \"" + agentName + "\", a participant in discussion #" + jsutil.NumberString(float64(d.ID)) + ".",
		"Topic: " + d.Topic,
	}
	if str(d.Context) != "" {
		lines = append(lines, "Context: "+*d.Context)
	}
	lines = append(lines, "Mode: "+d.Mode)
	return strings.Join(lines, "\n")
}

// buildSystemPrompt is the discussion prompt: the static part (bootstrap,
// instructions, soul, impressions, the discussion) is cached between calls;
// the dynamic part is the recalled notes and the reply rules.
func (s *Service) buildSystemPrompt(a *Agent, d *discussionRow, ragContext, soul, peopleContext string) (providers.SystemPrompt, error) {
	bootstrap, err := s.globalBootstrap()
	if err != nil {
		return providers.SystemPrompt{}, err
	}
	static := joinBlocks(
		wrapBlock("Bootstrap", "global-operating-directives", directiveBootstrap, bootstrap),
		wrapBlock("Instructions", "operating-rules", directiveInstructions, str(a.StartupInstructions)),
		wrapBlock("Self", "voice-identity", directiveSelf, soul),
		wrapBlock("Impressions", "private-relationship-notes", directiveImpressions, peopleContext),
		wrapBlock("Discussion", "active-context", directiveDiscussion, renderDiscussionContext(a.Name, d)),
	)
	dynamic := joinBlocks(
		wrapBlock("Recall", "relevant-memories", directiveRecall, ragContext),
		wrapStandalone("ReplyPolicy", "response-discretion", directiveReplyPolicy),
		wrapStandalone("OutputDirective", "response-format", directiveOutput),
	)
	return providers.StructuredPrompt(static, dynamic), nil
}

// buildUserMessage is the history as a JSON array of {sender, content}, so a
// message body can never pass for a new turn, plus the Vote block on a vote
// trigger with a question.
func buildUserMessage(history []historyMessage, triggerType string, voteQuestion *string) string {
	if len(history) == 0 {
		return "The discussion has just started. Share your initial thoughts on the topic."
	}
	entries := make([]any, len(history))
	for i, m := range history {
		o := jsutil.NewObject()
		o.Set("sender", canonicalSpeakerID(m.FromAgent))
		o.Set("content", str(m.Message))
		entries[i] = o
	}
	parts := []string{wrapBlock("Conversation", "active-dialogue", directiveConversation, jsutil.Marshal(entries))}
	if triggerType == "vote-proposed" && voteQuestion != nil && *voteQuestion != "" {
		parts = append(parts, wrapBlock("Vote", "ballot-required", directiveVote, jsutil.Marshal(*voteQuestion)))
	}
	return joinBlocks(parts...)
}

// voteChoice is parseVoteResponse's answer: choice is a JSON number, reason
// whatever the reply held (nil for a falsy one).
type voteChoice struct {
	choice float64
	reason any
}

var voteObject = regexp.MustCompile(`\{[^}]+\}`)

// parseVoteResponse reads the first {...} in a vote reply (up to its first
// closing brace) as JSON; nil when there is none, it is not JSON, or its
// choice is not a number.
func parseVoteResponse(response string) *voteChoice {
	m := voteObject.FindString(response)
	if m == "" {
		return nil
	}
	v, err := jsutil.Parse([]byte(m))
	if err != nil {
		return nil
	}
	o, ok := v.(*jsutil.Object)
	if !ok {
		return nil
	}
	n, isNumber := o.Value("choice").(json.Number)
	if !isNumber {
		return nil
	}
	choice, _ := jsutil.AsFloat(n)
	var reason any
	if r := o.Value("reason"); jsutil.Truthy(r) {
		reason = r
	}
	return &voteChoice{choice: choice, reason: reason}
}

// castVote is castVote: the agent's ballot, once, then the vote's
// evaluation (it may close the vote and conclude the discussion).
func (s *Service) castVote(ctx context.Context, voteID discussion.ID, agentName string, v *voteChoice) error {
	actor, err := s.Actors.RequireByName(ctx, agentName)
	if err != nil {
		return err
	}
	var one int
	err = s.DB.QueryRow(ctx, "SELECT 1 FROM discussion_ballots WHERE vote_id = $1 AND actor_id = $2", voteID.Param(), actor.ID).Scan(&one)
	if err == nil {
		s.logVA("already-voted", logger.Fields{{Key: "voteId", Value: voteID}, {Key: "agent", Value: agentName}})
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := s.DB.Exec(ctx, "INSERT INTO discussion_ballots (vote_id, actor_id, choice, reason) VALUES ($1, $2, $3, $4)",
		voteID.Param(), actor.ID, bindJS(v.choice), bindJS(v.reason)); err != nil {
		return err
	}
	if err := s.Discussions.EvaluateVote(ctx, voteID); err != nil {
		return err
	}
	s.logVA("vote-cast", logger.Fields{{Key: "voteId", Value: voteID}, {Key: "agent", Value: agentName}, {Key: "choice", Value: jsNumber(v.choice)}})
	return nil
}

// discussionSend posts as the agent to the discussion.
func (s *Service) discussionSend(ctx context.Context, from string, d *discussionRow, message string, isError bool) error {
	_, _, err := s.Chat.Send(ctx, chat.SendInput{FromAgent: from, DiscussionID: int64(d.ID), Message: &message, IsError: isError})
	return err
}

// postError posts "[Error: <error>]" to the discussion as the agent, as an
// error row; a failure to post is only logged.
func (s *Service) postError(ctx context.Context, agentName string, d *discussionRow, text string) {
	if err := s.discussionSend(ctx, agentName, d, "[Error: "+text+"]", true); err != nil {
		s.logVA("post-error-failed", logger.Fields{{Key: "agent", Value: agentName}, {Key: "error", Value: err.Error()}})
	}
}

// inFlight is the coalescing slot of one (discussion, agent): while a
// message or discussion-active generation runs, a new trigger of those kinds
// only marks a rerun.
type inFlight struct {
	rerunPending bool
}

// vaPayload is the virtual-agent system message: {type, discussionId,
// triggerType, voteId}. The ids are kept as sent.
type vaPayload struct {
	raw          any
	discussionID discussion.ID
	triggerType  any
	trigger      string
	voteID       discussion.ID
}

func parsePayload(payload json.RawMessage) (*vaPayload, error) {
	raw, err := jsutil.Parse(payload)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	p := &vaPayload{raw: raw, discussionID: discussion.RawID(fields["discussionId"]), voteID: discussion.RawID(fields["voteId"])}
	p.triggerType = jsutil.Undefined
	if o, ok := raw.(*jsutil.Object); ok {
		p.triggerType = o.Value("triggerType")
	}
	p.trigger, _ = p.triggerType.(string)
	return p, nil
}

// addressedTo reports whether body names the agent: any hyphen-separated
// part of its name as a whole word, case folded ("zbbs-ezekiel-crane" is
// named by "Ezekiel" or "crane").
func addressedTo(ctx context.Context, agentName, body string) (bool, error) {
	for _, token := range strings.Split(agentName, "-") {
		re, err := jsregex.CompileIgnoreCase(`\b` + escapeRegExp(token) + `\b`)
		if err != nil {
			return false, err
		}
		ok, err := re.MatchString(body, jsregex.NewBudget(ctx, impersonationBudget))
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// HandleVirtualAgent is handleVirtualAgent, the system handler for
// "virtual-agent" payloads: a discussion's joined virtual participants
// answer a message, open the discussion, or vote. Message triggers go only
// to the virtual agents the latest human message names (all of them when it
// names none), and none when the latest message is a virtual agent's own
// (that would loop). Message and discussion-active replies are paced
// (virtual_agent_response_delay_seconds, then
// virtual_agent_response_stagger_seconds per agent) and coalesced: while an
// agent's generation runs, another such trigger for it only queues one
// rerun on the fresh history. A vote reply is a ballot. An error returned
// is what Node's handler threw, which the system handler logs.
//
// ctx must outlive the pacing waits and slow providers.
func (s *Service) HandleVirtualAgent(ctx context.Context, payload json.RawMessage, meta system.Meta) error {
	p, err := parsePayload(payload)
	if err != nil {
		return err
	}
	if !p.discussionID.Truthy() {
		s.logVA("missing-discussion-id", logger.Fields{{Key: "payload", Value: logJS(p.raw)}})
		return nil
	}

	d, participants, err := s.loadDiscussion(ctx, p.discussionID)
	if err != nil {
		return err
	}
	if d == nil {
		s.logVA("discussion-not-found", logger.Fields{{Key: "discussionId", Value: p.discussionID}})
		return nil
	}
	if d.Status != "active" {
		s.logVA("discussion-not-active", logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "status", Value: d.Status}})
		return nil
	}

	var joinedVirtual []*Agent
	for _, part := range participants {
		if part.Status != "joined" {
			continue
		}
		a, err := s.LoadAgent(ctx, part.Agent)
		if err != nil {
			return err
		}
		if a != nil && a.Virtual {
			joinedVirtual = append(joinedVirtual, a)
		}
	}
	if len(joinedVirtual) == 0 {
		return nil
	}
	virtualNames := map[string]bool{}
	for _, a := range joinedVirtual {
		virtualNames[a.Name] = true
	}

	history, err := s.loadChatHistory(ctx, p.discussionID)
	if err != nil {
		return err
	}

	if p.trigger == "message" && len(history) > 0 {
		for i := len(history) - 1; i >= 0; i-- {
			if m := history[i]; m.FromAgent != "system" {
				if virtualNames[m.FromAgent] {
					s.logVA("skip-self-response", logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "lastFrom", Value: m.FromAgent}})
					return nil
				}
				break
			}
		}
	}

	var voteQuestion *string
	if p.trigger == "vote-proposed" && p.voteID.Truthy() {
		var q string
		err := s.DB.QueryRow(ctx, "SELECT question FROM discussion_votes WHERE id = $1", p.voteID.Param()).Scan(&q)
		if err == nil {
			voteQuestion = &q
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}

	targets := joinedVirtual
	if p.trigger == "message" && len(history) > 0 {
		if last := lastNonVA(history, virtualNames); last != nil && str(last.Message) != "" {
			var addressed, skipped []*Agent
			for _, a := range joinedVirtual {
				ok, err := addressedTo(ctx, a.Name, *last.Message)
				if err != nil {
					return err
				}
				if ok {
					addressed = append(addressed, a)
				} else {
					skipped = append(skipped, a)
				}
			}
			if len(addressed) > 0 {
				targets = addressed
				s.logVA("addressed", logger.Fields{
					{Key: "discussionId", Value: p.discussionID}, {Key: "from", Value: last.FromAgent},
					{Key: "addressed", Value: agentNames(addressed)}, {Key: "skipped", Value: agentNames(skipped)},
				})
			}
		}
	}

	// Votes skip the pacing so ballots stay responsive.
	baseDelaySec, staggerSec := 0.0, 0.0
	if p.trigger != "vote-proposed" {
		if baseDelaySec, err = s.configIntOr0("virtual_agent_response_delay_seconds"); err != nil {
			return err
		}
		if staggerSec, err = s.configIntOr0("virtual_agent_response_stagger_seconds"); err != nil {
			return err
		}
	}
	if baseDelaySec > 0 {
		if err := s.setTimeout(ctx, baseDelaySec*1000); err != nil {
			return err
		}
	}

	var loggedTrigger any = p.triggerType
	if !jsutil.Truthy(p.triggerType) {
		loggedTrigger = "message"
	}
	s.logVA("processing", logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "triggerType", Value: logJS(loggedTrigger)}, {Key: "virtualAgents", Value: agentNames(targets)}})

	coalesceable := p.trigger == "message" || p.trigger == "discussion-active"
	for i, agent := range targets {
		key := p.discussionID.String() + ":" + agent.Name
		if coalesceable {
			s.inFlightMu.Lock()
			if entry, busy := s.inFlight[key]; busy {
				entry.rerunPending = true
				s.inFlightMu.Unlock()
				s.logVA("coalesce-trigger", withJS(logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "agent", Value: agent.Name}}, "triggerType", p.triggerType))
				continue
			}
			if s.inFlight == nil {
				s.inFlight = map[string]*inFlight{}
			}
			s.inFlight[key] = &inFlight{}
			s.inFlightMu.Unlock()
		}

		// Node waited and reloaded before its try block, so a failure here
		// left the slot taken and ended the handler.
		if staggerSec > 0 && i > 0 {
			if err := s.setTimeout(ctx, float64(i)*staggerSec*1000); err != nil {
				return err
			}
		}
		// Fresh history: messages may have come in during the delay and the
		// stagger, and answering an older view would make the rerun check
		// answer them a second time.
		fresh, err := s.loadChatHistory(ctx, p.discussionID)
		if err != nil {
			return err
		}

		err = s.answerDiscussion(ctx, agent, d, participants, p, fresh, voteQuestion, key, coalesceable, virtualNames)
		if coalesceable {
			s.inFlightMu.Lock()
			delete(s.inFlight, key)
			s.inFlightMu.Unlock()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// isHumanMessage is a message from neither system nor a virtual
// participant.
func isHumanMessage(m historyMessage, virtualNames map[string]bool) bool {
	return m.FromAgent != "system" && !virtualNames[m.FromAgent]
}

// lastNonVA is the newest human message.
func lastNonVA(history []historyMessage, virtualNames map[string]bool) *historyMessage {
	for i := len(history) - 1; i >= 0; i-- {
		if isHumanMessage(history[i], virtualNames) {
			return &history[i]
		}
	}
	return nil
}

// withJS appends a JavaScript value to log fields; JSON.stringify leaves an
// undefined member out.
func withJS(fields logger.Fields, key string, v any) logger.Fields {
	if v == jsutil.Undefined {
		return fields
	}
	return append(fields, logger.Field{Key: key, Value: logJS(v)})
}

func agentNames(agents []*Agent) []string {
	out := []string{}
	for _, a := range agents {
		out = append(out, a.Name)
	}
	return out
}

// configIntOr0 is parseInt(config.get(key)) || 0.
func (s *Service) configIntOr0(key string) (float64, error) {
	n, err := s.configInt(key)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(n) {
		return 0, nil
	}
	return n, nil
}

// answerDiscussion is Node's per-agent try block: one generation, then once
// more for each coalesced burst that brought a new human message.
func (s *Service) answerDiscussion(ctx context.Context, agent *Agent, d *discussionRow, participants []participant, p *vaPayload,
	history []historyMessage, voteQuestion *string, key string, coalesceable bool, virtualNames map[string]bool) error {
	trigger := p.trigger
	var triggerType any = p.triggerType
	for {
		// The newest human message this generation answers: a rerun fires
		// only for one newer than it, so another virtual agent posting in
		// the meantime cannot hide a pending human message.
		var watermark *int
		if m := lastNonVA(history, virtualNames); m != nil {
			id := m.ID
			watermark = &id
		}

		done, err := s.generateDiscussionReply(ctx, agent, d, participants, p, history, trigger, triggerType, voteQuestion)
		if err != nil {
			return s.discussionFailed(ctx, agent, err)
		}
		if done || !coalesceable {
			return nil
		}

		s.inFlightMu.Lock()
		entry := s.inFlight[key]
		rerun := entry != nil && entry.rerunPending
		if rerun {
			entry.rerunPending = false
		}
		s.inFlightMu.Unlock()
		if !rerun {
			return nil
		}
		fresh, err := s.loadChatHistory(ctx, p.discussionID)
		if err != nil {
			return s.discussionFailed(ctx, agent, err)
		}
		hasNew := false
		for _, m := range fresh {
			if isHumanMessage(m, virtualNames) && (watermark == nil || m.ID > *watermark) {
				hasNew = true
				break
			}
		}
		if !hasNew {
			s.logVA("rerun-skip-no-new-message", logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "agent", Value: agent.Name}})
			return nil
		}
		history = fresh
		trigger = "message"
		triggerType = "message"
		s.logVA("rerun", logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "agent", Value: agent.Name}})
	}
}

// generateDiscussionReply is one pass of Node's rerun loop up to the rerun
// check. done is a stop before the call (no key, no provider or model, rate
// limited, over budget), which posts an [Error: ...] and skips the rerun.
func (s *Service) generateDiscussionReply(ctx context.Context, agent *Agent, d *discussionRow, participants []participant, p *vaPayload,
	history []historyMessage, trigger string, triggerType any, voteQuestion *string) (done bool, err error) {
	if str(agent.APIKey) == "" {
		s.postError(ctx, agent.Name, d, "No API key configured")
		return true, nil
	}
	if str(agent.Provider) == "" || str(agent.Model) == "" {
		s.postError(ctx, agent.Name, d, "No provider/model configured")
		return true, nil
	}
	provider, model := *agent.Provider, *agent.Model
	apiKey, err := s.Providers.DecryptAPIKey(*agent.APIKey)
	if err != nil {
		return false, err
	}
	conf, err := BuildProviderConf(agent)
	if err != nil {
		return false, err
	}

	ragContext := s.loadRAGContext(ctx, agent.Name, d.Topic)
	soul := s.loadSoul(ctx, agent.Name)
	var counterparts []string
	for _, part := range participants {
		if part.Agent != agent.Name && part.Status == "joined" {
			counterparts = append(counterparts, part.Agent)
		}
	}
	peopleContext := s.loadPeopleContext(ctx, agent.Name, counterparts)

	systemPrompt, err := s.buildSystemPrompt(agent, d, ragContext, soul, peopleContext)
	if err != nil {
		return false, err
	}
	userMessage := buildUserMessage(history, trigger, voteQuestion)

	limited, err := s.IsRateLimited(ctx, agent.Name, "")
	if err != nil {
		return false, err
	}
	if limited {
		s.postError(ctx, agent.Name, d, "Rate limited — too many API calls. Cooling down.")
		return true, nil
	}
	reason, err := s.isOverCostLimit(ctx, agent)
	if err != nil {
		return false, err
	}
	if reason != "" {
		s.postError(ctx, agent.Name, d, reason)
		return true, nil
	}

	// One attempt, no retry: a failing agent leaves the discussion rather
	// than holding the others up through a long backoff.
	call, err := s.Providers.CreateProvider(provider, model, apiKey, conf)
	if err != nil {
		return false, err
	}
	fromAgents := make([]string, len(history))
	for i, m := range history {
		fromAgents[i] = m.FromAgent
	}
	speakers := collectDistinctSpeakers(fromAgents, agent.Name)

	if err := s.RecordCall(agent.Name, ""); err != nil {
		return false, err
	}
	start := s.now()
	callWith := func(opts *providers.RawOpts) (*providers.Result, error) {
		return WithActivity(ctx, s, agent.Name, func() (*providers.Result, error) {
			return call(ctx, systemPrompt, userMessage, opts)
		})
	}
	result, err := callWith(&providers.RawOpts{Cache: true, Stop: discussionStop})
	if err != nil {
		// The one failure retried: this provider or model refuses the stop
		// field. The scan below still catches an impersonation.
		if !isStopUnsupportedError(err) {
			return false, err
		}
		s.logVA("stop-unsupported", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "provider", Value: provider}, {Key: "model", Value: model}, {Key: "error", Value: err.Error()}})
		if result, err = callWith(&providers.RawOpts{Cache: true}); err != nil {
			return false, err
		}
	}

	// The scan runs whether or not the stop fired: it cuts the reply at the
	// first line that starts another speaker's turn.
	response := result.Text
	scan := ImpersonationScan{}
	if text, ok := response.(string); ok && text != "" {
		scan = scanForImpersonation(text, speakers)
		response = scan.Text
		if scan.Truncated {
			s.logVA("impersonation-truncated", logger.Fields{
				{Key: "discussionId", Value: p.discussionID}, {Key: "agent", Value: agent.Name}, {Key: "rule", Value: scan.Rule},
				{Key: "droppedChars", Value: scan.DroppedChars}, {Key: "provider", Value: provider}, {Key: "model", Value: model},
			})
		}
	}

	duration := s.now().Sub(start)
	usage := result.Usage
	usageID, err := s.recordUsage(ctx, agent.Name, provider, model, usage, "discussion", "", "")
	if err != nil {
		return false, err
	}
	cost, err := providers.CalculateCost(provider, model, usage)
	if err != nil {
		return false, err
	}
	s.goLogCall(CallLog{
		ActorID: agent.ActorID, AgentName: agent.Name, Context: "discussion", ContextID: p.discussionID.String(),
		Provider: provider, Model: model, SystemPrompt: &systemPrompt, UserMessage: userMessage,
		Response: response, Usage: &usage, Cost: cost, Duration: duration, UsageID: &usageID,
	})

	if trigger == "vote-proposed" && p.voteID.Truthy() {
		text, err := matchTarget(response)
		if err != nil {
			return false, err
		}
		if v := parseVoteResponse(text); v != nil {
			if err := s.castVote(ctx, p.voteID, agent.Name, v); err != nil {
				return false, err
			}
			if v.reason != nil {
				reasonText, err := messageParam(v.reason)
				if err != nil {
					return false, err
				}
				if err := s.discussionSend(ctx, agent.Name, d, reasonText, false); err != nil {
					return false, err
				}
			}
		} else if jsutil.Trim(text) != "" {
			s.logVA("vote-parse-failed", logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "agent", Value: agent.Name}})
			if err := s.discussionSend(ctx, agent.Name, d, text, false); err != nil {
				return false, err
			}
		} else {
			// Empty, or all of it cut by the scan: nothing is posted.
			s.logVA("vote-parse-failed-empty", logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "agent", Value: agent.Name}, {Key: "truncated", Value: scan.Truncated}})
		}
	} else {
		text, posted, err := replyText(response)
		if err != nil {
			return false, err
		}
		if posted {
			if err := s.discussionSend(ctx, agent.Name, d, text, false); err != nil {
				return false, err
			}
		} else {
			s.logVA("response-empty-skipped", logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "agent", Value: agent.Name}, {Key: "truncated", Value: scan.Truncated}})
		}
	}

	// Past the posting above, a truthy reply is a string.
	length := 0
	if text, ok := response.(string); ok {
		length = jsutil.UTF16Len(text)
	}
	responded := withJS(logger.Fields{{Key: "discussionId", Value: p.discussionID}, {Key: "agent", Value: agent.Name}}, "triggerType", triggerType)
	s.logVA("responded", append(responded, logger.Field{Key: "responseLength", Value: length}))

	// A ballot is not learning material.
	if trigger != "vote-proposed" {
		goDetached(func(bg context.Context) {
			if err := s.ExtractLearnings(bg, agent, systemPrompt, userMessage, response, "discussion", d.Topic); err != nil {
				s.logVA("learning-extraction-failed", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "error", Value: err.Error()}})
			}
		})
	}
	goDetached(func(bg context.Context) {
		meta := TranscriptMeta{DiscussionID: p.discussionID.String(), Model: model}
		if err := s.LogTranscript(bg, agent.Name, systemPrompt, userMessage, response, &usage, "discussion", meta); err != nil {
			s.logVA("transcript-log-failed", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "error", Value: err.Error()}})
		}
	})
	return false, nil
}

// matchTarget is the reply as response.match reads it: a string, or V8's
// TypeError for anything else.
func matchTarget(response any) (string, error) {
	switch x := response.(type) {
	case string:
		return x, nil
	case nil:
		return "", typeError("Cannot read properties of null (reading 'match')")
	}
	if response == jsutil.Undefined {
		return "", typeError("Cannot read properties of undefined (reading 'match')")
	}
	return "", typeError("response.match is not a function")
}

// replyText is `response && response.trim().length > 0`: posted is false
// for a falsy or blank reply; a truthy reply that is not a string has no
// trim.
func replyText(response any) (text string, posted bool, err error) {
	if !jsutil.Truthy(response) {
		return "", false, nil
	}
	s, ok := response.(string)
	if !ok {
		return "", false, typeError("response.trim is not a function")
	}
	return s, jsutil.Trim(s) != "", nil
}

// discussionFailed is Node's catch block for a failed generation. Its
// logCall call named systemPrompt and userMessage, which were declared
// inside the try block, so it threw "systemPrompt is not defined" right
// after the error usage row: no call row, no error_log row, no
// [Malfunction] message, no leave, and the agents after this one in the
// wave are not served. The system handler logs that error.
func (s *Service) discussionFailed(ctx context.Context, agent *Agent, cause error) error {
	s.recordUsage(ctx, agent.Name, str(agent.Provider), str(agent.Model), providers.Usage{}, "discussion", "error", cause.Error())
	return errors.New("systemPrompt is not defined")
}
