// sanitize.js — Input sanitization helpers.
// Shared across routes, middleware, and services.
//
// Three tiers:
//   agentName()  — identity fields (agent names, usernames). ASCII whitelist.
//   identifier() — structural fields (slugs, filenames, channels, provider/model).
//                   ASCII + common punctuation whitelist.
//   content()    — free-text fields (note content, mail body, chat messages,
//                   discussion topics, etc.). Allows full Unicode but strips
//                   dangerous invisible/control characters.
//
// See shared/notes/codebase/llm-memory-api/input-sanitization for the full guide.

// Strip C0 control chars (U+0000-U+001F except tab/newline/CR),
// C1 control chars (U+0080-U+009F), DEL (U+007F),
// zero-width chars (U+200B, U+200C, U+200D, U+FEFF),
// and bidi override chars (U+202A-U+202E, U+2066-U+2069).
// Preserves all normal Unicode (emoji, CJK, Cyrillic, Arabic, etc.).
const DANGEROUS_CHARS = /[\x00-\x08\x0B\x0C\x0E-\x1F\x7F\x80-\x9F\u200B\u200C\u200D\uFEFF\u202A-\u202E\u2066-\u2069]/g;

// Normalize an agent name: trim, lowercase, strip everything except
// ASCII alphanumeric, hyphen, and underscore.  Whitelist-based so it
// catches XSS, injection, Unicode homoglyphs, zero-width chars, etc.
function agentName(name) {
    if (typeof name !== 'string') return name;
    return name.trim().toLowerCase().replace(/[^a-z0-9_-]/g, '');
}

// Sanitize structural identifiers: slugs, filenames, channels, provider/model.
// Allows ASCII alphanumeric, hyphens, underscores, dots, slashes, and spaces.
// Strips everything else. Does not lowercase (slugs are case-sensitive).
function identifier(value) {
    if (typeof value !== 'string') return value;
    return value.trim().replace(/[^a-zA-Z0-9_\-./\s]/g, '');
}

// Sanitize free-text content: note bodies, mail, chat messages, discussion
// topics, vote questions, expertise tags, etc.
// Allows full Unicode (any language, emoji) but strips dangerous invisible
// and control characters that can break display, logging, or string comparison.
// Preserves tabs, newlines, and carriage returns (needed for markdown/code).
function content(value) {
    if (typeof value !== 'string') return value;
    return value.replace(DANGEROUS_CHARS, '');
}

// Parse a value as a positive integer, rejecting junk like "3abc" that
// parseInt would accept. Returns the integer if valid, or null if not.
// Optional min/max bounds (inclusive).
function positiveInt(value, min, max) {
    if (value === undefined || value === null) return null;
    var str = String(value).trim();
    if (!/^\d+$/.test(str)) return null;
    var n = Number(str);
    if (min !== undefined && n < min) return null;
    if (max !== undefined && n > max) return null;
    return n;
}

module.exports = { agentName, identifier, content, positiveInt };
