// Package cleanup is the nightly maintenance job (Node's services/cleanup.js):
//
//  1. Decay cleanup soft-deletes notes whose search decay factor,
//     0.5 ^ (age_days / half_life), has fallen below cleanup_decay_threshold.
//     Age counts from the latest of created_at, updated_at and
//     last_accessed.
//  2. The virtual_agent_calls purge hard-deletes call-log rows older than
//     va_call_log_retention_days.
//  3. Conversation retention soft-deletes conversation notes older than
//     conversation_retention_days, then turns them into tombstones (content
//     emptied, metadata.purged_at set) a retention window later.
//
// The three run in that order on cleanup_cron_schedule, each on its own, so
// one failing does not stop the others.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/cron"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// Logger is the part of *logger.Logger the job uses.
type Logger interface {
	Log(subsystem, action string, details logger.Fields)
	LogError(subsystem, action string, opts logger.ErrorOpts)
	Errorf(format string, args ...any)
}

// Job runs the cleanup tasks.
type Job struct {
	DB     db.DB
	Config *config.Store
	Log    Logger
}

func (j *Job) log(action string, details logger.Fields) {
	j.Log.Log("cleanup", action, details)
}

// jsNumber is a float64 in a log line as JSON.stringify writes a number:
// JavaScript's text, and null for NaN and the infinities.
type jsNumber float64

func (n jsNumber) MarshalJSON() ([]byte, error) {
	x := float64(n)
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return []byte("null"), nil
	}
	return []byte(jsutil.NumberString(x)), nil
}

// message is the text Node logged for err: a Postgres error's own message,
// as node-postgres puts it in err.message, else Go's text.
func message(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}

// usageSQL is Node's documents.updateUsage. Here it runs inside the
// transaction that retires the notes, so the counters cannot miss a
// retirement.
const usageSQL = `
        INSERT INTO namespace_usage (namespace, note_count, total_bytes, updated_at)
        VALUES ($1, $2, $3, NOW())
        ON CONFLICT (namespace) DO UPDATE SET
            note_count = GREATEST(0, namespace_usage.note_count + $2),
            total_bytes = GREATEST(0, namespace_usage.total_bytes + $3),
            updated_at = NOW()
    `

const chunksSQL = `DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)`

// decayHalfLife reads one half-life the way Node's cleanup did: a negative
// value disables the rule; otherwise config.ParseNonNegativeFinite (blank
// and unparseable give the fallback, 0 stays 0). Search ranking reads a
// negative as the fallback; this path deletes, so it reads it as off.
func (j *Job) decayHalfLife(key string, fallback float64) (float64, error) {
	raw, err := j.Config.Get(key)
	if err != nil {
		return 0, err
	}
	if jsutil.StringToNumber(raw) < 0 {
		return 0, nil
	}
	return config.ParseNonNegativeFinite(raw, true, fallback), nil
}

const ageExpr = `EXTRACT(EPOCH FROM (NOW() - GREATEST(d.created_at, COALESCE(d.updated_at, d.created_at), COALESCE(d.last_accessed, d.created_at)))) / 86400.0`

var (
	decayKinds     = []string{"task", "learning", "note", "conversation", "dream"}
	cognitiveTypes = []struct {
		name     string
		fallback float64
	}{{"episodic", 90}, {"reflective", 180}}
)

// BuildDecayConditions is Node's buildDecayConditions: one condition per
// kind and cognitive type with a positive half-life, and the parameters,
// $1 being the threshold. A cognitive type with its own half-life overrides
// the note's kind. Numbers are bound as node-postgres sent them.
func (j *Job) BuildDecayConditions(threshold float64) ([]string, []any, error) {
	var conditions []string
	params := []any{jsutil.Num(threshold)}
	for _, kind := range decayKinds {
		halfLife, err := j.decayHalfLife("search_decay_halflife_"+kind, 0)
		if err != nil {
			return nil, nil, err
		}
		if halfLife <= 0 {
			continue
		}
		params = append(params, jsutil.Num(halfLife))
		conditions = append(conditions, fmt.Sprintf(
			`(d.kind = '%s' AND (d.metadata->>'cognitive_type' IS NULL OR LOWER(TRIM(d.metadata->>'cognitive_type')) NOT IN ('episodic', 'reflective')) AND POWER(0.5, %s / $%d::numeric) < $1)`,
			kind, ageExpr, len(params)))
	}
	for _, ct := range cognitiveTypes {
		halfLife, err := j.decayHalfLife("search_decay_halflife_"+ct.name, ct.fallback)
		if err != nil {
			return nil, nil, err
		}
		if halfLife <= 0 {
			continue
		}
		params = append(params, jsutil.Num(halfLife))
		conditions = append(conditions, fmt.Sprintf(
			`(LOWER(TRIM(d.metadata->>'cognitive_type')) = '%s' AND POWER(0.5, %s / $%d::numeric) < $1)`,
			ct.name, ageExpr, len(params)))
	}
	return conditions, params, nil
}

// isoOrNil is a nullable timestamptz as JSON.stringify wrote node-postgres's
// Date, or null.
func isoOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return jsutil.DateISO(*t)
}

type decayRow struct {
	id                             int64
	namespace, slug, kind          string
	cognitiveType                  *string
	createdAt, updatedAt, accessed *time.Time
}

// RunDecayCleanup soft-deletes the notes below the decay threshold and
// hard-deletes their chunks. The result is the object Node returned.
func (j *Job) RunDecayCleanup(ctx context.Context) (logger.Fields, error) {
	enabled, err := j.Config.Get("cleanup_enabled")
	if err != nil {
		return nil, err
	}
	if enabled != "true" {
		j.log("decay-skip", logger.Fields{{Key: "reason", Value: "cleanup_enabled is false"}})
		return logger.Fields{{Key: "skipped", Value: true}, {Key: "reason", Value: "disabled"}}, nil
	}

	rawThreshold, err := j.Config.Get("cleanup_decay_threshold")
	if err != nil {
		return nil, err
	}
	// parseFloat(x) || 0.05: NaN and zero take the default.
	threshold, ok := jsutil.ParseFloat(rawThreshold)
	if !ok || threshold == 0 {
		threshold = 0.05
	}
	conditions, params, err := j.BuildDecayConditions(threshold)
	if err != nil {
		return nil, err
	}
	if len(conditions) == 0 {
		j.log("decay-skip", logger.Fields{{Key: "reason", Value: "No decaying note types configured"}})
		return logger.Fields{{Key: "skipped", Value: true}, {Key: "reason", Value: "No decaying note types"}}, nil
	}

	candidates, err := j.decayCandidates(ctx, strings.Join(conditions, " OR "), params)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		j.log("decay-complete", logger.Fields{{Key: "deleted", Value: 0}, {Key: "threshold", Value: jsNumber(threshold)}})
		return logger.Fields{{Key: "deleted", Value: 0}, {Key: "threshold", Value: jsNumber(threshold)}}, nil
	}
	j.log("decay-found", logger.Fields{{Key: "count", Value: len(candidates)}, {Key: "threshold", Value: jsNumber(threshold)}})

	deleted := 0
	for _, row := range candidates {
		// Shutdown cancelled the pass: stop here, as one error, rather
		// than log a failure for every note left.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := j.retireNote(ctx, row); err != nil {
			j.log("decay-delete-error", logger.Fields{
				{Key: "namespace", Value: row.namespace},
				{Key: "slug", Value: row.slug},
				{Key: "error", Value: message(err)},
			})
			continue
		}
		j.log("decay-deleted", logger.Fields{
			{Key: "namespace", Value: row.namespace},
			{Key: "slug", Value: row.slug},
			{Key: "kind", Value: row.kind},
			{Key: "cognitiveType", Value: row.cognitiveType},
			{Key: "created", Value: isoOrNil(row.createdAt)},
			{Key: "lastAccessed", Value: isoOrNil(row.accessed)},
		})
		deleted++
	}
	j.log("decay-complete", logger.Fields{
		{Key: "deleted", Value: deleted},
		{Key: "total", Value: len(candidates)},
		{Key: "threshold", Value: jsNumber(threshold)},
	})
	return logger.Fields{{Key: "deleted", Value: deleted}, {Key: "threshold", Value: jsNumber(threshold)}}, nil
}

func (j *Job) decayCandidates(ctx context.Context, where string, params []any) ([]decayRow, error) {
	rows, err := j.DB.Query(ctx, `SELECT id, namespace, slug, kind,
                d.metadata->>'cognitive_type' AS cognitive_type,
                d.created_at, d.updated_at, d.last_accessed
         FROM documents d
         WHERE d.deleted_at IS NULL AND (`+where+`)`, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []decayRow
	for rows.Next() {
		var r decayRow
		if err := rows.Scan(&r.id, &r.namespace, &r.slug, &r.kind, &r.cognitiveType, &r.createdAt, &r.updatedAt, &r.accessed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// retireNote soft-deletes one note, credits its size back to the namespace
// usage, and deletes its chunks, in one transaction. Node ran the three on
// their own, so a crash between them left a deleted note whose chunks still
// came up in search, or a usage counter never credited. The deleted_at
// guard keeps a note that a request deleted after the candidate query from
// being credited twice; its chunks are deleted either way, as in Node.
func (j *Job) retireNote(ctx context.Context, row decayRow) error {
	tx, err := j.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var length *int64
	err = tx.QueryRow(ctx,
		`UPDATE documents SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL RETURNING LENGTH(content) AS content_length`,
		row.id).Scan(&length)
	switch {
	case err == nil:
		var bytes int64
		if length != nil {
			bytes = *length
		}
		if _, err := tx.Exec(ctx, usageSQL, row.namespace, -1, -bytes); err != nil {
			return err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if _, err := tx.Exec(ctx, chunksSQL, row.namespace, row.slug); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PurgeCallLogs hard-deletes virtual_agent_calls rows past
// va_call_log_retention_days (parseInt, 0 or less is off). It returns the
// result object and the number of rows deleted.
func (j *Job) PurgeCallLogs(ctx context.Context) (logger.Fields, int64, error) {
	raw, err := j.Config.Get("va_call_log_retention_days")
	if err != nil {
		return nil, 0, err
	}
	days, ok := jsutil.ParseInt(raw)
	if !ok || days <= 0 {
		return logger.Fields{{Key: "purged", Value: 0}}, 0, nil
	}
	tag, err := j.DB.Exec(ctx, `DELETE FROM virtual_agent_calls
         WHERE created_at < NOW() - INTERVAL '1 day' * $1
         RETURNING id`, jsutil.Num(days))
	if err != nil {
		return nil, 0, err
	}
	purged := tag.RowsAffected()
	result := logger.Fields{{Key: "purged", Value: purged}, {Key: "retentionDays", Value: jsNumber(days)}}
	if purged > 0 {
		j.log("call-logs-purged", result)
	}
	return result, purged, nil
}

// Windows are the two conversation retention windows, in days: from a
// session's date to its soft delete, and from the soft delete to the
// tombstone.
type Windows struct {
	RetentionDays, SoftAfterDays, PurgeAfterDays float64
}

// ConversationRetentionWindows reads conversation_retention_days as Node
// did. Blank, unparseable, fractional and negative values turn retention
// off (nil), since this path deletes. Retention N soft-deletes at N + 1
// days, so a session held on day D stays through all of day D + N.
func ConversationRetentionWindows(raw string) *Windows {
	if jsutil.Trim(raw) == "" {
		return nil
	}
	days := jsutil.StringToNumber(raw)
	if math.IsNaN(days) || math.IsInf(days, 0) || days != math.Trunc(days) || days < 0 {
		return nil
	}
	return &Windows{RetentionDays: days, SoftAfterDays: days + 1, PurgeAfterDays: days + 1}
}

type retiredRow struct {
	namespace, slug string
	length          *int64
}

// RetireConversations runs the two retention stages. Each stage is one
// transaction: the soft deletes, their chunk deletes and the usage credit
// land together, and so do the tombstones and their chunk deletes. A failed
// statement rolls its stage back, and the next run tries again; Node counted
// a failed chunk delete in chunkErrors and kept the rest, so chunkErrors is
// always 0 here.
func (j *Job) RetireConversations(ctx context.Context) (logger.Fields, error) {
	raw, err := j.Config.Get("conversation_retention_days")
	if err != nil {
		return nil, err
	}
	windows := ConversationRetentionWindows(raw)
	if windows == nil {
		j.log("conversation-skip", logger.Fields{{Key: "reason", Value: "conversation_retention_days is unset or invalid"}})
		return logger.Fields{{Key: "skipped", Value: true}, {Key: "reason", Value: "no retention window"}}, nil
	}

	// Age is the session's own date (metadata.session_date, written by the
	// memory-sync client), else created_at. pg_input_is_valid keeps a
	// malformed client-supplied date from failing the statement; CASE casts
	// only on the branch it takes.
	softDeleted, err := j.retireStage(ctx, `UPDATE documents
         SET deleted_at = NOW()
         WHERE kind = 'conversation' AND deleted_at IS NULL
           AND COALESCE(
                   CASE WHEN pg_input_is_valid(metadata->>'session_date', 'date')
                        THEN (metadata->>'session_date')::date END,
                   created_at::date
               ) < (NOW() - INTERVAL '1 day' * $1)::date
         RETURNING namespace, slug, LENGTH(content) AS content_length`, windows.SoftAfterDays, true)
	if err != nil {
		return nil, err
	}

	// A tombstone stays deleted and gains purged_at; the row is kept, so the
	// memory-sync diff never offers the session back as missing. The
	// purged_at guard stops the same rows being rewritten each night. The
	// chunk sweep covers rows retired before soft deletes removed chunks.
	purged, err := j.retireStage(ctx, `UPDATE documents
         SET content = '',
             metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object('purged_at', NOW())
         WHERE kind = 'conversation' AND deleted_at IS NOT NULL
           AND metadata->>'purged_at' IS NULL
           AND deleted_at < NOW() - INTERVAL '1 day' * $1
         RETURNING namespace, slug`, windows.PurgeAfterDays, false)
	if err != nil {
		return nil, err
	}

	summary := logger.Fields{
		{Key: "softDeleted", Value: softDeleted},
		{Key: "purged", Value: purged},
		{Key: "chunkErrors", Value: 0},
		{Key: "retentionDays", Value: jsNumber(windows.RetentionDays)},
	}
	if softDeleted > 0 || purged > 0 {
		j.log("conversations-retired", summary)
	}
	return summary, nil
}

// retireStage runs one retention UPDATE and deletes the chunks of the rows
// it returns, in one transaction. With credit, the rows' sizes are credited
// back to their namespaces, one usage update per namespace in first-seen
// order.
func (j *Job) retireStage(ctx context.Context, sql string, days float64, credit bool) (int, error) {
	tx, err := j.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	rows, err := tx.Query(ctx, sql, jsutil.Num(days))
	if err != nil {
		return 0, err
	}
	var retired []retiredRow
	for rows.Next() {
		var r retiredRow
		dest := []any{&r.namespace, &r.slug}
		if credit {
			dest = append(dest, &r.length)
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return 0, err
		}
		retired = append(retired, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	type usage struct {
		namespace     string
		count, nbytes int64
	}
	var credits []*usage
	byNamespace := map[string]*usage{}
	for _, r := range retired {
		if credit {
			u := byNamespace[r.namespace]
			if u == nil {
				u = &usage{namespace: r.namespace}
				byNamespace[r.namespace] = u
				credits = append(credits, u)
			}
			u.count++
			if r.length != nil {
				u.nbytes += *r.length
			}
		}
		if _, err := tx.Exec(ctx, chunksSQL, r.namespace, r.slug); err != nil {
			j.log("conversation-chunk-error", logger.Fields{
				{Key: "namespace", Value: r.namespace},
				{Key: "slug", Value: r.slug},
				{Key: "error", Value: message(err)},
			})
			return 0, err
		}
	}
	for _, u := range credits {
		if _, err := tx.Exec(ctx, usageSQL, u.namespace, -u.count, -u.nbytes); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(retired), nil
}

// Pass is one scheduled run: the three tasks in order, each failure logged
// (and recorded in error_log) without stopping the others. A task that
// shutdown cancelled ends the pass; the tasks after it do not start.
func (j *Job) Pass(ctx context.Context, schedule string) {
	j.log("cron-trigger", logger.Fields{{Key: "schedule", Value: schedule}})

	if result, err := j.RunDecayCleanup(ctx); err != nil {
		j.fail("cron-decay-error", err)
	} else {
		j.log("cron-decay-complete", logger.Fields{{Key: "result", Value: result}})
	}
	if ctx.Err() != nil {
		return
	}

	if result, purged, err := j.PurgeCallLogs(ctx); err != nil {
		j.fail("cron-purge-error", err)
	} else if purged > 0 {
		j.log("cron-purge-complete", logger.Fields{{Key: "result", Value: result}})
	}
	if ctx.Err() != nil {
		return
	}

	if result, err := j.RetireConversations(ctx); err != nil {
		j.fail("cron-conversations-error", err)
	} else {
		j.log("cron-conversations-complete", logger.Fields{{Key: "result", Value: result}})
	}
}

func (j *Job) fail(action string, err error) {
	msg := message(err)
	j.log(action, logger.Fields{{Key: "error", Value: msg}})
	j.Log.LogError("cleanup", action, logger.ErrorOpts{Message: msg, Detail: fmt.Sprintf("%+v", err)})
}

// Start schedules the job on cleanup_cron_schedule, as Node's
// startCleanupScheduler did. An empty schedule or one node-cron refuses
// leaves it off (nil task) with a log line. The error is what stopped the
// Node server from starting: a missing config key, or a schedule with no
// time to run in the next hundred years.
func (j *Job) Start() (*cron.Task, error) {
	schedule, err := j.Config.Get("cleanup_cron_schedule")
	if err != nil {
		return nil, err
	}
	if schedule == "" {
		j.log("scheduler", logger.Fields{{Key: "message", Value: "No cleanup_cron_schedule configured, scheduler disabled"}})
		return nil, nil
	}
	sched, err := cron.Parse(schedule)
	if err != nil {
		j.log("scheduler-error", logger.Fields{{Key: "message", Value: "Invalid cron expression: " + schedule}})
		return nil, nil
	}
	task, err := cron.Start(sched, func(ctx context.Context) { j.Pass(ctx, schedule) }, cron.Options{
		Missed: func(slot time.Time) {
			j.Log.Errorf("cron: missed execution at %s! Possible blocking IO or high CPU use in the server process.", slot.UTC().Format(time.RFC3339))
		},
		Failed: func(err error) {
			j.Log.Errorf("cron: cleanup scheduler stopped: %s", err)
		},
	})
	if err != nil {
		return nil, err
	}
	j.log("scheduler", logger.Fields{{Key: "message", Value: "Cleanup scheduler started"}, {Key: "schedule", Value: schedule}})
	return task, nil
}
