package virtualagent

import (
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// The typed prompt blocks. Each piece of context in a system prompt is an
// XML block named for what it is, with a line telling the model how to use
// it, so the model does not read one kind of context as another (a private
// impression aloud, a recalled note as new input). The wording is a prompt
// design decision, so it lives here, not in config.
const (
	directiveBootstrap          = "System-wide rules that apply to every agent in this system. Treat as established ground rules, not negotiable."
	directiveInstructions       = "Your standing operating instructions. Follow them as part of how you behave. Do not read them aloud or restate them to the person you are talking to."
	directiveSelf               = "Your self-narrative and accumulated identity. Speak FROM this perspective. Never quote it, describe yourself in third person from it, or narrate it as exposition."
	directiveImpressions        = "Your private notes about the other participants in this conversation. Let them shape your tone, what you engage with, and what you remember. NEVER read them aloud, quote them, or attribute them back to the person."
	directiveRecall             = "Excerpts from your past notes and conversations that are semantically related to the current topic. These are NOT what anyone said in this conversation. Reference them only when directly useful; otherwise ignore."
	directiveDirectContext      = "The direct-chat context. You are chatting one-on-one with another agent. Stable for this conversation."
	directiveDirectConversation = "The live one-on-one conversation, oldest first. Each line has the shape \"<relative-time> <speaker>: <message>\". Lines of the form \"--- gap: Nh ---\" mark time gaps between messages. The last line is what you are replying to."
	directiveReplyPolicy        = "You do not have to respond. If a reply is not warranted — you have nothing substantive to add, someone else's response covers it, or the thread has naturally concluded — stay silent by returning an empty response. Silence is a valid choice."
	directiveOutputChat         = "Respond concisely and naturally, the way a person would in conversation. Your reply only — no JSON, no speaker labels, no framing, no timestamp."
	directiveStanding           = "Standing facts about your character — who you are, how you live and trade, where home and work are. They change rarely; treat them as background identity. What is happening right now arrives in the user messages."
	directiveSimContext         = "You are a character inside a village simulation. The user messages are perception updates emitted by the simulation engine — what your character sees, hears, or experiences in the moment. They are not chat from a person; do not address the engine, the narrator, or \"the user\". Choose actions by calling the provided tools. Do not reply with ordinary prose. If you want your character to say something, use the speak tool. Speak only to characters confirmed present in your perception or by tool results. If you want to go somewhere, use move_to. If no action is appropriate this round, call done. Treat perception and tool results as authoritative — do not invent unseen characters, unavailable locations, or events not supported by the simulation state. Use your identity, memories, and impressions to decide what your character wants, but express every action through tools."
	directiveSimReflection      = "You are a character inside a village simulation. This turn is a private reflection, not a scene or an action tick — the user message says what to reflect on. No tools are available this turn: respond with prose only, written in your own voice as the character. Do not call tools, and do not address the engine, the narrator, or \"the user\". Treat the material you are given as authoritative — do not invent people, places, or events it does not support."
	// The magistrate is not a character in a scene, so the NPC SimContext
	// (speak, move_to, done) would be wrong for him (LLM-695).
	directiveCourt  = "You sit as the magistrate of the court in Salem Town. The village simulation brings you one matter at a time; the matter, the village roster and your bench book are set out in the Matter block. The user messages come from the court's clerk, not from a person — do not address them. Work only through the tools provided: read what you need from the record, then give your ruling with the rule tool, which ends the hearing. Do not reply with ordinary prose."
	directiveMatter = "The matter before the court this session, who lives in the village, and your bench book. Authoritative for this session."
)

// wrapBlock is a typed block around content, or "" when the trimmed content
// is empty: an empty block only adds noise.
func wrapBlock(tag, purpose, directive, content string) string {
	text := jsutil.Trim(content)
	if text == "" {
		return ""
	}
	return "<" + tag + " purpose=\"" + purpose + "\">\n" + directive + "\n\n" + text + "\n</" + tag + ">"
}

// wrapStandalone is a block that is only its directive.
func wrapStandalone(tag, purpose, directive string) string {
	return "<" + tag + " purpose=\"" + purpose + "\">\n" + directive + "\n</" + tag + ">"
}

// joinBlocks joins the non-empty blocks with a blank line.
func joinBlocks(blocks ...string) string {
	var kept []string
	for _, b := range blocks {
		if b != "" {
			kept = append(kept, b)
		}
	}
	return strings.Join(kept, "\n\n")
}

// simCourtAgents are the agents that sit as the salem court (LLM-695): the
// engine drives them like an NPC, but they get the court framing, no soul
// or impressions, and the whole hearing as history.
var simCourtAgents = map[string]bool{"salem-magistrate": true}

const (
	maxDirectChatHistory = 50
	// A court hearing is replayed whole (LLM-819): the engine stops a
	// hearing at 24 rounds, each one assistant row plus one row per tool
	// result, and a window sliding inside it drops records the magistrate
	// already read. 500 covers 24 rounds of up to 19 parallel calls.
	maxCourtChatHistory = 500
)

// historyCapFor is the row cap of one agent's direct-chat history.
func historyCapFor(agentName string) int {
	if simCourtAgents[agentName] {
		return maxCourtChatHistory
	}
	return maxDirectChatHistory
}

// globalBootstrap is config global_bootstrap || "".
func (s *Service) globalBootstrap() (string, error) {
	return s.Config.Get("global_bootstrap")
}

func renderDirectChatContext(a *Agent) string {
	text := "You are \"" + a.Name + "\". You are chatting one-on-one with another agent."
	if str(a.Personality) != "" {
		text += "\nYour personality: " + *a.Personality
	}
	return text
}

// buildDirectChatSystemPrompt is the companion-chat prompt: the static part
// is cached between calls, the dynamic part (recall, the reply and output
// rules) changes per message.
func (s *Service) buildDirectChatSystemPrompt(a *Agent, ragContext, soul, peopleContext string) (providers.SystemPrompt, error) {
	bootstrap, err := s.globalBootstrap()
	if err != nil {
		return providers.SystemPrompt{}, err
	}
	static := joinBlocks(
		wrapBlock("Bootstrap", "global-operating-directives", directiveBootstrap, bootstrap),
		wrapBlock("Instructions", "operating-rules", directiveInstructions, str(a.StartupInstructions)),
		wrapBlock("Self", "voice-identity", directiveSelf, soul),
		wrapBlock("Impressions", "private-relationship-notes", directiveImpressions, peopleContext),
		wrapBlock("DirectChat", "active-context", directiveDirectContext, renderDirectChatContext(a)),
	)
	dynamic := joinBlocks(
		wrapBlock("Recall", "relevant-memories", directiveRecall, ragContext),
		wrapStandalone("ReplyPolicy", "response-discretion", directiveReplyPolicy),
		wrapStandalone("OutputDirective", "response-format", directiveOutputChat),
	)
	return providers.StructuredPrompt(static, dynamic), nil
}

// buildSimChatSystemPrompt is a sim NPC's action-tick prompt. The companion
// blocks fight tool use (the reply policy makes "done" the easy way out,
// the output rule asks for prose), so a SimContext block takes their place.
func (s *Service) buildSimChatSystemPrompt(a *Agent, ragContext, soul, peopleContext string) (providers.SystemPrompt, error) {
	bootstrap, err := s.globalBootstrap()
	if err != nil {
		return providers.SystemPrompt{}, err
	}
	static := joinBlocks(
		wrapBlock("Bootstrap", "global-operating-directives", directiveBootstrap, bootstrap),
		wrapBlock("Instructions", "operating-rules", directiveInstructions, str(a.StartupInstructions)),
		wrapBlock("Self", "voice-identity", directiveSelf, soul),
		wrapBlock("Impressions", "private-relationship-notes", directiveImpressions, peopleContext),
		wrapStandalone("SimContext", "simulation-context", directiveSimContext),
	)
	dynamic := joinBlocks(wrapBlock("Recall", "relevant-memories", directiveRecall, ragContext))
	return providers.StructuredPrompt(static, dynamic), nil
}

// buildSimCourtSystemPrompt is a court session's prompt. The engine sends
// the matter, the roster and the bench book as stable_context; it goes in
// the cached prompt, never in the history, so a long session cannot push
// the case out of the replay.
func buildSimCourtSystemPrompt(a *Agent, matter string) providers.SystemPrompt {
	static := joinBlocks(
		wrapBlock("Instructions", "operating-rules", directiveInstructions, str(a.StartupInstructions)),
		wrapStandalone("CourtContext", "court-context", directiveCourt),
		wrapBlock("Matter", "the-matter-before-the-court", directiveMatter, matter),
	)
	return providers.StructuredPrompt(static, "")
}

// jsSpace is JavaScript's \s.
const jsSpace = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

// asciiFold spells out an ASCII pattern case-insensitively, letter by
// letter. JavaScript's i flag without u never matches a non-ASCII
// character to an ASCII letter (no long s for "s", no Kelvin sign for "k"),
// which Go's (?i) would.
func asciiFold(literal string) string {
	var b strings.Builder
	for _, c := range literal {
		lower, upper := strings.ToLower(string(c)), strings.ToUpper(string(c))
		if lower != upper {
			b.WriteString("[" + upper + lower + "]")
		} else {
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

var (
	toolsOfferedTail = regexp.MustCompile(asciiFold("Use only the tools offered.") + jsSpace + `*`)
	sayDoneTail      = regexp.MustCompile(asciiFold("When you have nothing more to add,") + jsSpace + `*` + asciiFold("say done.") + jsSpace + `*`)
)

// stripSimToolInstructions removes the action-mode tool tail the shared
// salem VAs carry in their instructions ("Use only the tools offered. When
// you have nothing more to add, say done."), which would contradict a prose
// reflection.
func stripSimToolInstructions(text string) string {
	if text == "" {
		return text
	}
	text = toolsOfferedTail.ReplaceAllLiteralString(text, "")
	text = sayDoneTail.ReplaceAllLiteralString(text, "")
	return jsutil.Trim(text)
}

// buildSimReflectionSystemPrompt is an engine call with no tools: a
// prose-only reflection (consolidation, narrative, atmosphere,
// noticeboard). The action SimContext ("use tools, do not reply with
// prose") would contradict it, so a reflection framing replaces it.
func (s *Service) buildSimReflectionSystemPrompt(a *Agent, ragContext, soul, peopleContext string) (providers.SystemPrompt, error) {
	bootstrap, err := s.globalBootstrap()
	if err != nil {
		return providers.SystemPrompt{}, err
	}
	static := joinBlocks(
		wrapBlock("Bootstrap", "global-operating-directives", directiveBootstrap, bootstrap),
		wrapBlock("Instructions", "operating-rules", directiveInstructions, stripSimToolInstructions(str(a.StartupInstructions))),
		wrapBlock("Self", "voice-identity", directiveSelf, soul),
		wrapBlock("Impressions", "private-relationship-notes", directiveImpressions, peopleContext),
		wrapStandalone("SimContext", "simulation-reflection", directiveSimReflection),
	)
	dynamic := joinBlocks(wrapBlock("Recall", "relevant-memories", directiveRecall, ragContext))
	return providers.StructuredPrompt(static, dynamic), nil
}

// relativeUnit is "1 hour ago" or "3 hours ago".
func relativeUnit(count float64, unit string) string {
	if count == 1 {
		return "1 " + unit + " ago"
	}
	return jsutil.NumberString(count) + " " + unit + "s ago"
}

// formatRelativeTime is the age put before a companion-chat history line,
// in words, which weak models read as "this is old" more reliably than
// "[3h]".
func formatRelativeTime(nowMs float64, sentAt time.Time) string {
	diffSec := math.Floor((nowMs - sentMs(sentAt)) / 1000)
	if diffSec < 60 {
		return "[just now]"
	}
	diffMin := math.Floor(diffSec / 60)
	if diffMin < 60 {
		return "[" + relativeUnit(diffMin, "minute") + "]"
	}
	diffHr := math.Floor(diffMin / 60)
	if diffHr < 24 {
		return "[" + relativeUnit(diffHr, "hour") + "]"
	}
	return "[" + relativeUnit(math.Floor(diffHr/24), "day") + "]"
}

// buildDirectChatUserMessage is the companion-chat user message: the
// history as "<age> <speaker>: <message>" lines with a gap line after a
// pause of 2 hours or more, in a Conversation block. latest is
// `${messageText}` (undefined for none).
func (s *Service) buildDirectChatUserMessage(history []chatRow, fromAgent, latest string) string {
	if len(history) <= 1 {
		return wrapBlock("Conversation", "active-dialogue", directiveDirectConversation, fromAgent+": "+latest)
	}
	const gapThresholdMs = 2 * 60 * 60 * 1000
	now := s.nowMs()
	lines := make([]string, 0, len(history))
	for i, m := range history {
		if i > 0 && m.SentAt != nil && history[i-1].SentAt != nil {
			gap := sentMs(*m.SentAt) - sentMs(*history[i-1].SentAt)
			if gap >= gapThresholdMs {
				lines = append(lines, "--- gap: "+jsutil.NumberString(jsutil.Round(gap/(60*60*1000)))+"h ---")
			}
		}
		timestamp := ""
		if m.SentAt != nil {
			timestamp = formatRelativeTime(now, *m.SentAt)
		}
		lines = append(lines, timestamp+" "+m.FromAgent+": "+m.messageText())
	}
	return wrapBlock("Conversation", "active-dialogue", directiveDirectConversation, strings.Join(lines, "\n"))
}
