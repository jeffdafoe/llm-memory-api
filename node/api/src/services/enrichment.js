// Note enrichment — classifies each saved note into a cognitive type
// (semantic/episodic/procedural/reflective), which drives per-type decay
// half-lives in search ranking. See memory.js for how cognitive_type is
// consumed; see migrations MEM-106 for the half-life config keys.
//
// Prior versions also produced keywords, tags, and LLM-suggested relations
// — those were removed in MEM-116 after the relation graph was found to
// amplify circular LLM judgments rather than real structure.

const pool = require('../db');
const config = require('./config');
const { log } = require('./logger');

const ENRICHMENT_AGENT = 'memory-enrichment';

// Kinds that aren't worth the LLM cost to enrich.
const SKIP_KINDS = new Set(['conversation', 'context', 'dream']);

// Prevents re-enrichment on rapid successive saves.
const DEBOUNCE_MS = 60000;

function kindFromSlug(slug) {
    if (slug.startsWith('instructions/')) return 'instruction';
    if (slug.startsWith('notes/codebase/')) return 'reference';
    if (slug.startsWith('conversations/')) return 'conversation';
    if (slug.startsWith('context/')) return 'context';
    if (slug.startsWith('dreams/')) return 'dream';
    if (slug.startsWith('tasks/')) return 'task';
    if (slug.startsWith('learnings/')) return 'learning';
    if (slug.startsWith('notes/')) return 'note';
    return 'note';
}

function logEnrich(action, details) {
    log('enrichment', action, details);
}

function buildUserMessage(namespace, slug, title, content) {
    var truncatedContent = content.length > 4000
        ? content.substring(0, 4000) + '\n\n[... truncated]'
        : content;

    return '## Note to classify\n\n'
        + '**Namespace:** ' + namespace + '\n'
        + '**Slug:** ' + slug + '\n'
        + '**Title:** ' + title + '\n\n'
        + truncatedContent
        + '\n\nClassify this note and return a JSON object with a single field:\n'
        + '- `cognitive_type`: one of "semantic" (facts, definitions, knowledge), '
        + '"episodic" (events, things that happened), '
        + '"procedural" (decisions, conventions, how-tos, institutional knowledge), '
        + 'or "reflective" (synthesized insights, lessons learned, analysis)\n\n'
        + 'Return ONLY valid JSON, no markdown fences, no explanation.';
}

// Parse and validate the enrichment response. Returns cognitive_type string or null.
function parseResponse(text) {
    if (!text) return null;

    var cleaned = text.trim();
    if (cleaned.startsWith('```')) {
        cleaned = cleaned.replace(/^```(?:json)?\s*\n?/, '').replace(/\n?```\s*$/, '');
    }

    var parsed;
    try {
        parsed = JSON.parse(cleaned);
    } catch (e) {
        return null;
    }

    if (!parsed || typeof parsed !== 'object') return null;

    var VALID_COGNITIVE_TYPES = ['semantic', 'episodic', 'procedural', 'reflective'];
    if (typeof parsed.cognitive_type !== 'string') return null;
    var normalized = parsed.cognitive_type.trim().toLowerCase();
    if (!VALID_COGNITIVE_TYPES.includes(normalized)) return null;
    return normalized;
}

// Main enrichment function. Called fire-and-forget from saveNote.
async function enrichNote(namespace, slug, title, content, existingMetadata) {
    try {
        if (config.get('note_enrichment_enabled') !== 'true') return;
    } catch (e) {
        return;
    }

    var kind = kindFromSlug(slug);
    if (SKIP_KINDS.has(kind)) return;

    var meta = existingMetadata || {};
    if (meta._enriched_at) {
        var lastEnriched = new Date(meta._enriched_at).getTime();
        if (Date.now() - lastEnriched < DEBOUNCE_MS) return;
    }

    var { loadAgent, invokeAgent } = require('./virtual-agent');
    var agent = await loadAgent(ENRICHMENT_AGENT);
    if (!agent) return;
    if (!agent.provider || !agent.model || !agent.api_key) return;

    logEnrich('start', { namespace, slug, kind });

    var userMessage = buildUserMessage(namespace, slug, title, content);
    var response;
    try {
        response = await invokeAgent(ENRICHMENT_AGENT, {
            userMessage: userMessage,
            skipRateLimit: true,
            skipCostLimit: false,
            skipRetry: true,
            context: 'note-enrichment'
        });
    } catch (e) {
        logEnrich('agent-error', { namespace, slug, error: e.message });
        return;
    }

    var cognitiveType = parseResponse(response.text);
    if (!cognitiveType) {
        logEnrich('parse-failed', { namespace, slug, responsePreview: (response.text || '').substring(0, 200) });
        return;
    }

    // Merge cognitive_type + _enriched_at into metadata in SQL so we don't
    // overwrite any concurrent metadata updates that happened between the
    // original save and this fire-and-forget completion. JSONB || overlays
    // only the two keys we're setting; all other fields survive.
    try {
        await pool.query(
            `UPDATE documents
             SET metadata = COALESCE(metadata, '{}'::jsonb)
                 || jsonb_build_object('cognitive_type', $1::text, '_enriched_at', $2::text)
             WHERE namespace = $3 AND LOWER(slug) = LOWER($4) AND deleted_at IS NULL`,
            [cognitiveType, new Date().toISOString(), namespace, slug]
        );
    } catch (e) {
        logEnrich('metadata-write-failed', { namespace, slug, error: e.message });
        return;
    }

    logEnrich('complete', {
        namespace, slug,
        cognitiveType: cognitiveType,
        cost: response.cost ? response.cost.toFixed(6) : 'unknown'
    });
}

module.exports = { enrichNote };
