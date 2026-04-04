// Clustering service — runs HDBSCAN on each agent's visible embeddings.
// Called by the internal cron scheduler. Uses the existing permission system
// to determine which embeddings each agent can see, then shells out to
// a Python script for the actual clustering math.

const { execFile } = require('child_process');
const path = require('path');
const crypto = require('crypto');
const pool = require('../db');
const config = require('./config');
const { log } = require('./logger');
const { getReadableNamespaces } = require('./namespace-permissions');

// Path to the Python clustering script.
// On VPS: /var/www/memory-api/scripts/clustering/cluster.py (sibling of src/)
// Local dev: C:\dev\llm-memory-api\scripts\clustering\cluster.py (repo root)
// We check the deployment path first (src/../scripts), then fall back to repo root.
const fs = require('fs');
const PROJECT_ROOT = path.resolve(__dirname, '..', '..');
let CLUSTER_SCRIPT = path.join(PROJECT_ROOT, 'scripts', 'clustering', 'cluster.py');
if (!fs.existsSync(CLUSTER_SCRIPT)) {
    // Local dev: scripts/ is at the repo root, not inside node/api/
    CLUSTER_SCRIPT = path.resolve(PROJECT_ROOT, '..', '..', 'scripts', 'clustering', 'cluster.py');
}

// Python binary — defaults to the venv created by Ansible.
// Configurable via clustering_python_bin for local dev or custom setups.
function getPythonBin() {
    const configured = config.get('clustering_python_bin');
    if (configured) return configured;

    // Default: venv in the scripts/clustering directory (deployed by Ansible)
    const venvPython = path.resolve(CLUSTER_SCRIPT, '..', '.venv', 'bin', 'python');
    return venvPython;
}

function logCluster(action, details) {
    log('clustering', action, details);
}

// Get all non-virtual, active agents
async function getClusterableAgents() {
    const result = await pool.query(`
        SELECT a.id, a.name
        FROM actors a
        JOIN agent_configuration agc ON agc.actor_id = a.id
        WHERE a.status = 'active'
          AND agc.virtual = false
        ORDER BY a.name
    `);
    return result.rows;
}

// Gather all embeddings visible to an agent.
// Uses namespace_permissions for namespace-level access, plus
// note_permissions for individual note grants.
// Returns one embedding per note (the first chunk — representative enough
// for clustering purposes without duplicating notes across chunks).
async function getVisibleEmbeddings(actorId, actorName) {
    // Get readable namespaces (null = wildcard, all namespaces)
    const readable = await getReadableNamespaces(actorId, actorName, 'agent');

    let query;
    let params;

    if (readable === null) {
        // Wildcard access — all namespaces
        // Get one embedding per note (the first chunk by ingested_at)
        query = `
            SELECT DISTINCT ON (mc.namespace, mc.source_file)
                mc.namespace,
                mc.source_file AS slug,
                mc.embedding::text
            FROM memory_chunks mc
            JOIN documents d ON d.namespace = mc.namespace
                AND LOWER(d.slug) = LOWER(mc.source_file)
                AND d.deleted_at IS NULL
            WHERE mc.embedding IS NOT NULL
            ORDER BY mc.namespace, mc.source_file, mc.ingested_at
        `;
        params = [];
    } else {
        // Filtered access — own namespace + granted namespaces + note_permissions
        query = `
            SELECT DISTINCT ON (mc.namespace, mc.source_file)
                mc.namespace,
                mc.source_file AS slug,
                mc.embedding::text
            FROM memory_chunks mc
            JOIN documents d ON d.namespace = mc.namespace
                AND LOWER(d.slug) = LOWER(mc.source_file)
                AND d.deleted_at IS NULL
            WHERE mc.embedding IS NOT NULL
              AND (
                  mc.namespace = ANY($1)
                  OR EXISTS (
                      SELECT 1 FROM note_permissions np
                      WHERE np.owner_namespace = mc.namespace
                        AND (np.slug_pattern = mc.source_file
                             OR mc.source_file LIKE np.slug_pattern || '%')
                        AND (np.grantee_actor_id = $2 OR np.grantee_actor_id IS NULL)
                        AND np.revoked_at IS NULL
                        AND np.can_read = true
                  )
              )
            ORDER BY mc.namespace, mc.source_file, mc.ingested_at
        `;
        params = [readable, actorId];
    }

    const result = await pool.query(query, params);

    // Parse the pgvector text representation back to float arrays
    return result.rows.map(row => ({
        namespace: row.namespace,
        slug: row.slug,
        vector: parseVector(row.embedding)
    }));
}

// Parse pgvector text format "[0.1,0.2,...]" to a JS array of floats
function parseVector(text) {
    return text.slice(1, -1).split(',').map(Number);
}

// Shell out to the Python HDBSCAN script.
// Sends embeddings as JSON on stdin, reads cluster assignments from stdout.
function runHdbscan(embeddings, minClusterSize) {
    return new Promise((resolve, reject) => {
        const input = JSON.stringify({
            embeddings: embeddings,
            min_cluster_size: minClusterSize
        });

        const pythonBin = getPythonBin();
        const proc = execFile(pythonBin, [CLUSTER_SCRIPT], {
            maxBuffer: 50 * 1024 * 1024, // 50MB — embeddings can be large
            timeout: 120000              // 2 minutes
        }, (error, stdout, stderr) => {
            if (error) {
                reject(new Error('HDBSCAN failed: ' + (stderr || error.message)));
                return;
            }
            try {
                resolve(JSON.parse(stdout));
            } catch (e) {
                reject(new Error('HDBSCAN output parse error: ' + e.message + ' (stdout: ' + stdout.substring(0, 200) + ')'));
            }
        });

        // Write embeddings to stdin
        proc.stdin.write(input);
        proc.stdin.end();
    });
}

// Write cluster results to the note_clusters table.
// Replaces any existing results for this agent.
async function writeClusters(actorId, runId, results) {
    const client = await pool.connect();
    try {
        await client.query('BEGIN');

        // Delete old results for this agent
        await client.query('DELETE FROM note_clusters WHERE actor_id = $1', [actorId]);

        // Batch insert new results (500 rows per INSERT)
        if (results.clusters && results.clusters.length > 0) {
            const batchSize = 500;
            for (let i = 0; i < results.clusters.length; i += batchSize) {
                const batch = results.clusters.slice(i, i + batchSize);
                const values = [];
                const params = [];
                let idx = 1;

                for (const cluster of batch) {
                    const label = results.labels
                        ? (results.labels[String(cluster.cluster_id)] || null)
                        : null;
                    values.push(`($${idx}, $${idx + 1}, $${idx + 2}, $${idx + 3}, $${idx + 4}, $${idx + 5})`);
                    params.push(actorId, cluster.namespace, cluster.slug, cluster.cluster_id, label, runId);
                    idx += 6;
                }

                await client.query(
                    `INSERT INTO note_clusters (actor_id, namespace, slug, cluster_id, cluster_label, run_id)
                     VALUES ${values.join(', ')}`,
                    params
                );
            }
        }

        await client.query('COMMIT');
    } catch (e) {
        await client.query('ROLLBACK');
        throw e;
    } finally {
        client.release();
    }
}

// Main entry point — cluster all agents.
// Called by the cron scheduler.
async function runClustering() {
    let enabled;
    try {
        enabled = config.get('clustering_enabled');
    } catch (e) {
        logCluster('skip', { reason: 'clustering config keys not found (pre-migration)' });
        return;
    }
    if (enabled !== 'true') {
        logCluster('skip', { reason: 'clustering_enabled is not true' });
        return;
    }

    const minClusterSize = parseInt(config.get('clustering_min_cluster_size') || '5', 10);
    const agents = await getClusterableAgents();

    logCluster('start', { agents: agents.length, min_cluster_size: minClusterSize });

    let succeeded = 0;
    let failed = 0;

    for (const agent of agents) {
        const runId = agent.name + '-' + crypto.randomUUID().substring(0, 8);
        try {
            // Gather visible embeddings
            const embeddings = await getVisibleEmbeddings(agent.id, agent.name);
            logCluster('agent-start', {
                agent: agent.name,
                embeddings: embeddings.length,
                run_id: runId
            });

            if (embeddings.length === 0) {
                logCluster('agent-skip', { agent: agent.name, reason: 'no embeddings' });
                continue;
            }

            // Run HDBSCAN
            const results = await runHdbscan(embeddings, minClusterSize);

            logCluster('agent-clustered', {
                agent: agent.name,
                run_id: runId,
                stats: results.stats,
                labels: results.labels
            });

            // Write to database
            await writeClusters(agent.id, runId, results);
            succeeded++;

        } catch (e) {
            logCluster('agent-error', {
                agent: agent.name,
                run_id: runId,
                error: e.message
            });
            failed++;
        }
    }

    logCluster('complete', { succeeded, failed, total: agents.length });
}

// Start the clustering scheduler. Reads clustering_cron_schedule from config
// and schedules runClustering() accordingly. Called once at server startup.
let scheduledTask = null;

function startClusteringScheduler() {
    const cron = require('node-cron');
    let schedule;
    try {
        schedule = config.get('clustering_cron_schedule') || '';
    } catch (e) {
        // Config key doesn't exist yet (pre-migration) — skip silently
        logCluster('scheduler', { message: 'Config key clustering_cron_schedule not found, scheduler disabled' });
        return;
    }

    if (!schedule) {
        logCluster('scheduler', { message: 'No clustering_cron_schedule configured, scheduler disabled' });
        return;
    }

    if (!cron.validate(schedule)) {
        logCluster('scheduler-error', { message: 'Invalid cron expression: ' + schedule });
        return;
    }

    // Stop any existing scheduled task (in case of hot reload)
    if (scheduledTask) {
        scheduledTask.stop();
    }

    scheduledTask = cron.schedule(schedule, async () => {
        logCluster('cron-trigger', { schedule });
        try {
            await runClustering();
            logCluster('cron-complete', {});
        } catch (err) {
            logCluster('cron-error', { error: err.message });
            const { logError } = require('./error-handler');
            logError('clustering', 'cron-error', { message: err.message, detail: err.stack });
        }
    });

    logCluster('scheduler', { message: 'Clustering scheduler started', schedule });
}

module.exports = { runClustering, startClusteringScheduler };
