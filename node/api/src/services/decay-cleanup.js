// Decay cleanup — soft-deletes notes whose decay factor has dropped below the
// configured threshold. Runs on a cron schedule (after dream processing).
//
// Uses the same decay formula as search: 0.5 ^ (age_days / half_life), where
// age is based on the most recent of created_at, updated_at, or last_accessed.
// Notes that keep getting accessed stay fresh; forgotten ones eventually get
// cleaned up.
//
// Only targets note kinds/cognitive types that have a non-zero half-life.
// Instructions, references, and other non-decaying kinds are never touched.

const pool = require('../db');
const config = require('./config');
const { log, logError } = require('./logger');

function logCleanup(action, details) {
    log('decay-cleanup', action, details);
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
        task: parseFloat(config.get('search_decay_halflife_task')) || 0,
        learning: parseFloat(config.get('search_decay_halflife_learning')) || 0,
        note: parseFloat(config.get('search_decay_halflife_note')) || 0,
        conversation: parseFloat(config.get('search_decay_halflife_conversation')) || 0,
        dream: parseFloat(config.get('search_decay_halflife_dream')) || 0,
    };

    // Cognitive type half-lives (override kind when set)
    const cognitiveHalfLives = {
        episodic: parseFloat(config.get('search_decay_halflife_episodic')) || 90,
        reflective: parseFloat(config.get('search_decay_halflife_reflective')) || 180,
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

async function runDecayCleanup() {
    if (config.get('decay_cleanup_enabled') !== 'true') {
        logCleanup('skip', { reason: 'decay_cleanup_enabled is false' });
        return { skipped: true, reason: 'disabled' };
    }

    const threshold = parseFloat(config.get('search_decay_cleanup_threshold')) || 0.05;
    const { conditions, params } = buildDecayConditions(threshold);

    if (conditions.length === 0) {
        logCleanup('skip', { reason: 'No decaying note types configured' });
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
        logCleanup('complete', { deleted: 0, threshold });
        return { deleted: 0, threshold };
    }

    logCleanup('found', { count: result.rows.length, threshold });

    // Soft-delete each note and hard-delete its vector chunks
    let deleted = 0;
    for (const row of result.rows) {
        try {
            await pool.query(
                'UPDATE documents SET deleted_at = NOW() WHERE id = $1',
                [row.id]
            );

            // Hard-delete vector chunks so they can't appear in search results
            await pool.query(
                'DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)',
                [row.namespace, row.slug]
            );

            logCleanup('deleted', {
                namespace: row.namespace,
                slug: row.slug,
                kind: row.kind,
                cognitiveType: row.cognitive_type,
                created: row.created_at,
                lastAccessed: row.last_accessed
            });
            deleted++;
        } catch (err) {
            logCleanup('delete-error', {
                namespace: row.namespace,
                slug: row.slug,
                error: err.message
            });
        }
    }

    logCleanup('complete', { deleted, total: result.rows.length, threshold });
    return { deleted, threshold };
}

// Start the decay cleanup scheduler. Reads config and schedules runDecayCleanup().
// Called once at server startup.
let scheduledTask = null;

function startDecayCleanupScheduler() {
    const cron = require('node-cron');
    const schedule = config.get('decay_cleanup_cron_schedule') || '';

    if (!schedule) {
        logCleanup('scheduler', { message: 'No decay_cleanup_cron_schedule configured, scheduler disabled' });
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
        try {
            const result = await runDecayCleanup();
            logCleanup('cron-complete', { result });
        } catch (err) {
            logCleanup('cron-error', { error: err.message });
            logError('decay-cleanup', 'cron-error', { message: err.message, detail: err.stack });
        }
    });

    logCleanup('scheduler', { message: 'Decay cleanup scheduler started', schedule });
}

module.exports = { runDecayCleanup, startDecayCleanupScheduler };
