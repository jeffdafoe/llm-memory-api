// Package dream is the nightly dream job (Node's services/dream.js). For
// every agent with a dream mode, it reads the day's conversation logs (or,
// for dream_source notes, its curated notes) since the agent's
// last_dream_at, one UTC day at a time, and has the dream virtual agents
// write a dream note, the agent's soul, its relationship files and the
// day's learnings. Shared-VA agents (dream_mode sim-shared) dream each
// villager of their sim_shared_actor roster under the villager's slug
// prefix. A run holds a Postgres advisory lock, so two runs never race on
// the cursors.
package dream

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/cron"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// Config is the config table (config.Store).
type Config interface {
	Get(key string) (string, error)
}

// Logger is the part of *logger.Logger the job uses.
type Logger interface {
	Log(subsystem, action string, details logger.Fields)
	LogError(subsystem, action string, opts logger.ErrorOpts)
	Errorf(format string, args ...any)
}

// Notes saves, reads and lists notes (notes.Service).
type Notes interface {
	Save(ctx context.Context, in notes.SaveInput) (notes.SavedNote, error)
	Read(ctx context.Context, namespace, slug string) (notes.Note, error)
	ListLive(ctx context.Context, in notes.ListInput) ([]notes.ListedNote, error)
}

// Agents calls virtual agents (virtualagent.Service).
type Agents interface {
	Invoke(ctx context.Context, agentName string, opts virtualagent.InvokeOptions) (*virtualagent.InvokeResult, error)
}

// Trust says whether an actor may own a dream agent
// (permissions.Admin.IsTrustedCreator).
type Trust interface {
	IsTrustedCreator(ctx context.Context, actorID int) (bool, error)
}

// Job runs dreams.
type Job struct {
	DB     db.DB
	Conns  Connector
	Config Config
	Log    Logger
	Notes  Notes
	Agents Agents
	Trust  Trust

	// Now is the clock and Sleep the pause between agents, villagers and
	// chunks (milliseconds); tests replace them.
	Now   func() time.Time
	Sleep func(ctx context.Context, ms float64) error

	// task is the running schedule. Start replaces it, as Node's
	// startDreamScheduler stopped its scheduledTask (hot reload).
	taskMu sync.Mutex
	task   *cron.Task
}

func (j *Job) log(action string, details logger.Fields) {
	j.Log.Log("dream", action, details)
}

func (j *Job) logError(action string, opts logger.ErrorOpts) {
	j.Log.LogError("dream", action, opts)
}

// now is new Date(): milliseconds.
func (j *Job) now() time.Time {
	t := time.Now()
	if j.Now != nil {
		t = j.Now()
	}
	return time.UnixMilli(t.UnixMilli()).UTC()
}

func (j *Job) sleep(ctx context.Context, ms float64) error {
	if j.Sleep != nil {
		return j.Sleep(ctx, ms)
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// detail is error_log.detail: Node stored err.stack; Go stores the error's
// %+v, as every ported job and route does. The column is for people to read;
// no code parses it.
func detail(err error) string { return fmt.Sprintf("%+v", err) }

// stopTask stops a replaced schedule; tests record it.
var stopTask = (*cron.Task).Stop

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// jsNum is a float64 in a log line as JSON.stringify writes it.
type jsNum float64

func (n jsNum) MarshalJSON() ([]byte, error) {
	x := float64(n)
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return []byte("null"), nil
	}
	return []byte(jsutil.NumberString(x)), nil
}

// abort reports whether err ends the whole run: a lost lock, or shutdown.
func abort(ctx context.Context, err error) bool {
	return errors.Is(err, errRunLockLost) || ctx.Err() != nil
}

// boundary is the check before each unit of work and cursor write: the
// run stops when its lock is gone (another run may be underway) or the
// server is shutting down.
func boundary(ctx context.Context, lock *runLock) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return lock.check(ctx)
}

func get(f logger.Fields, key string) (any, bool) {
	for _, field := range f {
		if field.Key == key {
			return field.Value, true
		}
	}
	return nil, false
}

// truthy is a result field read as JavaScript truthiness.
func truthy(f logger.Fields, key string) bool {
	v, ok := get(f, key)
	if !ok {
		return false
	}
	switch x := v.(type) {
	case string:
		return x != ""
	case int:
		return x != 0
	case bool:
		return x
	case nil:
		return false
	}
	return true
}

// countFailedActors counts the villagers that failed in a run: an error of
// their own or a failed chunk; a skipped chunk is not a failure.
func countFailedActors(actorResults []logger.Fields) int {
	n := 0
	for _, r := range actorResults {
		if truthy(r, "error") {
			n++
			continue
		}
		if chunks, ok := get(r, "chunks"); ok {
			if list, ok := chunks.([]logger.Fields); ok {
				for _, c := range list {
					if truthy(c, "error") {
						n++
						break
					}
				}
			}
		}
	}
	return n
}

// agentRow is one row of the dream-mode agent query.
type agentRow struct {
	name                string
	actorID             int
	dreamMode           string
	dreamSource         string
	lastDreamAt         *time.Time
	startupInstructions *string
	// dreamNotesKind is "note" for a notes-fallback run, which reads only
	// the agent's own notes.
	dreamNotesKind string
}

func (a *agentRow) source() string { return a.dreamSource }

// findDreamAgent is the dream agent with the expertise tag: the first one
// created by the system actor or a trusted creator, if it has a provider,
// model and API key. "" when there is none (logged).
func (j *Job) findDreamAgent(ctx context.Context, tag string) (string, error) {
	rows, err := j.DB.Query(ctx, `SELECT ac.id, ac.name, ac.created_by, agc.provider, agc.model, agc.api_key
         FROM actors ac
         JOIN agent_configuration agc ON agc.actor_id = ac.id
         WHERE ac.expertise @> jsonb_build_array($1::text)`, tag)
	if err != nil {
		return "", err
	}
	type candidate struct {
		id                      int
		name                    string
		createdBy               *int
		provider, model, apiKey *string
	}
	var found []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.name, &c.createdBy, &c.provider, &c.model, &c.apiKey); err != nil {
			rows.Close()
			return "", err
		}
		found = append(found, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(found) == 0 {
		j.log("error", logger.Fields{{Key: "message", Value: "No agent found with expertise: " + tag}})
		return "", nil
	}
	var agent *candidate
	for i := range found {
		// No creator is never trusted; Node asked the permission check,
		// which finds nothing for a null id.
		if found[i].createdBy == nil {
			continue
		}
		ok, err := j.Trust.IsTrustedCreator(ctx, *found[i].createdBy)
		if err != nil {
			return "", err
		}
		if ok {
			agent = &found[i]
			break
		}
	}
	if agent == nil {
		j.log("error", logger.Fields{{Key: "message", Value: `Agent with expertise "` + tag + `" not owned by system or trusted creator`}})
		return "", nil
	}
	if str(agent.apiKey) == "" || str(agent.provider) == "" || str(agent.model) == "" {
		j.log("error", logger.Fields{{Key: "message", Value: agent.name + " (expertise: " + tag + ") missing provider/model/api_key"}})
		return "", nil
	}
	return agent.name, nil
}

// Run is runDream: one dream run, serialized against any other run by the
// advisory lock. The result is Node's summary object.
func (j *Job) Run(ctx context.Context) (logger.Fields, error) {
	// Before the lock: a disabled run need not check out a connection.
	enabled, err := j.Config.Get("dream_processing_enabled")
	if err != nil {
		return nil, err
	}
	if enabled != "true" {
		j.log("skip", logger.Fields{{Key: "reason", Value: "dream_processing_enabled is false"}})
		return logger.Fields{{Key: "skipped", Value: true}, {Key: "reason", Value: "disabled"}}, nil
	}
	lock, err := j.acquireRunLock(ctx)
	if err != nil {
		return nil, err
	}
	if lock == nil {
		j.log("skip", logger.Fields{{Key: "reason", Value: "another dream run holds the lock"}})
		return logger.Fields{{Key: "skipped", Value: true}, {Key: "reason", Value: "already running"}}, nil
	}
	defer lock.release(ctx)
	result, err := j.runAgents(ctx, lock)
	if err != nil {
		return nil, err
	}
	// A loss (or a shutdown) after the last check would otherwise read as
	// a clean run.
	if err := boundary(ctx, lock); err != nil {
		return nil, err
	}
	return result, nil
}

var dreamTags = []string{
	"dream-companion", "dream-technical", "dream-sim",
	"dream-companion-soul", "dream-technical-soul", "dream-sim-soul",
	"dream-companion-people", "dream-sim-people", "dream-sim-learnings",
}

// runAgents is runDreamAgents: every cursor read and write below runs
// under the caller's lock.
func (j *Job) runAgents(ctx context.Context, lock *runLock) (logger.Fields, error) {
	found := map[string]string{}
	for _, tag := range dreamTags {
		name, err := j.findDreamAgent(ctx, tag)
		if err != nil {
			return nil, err
		}
		found[tag] = name
	}
	if found["dream-companion"] == "" && found["dream-technical"] == "" && found["dream-sim"] == "" {
		j.log("abort", logger.Fields{{Key: "reason", Value: "No dream agent found or valid"}})
		return logger.Fields{{Key: "error", Value: "No valid dream agents found. At least one of dream-companion, dream-technical, or dream-sim must exist and be created by a trusted creator."}}, nil
	}

	rows, err := j.DB.Query(ctx, `SELECT ac.name, ac.id AS actor_id, agc.dream_mode, agc.dream_source,
                agc.last_dream_at, agc.startup_instructions
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE agc.dream_mode IN ('companion', 'technical', 'sim', 'sim-shared')`)
	if err != nil {
		return nil, err
	}
	var agents []*agentRow
	for rows.Next() {
		a := &agentRow{}
		if err := rows.Scan(&a.name, &a.actorID, &a.dreamMode, &a.dreamSource, &a.lastDreamAt, &a.startupInstructions); err != nil {
			rows.Close()
			return nil, err
		}
		if a.lastDreamAt != nil {
			t := jsDate(*a.lastDreamAt)
			a.lastDreamAt = &t
		}
		agents = append(agents, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		j.log("skip", logger.Fields{{Key: "reason", Value: "No agents with dream mode enabled"}})
		return logger.Fields{{Key: "processed", Value: 0}, {Key: "reason", Value: "No agents with dream mode enabled"}}, nil
	}

	labels := make([]string, len(agents))
	for i, a := range agents {
		labels[i] = a.name + ":" + a.dreamMode
	}
	j.log("start", logger.Fields{{Key: "agents", Value: labels}})

	results := []logger.Fields{}
	for _, agent := range agents {
		if err := boundary(ctx, lock); err != nil {
			return nil, err
		}
		skipDelay, err := j.dreamAgent(ctx, agent, found, &results, lock)
		if err != nil {
			if abort(ctx, err) {
				return nil, err
			}
			j.log("error", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "error", Value: message(err)}})
			j.logError("agent-error", logger.ErrorOpts{Agent: agent.name, Message: message(err), Detail: detail(err)})
			failure := logger.Fields{{Key: "agent", Value: agent.name}, {Key: "error", Value: message(err)}}
			if agent.dreamMode == "sim-shared" {
				failure = append(failure, logger.Field{Key: "mode", Value: "sim-shared"}, logger.Field{Key: "failedActorCount", Value: 1})
			}
			results = append(results, failure)
		} else if skipDelay {
			// Node's `continue` inside the try skipped the pause.
			continue
		}
		delay, err := j.dreamDelayMs("dream_interagent_delay", 2000)
		if err != nil {
			return nil, err
		}
		if delay > 0 {
			if err := j.sleep(ctx, delay); err != nil {
				return nil, err
			}
		}
	}

	// The run-level failure signal covers shared-VA villagers only; a
	// dedicated agent's failure stays in its result.
	failedShared := 0
	for _, r := range results {
		if mode, _ := get(r, "mode"); mode == "sim-shared" {
			if n, ok := get(r, "failedActorCount"); ok {
				if count, ok := n.(int); ok {
					failedShared += count
				}
			}
		}
	}
	j.log("complete", logger.Fields{{Key: "processed", Value: len(results)}, {Key: "failedSharedActorCount", Value: failedShared}})
	return logger.Fields{{Key: "processed", Value: len(results)}, {Key: "failedSharedActorCount", Value: failedShared}, {Key: "results", Value: results}}, nil
}

// dreamAgent dreams one agent and appends its result. skipDelay is true
// where Node continued to the next agent without the pause.
func (j *Job) dreamAgent(ctx context.Context, agent *agentRow, found map[string]string, results *[]logger.Fields, lock *runLock) (skipDelay bool, err error) {
	if agent.dreamMode == "sim-shared" {
		return true, j.processSharedAgent(ctx, agent, found, results, lock)
	}

	var names agentNames
	switch agent.dreamMode {
	case "companion":
		names = agentNames{dream: found["dream-companion"], soul: found["dream-companion-soul"], people: found["dream-companion-people"]}
	case "technical":
		names = agentNames{dream: found["dream-technical"], soul: found["dream-technical-soul"]}
	case "sim":
		names = agentNames{dream: found["dream-sim"], soul: found["dream-sim-soul"], people: found["dream-sim-people"], learnings: found["dream-sim-learnings"]}
	}
	if names.dream == "" {
		*results = append(*results, logger.Fields{{Key: "agent", Value: agent.name}, {Key: "error", Value: "dream-" + agent.dreamMode + " agent not available"}})
		return true, nil
	}

	// An account on the conversation source that never uploads a log
	// would dream nothing every night; if it keeps notes of its own it
	// dreams those for this run. The stored source is not changed, so its
	// first uploaded log brings it back. Sim NPCs are left out: the engine
	// pushes their conversations.
	var backfillFrom *time.Time
	if agent.dreamSource == "conversation" && (agent.dreamMode == "companion" || agent.dreamMode == "technical") {
		useNotes, from, err := j.decideNotesFallback(ctx, agent.name)
		if err != nil {
			return false, err
		}
		if useNotes {
			j.log("source-fallback", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "from", Value: "conversation"}, {Key: "to", Value: "notes"}})
			copied := *agent
			copied.dreamSource = "notes"
			copied.dreamNotesKind = "note"
			agent = &copied
			backfillFrom = from
		}
	}

	// First runs take the last 24 hours, except in notes mode, which
	// starts at the earliest note so the soul grows over the whole history.
	since := agent.lastDreamAt
	if backfillFrom != nil {
		// A fallback account's cursor walked over empty conversation days;
		// until it has a dream it starts at its earliest note.
		since = backfillFrom
	} else if since == nil && agent.dreamSource == "notes" {
		var minUpdated *time.Time
		if err := j.DB.QueryRow(ctx, `SELECT MIN(updated_at) AS min_updated FROM documents
                     WHERE namespace = $1 AND deleted_at IS NULL
                     AND slug NOT LIKE 'conversations/%'
                     AND slug NOT LIKE 'dreams/%'
                     AND slug NOT LIKE 'context/%'
                     AND slug NOT LIKE 'learnings/%'`, agent.name).Scan(&minUpdated); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if minUpdated == nil {
			j.log("no-notes", logger.Fields{{Key: "agent", Value: agent.name}})
			*results = append(*results, logger.Fields{{Key: "agent", Value: agent.name}, {Key: "skipped", Value: true}, {Key: "reason", Value: "dream_source=notes but namespace has no source notes"}})
			return true, nil
		}
		start := windowStartIncluding(*minUpdated)
		since = &start
	}
	if since == nil {
		start := time.UnixMilli(j.now().UnixMilli() - dayMs).UTC()
		since = &start
	}
	chunks := computeDailyChunks(*since, j.now())
	if len(chunks) == 0 {
		j.log("no-window", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "since", Value: jsutil.DateISO(*since)}})
		*results = append(*results, logger.Fields{{Key: "agent", Value: agent.name}, {Key: "skipped", Value: true}, {Key: "reason", Value: "last_dream_at is in the future"}})
		return true, nil
	}
	j.log("chunks-planned", logger.Fields{
		{Key: "agent", Value: agent.name}, {Key: "count", Value: len(chunks)},
		{Key: "from", Value: jsutil.DateISO(chunks[0].from)}, {Key: "to", Value: jsutil.DateISO(chunks[len(chunks)-1].to)},
	})

	chunkResults, err := j.runChunkLoop(ctx, agent, names, chunks, scope{selfName: agent.name},
		func(to time.Time) error {
			_, err := j.DB.Exec(ctx, "UPDATE agent_configuration SET last_dream_at = $1 WHERE actor_id = $2", to, agent.actorID)
			return err
		}, lock)
	if err != nil {
		return false, err
	}
	*results = append(*results, logger.Fields{
		{Key: "agent", Value: agent.name}, {Key: "mode", Value: agent.dreamMode},
		{Key: "chunkCount", Value: len(chunks)}, {Key: "chunks", Value: chunkResults},
	})
	return false, nil
}

// windowStartIncluding backs off 1 ms from a note's timestamp, since a
// chunk's window is exclusive at its start.
func windowStartIncluding(t time.Time) time.Time {
	return time.UnixMilli(jsDate(t).UnixMilli() - 1).UTC()
}

// decideNotesFallback: a conversation-source agent dreams from its notes
// this run when it has no live conversation log and at least one note of
// its own (kind note: the signup template is kind instruction). from is
// set while it has no dream yet: the window then starts at its earliest
// note.
func (j *Job) decideNotesFallback(ctx context.Context, agentName string) (bool, *time.Time, error) {
	var hasConversations, hasDreams *bool
	var minNote *time.Time
	err := j.DB.QueryRow(ctx, `SELECT
             EXISTS (SELECT 1 FROM documents
                     WHERE namespace = $1 AND deleted_at IS NULL
                     AND slug LIKE 'conversations/%') AS has_conversations,
             EXISTS (SELECT 1 FROM documents
                     WHERE namespace = $1 AND deleted_at IS NULL
                     AND slug LIKE 'dreams/%') AS has_dreams,
             (SELECT MIN(updated_at) FROM documents
              WHERE namespace = $1 AND deleted_at IS NULL AND kind = 'note'
              AND slug NOT LIKE 'conversations/%'
              AND slug NOT LIKE 'dreams/%'
              AND slug NOT LIKE 'context/%'
              AND slug NOT LIKE 'learnings/%') AS min_note_updated`, agentName).Scan(&hasConversations, &hasDreams, &minNote)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if hasConversations != nil && *hasConversations || minNote == nil {
		return false, nil, nil
	}
	if hasDreams != nil && *hasDreams {
		return true, nil, nil
	}
	from := windowStartIncluding(*minNote)
	return true, &from, nil
}

// runChunkLoop is the per-day loop of one identity, shared by dedicated
// agents and shared-VA villagers. advance stamps the cursor after each
// chunk, so a later failure keeps the earlier days; a failed chunk stops
// the loop (so no unprocessed day is skipped) and the next run retries it.
func (j *Job) runChunkLoop(ctx context.Context, agent *agentRow, names agentNames, chunks []chunk, sc scope,
	advance func(to time.Time) error, lock *runLock) ([]logger.Fields, error) {
	interChunk, err := j.dreamDelayMs("dream_interchunk_delay", 1000)
	if err != nil {
		return nil, err
	}
	results := []logger.Fields{}
	for i, c := range chunks {
		if err := boundary(ctx, lock); err != nil {
			return nil, err
		}
		r, err := j.processChunk(ctx, agent, names, c, sc)
		if err == nil {
			results = append(results, r)
			// The chunk spent minutes in a model call; the cursor write is
			// what loses data when two runs race, so it gets the closest check.
			if err = boundary(ctx, lock); err == nil {
				err = advance(c.to)
			}
		}
		if err != nil {
			if abort(ctx, err) {
				return nil, err
			}
			chunkDate := utcDate(c.from)
			fields := logger.Fields{{Key: "agent", Value: agent.name}, {Key: "chunkDate", Value: chunkDate}, {Key: "error", Value: message(err)}}
			if sc.prefix.value != "" {
				fields = append(fields, logger.Field{Key: "prefix", Value: sc.prefix.value})
			}
			j.log("chunk-error", fields)
			j.logError("chunk-error", logger.ErrorOpts{Agent: agent.name, Message: message(err), Detail: detail(err)})
			results = append(results, logger.Fields{{Key: "chunkDate", Value: chunkDate}, {Key: "error", Value: message(err)}})
			break
		}
		if i+1 < len(chunks) && interChunk > 0 {
			if err := j.sleep(ctx, interChunk); err != nil {
				return nil, err
			}
		}
	}
	return results, nil
}

// processSharedAgent dreams every villager of a pooled shared-VA agent
// (dream_mode sim-shared) under its own prefix and cursor, without a soul
// (a villager's soul is the engine's about_me). One bad villager never
// fails the others; the aggregate result counts the failures.
func (j *Job) processSharedAgent(ctx context.Context, agent *agentRow, found map[string]string, results *[]logger.Fields, lock *runLock) error {
	if found["dream-sim"] == "" {
		*results = append(*results, logger.Fields{
			{Key: "agent", Value: agent.name}, {Key: "mode", Value: "sim-shared"},
			{Key: "failedActorCount", Value: 1}, {Key: "error", Value: "dream-sim agent not available"},
		})
		return nil
	}
	// The notes-mode source query is not prefix-scoped, so a shared agent
	// on it would write the pool's notes into one villager's subtree.
	source := agent.dreamSource
	if source == "" {
		source = "conversation"
	}
	if source != "conversation" {
		msg := "dream_source must be conversation for sim-shared (got " + source + ")"
		j.log("shared-unsupported-source", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "source", Value: source}})
		j.logError("shared-unsupported-source", logger.ErrorOpts{Agent: agent.name, Message: msg})
		*results = append(*results, logger.Fields{
			{Key: "agent", Value: agent.name}, {Key: "mode", Value: "sim-shared"},
			{Key: "failedActorCount", Value: 1}, {Key: "error", Value: msg},
		})
		return nil
	}
	names := agentNames{dream: found["dream-sim"], people: found["dream-sim-people"], learnings: found["dream-sim-learnings"]}

	actorResults := []logger.Fields{}
	setupError := ""
	rosterSize, err := j.dreamSharedRoster(ctx, agent, names, &actorResults, lock)
	if err != nil {
		if abort(ctx, err) {
			return err
		}
		// Recorded, and the villagers already done are still reported.
		j.log("shared-agent-error", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "error", Value: message(err)}})
		j.logError("shared-agent-error", logger.ErrorOpts{Agent: agent.name, Message: message(err), Detail: detail(err)})
		setupError = message(err)
	}
	if rosterSize == 0 && len(actorResults) == 0 && setupError == "" {
		j.log("shared-no-roster", logger.Fields{{Key: "agent", Value: agent.name}})
		*results = append(*results, logger.Fields{{Key: "agent", Value: agent.name}, {Key: "mode", Value: "sim-shared"}, {Key: "actorCount", Value: 0}})
		return nil
	}
	failed := countFailedActors(actorResults)
	if setupError != "" {
		failed++
	}
	summary := logger.Fields{
		{Key: "agent", Value: agent.name}, {Key: "mode", Value: "sim-shared"},
		{Key: "actorCount", Value: len(actorResults)}, {Key: "failedActorCount", Value: failed},
		{Key: "actors", Value: actorResults},
	}
	if setupError != "" {
		summary = append(summary, logger.Field{Key: "error", Value: setupError})
	}
	*results = append(*results, summary)
	return nil
}

type rosterRow struct {
	slugPrefix, displayName *string
	lastDreamAt             *time.Time
}

// dreamSharedRoster dreams each villager of the roster, appending one
// result per villager, and returns the roster size.
func (j *Job) dreamSharedRoster(ctx context.Context, agent *agentRow, names agentNames, actorResults *[]logger.Fields, lock *runLock) (int, error) {
	rows, err := j.DB.Query(ctx, `SELECT slug_prefix, display_name, last_dream_at
         FROM sim_shared_actor
         WHERE shared_actor_id = $1
         ORDER BY slug_prefix ASC`, agent.actorID)
	if err != nil {
		return 0, err
	}
	var roster []rosterRow
	for rows.Next() {
		var r rosterRow
		if err := rows.Scan(&r.slugPrefix, &r.displayName, &r.lastDreamAt); err != nil {
			rows.Close()
			return 0, err
		}
		if r.lastDreamAt != nil {
			t := jsDate(*r.lastDreamAt)
			r.lastDreamAt = &t
		}
		roster = append(roster, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(roster) == 0 {
		return 0, nil
	}
	interActor, err := j.dreamDelayMs("dream_interagent_delay", 2000)
	if err != nil {
		return 0, err
	}
	for i, row := range roster {
		if err := boundary(ctx, lock); err != nil {
			return 0, err
		}
		skipDelay, prefix, err := j.dreamVillager(ctx, agent, names, row, actorResults, lock)
		if err != nil {
			if abort(ctx, err) {
				return 0, err
			}
			var p any = row.slugPrefix
			if prefix != "" {
				p = prefix
			}
			j.log("shared-actor-error", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "prefix", Value: p}, {Key: "error", Value: message(err)}})
			ctxLabel := prefix
			if ctxLabel == "" {
				ctxLabel = str(row.slugPrefix)
			}
			j.logError("shared-actor-error", logger.ErrorOpts{Agent: agent.name, Context: ctxLabel, Message: message(err), Detail: detail(err)})
			*actorResults = append(*actorResults, logger.Fields{{Key: "prefix", Value: p}, {Key: "error", Value: message(err)}})
		} else if skipDelay {
			continue
		}
		if i+1 < len(roster) && interActor > 0 {
			if err := j.sleep(ctx, interActor); err != nil {
				return 0, err
			}
		}
	}
	return len(roster), nil
}

// dreamVillager dreams one roster row. prefix is the validated prefix, set
// once it is known (the error path reports it).
func (j *Job) dreamVillager(ctx context.Context, agent *agentRow, names agentNames, row rosterRow, actorResults *[]logger.Fields, lock *runLock) (skipDelay bool, prefix string, err error) {
	// Roster rows are database state, validated here, not trusted because
	// the distiller writes them canonically.
	slugPrefix := validateRosterPrefix(row.slugPrefix)
	if slugPrefix == "" {
		label := "null"
		if row.slugPrefix != nil {
			label = *row.slugPrefix
		}
		j.log("shared-actor-invalid-prefix", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "prefix", Value: row.slugPrefix}})
		j.logError("shared-actor-invalid-prefix", logger.ErrorOpts{Agent: agent.name, Message: "invalid roster slug prefix: " + label})
		*actorResults = append(*actorResults, logger.Fields{{Key: "prefix", Value: row.slugPrefix}, {Key: "error", Value: "invalid slug prefix"}})
		return true, "", nil
	}
	// The display name is the villager's self identity; without it the
	// pooled agent's name would stand in and its own lines would read as a
	// counterparty's.
	if row.displayName == nil || jsutil.Trim(*row.displayName) == "" {
		j.log("shared-actor-invalid-display", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "prefix", Value: slugPrefix}})
		j.logError("shared-actor-invalid-display", logger.ErrorOpts{Agent: agent.name, Message: "missing display name for roster prefix " + slugPrefix})
		*actorResults = append(*actorResults, logger.Fields{{Key: "prefix", Value: slugPrefix}, {Key: "error", Value: "missing display name"}})
		return true, slugPrefix, nil
	}
	sc := scope{prefix: scopePrefix{value: slugPrefix}, selfName: *row.displayName}

	// A first run starts at the villager's earliest conversation note, so a
	// pushed backlog is dreamed rather than skipped.
	since := row.lastDreamAt
	if since == nil {
		var minCreated *time.Time
		if err := j.DB.QueryRow(ctx, `SELECT MIN(created_at) AS min_created FROM documents
                     WHERE namespace = $1 AND slug LIKE $2 AND deleted_at IS NULL`,
			agent.name, slugPrefix+"conversations/%").Scan(&minCreated); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, slugPrefix, err
		}
		if minCreated == nil {
			j.log("shared-no-conversations", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "prefix", Value: slugPrefix}})
			*actorResults = append(*actorResults, logger.Fields{{Key: "prefix", Value: slugPrefix}, {Key: "skipped", Value: true}, {Key: "reason", Value: "no conversation notes"}})
			return true, slugPrefix, nil
		}
		start := windowStartIncluding(*minCreated)
		since = &start
	}
	chunks := computeDailyChunks(*since, j.now())
	if len(chunks) == 0 {
		j.log("shared-no-window", logger.Fields{{Key: "agent", Value: agent.name}, {Key: "prefix", Value: slugPrefix}, {Key: "since", Value: jsutil.DateISO(*since)}})
		*actorResults = append(*actorResults, logger.Fields{{Key: "prefix", Value: slugPrefix}, {Key: "skipped", Value: true}, {Key: "reason", Value: "last_dream_at is in the future"}})
		return true, slugPrefix, nil
	}
	j.log("shared-chunks-planned", logger.Fields{
		{Key: "agent", Value: agent.name}, {Key: "prefix", Value: slugPrefix}, {Key: "count", Value: len(chunks)},
		{Key: "from", Value: jsutil.DateISO(chunks[0].from)}, {Key: "to", Value: jsutil.DateISO(chunks[len(chunks)-1].to)},
	})
	chunkResults, err := j.runChunkLoop(ctx, agent, names, chunks, sc, func(to time.Time) error {
		// Keyed by the stored prefix, so it hits exactly this roster row.
		_, err := j.DB.Exec(ctx, `UPDATE sim_shared_actor SET last_dream_at = $1
                     WHERE shared_actor_id = $2 AND slug_prefix = $3`, to, agent.actorID, row.slugPrefix)
		return err
	}, lock)
	if err != nil {
		return false, slugPrefix, err
	}
	completed := 0
	for _, r := range chunkResults {
		if !truthy(r, "error") {
			completed++
		}
	}
	*actorResults = append(*actorResults, logger.Fields{
		{Key: "prefix", Value: slugPrefix}, {Key: "plannedChunks", Value: len(chunks)},
		{Key: "completedChunks", Value: completed}, {Key: "chunks", Value: chunkResults},
	})
	return false, slugPrefix, nil
}

// cronEvent is one of planCronReport's events.
type cronEvent struct {
	kind   string // "shared-failures" or "complete"
	count  int
	status string
}

// planCronReport maps a run result to the scheduler's events: a
// shared-failures event when villagers failed, then the completion event,
// whose status ("completed-with-errors" or "ok") describes the run.
func planCronReport(result logger.Fields) []cronEvent {
	failures := 0
	if v, ok := get(result, "failedSharedActorCount"); ok {
		if n, ok := v.(int); ok {
			failures = n
		}
	}
	var events []cronEvent
	if failures > 0 {
		events = append(events, cronEvent{kind: "shared-failures", count: failures})
	}
	status := "ok"
	if failures > 0 {
		status = "completed-with-errors"
	}
	return append(events, cronEvent{kind: "complete", status: status})
}

// Pass is the scheduled callback: one run and its report.
func (j *Job) Pass(ctx context.Context, schedule string) {
	j.log("cron-trigger", logger.Fields{{Key: "schedule", Value: schedule}})
	result, err := j.Run(ctx)
	if err != nil {
		j.log("cron-error", logger.Fields{{Key: "error", Value: message(err)}})
		j.logError("cron-error", logger.ErrorOpts{Message: message(err), Detail: detail(err)})
		return
	}
	for _, ev := range planCronReport(result) {
		if ev.kind == "shared-failures" {
			j.logError("cron-shared-actor-failures", logger.ErrorOpts{Message: fmt.Sprintf("%d shared-VA actor(s) failed this dream run", ev.count)})
			continue
		}
		j.log("cron-complete", logger.Fields{{Key: "result", Value: result}, {Key: "status", Value: ev.status}})
	}
}

// Start schedules the job on dream_cron_schedule, as Node's
// startDreamScheduler did. An empty schedule or one node-cron's validate
// refuses leaves it off (nil task) with a log line. The error is what
// stopped the Node server from starting: a missing config key, or a
// schedule that validate accepted and schedule refused.
func (j *Job) Start() (*cron.Task, error) {
	schedule, err := j.Config.Get("dream_cron_schedule")
	if err != nil {
		return nil, err
	}
	if schedule == "" {
		j.log("scheduler", logger.Fields{{Key: "message", Value: "No dream_cron_schedule configured, scheduler disabled"}})
		return nil, nil
	}
	if !cron.Validate(schedule) {
		j.log("scheduler-error", logger.Fields{{Key: "message", Value: "Invalid cron expression: " + schedule}})
		return nil, nil
	}
	// A replacement that validates stops the running schedule, even when
	// node-cron's schedule() then threw; an empty or refused one above
	// leaves it running, as in Node.
	j.taskMu.Lock()
	defer j.taskMu.Unlock()
	if j.task != nil {
		stopTask(j.task)
		j.task = nil
	}
	sched, err := cron.Parse(schedule)
	if err != nil {
		return nil, err
	}
	task, err := cron.Start(sched, func(ctx context.Context) { j.Pass(ctx, schedule) }, cron.Options{
		Missed: func(slot time.Time) {
			j.Log.Errorf("cron: missed execution at %s! Possible blocking IO or high CPU use in the server process.", slot.UTC().Format(time.RFC3339))
		},
		Failed: func(err error) {
			j.Log.Errorf("cron: dream scheduler stopped: %s", err)
		},
	})
	if err != nil {
		return nil, err
	}
	j.task = task
	j.log("scheduler", logger.Fields{{Key: "message", Value: "Dream scheduler started"}, {Key: "schedule", Value: schedule}})
	return task, nil
}

// FindDreamAgent is findDreamAgent for the other callers of the dream
// agents (/sim/soul): the agent with the expertise tag, or "" (logged).
// It needs DB, Log and Trust.
func (j *Job) FindDreamAgent(ctx context.Context, tag string) (string, error) {
	return j.findDreamAgent(ctx, tag)
}

// DetectReasoningPreamble is detectReasoningPreamble, shared with
// /sim/soul.
func DetectReasoningPreamble(text string) string { return detectReasoningPreamble(text) }
