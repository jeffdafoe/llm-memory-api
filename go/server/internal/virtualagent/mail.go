package virtualagent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// missingConfigParts names the settings a virtual agent lacks before it can
// call its provider, in Node's order; empty means it has all three.
func missingConfigParts(a *Agent) []string {
	var missing []string
	if str(a.APIKey) == "" {
		missing = append(missing, "API key")
	}
	if str(a.Provider) == "" {
		missing = append(missing, "provider")
	}
	if str(a.Model) == "" {
		missing = append(missing, "model")
	}
	return missing
}

// unanswerableNotice is the [Error] text a virtual agent sends back when it
// will not reply at all: the sender has no access to it ("no-access"), or it
// lacks the named settings.
func unanswerableNotice(agentName, reason string, missing []string) string {
	if reason == "no-access" {
		return "[Error] You do not have access to " + agentName + ", so it will not reply. " +
			"Its owner can grant access in the dashboard (Agents → " + agentName + " → Permissions)."
	}
	pronoun := "them"
	if len(missing) == 1 {
		pronoun = "it"
	}
	return "[Error] " + agentName + " cannot reply: it has no " + strings.Join(missing, ", ") + " set. " +
		"Its owner can add " + pronoun + " in the dashboard (Agents → " + agentName + ")."
}

// UnanswerableNotice is unanswerableNotice, for mail and chat.
func (s *Service) UnanswerableNotice(agentName, reason string, missing []string) string {
	return unanswerableNotice(agentName, reason, missing)
}

// formatDuration is formatDuration: "45 seconds", "5 minutes", "1 hour(s)
// 10 minutes". Minutes round up.
func formatDuration(totalSeconds float64) string {
	if totalSeconds < 60 {
		return jsutil.NumberString(totalSeconds) + " seconds"
	}
	hours := math.Floor(totalSeconds / 3600)
	minutes := math.Ceil(math.Mod(totalSeconds, 3600) / 60)
	switch {
	case hours > 0 && minutes > 0:
		return jsutil.NumberString(hours) + " hour(s) " + jsutil.NumberString(minutes) + " minutes"
	case hours > 0:
		return jsutil.NumberString(hours) + " hour(s)"
	}
	return jsutil.NumberString(minutes) + " minutes"
}

// replySubject is the subject with "Re: " in front, unless it has it.
func replySubject(subject string) string {
	if strings.HasPrefix(subject, "Re: ") {
		return subject
	}
	return "Re: " + subject
}

// buildMailSystemPrompt is buildMailSystemPrompt. The static part is the
// global bootstrap, the agent's instructions, its soul, its impressions of
// the sender (after people_context_preamble) and who it is; the dynamic part
// is the recalled notes and the reply instruction.
func (s *Service) buildMailSystemPrompt(a *Agent, ragContext, soul, peopleContext string) (providers.SystemPrompt, error) {
	var static strings.Builder
	bootstrap, err := s.Config.Get("global_bootstrap")
	if err != nil {
		return providers.SystemPrompt{}, err
	}
	if bootstrap != "" {
		static.WriteString(bootstrap + "\n\n")
	}
	if str(a.StartupInstructions) != "" {
		static.WriteString(*a.StartupInstructions + "\n\n")
	}
	if soul != "" {
		static.WriteString(soul + "\n\n")
	}
	if peopleContext != "" {
		preamble, err := s.Config.Get("people_context_preamble")
		if err != nil {
			return providers.SystemPrompt{}, err
		}
		if preamble != "" {
			static.WriteString(preamble + "\n\n")
		}
		static.WriteString(peopleContext + "\n\n")
	}
	static.WriteString(`You are "` + a.Name + `". You have received a mail message and should compose a reply.`)
	if str(a.Personality) != "" {
		static.WriteString("\nYour personality: " + *a.Personality)
	}

	dynamic := ""
	if ragContext != "" {
		dynamic += "Relevant knowledge from your notes:\n" + ragContext + "\n\n"
	}
	dynamic += "Compose a thoughtful reply. Write only the reply body — no subject line or headers."
	return providers.StructuredPrompt(static.String(), dynamic), nil
}

// buildMailUserMessage is the incoming mail as the user message.
func buildMailUserMessage(fromAgent, subject, body string) string {
	return "From: " + fromAgent + "\nSubject: " + subject + "\n\n" + body
}

// incomingMail is the mail row handleDirectMail reads.
type incomingMail struct {
	Subject   string
	Body      string
	FromAgent string
}

const ackMailSQL = "UPDATE mail SET acked_at = NOW() WHERE id = $1 AND to_actor_id = $2 AND acked_at IS NULL"

// HandleDirectMail is handleDirectMail: a virtual agent's reply to mail an
// agent sent it. The reply is threaded onto the mail and the mail acked. An
// agent short of settings, rate-limited or over its cost limit sends an
// [Error] notice instead. A failed call is retried on the backoff, with a
// [Retrying] notice after the first failure; when it fails for good the
// mail is acked and a "Delivery failed" notice sent. Learning extraction and
// the transcript run in the background after the reply.
//
// An error is returned only when the agent or the mail cannot be read; the
// rest is answered in mail. ctx must outlive a slow provider and the retry
// backoff, so the caller does not bound it.
func (s *Service) HandleDirectMail(ctx context.Context, virtualAgentName, fromAgent, mailID string) error {
	s.logVA("direct-mail-entry", logger.Fields{{Key: "agent", Value: virtualAgentName}, {Key: "from", Value: fromAgent}, {Key: "mailId", Value: mailID}})
	agent, err := s.LoadAgent(ctx, virtualAgentName)
	if err != nil {
		return err
	}
	if agent == nil || !agent.Virtual {
		return nil
	}

	var m incomingMail
	err = s.DB.QueryRow(ctx,
		`SELECT m.subject, m.body, fa.name AS from_agent
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         JOIN actors ta ON ta.id = m.to_actor_id
         WHERE m.id = $1`, mailID).Scan(&m.Subject, &m.Body, &m.FromAgent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	// Checked after the mail is read, so the notice threads onto its subject.
	if missing := missingConfigParts(agent); len(missing) > 0 {
		s.logVA("direct-mail-skip", logger.Fields{{Key: "agent", Value: virtualAgentName}, {Key: "reason", Value: "missing config"}})
		if _, err := s.Mail.Send(ctx, fromAgent, virtualAgentName, replySubject(m.Subject),
			unanswerableNotice(virtualAgentName, "missing-config", missing), mailID); err != nil {
			s.logVA("error-feedback-failed", logger.Fields{{Key: "agent", Value: virtualAgentName}, {Key: "error", Value: err.Error()}})
		}
		return nil
	}

	s.logVA("direct-mail-processing", logger.Fields{{Key: "agent", Value: virtualAgentName}, {Key: "from", Value: fromAgent}, {Key: "mailId", Value: mailID}, {Key: "subject", Value: m.Subject}})

	if err := s.answerMail(ctx, agent, fromAgent, mailID, m); err != nil {
		s.mailFailed(ctx, agent, virtualAgentName, fromAgent, mailID, err)
	}
	return nil
}

// answerMail is handleDirectMail's try block.
func (s *Service) answerMail(ctx context.Context, agent *Agent, fromAgent, mailID string, m incomingMail) error {
	provider, model := *agent.Provider, *agent.Model
	apiKey, err := s.Providers.DecryptAPIKey(*agent.APIKey)
	if err != nil {
		return err
	}
	conf, err := BuildProviderConf(agent)
	if err != nil {
		return err
	}

	ragContext := s.loadRAGContext(ctx, agent.Name, m.Subject+" "+m.Body)
	soul := s.loadSoul(ctx, agent.Name)
	peopleContext := s.loadPeopleContext(ctx, agent.Name, []string{fromAgent})

	system, err := s.buildMailSystemPrompt(agent, ragContext, soul, peopleContext)
	if err != nil {
		return err
	}
	userMessage := buildMailUserMessage(m.FromAgent, m.Subject, m.Body)
	subject := replySubject(m.Subject)

	limited, err := s.IsRateLimited(ctx, agent.Name, "")
	if err != nil {
		return err
	}
	if limited {
		s.logVA("direct-mail-rate-limited", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "from", Value: fromAgent}, {Key: "mailId", Value: mailID}})
		_, err := s.Mail.Send(ctx, fromAgent, agent.Name, subject,
			"[Error] Rate limited — too many API calls. Please wait before trying again.", mailID)
		return err
	}

	reason, err := s.isOverCostLimit(ctx, agent)
	if err != nil {
		return err
	}
	if reason != "" {
		s.logVA("direct-mail-over-cost-limit", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "from", Value: fromAgent}, {Key: "mailId", Value: mailID}, {Key: "reason", Value: reason}})
		_, err := s.Mail.Send(ctx, fromAgent, agent.Name, subject, "[Error] "+reason, mailID)
		return err
	}

	call, err := s.Providers.CreateProvider(provider, model, apiKey, conf)
	if err != nil {
		return err
	}
	if err := s.RecordCall(agent.Name, ""); err != nil {
		return err
	}
	start := s.now()
	result, err := retryWithBackoff(ctx, s, agent.Name, func(ctx context.Context) (*providers.Result, error) {
		return WithActivity(ctx, s, agent.Name, func() (*providers.Result, error) {
			return call(ctx, system, userMessage, nil)
		})
	}, func(err error, info RetryInfo) error {
		_, sendErr := s.Mail.Send(ctx, fromAgent, agent.Name, subject,
			"[Retrying] Initial attempt failed: "+err.Error()+"\n\nRetrying "+strconv.Itoa(info.RetriesRemaining)+
				" more time(s) over the next ~"+formatDuration(info.TotalSeconds)+". You'll receive the final response or a failure notice.",
			mailID)
		return sendErr
	})
	if err != nil {
		return err
	}
	duration := s.now().Sub(start)
	usageID, err := s.recordUsage(ctx, agent.Name, provider, model, result.Usage, "mail", "", "")
	if err != nil {
		return err
	}
	cost, err := providers.CalculateCost(provider, model, result.Usage)
	if err != nil {
		return err
	}
	s.goLogCall(CallLog{
		ActorID: agent.ActorID, AgentName: agent.Name, Context: "mail", ContextID: mailID,
		Provider: provider, Model: model, SystemPrompt: &system, UserMessage: userMessage,
		Response: result.Text, Usage: &result.Usage, Cost: cost, Duration: duration, UsageID: &usageID,
	})

	if _, err := s.DB.Exec(ctx, ackMailSQL, mailID, agent.ActorID); err != nil {
		return err
	}

	// An empty reply is refused here: mail send would reject it with its
	// generic "Required fields" message, which reads as the agent being
	// offline.
	response, err := mailResponse(result.Text)
	if err != nil {
		return err
	}
	if _, err := s.Mail.Send(ctx, fromAgent, agent.Name, subject, response, mailID); err != nil {
		return err
	}
	s.logVA("direct-mail-responded", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "to", Value: fromAgent}, {Key: "mailId", Value: mailID}, {Key: "responseLength", Value: jsutil.UTF16Len(response)}})

	usage := result.Usage
	db.BackgroundUnbounded(func(ctx context.Context) {
		if err := s.ExtractLearnings(ctx, agent, system, userMessage, response, "mail", ""); err != nil {
			s.logVA("learning-extraction-failed", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "error", Value: err.Error()}})
		}
	})
	db.BackgroundUnbounded(func(ctx context.Context) {
		if err := s.LogTranscript(ctx, agent.Name, system, userMessage, response, &usage, "mail",
			TranscriptMeta{RequestingAgent: fromAgent, Model: model}); err != nil {
			s.logVA("transcript-log-failed", logger.Fields{{Key: "agent", Value: agent.Name}, {Key: "error", Value: err.Error()}})
		}
	})
	return nil
}

// mailResponse is the provider's text as the reply body: an empty or blank
// text is refused, and one that is not a string has no trim.
func mailResponse(text any) (string, error) {
	if !jsutil.Truthy(text) {
		return "", errors.New("LLM returned empty response")
	}
	s, ok := text.(string)
	if !ok {
		return "", errors.New("response.trim is not a function")
	}
	if jsutil.Trim(s) == "" {
		return "", errors.New("LLM returned empty response")
	}
	return s, nil
}

// mailFailed is handleDirectMail's catch block: the failure is recorded as
// an error usage row and call (its prompts and duration are null, as they
// were out of the catch block's scope in Node), the mail is acked so it
// does not stay stuck, and the sender gets a threaded "Delivery failed".
func (s *Service) mailFailed(ctx context.Context, agent *Agent, virtualAgentName, fromAgent, mailID string, cause error) {
	provider, model := *agent.Provider, *agent.Model
	var usageID *int
	if id, err := s.recordUsage(ctx, agent.Name, provider, model, providers.Usage{}, "mail", "error", cause.Error()); err == nil {
		usageID = &id
	}
	s.goLogCall(CallLog{
		ActorID: agent.ActorID, AgentName: virtualAgentName, Context: "mail", ContextID: mailID,
		Provider: provider, Model: model, Err: cause, UsageID: usageID,
	})
	s.Log.LogError("virtual-agent", "direct-mail-error", logger.ErrorOpts{
		Agent: virtualAgentName, Context: "mail", ContextID: mailID,
		Message: cause.Error(), Detail: fmt.Sprintf("%+v", cause), StatusCode: 500,
	})
	s.DB.Exec(ctx, ackMailSQL, mailID, agent.ActorID)
	if _, err := s.Mail.Send(ctx, fromAgent, virtualAgentName, "Delivery failed",
		virtualAgentName+" could not process your message: "+cause.Error()+". Your message has been acked — try resending later.",
		mailID); err != nil {
		s.logVA("error-feedback-failed", logger.Fields{{Key: "agent", Value: virtualAgentName}, {Key: "error", Value: err.Error()}})
	}
}
