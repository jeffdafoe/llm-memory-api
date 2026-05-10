// Provider-side coercion helpers.
//
// Lives in its own module to avoid the circular require chain that would
// happen if these were defined in providers/index.js (which loads each
// provider module by name; provider modules can't require index.js back).

// asNumber coerces a config value to a finite number. Returns undefined
// for anything that isn't (null, '', undefined, NaN, Infinity, non-numeric
// strings). Provider modules use this on body fields whose underlying API
// requires numeric types — Anthropic, OpenAI, Google, etc. all reject
// string-typed temperature even though OpenRouter/llama happens to coerce
// on its end. Storage stays whatever the user typed (the admin form ends
// up with string-typed numeric inputs because pg returns numeric columns
// as strings, and the admin save round-trips them as-is). Each provider
// enforces its own type contract at send time.
function asNumber(v) {
    if (v === null || v === undefined || v === '') return undefined;
    const n = Number(v);
    return Number.isFinite(n) ? n : undefined;
}

module.exports = { asNumber };
