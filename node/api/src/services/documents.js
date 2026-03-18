// Service layer for document CRUD (save_note, list_notes, read_note, delete_note).
// Documents are stored in the documents table and auto-indexed into the vector DB.

const pool = require('../db');
const { ingestContent } = require('./memory');
const { resolveByName } = require('./actors');
const { handleError } = require('./error-handler');

function slugToKind(slug) {
    if (slug.startsWith('instructions/')) return 'instruction';
    if (slug.startsWith('notes/codebase/')) return 'reference';
    if (slug.startsWith('tasks/done/')) return 'task';
    if (slug.startsWith('tasks/')) return 'task';
    if (slug.startsWith('learnings/')) return 'learning';
    if (slug.startsWith('notes/')) return 'note';
    return 'note';
}

function titleToSlug(title) {
    return title
        .toLowerCase()
        .replace(/[^a-z0-9\s-]/g, '')
        .replace(/\s+/g, '-')
        .replace(/-+/g, '-')
        .replace(/^-|-$/g, '')
        .substring(0, 200);
}

async function saveNote(namespace, title, content, slug, createdBy) {
    if (!title || !content) {
        throw Object.assign(new Error('Required fields: title, content'), { statusCode: 400 });
    }

    const resolvedSlug = slug || titleToSlug(title);

    if (!resolvedSlug) {
        throw Object.assign(new Error('Could not generate slug from title'), { statusCode: 400 });
    }

    // Detect redundant namespace prefix in slug (e.g. slug="shared/ideas/foo" in namespace="shared")
    if (resolvedSlug.startsWith(namespace + '/')) {
        throw Object.assign(new Error(
            'Slug "' + resolvedSlug + '" starts with its own namespace "' + namespace + '/". ' +
            'The namespace is already implicit — use "' + resolvedSlug.slice(namespace.length + 1) + '" instead.'
        ), { statusCode: 400 });
    }

    // Check if the slug already exists (including soft-deleted rows)
    // Case-insensitive lookup — slugs are preserved as-is, only matching is lowered
    const existing = await pool.query(
        'SELECT id FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2)',
        [namespace, resolvedSlug]
    );

    // Resolve createdBy name to actor_id (only needed for inserts)
    let createdByActorId = null;
    if (createdBy) {
        const actor = await resolveByName(createdBy);
        if (actor) createdByActorId = actor.id;
    }

    const kind = slugToKind(resolvedSlug);

    let result;
    if (existing.rows.length > 0) {
        // Update existing row — also clears deleted_at if it was soft-deleted.
        // Don't overwrite created_by_actor_id on updates — preserve original author.
        result = await pool.query(`
            UPDATE documents
            SET title = $1, content = $2, deleted_at = NULL, updated_at = NOW(), kind = $5
            WHERE namespace = $3 AND LOWER(slug) = LOWER($4)
            RETURNING id, namespace, slug, title, created_by_actor_id, created_at, updated_at
        `, [title, content, namespace, resolvedSlug, kind]);
    } else {
        result = await pool.query(`
            INSERT INTO documents (namespace, slug, title, content, created_by_actor_id, kind)
            VALUES ($1, $2, $3, $4, $5, $6)
            RETURNING id, namespace, slug, title, created_by_actor_id, created_at, updated_at
        `, [namespace, resolvedSlug, title, content, createdByActorId, kind]);
    }

    const doc = result.rows[0];

    // Resolve created_by_actor_id to name for API response
    if (doc.created_by_actor_id) {
        const { resolveById } = require('./actors');
        const actor = await resolveById(doc.created_by_actor_id);
        doc.created_by = actor ? actor.name : null;
    } else {
        doc.created_by = null;
    }

    // Auto-index into vector DB (fire-and-forget — don't fail the save if indexing fails)
    ingestContent(namespace, resolvedSlug, content).catch(err => {
        handleError(null, 'documents', 'AUTO_INDEX_FAILED', {
            namespace, slug: resolvedSlug, error: err.message
        }).catch(() => {});
    });

    return doc;
}

async function listNotes(namespace, limit, offset, prefix) {
    const maxResults = limit || 50;
    const skip = offset || 0;

    let sql, params;
    if (prefix) {
        // Filter by slug prefix — like listing a directory (e.g., "tasks/pending/")
        sql = `
            SELECT d.id, d.slug, d.title,
                   LEFT(d.content, 200) AS snippet,
                   ac.name AS created_by, d.created_at, d.updated_at
            FROM documents d
            LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
            WHERE d.namespace = $1 AND LOWER(d.slug) LIKE LOWER($4) AND d.deleted_at IS NULL
            ORDER BY d.updated_at DESC, d.slug ASC
            LIMIT $2 OFFSET $3
        `;
        params = [namespace, maxResults, skip, prefix + '%'];
    } else {
        sql = `
            SELECT d.id, d.slug, d.title,
                   LEFT(d.content, 200) AS snippet,
                   ac.name AS created_by, d.created_at, d.updated_at
            FROM documents d
            LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
            WHERE d.namespace = $1 AND d.deleted_at IS NULL
            ORDER BY d.updated_at DESC
            LIMIT $2 OFFSET $3
        `;
        params = [namespace, maxResults, skip];
    }

    const result = await pool.query(sql, params);
    return { notes: result.rows };
}

async function readNote(namespace, slug) {
    const result = await pool.query(`
        SELECT d.id, d.namespace, d.slug, d.title, d.content,
               ac.name AS created_by, d.created_at, d.updated_at
        FROM documents d
        LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
        WHERE d.namespace = $1 AND LOWER(d.slug) = LOWER($2) AND d.deleted_at IS NULL
    `, [namespace, slug]);

    if (result.rows.length === 0) {
        throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
    }

    // Touch last_accessed for search decay/boost (fire-and-forget)
    pool.query(
        'UPDATE documents SET last_accessed = NOW() WHERE namespace = $1 AND LOWER(slug) = LOWER($2)',
        [namespace, slug]
    ).catch(err => {
        handleError(null, 'documents', 'LAST_ACCESSED_UPDATE_FAILED', {
            namespace, slug, error: err.message
        }).catch(() => {});
    });

    return result.rows[0];
}

async function deleteNote(namespace, slug) {
    // Soft delete — set deleted_at timestamp, keep vector chunks in place.
    // Chunks are filtered out of search results via a NOT EXISTS join.
    // Use restoreNote to undo.
    const result = await pool.query(`
        UPDATE documents
        SET deleted_at = NOW()
        WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL
        RETURNING id
    `, [namespace, slug]);

    if (result.rows.length === 0) {
        throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
    }

    return { deleted: true, slug };
}

async function restoreNote(namespace, slug) {
    // Clear the deleted_at flag to restore the note and its vector chunks
    const result = await pool.query(`
        UPDATE documents d
        SET deleted_at = NULL
        WHERE d.namespace = $1 AND LOWER(d.slug) = LOWER($2) AND d.deleted_at IS NOT NULL
        RETURNING d.id, d.namespace, d.slug, d.title
    `, [namespace, slug]);

    if (result.rows.length === 0) {
        throw Object.assign(new Error(`No deleted note found: ${slug}`), { statusCode: 404 });
    }

    return result.rows[0];
}

// Search-and-replace edit on a note's content — like the Edit tool in Claude Code.
// Finds old_string in the document and replaces it with new_string.
// By default, old_string must appear exactly once (prevents ambiguous edits).
// Set replace_all to true to replace every occurrence.
async function editNote(namespace, slug, oldString, newString, replaceAll) {
    if (!oldString || newString === undefined || newString === null) {
        throw Object.assign(new Error('Required fields: old_string, new_string'), { statusCode: 400 });
    }

    // Fetch the current document
    const doc = await readNote(namespace, slug);
    const content = doc.content;

    // Count occurrences to validate uniqueness
    let count = 0;
    let pos = 0;
    while ((pos = content.indexOf(oldString, pos)) !== -1) {
        count++;
        pos += oldString.length;
    }

    if (count === 0) {
        throw Object.assign(
            new Error('old_string not found in document'),
            { statusCode: 400 }
        );
    }

    if (count > 1 && !replaceAll) {
        throw Object.assign(
            new Error(`old_string appears ${count} times — use replace_all to replace all occurrences, or provide more context to make it unique`),
            { statusCode: 400 }
        );
    }

    // Perform the replacement (split/join is literal — unlike String.replace,
    // it won't interpret $ sequences in newString as special patterns)
    const updatedContent = content.split(oldString).join(newString);

    // Save the updated content
    const result = await pool.query(`
        UPDATE documents d
        SET content = $1, updated_at = NOW()
        WHERE d.namespace = $2 AND LOWER(d.slug) = LOWER($3)
        RETURNING d.id, d.namespace, d.slug, d.title, d.created_by_actor_id, d.created_at, d.updated_at
    `, [updatedContent, namespace, slug]);

    // Resolve created_by_actor_id to name
    if (result.rows[0] && result.rows[0].created_by_actor_id) {
        const { resolveById } = require('./actors');
        const actor = await resolveById(result.rows[0].created_by_actor_id);
        result.rows[0].created_by = actor ? actor.name : null;
    } else if (result.rows[0]) {
        result.rows[0].created_by = null;
    }

    // Re-index into vector DB (fire-and-forget)
    ingestContent(namespace, slug, updatedContent).catch(err => {
        handleError(null, 'documents', 'REINDEX_FAILED', {
            namespace, slug, error: err.message
        }).catch(() => {});
    });

    return {
        ...result.rows[0],
        replacements: replaceAll ? count : 1
    };
}

// Text search across notes — like grep but for the notes database.
// Returns matching documents with line numbers and surrounding context.
async function grepNotes(pattern, namespace, limit, readableNamespaces) {
    if (!pattern) {
        throw Object.assign(new Error('Required field: pattern'), { statusCode: 400 });
    }

    const maxResults = limit || 20;
    const ilikePattern = `%${pattern}%`;

    let sql, params;
    if (namespace && namespace !== '*') {
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE namespace = $1 AND deleted_at IS NULL AND (content ILIKE $2 OR title ILIKE $2)
            ORDER BY updated_at DESC
            LIMIT $3
        `;
        params = [namespace, ilikePattern, maxResults];
    } else if (readableNamespaces) {
        // Filter at query level to ensure LIMIT returns correct result count
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE namespace = ANY($1) AND deleted_at IS NULL AND (content ILIKE $2 OR title ILIKE $2)
            ORDER BY updated_at DESC
            LIMIT $3
        `;
        params = [readableNamespaces, ilikePattern, maxResults];
    } else {
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE deleted_at IS NULL AND (content ILIKE $1 OR title ILIKE $1)
            ORDER BY updated_at DESC
            LIMIT $2
        `;
        params = [ilikePattern, maxResults];
    }

    const result = await pool.query(sql, params);

    // Extract matching lines with context (±2 lines, like grep -C 2)
    const lowerPattern = pattern.toLowerCase();
    return result.rows.map(doc => {
        const lines = doc.content.split('\n');
        const matchLineNumbers = new Set();

        // Find lines that contain the pattern
        for (let i = 0; i < lines.length; i++) {
            if (lines[i].toLowerCase().includes(lowerPattern)) {
                matchLineNumbers.add(i);
            }
        }

        // Expand to include context lines (±2)
        const contextLineNumbers = new Set();
        for (const lineNum of matchLineNumbers) {
            for (let offset = -2; offset <= 2; offset++) {
                const idx = lineNum + offset;
                if (idx >= 0 && idx < lines.length) {
                    contextLineNumbers.add(idx);
                }
            }
        }

        // Build match groups (consecutive context lines grouped together)
        const sortedLines = Array.from(contextLineNumbers).sort((a, b) => a - b);
        const matches = sortedLines.map(idx => ({
            lineNumber: idx + 1,
            line: lines[idx],
            isMatch: matchLineNumbers.has(idx)
        }));

        return {
            namespace: doc.namespace,
            slug: doc.slug,
            title: doc.title,
            matchCount: matchLineNumbers.size,
            matches
        };
    });
}

// Move/rename a note by changing its slug (and optionally namespace).
// Updates both the document row and any associated vector chunks.
async function moveNote(namespace, slug, newSlug, newNamespace) {
    const targetNamespace = newNamespace || namespace;

    // Verify source exists and isn't deleted
    const source = await pool.query(
        'SELECT id, title FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL',
        [namespace, slug]
    );
    if (source.rows.length === 0) {
        throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
    }

    // Check target slug isn't already taken (including soft-deleted — unique constraint covers both)
    const conflict = await pool.query(
        'SELECT id FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2)',
        [targetNamespace, newSlug]
    );
    if (conflict.rows.length > 0) {
        throw Object.assign(
            new Error(`Target slug already exists: ${targetNamespace}/${newSlug}`),
            { statusCode: 409 }
        );
    }

    // Update the document (recalculate kind from new slug)
    const newKind = slugToKind(newSlug);
    const result = await pool.query(`
        UPDATE documents
        SET slug = $1, namespace = $2, updated_at = NOW(), kind = $5
        WHERE namespace = $3 AND LOWER(slug) = LOWER($4)
        RETURNING id, namespace, slug, title, created_by_actor_id, created_at, updated_at
    `, [newSlug, targetNamespace, namespace, slug, newKind]);

    // Update vector chunks to match the new slug/namespace
    await pool.query(
        'UPDATE memory_chunks SET source_file = $1, namespace = $2 WHERE namespace = $3 AND LOWER(source_file) = LOWER($4)',
        [newSlug, targetNamespace, namespace, slug]
    );

    const doc = result.rows[0];

    // Resolve created_by_actor_id to name for API response
    if (doc.created_by_actor_id) {
        const { resolveById } = require('./actors');
        const actor = await resolveById(doc.created_by_actor_id);
        doc.created_by = actor ? actor.name : null;
    } else {
        doc.created_by = null;
    }

    return doc;
}

module.exports = { saveNote, listNotes, readNote, deleteNote, restoreNote, editNote, grepNotes, moveNote, titleToSlug };
