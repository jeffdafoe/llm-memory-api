// Filesystem-safe slug builder for context/people/* notes.
//
// Display names flow in from multiple sources — engine perceptions in
// sim mode, future player names, mail/chat sender slugs in companion
// mode — anything that isn't a fully trusted internal slug. So we
// whitelist [a-z0-9-] and reject anything that empties out, instead of
// just whitespace-to-hyphen which would let a name like "../secrets" or
// "foo/bar" build a path that traverses out of context/people/.
//
// Diacritic stripping via NFKD normalize + combining-mark removal so
// "Renee" reads as "renee" rather than getting silently flattened to
// nothing. Non-Latin scripts still won't slug well; if/when those
// matter, switch to an explicit stored slug field rather than deriving
// filesystem paths from display names.
//
// Returns null if the input is unusable (empty, non-string, no surviving
// characters), so callers can skip the read entirely.
//
// Lives in its own module so virtual-agent.js (write-time loader) and
// dream.js (write-time updater) share one implementation. Path-traversal
// defense in two places will inevitably drift.
function personContextSlug(name) {
    if (!name || typeof name !== 'string') return null;
    const slug = name
        .trim()
        .normalize('NFKD')
        .replace(/[̀-ͯ]/g, '')
        .toLowerCase()
        .replace(/\s+/g, '-')
        .replace(/[^a-z0-9-]/g, '')
        .replace(/-+/g, '-')
        .replace(/^-|-$/g, '');
    return slug || null;
}

module.exports = { personContextSlug };
