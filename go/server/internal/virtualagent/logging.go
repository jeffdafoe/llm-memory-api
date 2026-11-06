package virtualagent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// staticText is the part of a system prompt that is logged: a plain
// prompt, or a structured prompt's static part. The dynamic part (RAG
// results) is never stored, because stored text is chunked into the search
// index and would come back in later RAG results. A structured prompt with
// no static part is logged flattened, as in Node.
func staticText(p providers.SystemPrompt) string {
	if p.Static != "" {
		return p.Static
	}
	return providers.FlattenPrompt(p)
}

// CallLog is one virtual_agent_calls row (Node's logCall options).
type CallLog struct {
	ActorID   int
	AgentName string
	// Context is the trigger (mail, chat, discussion, dream, soul, learning);
	// ContextID the mail UUID, discussion ID and so on.
	Context   string
	ContextID string
	Provider  string
	Model     string
	// SystemPrompt nil is a NULL system_prompt.
	SystemPrompt *providers.SystemPrompt
	UserMessage  string
	// Response is the provider's text (a jsutil value).
	Response any
	Usage    *providers.Usage
	Cost     float64
	Duration time.Duration
	Err      error
	UsageID  *int
	// SceneID is the engine cascade UUID; SimActorID / SimActorName the
	// in-world actor a shared-VA turn is for. Empty outside sim chat.
	SceneID        string
	ConversationID string
	SimActorID     string
	SimActorName   string
}

// goLogCall writes the call row in the background; a failure is logged and
// never reaches the caller.
func (s *Service) goLogCall(c CallLog) {
	db.Background(func(ctx context.Context) {
		s.LogCall(ctx, c)
	})
}

// LogCall is logCall: the full request and response of one call, for
// diagnostics. A failure is logged, not returned.
func (s *Service) LogCall(ctx context.Context, c CallLog) {
	var system any
	if c.SystemPrompt != nil {
		system = staticText(*c.SystemPrompt)
	}
	usage := providers.Usage{}
	if c.Usage != nil {
		usage = *c.Usage
	}
	status := "success"
	var response, errorMessage, code any
	if c.Err != nil {
		status = "error"
		response = c.Err.Error()
		errorMessage = c.Err.Error()
		code = statusCode(c.Err)
	} else if jsutil.Truthy(c.Response) {
		response = bindJS(c.Response)
	}
	var servedBy any
	if usage.ServedBy != nil && *usage.ServedBy != "" {
		servedBy = *usage.ServedBy
	}
	var cost any = 0
	if c.Cost != 0 && !math.IsNaN(c.Cost) {
		cost = jsutil.Num(c.Cost)
	}
	var duration any
	if ms := c.Duration.Milliseconds(); ms != 0 {
		duration = ms
	}
	var usageID any
	if c.UsageID != nil && *c.UsageID != 0 {
		usageID = *c.UsageID
	}
	_, err := s.DB.Exec(ctx,
		`INSERT INTO virtual_agent_calls
             (actor_id, context, context_id, provider, model, system_prompt, user_message,
              response, status, status_code, error_message,
              input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
              cost, duration_ms, usage_id, scene_id, conversation_id,
              sim_actor_id, sim_actor_name, served_by)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
		c.ActorID, nullable(c.Context), nullable(c.ContextID), c.Provider, c.Model, system, nullable(c.UserMessage),
		response, status, code, errorMessage,
		bindJS(orZero(usage.InputTokens)), bindJS(orZero(usage.OutputTokens)),
		bindJS(orZero(usage.CacheReadInputTokens)), bindJS(orZero(usage.CacheCreationInputTokens)),
		cost, duration, usageID, nullable(c.SceneID), nullable(c.ConversationID),
		nullable(c.SimActorID), nullable(c.SimActorName), servedBy)
	if err != nil {
		s.logVA("call-log-error", logger.Fields{{Key: "agent", Value: c.AgentName}, {Key: "error", Value: err.Error()}})
	}
}

// localZone is the server's time zone, which the transcript slug uses (as
// Node's Date getters did); tests replace it.
var localZone = time.Local

// TranscriptMeta is the header of a logged transcript.
type TranscriptMeta struct {
	RequestingAgent string
	DiscussionID    int
	Model           string
}

// maxTranscriptContext bounds the system prompt plus the user message, in
// UTF-16 units, well under the note size cap.
const maxTranscriptContext = 100000

// LogTranscript is logTranscript: the interaction saved as a note in the
// agent's namespace (conversations/<local date>-<time>-<random>). Agents
// with dream_mode none, sim or sim-shared are skipped: nothing reads their
// transcripts, and sim payloads are tool-use JSON a dream cannot use (the
// sim distiller writes their daily note instead).
func (s *Service) LogTranscript(ctx context.Context, agentName string, system providers.SystemPrompt, userMessage string, response any, usage *providers.Usage, triggerType string, meta TranscriptMeta) error {
	var mode string
	err := s.DB.QueryRow(ctx,
		`SELECT agc.dream_mode FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE ac.name = $1`, agentName).Scan(&mode)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && (mode == "none" || mode == "sim" || mode == "sim-shared") {
		return nil
	}

	now := s.now().In(localZone)
	datePart := now.Format("20060102")
	timePart := now.Format("150405")
	slug := "conversations/" + datePart + "-" + timePart + "-" + randomSuffix()

	systemText := staticText(system)
	userText := userMessage
	if jsutil.UTF16Len(systemText)+jsutil.UTF16Len(userText) > maxTranscriptContext {
		available := maxTranscriptContext - jsutil.UTF16Len(userText)
		if available > 1000 {
			systemText = jsutil.TruncateUTF16(systemText, available) + "\n\n[... truncated ...]"
		} else {
			systemText = "[truncated — too large]"
			userText = jsutil.TruncateUTF16(userText, maxTranscriptContext) + "\n\n[... truncated ...]"
		}
	}

	var lines []string
	lines = append(lines, "- **Trigger:** "+triggerType)
	if meta.RequestingAgent != "" {
		lines = append(lines, "- **From:** "+meta.RequestingAgent)
	}
	if meta.DiscussionID != 0 {
		lines = append(lines, fmt.Sprintf("- **Discussion:** #%d", meta.DiscussionID))
	}
	if meta.Model != "" {
		lines = append(lines, "- **Model:** "+meta.Model)
	}
	if usage != nil {
		in, err := orUnknown(usage.InputTokens)
		if err != nil {
			return err
		}
		out, err := orUnknown(usage.OutputTokens)
		if err != nil {
			return err
		}
		lines = append(lines, "- **Tokens:** "+in+" in / "+out+" out")
	}
	responseText, err := jsutil.ValueString(response)
	if err != nil {
		return err
	}
	content := "## Metadata\n" + strings.Join(lines, "\n") + "\n\n## System Prompt\n\n" + systemText +
		"\n\n## User Message\n\n" + userText + "\n\n## Response\n\n" + responseText
	title := triggerType + " — " + datePart + " " + timePart
	_, err = s.Notes.Save(ctx, notes.SaveInput{Namespace: agentName, Title: title, Content: content, Slug: slug, CreatedBy: agentName})
	return err
}

// orUnknown is `${v || '?'}`.
func orUnknown(v any) (string, error) {
	if !jsutil.Truthy(v) {
		return "?", nil
	}
	return jsutil.ValueString(v)
}

// randomSuffix is Math.random().toString(36).substring(2, 6): four base-36
// characters.
func randomSuffix() string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 4)
	for i := range b {
		b[i] = digits[rand.IntN(len(digits))]
	}
	return string(b)
}

// isLearningEnabled: the global toggle is "true" and the agent has not
// turned learning off.
func (s *Service) isLearningEnabled(a *Agent) (bool, error) {
	v, err := s.Config.Get("virtual_agent_learning_enabled")
	if err != nil {
		return false, err
	}
	return v == "true" && a.LearningEnabled, nil
}

// buildExtractionPrompt is the learning-extraction question for an
// interaction type (discussion, chat, else mail).
func buildExtractionPrompt(interactionType, contextHint string) string {
	base := "Review the interaction above. Extract 1-3 factual observations worth remembering for future interactions. " +
		"Focus only on factual information, preferences, or decisions — not tone or style. " +
		"Include names, identifiers, and concrete details rather than vague generalizations. " +
		"If nothing new was learned, return exactly NONE.\n\n" +
		"Format each observation as a bullet point starting with \"- \"."
	switch interactionType {
	case "discussion":
		return "What factual information, decisions, or preferences did you learn from this discussion about \"" + contextHint + "\"?\n\n" + base
	case "chat":
		return "What factual information or preferences did you learn about " + contextHint + " from this conversation?\n\n" + base
	}
	return "What factual information or action items did you learn from this mail exchange?\n\n" + base
}

// ExtractLearnings is extractLearnings: after an interaction long enough to
// be worth it (virtual_agent_learning_min_tokens, estimated at four
// characters a token), the agent is asked for the facts it learned, and
// its answer is saved as learnings/<UTC date>-<time> in its namespace. NONE,
// an empty answer and a truncated answer save nothing.
func (s *Service) ExtractLearnings(ctx context.Context, a *Agent, system providers.SystemPrompt, userMessage string, response any, interactionType, contextHint string) error {
	enabled, err := s.isLearningEnabled(a)
	if err != nil || !enabled {
		return err
	}
	flat := providers.FlattenPrompt(system)
	minValue, err := s.Config.Get("virtual_agent_learning_min_tokens")
	if err != nil {
		return err
	}
	minTokens := jsutil.IntOr(minValue, 500)
	responseText, err := jsutil.ValueString(response)
	if err != nil {
		return err
	}
	estimated := int(math.Ceil(float64(jsutil.UTF16Len(flat+userMessage+responseText)) / 4))
	if estimated < minTokens {
		s.logVA("learning-skip-short", logger.Fields{{Key: "agent", Value: a.Name}, {Key: "estimatedTokens", Value: estimated}, {Key: "minTokens", Value: minTokens}})
		return nil
	}

	prompt := providers.TextPrompt("You are a knowledge extraction assistant. Your job is to identify key facts worth remembering from interactions.")
	user := "System prompt:\n" + flat + "\n\nUser message:\n" + userMessage + "\n\nYour response:\n" + responseText +
		"\n\n---\n\n" + buildExtractionPrompt(interactionType, contextHint)
	result, err := s.Invoke(ctx, a.Name, InvokeOptions{
		SystemPrompt: &prompt, UserMessage: user, Context: "learning",
		SkipRateLimit: true, SkipCostLimit: true,
	})
	if err != nil {
		return err
	}
	if result.Truncated {
		s.logVA("learning-truncated", logger.Fields{{Key: "agent", Value: a.Name}, {Key: "interactionType", Value: interactionType}})
		return nil
	}
	text, err := extractionText(result.Text)
	if err != nil {
		return err
	}
	trimmed := jsutil.Trim(text)
	if strings.ToUpper(trimmed) == "NONE" || trimmed == "" {
		s.logVA("learning-none", logger.Fields{{Key: "agent", Value: a.Name}, {Key: "interactionType", Value: interactionType}})
		return nil
	}

	stamp := s.now().UTC().Format("20060102-150405")
	slug := "learnings/" + stamp
	title := "Learning " + stamp + " (" + interactionType + ")"
	if _, err := s.Notes.Save(ctx, notes.SaveInput{Namespace: a.Name, Title: title, Content: text, Slug: slug, CreatedBy: a.Name}); err != nil {
		return err
	}
	s.logVA("learning-saved", logger.Fields{{Key: "agent", Value: a.Name}, {Key: "slug", Value: slug}, {Key: "interactionType", Value: interactionType}, {Key: "length", Value: jsutil.UTF16Len(text)}})
	return nil
}

// extractionText is the answer as `(extractionResult || ”).trim()` read it:
// a falsy answer is "", and anything else that is not a string has no trim.
func extractionText(v any) (string, error) {
	if !jsutil.Truthy(v) {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", errors.New(`(extractionResult || "").trim is not a function`)
}
