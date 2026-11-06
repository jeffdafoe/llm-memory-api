package dream

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// agentNames are the virtual agents that write one identity's day: the
// dream itself, and optionally the soul, people and learnings writers.
type agentNames struct {
	dream, soul, people, learnings string
}

// scope selects the slug subtree and the self identity of a chunk: the
// namespace root and the agent's own name for a dedicated agent, or a
// "<villager>/" prefix and the villager's display name for a shared-VA
// villager.
type scope struct {
	prefix   scopePrefix
	selfName string
}

// ledgerPrecedenceDirective tells the people writer how to weigh the
// ledger against speech (LLM-523, LLM-607): the ledger is what happened
// and wins; read its direction exactly; a one-way transfer is not a debt;
// overheard lines are not this pair's dealings.
var ledgerPrecedenceDirective = strings.Join([]string{
	"## How to weigh these",
	"",
	"The ledger is the record of what actually happened — coin and goods that truly changed hands. It is authoritative and complete: if something is not in it, it did not happen. Speech is only what someone said, and people misremember, overstate, and promise more than they deliver. Where the two disagree, the ledger wins.",
	"",
	"- A payment, gift, or delivery counts ONLY if the ledger records it. Someone saying they paid you, or promising to make good on a debt, is not payment. Do not write an unbacked promise into the file as a settled matter.",
	`- Read the ledger direction exactly as written. "(paid Josiah Thorne 1 coin for milk)" means you spent a coin and received milk — a purchase you made, not a gift you were given.`,
	`- Not everything that moves one way is owed back. A due, a levy, a gift, a wage — these are settled when they change hands, and the ledger records no return because none was ever coming. Do not turn a one-way line into a debt, and do not write that someone "never delivered" or "gave nothing back" unless the ledger itself shows goods promised and not handed over.`,
	`- Lines marked "overheard" were spoken while you were present but addressed to someone else. They tell you about this person's character and dealings; they are not your own transactions with them.`,
}, "\n")

type personMessage struct {
	selfLabel, display, today, existingFile string
	ledger                                  []string
	said                                    string
}

// buildPersonUserMessage is the people writer's user message: the current
// relationship file, the ledger section, the speech section and the
// directive. No speech is consolidation-only mode, which
// /admin/dream/consolidate-people drives with that sentinel wording; no
// ledger says so, because "nothing changed hands" is what stops a spoken
// promise being remembered as paid.
func buildPersonUserMessage(m personMessage) string {
	saidBlock := "(no new excerpts since last update — please consolidate any redundant bullets if present, or return file unchanged if already tight)"
	if jsutil.Trim(m.said) != "" {
		saidBlock = m.said
	}
	ledgerBlock := "(nothing — no coin or goods changed hands between you and " + m.display + " today)"
	if len(m.ledger) > 0 {
		ledgerBlock = strings.Join(m.ledger, "\n")
	}
	existing := m.existingFile
	if existing == "" {
		existing = "(empty — first encounter)"
	}
	return "## Agent: " + m.selfLabel + "\n" +
		"## Person: " + m.display + "\n" +
		"## Today's date: " + m.today + "\n\n" +
		"## Current relationship file\n\n" +
		existing +
		"\n\n## What the ledger records\n\n" +
		ledgerBlock +
		"\n\n## What was said in your hearing\n\n" +
		saidBlock +
		"\n\n" + ledgerPrecedenceDirective
}

// PersonOptions are runPersonContextUpdate's options.
type PersonOptions struct {
	// SlugPrefix scopes the note under a shared-VA villager; "" is the
	// namespace root. PrefixNotString refuses a non-string prefix.
	SlugPrefix      string
	PrefixNotString bool
	// SelfLabel is who the relationship is for in the prompt; empty is
	// the agent.
	SelfLabel string
	Ledger    []string
	// DryRun skips the write.
	DryRun bool
}

// PersonResult is runPersonContextUpdate's answer. UpdatedFile is the
// trimmed reply ("" when there was none).
type PersonResult struct {
	ExistingFile  string
	UpdatedFile   string
	Written       bool
	Changed       bool
	EmptyResponse bool
	Truncated     bool
}

// RunPersonContextUpdate is runPersonContextUpdate: one people-writer pass
// over one (agent, person) relationship file. It reads
// context/people/<slug> (under the scope's prefix), asks the people agent
// for the updated file and saves it when it changed. A truncated reply
// keeps the prior file, since the writer rewrites the whole file and a
// cut-off one would drop what it did not reach.
func (j *Job) RunPersonContextUpdate(ctx context.Context, agentName, peopleAgent, slug, display, excerpts, today string, opts PersonOptions) (PersonResult, error) {
	selfLabel := opts.SelfLabel
	if selfLabel == "" {
		selfLabel = agentName
	}
	safeSlug := validatePersonSlug(slug)
	if safeSlug == "" {
		return PersonResult{}, errors.New("runPersonContextUpdate: invalid person slug: " + slug)
	}
	peopleSlug, err := peopleNotePath(scopePrefix{value: opts.SlugPrefix, notString: opts.PrefixNotString}, safeSlug)
	if err != nil {
		return PersonResult{}, err
	}
	// Note titles are editable in the admin UI; a display name with line
	// breaks could add "## " sections to the prompt.
	// (Node replaced the line breaks first; \s+ covers them.)
	safeDisplay := jsutil.Trim(collapseSpace(display))
	if safeDisplay == "" {
		safeDisplay = safeSlug
	}
	display = safeDisplay

	existingFile := ""
	if n, err := j.Notes.Read(ctx, agentName, peopleSlug); err == nil {
		existingFile = n.Content
	}

	res, err := j.Agents.Invoke(ctx, peopleAgent, virtualagent.InvokeOptions{
		UserMessage: buildPersonUserMessage(personMessage{
			selfLabel: selfLabel, display: display, today: today, existingFile: existingFile,
			ledger: opts.Ledger, said: excerpts,
		}),
		Context:       "people",
		SkipRateLimit: true,
		SkipCostLimit: true,
		Retry:         true,
	})
	if err != nil {
		return PersonResult{}, err
	}
	if res.Truncated {
		j.log("person-context-error", logger.Fields{
			{Key: "agent", Value: agentName}, {Key: "person", Value: display}, {Key: "slug", Value: safeSlug},
			{Key: "today", Value: today}, {Key: "reason", Value: "truncated"}, {Key: "finishReason", Value: res.FinishReason},
		})
		return PersonResult{ExistingFile: existingFile, Truncated: true}, nil
	}

	updated, err := trimmedText(res.Text, "rawUpdatedFile")
	if err != nil {
		return PersonResult{}, err
	}
	changed := updated != "" && updated != jsutil.Trim(existingFile)
	empty := updated == ""
	if empty {
		j.log("person-context-empty-response", logger.Fields{
			{Key: "agent", Value: agentName}, {Key: "person", Value: display}, {Key: "slug", Value: safeSlug},
			{Key: "today", Value: today},
		})
	}
	written := false
	if changed && !opts.DryRun {
		if _, err := j.Notes.Save(ctx, notes.SaveInput{
			Namespace: agentName, Title: "People — " + display, Content: updated,
			Slug: peopleSlug, CreatedBy: peopleAgent, Upsert: true,
		}); err != nil {
			return PersonResult{}, err
		}
		written = true
	}
	return PersonResult{ExistingFile: existingFile, UpdatedFile: updated, Written: written, Changed: changed, EmptyResponse: empty}, nil
}

// collapseSpace is .replace(/\s+/g, ' ').
func collapseSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if jsutil.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// trimmedText is `text && text.trim()` (or `text ? text.trim() : ”`) on
// a reply's text: "" for a falsy value, the trimmed string, and V8's
// TypeError for a truthy value that is not a string. name is the variable
// Node's message named.
func trimmedText(v any, name string) (string, error) {
	if !jsutil.Truthy(v) {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", errors.New(name + ".trim is not a function")
	}
	return jsutil.Trim(s), nil
}

// matchText is response.match on the dream reply: the string, or V8's
// TypeError for anything else.
func matchText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case nil:
		return "", errors.New("Cannot read properties of null (reading 'match')")
	}
	if v == jsutil.Undefined {
		return "", errors.New("Cannot read properties of undefined (reading 'match')")
	}
	return "", errors.New("response.match is not a function")
}

// jsLength is (v || ”).length: ok is false where it is undefined (a
// number, boolean or object), and the key drops out of the log line.
func jsLength(v any) (int, bool) {
	if !jsutil.Truthy(v) {
		return 0, true
	}
	switch x := v.(type) {
	case string:
		return jsutil.UTF16Len(x), true
	case []any:
		return len(x), true
	}
	return 0, false
}

// processChunk is processDreamChunk: one (from, to] window of one
// identity. It reads the window's conversation logs (or curated notes),
// prefilters them, asks the dream agent for the day's dream and saves it
// as dreams/<date>-<title slug>, then runs the soul, people and learnings
// writers that are configured. A failed dream fails the chunk; the other
// writers' failures are logged and do not.
func (j *Job) processChunk(ctx context.Context, agent *agentRow, names agentNames, c chunk, sc scope) (logger.Fields, error) {
	chunkDate := utcDate(c.from)
	notesMode := agent.source() == "notes"

	slugPrefix, err := resolveScopePrefix(sc.prefix)
	if err != nil {
		return nil, err
	}
	// A shared scope must name its villager: falling back to the pooled
	// agent would read the villager's own lines as someone else's.
	if slugPrefix != "" && jsutil.Trim(sc.selfName) == "" {
		return nil, errors.New("processDreamChunk: shared scope requires a non-empty selfName")
	}
	selfName := sc.selfName
	if slugPrefix != "" {
		selfName = jsutil.Trim(sc.selfName)
	} else if selfName == "" {
		selfName = agent.name
	}

	var fullLog, filtered string
	var logCount int
	if notesMode {
		// The excluded prefixes keep the dream from reading its own output.
		var kind any
		if agent.dreamNotesKind != "" {
			kind = agent.dreamNotesKind
		}
		rows, err := j.DB.Query(ctx, `SELECT slug, content, updated_at FROM documents
             WHERE namespace = $1 AND deleted_at IS NULL
             AND slug NOT LIKE 'conversations/%'
             AND slug NOT LIKE 'dreams/%'
             AND slug NOT LIKE 'context/%'
             AND slug NOT LIKE 'learnings/%'
             AND ($4::text IS NULL OR kind = $4)
             AND updated_at > $2 AND updated_at <= $3
             ORDER BY updated_at ASC`, agent.name, c.from, c.to, kind)
		if err != nil {
			return nil, err
		}
		var notesRows []noteRow
		for rows.Next() {
			var r noteRow
			if err := rows.Scan(&r.slug, &r.content, &r.updatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			notesRows = append(notesRows, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		logCount = len(notesRows)
		if logCount > 0 {
			// Curated notes are already distilled and rarely carry the
			// conversational signals, so they go in whole.
			fullLog = buildNotesLog(notesRows)
			filtered = fullLog
		}
	} else {
		rows, err := j.DB.Query(ctx, `SELECT slug, content, created_at FROM documents
             WHERE namespace = $1 AND slug LIKE $4 AND deleted_at IS NULL
             AND created_at > $2 AND created_at <= $3
             ORDER BY created_at ASC`, agent.name, c.from, c.to, slugPrefix+"conversations/%")
		if err != nil {
			return nil, err
		}
		var contents []string
		for rows.Next() {
			var slug, content string
			var created time.Time
			if err := rows.Scan(&slug, &content, &created); err != nil {
				rows.Close()
				return nil, err
			}
			contents = append(contents, content)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		logCount = len(contents)
		fullLog = strings.Join(contents, "\n\n---\n\n")
	}
	if logCount == 0 {
		j.log("chunk-no-logs", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "source", Value: agent.dreamSource}, {Key: "chunkDate", Value: chunkDate}})
		return logger.Fields{{Key: "skipped", Value: true}, {Key: "reason", Value: "no logs"}, {Key: "chunkDate", Value: chunkDate}}, nil
	}
	if !notesMode {
		var ok bool
		if filtered, ok = prefilterLog(fullLog); !ok {
			j.log("chunk-no-signals", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate}, {Key: "logCount", Value: logCount}})
			return logger.Fields{{Key: "skipped", Value: true}, {Key: "reason", Value: "no signals"}, {Key: "chunkDate", Value: chunkDate}, {Key: "logCount", Value: logCount}}, nil
		}
	}
	// prefilterLog cannot give "" for a log with a signal or ledger line,
	// and a notes log always has its headers, so filtered is not empty
	// here, as Node's !filtered check after it assumed.

	j.log("chunk-processing", logger.Fields{
		{Key: "agent", Value: agent.name},
		{Key: "mode", Value: agent.dreamMode},
		{Key: "source", Value: agent.dreamSource},
		{Key: "chunkDate", Value: chunkDate},
		{Key: "logCount", Value: logCount},
		{Key: "originalSize", Value: jsutil.UTF16Len(fullLog)},
		{Key: "filteredSize", Value: jsutil.UTF16Len(filtered)},
	})

	var userMessage string
	if notesMode {
		userMessage = `Curated notes written or updated by agent "` + agent.name + `" on ` + chunkDate +
			" (this agent's memory lives in hand-curated notes — journals, identity documents, session summaries — rather than conversation logs):\n\n"
	} else {
		userMessage = `Conversation logs for agent "` + selfName + `" on ` + chunkDate + ":\n\n"
	}
	userMessage += filtered + "\n\nAlso provide a brief title summarizing the overarching subject of the day."

	res, err := j.Agents.Invoke(ctx, names.dream, virtualagent.InvokeOptions{
		UserMessage: userMessage, Context: "dream", SkipRateLimit: true, SkipCostLimit: true, Retry: true,
	})
	if err != nil {
		return nil, err
	}
	content, err := matchText(res.Text)
	if err != nil {
		return nil, err
	}
	title := dreamTitle(content)

	// Dated by the chunk, so catching up several days gives one note each.
	slug := slugPrefix + "dreams/" + chunkDate + "-" + slugify(title)
	if _, err := j.Notes.Save(ctx, notes.SaveInput{
		Namespace: agent.name, Title: title + " (" + chunkDate + ")", Content: content, Slug: slug, CreatedBy: names.dream,
	}); err != nil {
		return nil, err
	}
	j.log("chunk-saved", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "slug", Value: slug}, {Key: "contentLength", Value: jsutil.UTF16Len(content)}})

	// The soul builds on the prior chunk's soul. A shared villager's soul
	// lives in the engine (actor_narrative_state.about_me), and one
	// namespace-root context/soul would be shared by every pooled villager,
	// so a prefixed scope never writes one.
	if names.soul != "" && slugPrefix == "" {
		if err := j.soulPass(ctx, agent, names.soul, chunkDate, content); err != nil {
			j.log("chunk-soul-error", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate}, {Key: "error", Value: message(err)}})
		}
	}

	// Notes mode writes only dreams and the soul: the speaker parser would
	// read curated prose as phantom speakers.
	if names.people != "" && !notesMode {
		j.peoplePass(ctx, agent, names.people, filtered, selfName, slugPrefix, chunkDate)
	}

	// The day's learnings note, for sim agents, whose turns are all tool
	// use and so never reach the per-turn extractor.
	if names.learnings != "" && !notesMode {
		if err := j.learningsPass(ctx, agent, names.learnings, filtered, selfName, slugPrefix, chunkDate); err != nil {
			j.log("chunk-learnings-error", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate}, {Key: "error", Value: message(err)}})
		}
	}

	return logger.Fields{
		{Key: "processed", Value: true},
		{Key: "chunkDate", Value: chunkDate},
		{Key: "slug", Value: slug},
		{Key: "title", Value: title},
		{Key: "logCount", Value: logCount},
		{Key: "filteredSize", Value: jsutil.UTF16Len(filtered)},
		{Key: "responseSize", Value: jsutil.UTF16Len(content)},
	}, nil
}

// soulPass writes the agent's context/soul from the chunk's dream. With no
// usable prior soul (empty, or under dream_soul_min_chars, the mark of a
// truncated write) it rebuilds from the latest dreams instead of evolving
// the stub, which the writer would otherwise compound. A truncated reply,
// an empty one, or one that opens with the model's reasoning is not saved.
func (j *Job) soulPass(ctx context.Context, agent *agentRow, soulAgent, chunkDate, dreamContent string) error {
	existingSoul := ""
	if n, err := j.Notes.Read(ctx, agent.name, "context/soul"); err == nil {
		existingSoul = n.Content
	}
	// A missing key (the server started before its migration) is off, as
	// for dream_backload_count.
	soulMinChars := 0.0
	if raw, err := j.Config.Get("dream_soul_min_chars"); err == nil {
		soulMinChars = parseInt10(raw)
	}
	trimmedLen := jsutil.UTF16Len(jsutil.Trim(existingSoul))
	needsRebuild := soulNeedsRebuild(existingSoul, soulMinChars)
	if needsRebuild && trimmedLen > 0 {
		j.log("chunk-soul-degraded-rebuild", logger.Fields{
			{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate},
			{Key: "size", Value: trimmedLen}, {Key: "minChars", Value: jsNum(soulMinChars)},
		})
	}
	backload := ""
	if needsRebuild {
		count := 0.0
		if raw, err := j.Config.Get("dream_backload_count"); err == nil {
			count = parseInt10(raw)
		}
		// Capped at 20 whatever the config says: a bound on the reads.
		count = max(0, min(count, 20))
		if count > 0 {
			list, err := j.Notes.ListLive(ctx, notes.ListInput{
				Namespace: agent.name, Limit: []byte(jsutil.NumberString(count)), Offset: []byte("0"), Prefix: "dreams/",
			})
			if err != nil {
				return err
			}
			var parts []string
			for _, n := range list {
				d, err := j.Notes.Read(ctx, agent.name, n.Slug)
				if err != nil || d.Content == "" {
					continue
				}
				parts = append(parts, "### "+d.Slug+"\n\n"+d.Content)
			}
			backload = strings.Join(parts, "\n\n---\n\n")
		}
	}

	res, err := j.Agents.Invoke(ctx, soulAgent, virtualagent.InvokeOptions{
		UserMessage: buildSoulUserMessage(soulMessage{
			agentName: agent.name, startupInstructions: str(agent.startupInstructions),
			existingSoul: existingSoul, needsRebuild: needsRebuild, backloadDreams: backload,
			chunkDate: chunkDate, dreamContent: dreamContent,
		}),
		Context: "soul", SkipRateLimit: true, SkipCostLimit: true, Retry: true,
	})
	if err != nil {
		return err
	}
	if res.Truncated {
		fields := logger.Fields{
			{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate},
			{Key: "reason", Value: "truncated"}, {Key: "finishReason", Value: res.FinishReason},
		}
		if n, ok := jsLength(res.Text); ok {
			fields = append(fields, logger.Field{Key: "size", Value: n})
		}
		j.log("chunk-soul-error", fields)
		return nil
	}
	trimmed, err := trimmedText(res.Text, "updatedSoul")
	if err != nil {
		return err
	}
	if trimmed == "" {
		var outputTokens any = 0
		if t := res.Usage.OutputTokens; t != nil && t != jsutil.Undefined {
			outputTokens = t
		}
		j.log("chunk-soul-empty-response", logger.Fields{
			{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate}, {Key: "outputTokens", Value: outputTokens},
		})
		return nil
	}
	if marker := detectReasoningPreamble(trimmed); marker != "" {
		// The soul text stays out of the log: it is the model's reasoning
		// or the character's state.
		j.log("chunk-soul-error", logger.Fields{
			{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate},
			{Key: "reason", Value: "reasoning-preamble-detected"}, {Key: "marker", Value: marker},
			{Key: "size", Value: jsutil.UTF16Len(trimmed)},
		})
		return nil
	}
	if _, err := j.Notes.Save(ctx, notes.SaveInput{
		Namespace: agent.name, Title: "Soul", Content: trimmed, Slug: "context/soul", CreatedBy: soulAgent, Upsert: true,
	}); err != nil {
		return err
	}
	j.log("chunk-soul-updated", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate}, {Key: "size", Value: jsutil.UTF16Len(trimmed)}})
	return nil
}

// peoplePass updates the relationship file of each person in the day who
// has a ledger line or speech. One person's failure is logged and the
// others go on.
func (j *Job) peoplePass(ctx context.Context, agent *agentRow, peopleAgent, filtered, selfName, slugPrefix, chunkDate string) {
	sections := buildPersonExcerptSections(filtered, selfName, extractSpeakers(filtered, selfName))
	for _, s := range sections {
		if len(s.ledger) == 0 && len(s.said) == 0 {
			continue
		}
		res, err := j.RunPersonContextUpdate(ctx, agent.name, peopleAgent, s.slug, s.display,
			strings.Join(s.said, "\n"), chunkDate,
			PersonOptions{SlugPrefix: slugPrefix, SelfLabel: selfName, Ledger: s.ledger})
		if err != nil {
			j.log("chunk-people-error", logger.Fields{
				{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate},
				{Key: "person", Value: s.display}, {Key: "error", Value: message(err)},
			})
			continue
		}
		if res.Written {
			j.log("chunk-people-updated", logger.Fields{
				{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate},
				{Key: "person", Value: s.display}, {Key: "size", Value: jsutil.UTF16Len(res.UpdatedFile)},
			})
		}
	}
}

// learningsPass writes learnings/<date>-sim-day, refining the day's note
// when one exists. A truncated reply keeps the prior note; NONE or an
// empty reply writes nothing.
func (j *Job) learningsPass(ctx context.Context, agent *agentRow, learningsAgent, filtered, selfName, slugPrefix, chunkDate string) error {
	slug := slugPrefix + "learnings/" + chunkDate + "-sim-day"
	existing := ""
	if n, err := j.Notes.Read(ctx, agent.name, slug); err == nil {
		existing = n.Content
	}
	msg := "## Agent: " + selfName + "\n" + "## Date: " + chunkDate + "\n\n"
	if existing != "" {
		msg += "## Existing learnings for today (refine, integrate; do not duplicate)\n\n" + existing + "\n\n"
	}
	msg += "## Day's conversation excerpts\n\n" + filtered

	res, err := j.Agents.Invoke(ctx, learningsAgent, virtualagent.InvokeOptions{
		UserMessage: msg, Context: "learnings", SkipRateLimit: true, SkipCostLimit: true, Retry: true,
	})
	if err != nil {
		return err
	}
	trimmed, err := trimmedText(res.Text, "extractionResult")
	if err != nil {
		return err
	}
	switch {
	case res.Truncated:
		j.log("chunk-learnings-error", logger.Fields{
			{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate},
			{Key: "reason", Value: "truncated"}, {Key: "finishReason", Value: res.FinishReason},
		})
	case trimmed != "" && strings.ToUpper(trimmed) != "NONE":
		if _, err := j.Notes.Save(ctx, notes.SaveInput{
			Namespace: agent.name, Title: "Learnings — " + chunkDate, Content: trimmed,
			Slug: slug, CreatedBy: learningsAgent, Upsert: true,
		}); err != nil {
			return err
		}
		j.log("chunk-learnings-updated", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate}, {Key: "size", Value: jsutil.UTF16Len(trimmed)}})
	default:
		j.log("chunk-learnings-none", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate}})
	}
	return nil
}
