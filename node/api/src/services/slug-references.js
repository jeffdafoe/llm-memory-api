// Slug reference extraction — finds mentions of other notes' slugs in note content.
// Purely mechanical regex matching, no LLM. Runs fire-and-forget on save/edit.
// Results stored in slug_references table for graph visualization.

const pool = require('../db');

// Extract namespace/slug references from note content.
// Looks for patterns like "home/notes/foo" or bare "tasks/pending/bar"
// that match known namespaces.
async function extractSlugReferences(content, sourceNamespace) {
    // Get known namespaces to anchor the regex
    const nsResult = await pool.query(
        'SELECT DISTINCT namespace FROM documents WHERE deleted_at IS NULL'
    );
    const knownNamespaces = nsResult.rows.map(r => r.namespace);
    if (knownNamespaces.length === 0) return [];

    // Build regex: namespace/path where namespace is one of the known ones
    // Matches: work/tasks/dev-2329, shared/GUIDELINES, home/notes/active-work
    const nsPattern = knownNamespaces
        .map(ns => ns.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
        .join('|');
    const regex = new RegExp(
        '(?:^|[\\s`(\\["\'])(' + nsPattern + ')/([a-zA-Z0-9][a-zA-Z0-9/_-]*[a-zA-Z0-9])',
        'gm'
    );

    const refs = [];
    const seen = new Set();
    let match;
    while ((match = regex.exec(content)) !== null) {
        const ns = match[1];
        const slug = match[2];
        const key = (ns + '/' + slug).toLowerCase();
        if (!seen.has(key)) {
            seen.add(key);
            refs.push({ namespace: ns, slug });
        }
    }

    // Also match bare slug references (no namespace prefix) — assume sourceNamespace
    const bareRegex = /(?:^|[\s`(\["'])((?:tasks|notes|instructions|dreams|learnings|ideas)\/[a-zA-Z0-9][a-zA-Z0-9/_-]*[a-zA-Z0-9])/gm;
    while ((match = bareRegex.exec(content)) !== null) {
        const slug = match[1];
        const key = (sourceNamespace + '/' + slug).toLowerCase();
        if (!seen.has(key)) {
            seen.add(key);
            refs.push({ namespace: sourceNamespace, slug });
        }
    }

    return refs;
}

// Run extraction and upsert references for a note.
// Called fire-and-forget from saveNote and editNote.
async function updateSlugReferences(sourceNamespace, sourceSlug, content) {
    // Delete existing references from this source
    await pool.query(
        'DELETE FROM slug_references WHERE source_namespace = $1 AND LOWER(source_slug) = LOWER($2)',
        [sourceNamespace, sourceSlug]
    );

    const refs = await extractSlugReferences(content, sourceNamespace);

    for (const ref of refs) {
        // Skip self-references
        if (ref.namespace === sourceNamespace && ref.slug.toLowerCase() === sourceSlug.toLowerCase()) {
            continue;
        }
        await pool.query(`
            INSERT INTO slug_references (source_namespace, source_slug, target_namespace, target_slug)
            VALUES ($1, $2, $3, $4)
            ON CONFLICT (source_namespace, source_slug, target_namespace, target_slug) DO NOTHING
        `, [sourceNamespace, sourceSlug, ref.namespace, ref.slug]);
    }
}

// Delete all references involving a note (called on note delete).
async function deleteSlugReferences(namespace, slug) {
    await pool.query(`
        DELETE FROM slug_references
        WHERE (source_namespace = $1 AND LOWER(source_slug) = LOWER($2))
           OR (target_namespace = $1 AND LOWER(target_slug) = LOWER($2))
    `, [namespace, slug]);
}

// Update references when a note is moved/renamed.
async function updateSlugReferencesForMove(oldNs, oldSlug, newNs, newSlug) {
    await pool.query(
        'UPDATE slug_references SET source_namespace = $3, source_slug = $4 WHERE source_namespace = $1 AND LOWER(source_slug) = LOWER($2)',
        [oldNs, oldSlug, newNs, newSlug]
    );
    await pool.query(
        'UPDATE slug_references SET target_namespace = $3, target_slug = $4 WHERE target_namespace = $1 AND LOWER(target_slug) = LOWER($2)',
        [oldNs, oldSlug, newNs, newSlug]
    );
}

module.exports = {
    updateSlugReferences,
    deleteSlugReferences,
    updateSlugReferencesForMove,
};
