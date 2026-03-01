// Service layer for document CRUD (save_note, list_notes, read_note, delete_note).
// Documents are stored in the documents table and auto-indexed into the vector DB.

const pool = require('../db');
const { ingestContent, deleteMemory } = require('./memory');

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

    const result = await pool.query(`
        INSERT INTO documents (namespace, slug, title, content, created_by)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (namespace, slug) DO UPDATE
        SET title = EXCLUDED.title,
            content = EXCLUDED.content,
            updated_at = NOW()
        RETURNING id, namespace, slug, title, created_by, created_at, updated_at
    `, [namespace, resolvedSlug, title, content, createdBy || null]);

    const doc = result.rows[0];

    // Auto-index into vector DB (fire-and-forget — don't fail the save if indexing fails)
    ingestContent(namespace, resolvedSlug, content).catch(err => {
        console.error(`Document auto-index failed for ${namespace}/${resolvedSlug}:`, err.message);
    });

    return doc;
}

async function listNotes(namespace, limit, offset) {
    const maxResults = limit || 50;
    const skip = offset || 0;

    const result = await pool.query(`
        SELECT id, slug, title,
               LEFT(content, 200) AS snippet,
               created_by, created_at, updated_at
        FROM documents
        WHERE namespace = $1
        ORDER BY updated_at DESC
        LIMIT $2 OFFSET $3
    `, [namespace, maxResults, skip]);

    return { notes: result.rows };
}

async function readNote(namespace, slug) {
    const result = await pool.query(`
        SELECT id, namespace, slug, title, content, created_by, created_at, updated_at
        FROM documents
        WHERE namespace = $1 AND slug = $2
    `, [namespace, slug]);

    if (result.rows.length === 0) {
        throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
    }

    return result.rows[0];
}

async function deleteNote(namespace, slug) {
    const result = await pool.query(`
        DELETE FROM documents
        WHERE namespace = $1 AND slug = $2
        RETURNING id
    `, [namespace, slug]);

    if (result.rows.length === 0) {
        throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
    }

    // Clean up vector chunks too (fire-and-forget)
    deleteMemory(namespace, slug).catch(err => {
        console.error(`Document vector cleanup failed for ${namespace}/${slug}:`, err.message);
    });

    return { deleted: true, slug };
}

// Text search across notes — like grep but for the notes database.
// Returns matching documents with line numbers and surrounding context.
async function grepNotes(pattern, namespace, limit) {
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
            WHERE namespace = $1 AND (content ILIKE $2 OR title ILIKE $2)
            ORDER BY updated_at DESC
            LIMIT $3
        `;
        params = [namespace, ilikePattern, maxResults];
    } else {
        sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE content ILIKE $1 OR title ILIKE $1
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

module.exports = { saveNote, listNotes, readNote, deleteNote, grepNotes, titleToSlug };
