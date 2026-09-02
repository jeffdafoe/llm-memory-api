// Cleanup service — scheduled maintenance tasks that run on a daily cron.
//
// Tasks:
//   1. Decay cleanup — soft-deletes notes whose decay factor has dropped below
//      the configured threshold. Uses the same formula as search:
//      0.5 ^ (age_days / half_life), where age is the most recent of
//      created_at, updated_at, or last_accessed.
//   2. Call log purge — hard-deletes old virtual_agent_calls rows past
//      the retention period.
//   3. Conversation retention — soft-deletes conversation notes past
//      conversation_retention_days, then empties them to tombstones a
//      retention window later. Lived in scripts/db-cleanup.sh until LLM-642;
//      it moved here so the quota accounting and the tombstone semantics sit
//      next to the code that depends on them.

const pool = require('../db');
const config = require('./config');
const { parseNonNegativeFinite } = config;
const { updateUsage } = require('./documents');
const { log, logError } = require('./logger');

function logCleanup(action, details) {
    log('cleanup', action, details);
}

// Read one decay half-life: absent, blank and unparseable resolve to the
// fallback, an explicit 0 stays 0, and a negative disables the rule (see
// config.parseNonNegativeFinite for the first two, and below for the third —
// the negative is this function's one departure from the shared parser).
//
// The extra negative check is specific to cleanup. parseNonNegativeFinite
// resolves a negative to the fallback, which is right for search ranking in
// memory.js — a nonsense value there costs nothing but a ranking weight. Here
// it would be fail-open: this cron soft-deletes the note AND hard-deletes its
// vector chunks, so a restored note comes back unsearchable. A typo like "-90"
// must not turn "no cleanup for this category" into "delete at the default
// rate". Present-but-invalid disables the rule instead.
//
// The consequence is that ranking and cleanup can disagree for a negative
// value. That is deliberate, and is not the defect LLM-584 fixes: 0 is
// documented and must mean the same thing everywhere, a negative is undocumented
// garbage and the destructive path is entitled to be the conservative reader.
function decayHalfLife(key, fallback = 0) {
    const raw = config.get(key);
    if (Number(raw) < 0) return 0;
    return parseNonNegativeFinite(raw, fallback);
}

// Build SQL conditions that identify notes below the decay threshold.
// Returns { conditions, params } where conditions is an array of
// "kind = X AND decay < threshold" clauses, and params are the bound values.
// Mirrors the two-tier decay logic in memory.js (cognitive type first, then kind).
function buildDecayConditions(threshold) {
    const conditions = [];
    const params = [threshold]; // $1 = threshold
    let paramIdx = 2;

    // The effective age expression — most recent of created, updated, or accessed
    const ageExpr = `EXTRACT(EPOCH FROM (NOW() - GREATEST(d.created_at, COALESCE(d.updated_at, d.created_at), COALESCE(d.last_accessed, d.created_at)))) / 86400.0`;

    // Kind-based half-lives
    const kindHalfLives = {
        task: decayHalfLife('search_decay_halflife_task'),
        learning: decayHalfLife('search_decay_halflife_learning'),
        note: decayHalfLife('search_decay_halflife_note'),
        conversation: decayHalfLife('search_decay_halflife_conversation'),
        dream: decayHalfLife('search_decay_halflife_dream'),
    };

    // Cognitive type half-lives (override kind when set).
    //
    // These two carry a non-zero fallback, so they must not be read through
    // `|| 90`: both rows document "0 = no decay", and `||` would turn that 0
    // back into 90/180 and let the `halfLife <= 0` skip below never fire. The
    // note would keep decaying — and here that means the cron deletes it — on a
    // half-life the operator had switched off (LLM-584).
    const cognitiveHalfLives = {
        episodic: decayHalfLife('search_decay_halflife_episodic', 90),
        reflective: decayHalfLife('search_decay_halflife_reflective', 180),
    };

    // For each kind with a non-zero half-life, add a condition that matches
    // notes of that kind (without a cognitive type override) below the threshold.
    for (const [kind, halfLife] of Object.entries(kindHalfLives)) {
        if (halfLife <= 0) continue;

        params.push(halfLife);
        const hlIdx = paramIdx++;

        // Only match notes that don't have a cognitive type with its own half-life,
        // otherwise the cognitive type decay would apply instead.
        conditions.push(
            `(d.kind = '${kind}' AND (d.metadata->>'cognitive_type' IS NULL OR LOWER(TRIM(d.metadata->>'cognitive_type')) NOT IN ('episodic', 'reflective')) AND POWER(0.5, ${ageExpr} / $${hlIdx}::numeric) < $1)`
        );
    }

    // For each cognitive type with a non-zero half-life, add a condition
    // regardless of the note's kind.
    for (const [cogType, halfLife] of Object.entries(cognitiveHalfLives)) {
        if (halfLife <= 0) continue;

        params.push(halfLife);
        const hlIdx = paramIdx++;

        conditions.push(
            `(LOWER(TRIM(d.metadata->>'cognitive_type')) = '${cogType}' AND POWER(0.5, ${ageExpr} / $${hlIdx}::numeric) < $1)`
        );
    }

    return { conditions, params };
}

// Soft-delete notes whose decay factor has dropped below the threshold.
async function runDecayCleanup() {
    if (config.get('cleanup_enabled') !== 'true') {
        logCleanup('decay-skip', { reason: 'cleanup_enabled is false' });
        return { skipped: true, reason: 'disabled' };
    }

    const threshold = parseFloat(config.get('cleanup_decay_threshold')) || 0.05;
    const { conditions, params } = buildDecayConditions(threshold);

    if (conditions.length === 0) {
        logCleanup('decay-skip', { reason: 'No decaying note types configured' });
        return { skipped: true, reason: 'No decaying note types' };
    }

    const whereClause = conditions.join(' OR ');

    // Find all notes below the threshold that haven't been soft-deleted yet
    const result = await pool.query(
        `SELECT id, namespace, slug, kind,
                d.metadata->>'cognitive_type' AS cognitive_type,
                d.created_at, d.updated_at, d.last_accessed
         FROM documents d
         WHERE d.deleted_at IS NULL AND (${whereClause})`,
        params
    );

    if (result.rows.length === 0) {
        logCleanup('decay-complete', { deleted: 0, threshold });
        return { deleted: 0, threshold };
    }

    logCleanup('decay-found', { count: result.rows.length, threshold });

    // Soft-delete each note and hard-delete its vector chunks
    let deleted = 0;
    for (const row of result.rows) {
        try {
            // RETURNING the size so the quota is credited back the way a
            // manual deleteNote credits it. Before LLM-642 this UPDATE stood
            // alone and every note the cron retired kept counting against
            // namespace_usage forever. The deleted_at guard matters now that
            // a credit rides on the row: a request that deleted this note
            // between the candidate SELECT and here has already credited it.
            const retired = await pool.query(
                'UPDATE documents SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL RETURNING LENGTH(content) AS content_length',
                [row.id]
            );
            if (retired.rows.length > 0) {
                updateUsage(row.namespace, -1, -(retired.rows[0].content_length || 0));
            }

            // Hard-delete vector chunks so they can't appear in search results
            await pool.query(
                'DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)',
                [row.namespace, row.slug]
            );

            logCleanup('decay-deleted', {
                namespace: row.namespace,
                slug: row.slug,
                kind: row.kind,
                cognitiveType: row.cognitive_type,
                created: row.created_at,
                lastAccessed: row.last_accessed
            });
            deleted++;
        } catch (err) {
            logCleanup('decay-delete-error', {
                namespace: row.namespace,
                slug: row.slug,
                error: err.message
            });
        }
    }

    logCleanup('decay-complete', { deleted, total: result.rows.length, threshold });
    return { deleted, threshold };
}

// Hard-delete old rows from virtual_agent_calls.
// Retention period is configurable via va_call_log_retention_days.
async function purgeCallLogs() {
    const retentionDays = parseInt(config.get('va_call_log_retention_days')) || 0;
    if (retentionDays <= 0) {
        return { purged: 0 };
    }

    const result = await pool.query(
        `DELETE FROM virtual_agent_calls
         WHERE created_at < NOW() - INTERVAL '1 day' * $1
         RETURNING id`,
        [retentionDays]
    );

    const purged = result.rowCount;
    if (purged > 0) {
        logCleanup('call-logs-purged', { purged, retentionDays });
    }
    return { purged, retentionDays };
}

// Resolve the two conversation retention windows from the raw
// conversation_retention_days value. Returns null when retention is off:
// absent, blank, unparseable, fractional or negative. Same conservative
// reading as decayHalfLife above — this path deletes, so a garbled value must
// switch the rule off rather than fall back to a deleting default.
//
// softAfterDays counts from a conversation's session date to its soft-delete:
// retention + 1, so a session held on day N stays through all of day
// N + retention. purgeAfterDays counts from deleted_at to the tombstone. Both
// are the windows scripts/db-cleanup.sh used before LLM-642.
function conversationRetentionWindows(raw) {
    if (raw === undefined || raw === null || String(raw).trim() === '') return null;
    const days = Number(raw);
    if (!Number.isInteger(days) || days < 0) return null;
    return { retentionDays: days, softAfterDays: days + 1, purgeAfterDays: days + 1 };
}

// Retire conversation notes in two stages.
//
// Stage 1 soft-deletes conversations older than the retention window, with
// the same quota accounting and chunk removal as deleteNote.
//
// Stage 2 turns conversations soft-deleted a further window ago into
// tombstones: content emptied, chunks gone, the row and its metadata kept
// with a purged_at stamp. The row is deliberately never dropped. The
// memory-sync diff (routes/agent.js) treats any row for a session id as
// "present", so a retired session is never offered back to the client as
// missing. Hard-deleting the row was how every expired conversation came
// straight back on the next sync (LLM-642). A tombstone is a few hundred
// bytes of metadata; restoreNote refuses one.
async function retireConversations() {
    const windows = conversationRetentionWindows(config.get('conversation_retention_days'));
    if (!windows) {
        logCleanup('conversation-skip', { reason: 'conversation_retention_days is unset or invalid' });
        return { skipped: true, reason: 'no retention window' };
    }

    // Age is the session's own date (metadata.session_date, set by the
    // memory-sync client from the first message), not the upload time: a
    // months-old session first synced today is already old. created_at is
    // the fallback for rows without a usable session_date. pg_input_is_valid
    // (PG 16+) keeps a malformed date from throwing — metadata is client
    // supplied and unconstrained. CASE only evaluates the cast on the branch
    // it takes, so the guard is real.
    const expired = await pool.query(
        `UPDATE documents
         SET deleted_at = NOW()
         WHERE kind = 'conversation' AND deleted_at IS NULL
           AND COALESCE(
                   CASE WHEN pg_input_is_valid(metadata->>'session_date', 'date')
                        THEN (metadata->>'session_date')::date END,
                   created_at::date
               ) < (NOW() - INTERVAL '1 day' * $1)::date
         RETURNING namespace, slug, LENGTH(content) AS content_length`,
        [windows.softAfterDays]
    );

    // One usage update per namespace rather than per note — the counters are
    // a fire-and-forget upsert and a nightly batch can be hundreds of rows.
    // The rows above are retired regardless of what happens to their chunks,
    // so the count and the credit come from the UPDATE, and a chunk failure
    // is reported on its own.
    const usage = new Map();
    let chunkErrors = 0;
    for (const row of expired.rows) {
        const agg = usage.get(row.namespace) || { count: 0, bytes: 0 };
        agg.count++;
        agg.bytes += row.content_length || 0;
        usage.set(row.namespace, agg);
        try {
            await pool.query(
                'DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)',
                [row.namespace, row.slug]
            );
        } catch (err) {
            chunkErrors++;
            logCleanup('conversation-chunk-error', { namespace: row.namespace, slug: row.slug, error: err.message });
        }
    }
    for (const [namespace, agg] of usage) {
        updateUsage(namespace, -agg.count, -agg.bytes);
    }
    const softDeleted = expired.rowCount;

    // A tombstone keeps deleted_at (still deleted) and gains purged_at; the
    // purged_at guard keeps this from rewriting the same rows every night.
    const purged = await pool.query(
        `UPDATE documents
         SET content = '',
             metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object('purged_at', NOW())
         WHERE kind = 'conversation' AND deleted_at IS NOT NULL
           AND metadata->>'purged_at' IS NULL
           AND deleted_at < NOW() - INTERVAL '1 day' * $1
         RETURNING namespace, slug`,
        [windows.purgeAfterDays]
    );

    // Chunks normally went at soft-delete. Rows the old shell script retired
    // before LLM-642 never had that step, so sweep again here.
    for (const row of purged.rows) {
        try {
            await pool.query(
                'DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)',
                [row.namespace, row.slug]
            );
        } catch (err) {
            chunkErrors++;
            logCleanup('conversation-chunk-error', { namespace: row.namespace, slug: row.slug, error: err.message });
        }
    }

    const summary = { softDeleted, purged: purged.rowCount, chunkErrors, retentionDays: windows.retentionDays };
    if (softDeleted > 0 || purged.rowCount > 0 || chunkErrors > 0) {
        logCleanup('conversations-retired', summary);
    }
    return summary;
}

// Start the cleanup scheduler. Runs all cleanup tasks on the same cron schedule.
// Called once at server startup.
let scheduledTask = null;

function startCleanupScheduler() {
    const cron = require('node-cron');
    const schedule = config.get('cleanup_cron_schedule') || '';

    if (!schedule) {
        logCleanup('scheduler', { message: 'No cleanup_cron_schedule configured, scheduler disabled' });
        return;
    }

    if (!cron.validate(schedule)) {
        logCleanup('scheduler-error', { message: 'Invalid cron expression: ' + schedule });
        return;
    }

    if (scheduledTask) {
        scheduledTask.stop();
    }

    scheduledTask = cron.schedule(schedule, async () => {
        logCleanup('cron-trigger', { schedule });

        // Task 1: Decay cleanup (soft-delete old notes)
        try {
            const result = await runDecayCleanup();
            logCleanup('cron-decay-complete', { result });
        } catch (err) {
            logCleanup('cron-decay-error', { error: err.message });
            logError('cleanup', 'cron-decay-error', { message: err.message, detail: err.stack });
        }

        // Task 2: Purge old VA call logs (hard-delete)
        try {
            const purgeResult = await purgeCallLogs();
            if (purgeResult.purged > 0) {
                logCleanup('cron-purge-complete', { result: purgeResult });
            }
        } catch (err) {
            logCleanup('cron-purge-error', { error: err.message });
            logError('cleanup', 'cron-purge-error', { message: err.message, detail: err.stack });
        }

        // Task 3: Conversation retention (soft-delete, then tombstone)
        try {
            const result = await retireConversations();
            logCleanup('cron-conversations-complete', { result });
        } catch (err) {
            logCleanup('cron-conversations-error', { error: err.message });
            logError('cleanup', 'cron-conversations-error', { message: err.message, detail: err.stack });
        }
    });

    logCleanup('scheduler', { message: 'Cleanup scheduler started', schedule });
}

module.exports = { runDecayCleanup, purgeCallLogs, retireConversations, startCleanupScheduler, buildDecayConditions, conversationRetentionWindows };
