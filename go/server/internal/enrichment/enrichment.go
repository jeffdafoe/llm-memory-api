// Package enrichment is Node's services/enrichment.js: it classifies a saved
// note into a cognitive type (semantic, episodic, procedural, reflective),
// stored in documents.metadata.cognitive_type, which picks the note's decay
// half-life in search ranking (memory.buildSearch).
package enrichment

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// AgentName is the virtual agent that classifies notes. Without it (or
// without its provider, model or API key) nothing is enriched.
const AgentName = "memory-enrichment"

// debounceMs: a note enriched less than a minute ago is not enriched
// again, so rapid saves cost one call.
const debounceMs = 60000

// zone is the process's time zone, which new Date() reads a date-time
// with no offset in; tests replace it.
var zone = time.Local

// contentLimit is how much of the note (UTF-16 units) the prompt carries.
const contentLimit = 4000

// Config is the config table (config.Store).
type Config interface {
	Get(key string) (string, error)
}

// Logger writes activity lines (logger.Logger).
type Logger interface {
	Log(subsystem, action string, details logger.Fields)
}

// Agents loads and calls virtual agents (virtualagent.Service).
type Agents interface {
	LoadAgent(ctx context.Context, name string) (*virtualagent.Agent, error)
	Invoke(ctx context.Context, agentName string, opts virtualagent.InvokeOptions) (*virtualagent.InvokeResult, error)
}

// Enricher implements notes.Enricher.
type Enricher struct {
	DB     db.DB
	Config Config
	Agents Agents
	Log    Logger
	// Now is the clock; tests replace it.
	Now func() time.Time
}

func (e *Enricher) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// skipKinds are the kinds not worth a model call.
var skipKinds = map[string]bool{"conversation": true, "context": true, "dream": true}

// kindFromSlug is enrichment.js's own copy of the slug-to-kind rule.
func kindFromSlug(slug string) string {
	switch {
	case strings.HasPrefix(slug, "instructions/"):
		return "instruction"
	case strings.HasPrefix(slug, "notes/codebase/"):
		return "reference"
	case strings.HasPrefix(slug, "conversations/"):
		return "conversation"
	case strings.HasPrefix(slug, "context/"):
		return "context"
	case strings.HasPrefix(slug, "dreams/"):
		return "dream"
	case strings.HasPrefix(slug, "tasks/"):
		return "task"
	case strings.HasPrefix(slug, "learnings/"):
		return "learning"
	}
	return "note"
}

func (e *Enricher) log(action string, details logger.Fields) {
	if e.Log != nil {
		e.Log.Log("enrichment", action, details)
	}
}

// Enrich is enrichNote. The steps that Node caught and logged return nil;
// what escaped Node's function (a failed agent lookup, a provider text that
// is not a string, an _enriched_at that cannot become a primitive) is
// returned, for the caller's NOTE_ENRICHMENT_FAILED.
func (e *Enricher) Enrich(ctx context.Context, namespace, slug, title, content string, metadata []byte) error {
	// A missing key (config.get threw) is off, as Node caught it.
	if v, err := e.Config.Get("note_enrichment_enabled"); err != nil || v != "true" {
		return nil
	}
	kind := kindFromSlug(slug)
	if skipKinds[kind] {
		return nil
	}

	recent, err := e.recentlyEnriched(metadata)
	if err != nil {
		return err
	}
	if recent {
		return nil
	}

	agent, err := e.Agents.LoadAgent(ctx, AgentName)
	if err != nil {
		return errors.New(message(err))
	}
	if agent == nil || str(agent.Provider) == "" || str(agent.Model) == "" || str(agent.APIKey) == "" {
		return nil
	}

	e.log("start", logger.Fields{{Key: "namespace", Value: namespace}, {Key: "slug", Value: slug}, {Key: "kind", Value: kind}})

	response, err := e.Agents.Invoke(ctx, AgentName, virtualagent.InvokeOptions{
		UserMessage:   buildUserMessage(namespace, slug, title, content),
		SkipRateLimit: true,
		Context:       "note-enrichment",
	})
	if err != nil {
		e.log("agent-error", logger.Fields{{Key: "namespace", Value: namespace}, {Key: "slug", Value: slug}, {Key: "error", Value: message(err)}})
		return nil
	}

	cognitiveType, err := parseResponse(response.Text)
	if err != nil {
		return err
	}
	if cognitiveType == "" {
		// (response.text || '').substring(0, 200): a falsy text is "", and
		// a truthy one is a string (parseResponse threw on anything else).
		preview := ""
		if s, ok := response.Text.(string); ok {
			preview = substringUTF16(s, 200)
		}
		e.log("parse-failed", logger.Fields{{Key: "namespace", Value: namespace}, {Key: "slug", Value: slug}, {Key: "responsePreview", Value: preview}})
		return nil
	}

	// The two keys are merged in SQL, so metadata changed since the save
	// (this runs after it, in the background) survives.
	_, err = e.DB.Exec(ctx,
		`UPDATE documents
             SET metadata = COALESCE(metadata, '{}'::jsonb)
                 || jsonb_build_object('cognitive_type', $1::text, '_enriched_at', $2::text)
             WHERE namespace = $3 AND LOWER(slug) = LOWER($4) AND deleted_at IS NULL`,
		cognitiveType, jsutil.DateISO(e.now()), namespace, slug)
	if err != nil {
		e.log("metadata-write-failed", logger.Fields{{Key: "namespace", Value: namespace}, {Key: "slug", Value: slug}, {Key: "error", Value: message(err)}})
		return nil
	}

	cost := "unknown"
	if jsutil.Truthy(response.Cost) {
		cost = jsutil.ToFixed(response.Cost, 6)
	}
	e.log("complete", logger.Fields{
		{Key: "namespace", Value: namespace}, {Key: "slug", Value: slug},
		{Key: "cognitiveType", Value: cognitiveType}, {Key: "cost", Value: cost},
	})
	return nil
}

// recentlyEnriched is the debounce: `meta._enriched_at` of
// `existingMetadata || {}`, when truthy, read with new Date(...).getTime()
// and compared with now. A value that is no date (NaN) never debounces.
func (e *Enricher) recentlyEnriched(metadata []byte) (bool, error) {
	if metadata == nil {
		return false, nil
	}
	meta, err := jsutil.Parse(metadata)
	if err != nil {
		return false, err
	}
	// Only an object has an _enriched_at; on a string, number, boolean or
	// array the property read is undefined.
	obj, ok := meta.(*jsutil.Object)
	if !ok {
		return false, nil
	}
	at, _ := obj.Get("_enriched_at")
	if !jsutil.Truthy(at) {
		return false, nil
	}
	ms, valid, err := jsutil.DateValue(at, zone)
	if err != nil {
		return false, err
	}
	return valid && float64(e.now().UnixMilli())-ms < debounceMs, nil
}

// buildUserMessage is the classification prompt.
func buildUserMessage(namespace, slug, title, content string) string {
	truncated := content
	if jsutil.UTF16Len(content) > contentLimit {
		truncated = substringUTF16(content, contentLimit) + "\n\n[... truncated]"
	}
	return "## Note to classify\n\n" +
		"**Namespace:** " + namespace + "\n" +
		"**Slug:** " + slug + "\n" +
		"**Title:** " + title + "\n\n" +
		truncated +
		"\n\nClassify this note and return a JSON object with a single field:\n" +
		"- `cognitive_type`: one of \"semantic\" (facts, definitions, knowledge), " +
		"\"episodic\" (events, things that happened), " +
		"\"procedural\" (decisions, conventions, how-tos, institutional knowledge), " +
		"or \"reflective\" (synthesized insights, lessons learned, analysis)\n\n" +
		"Return ONLY valid JSON, no markdown fences, no explanation."
}

// jsSpace is JavaScript's \s as a Go character class.
const jsSpace = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

var (
	openingFence = regexp.MustCompile("^```(?:json)?" + jsSpace + `*\n?`)
	closingFence = regexp.MustCompile("\\n?```" + jsSpace + `*$`)
)

var validTypes = map[string]bool{"semantic": true, "episodic": true, "procedural": true, "reflective": true}

// parseResponse is enrichment.js's parseResponse: the cognitive type, or
// "" when the answer holds none. A truthy text that is not a string fails
// as Node's text.trim() threw.
func parseResponse(text any) (string, error) {
	if !jsutil.Truthy(text) {
		return "", nil
	}
	s, ok := text.(string)
	if !ok {
		return "", errors.New("text.trim is not a function")
	}
	cleaned := jsutil.Trim(s)
	if strings.HasPrefix(cleaned, "```") {
		// String.prototype.replace without the g flag: the first match only.
		cleaned = replaceFirst(openingFence, cleaned)
		cleaned = replaceFirst(closingFence, cleaned)
	}
	parsed, err := jsutil.Parse([]byte(cleaned))
	if err != nil {
		return "", nil
	}
	// !parsed || typeof parsed !== 'object' lets objects and arrays through;
	// an array has no cognitive_type.
	obj, ok := parsed.(*jsutil.Object)
	if !ok {
		return "", nil
	}
	ct, _ := obj.Get("cognitive_type")
	ctString, ok := ct.(string)
	if !ok {
		return "", nil
	}
	normalized := jsutil.ToLower(jsutil.Trim(ctString))
	if !validTypes[normalized] {
		return "", nil
	}
	return normalized, nil
}

func replaceFirst(re *regexp.Regexp, s string) string {
	loc := re.FindStringIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[0]] + s[loc[1]:]
}

// substringUTF16 is s.substring(0, n). A cut through a surrogate pair
// leaves Node a lone high surrogate, which a Go string cannot hold; it is
// U+FFFD here, what it becomes in UTF-8 (Postgres, the log file) anyway.
func substringUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if units+w > n {
			if units < n {
				return s[:i] + "\uFFFD"
			}
			return s[:i]
		}
		units += w
	}
	return s
}

// message is err.message: a Postgres error's own message, as node-postgres
// puts it there, else Go's text.
func message(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}

// str is a nullable text column as JavaScript read it: "" for NULL.
func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
