// util.js — shared safe-coercion helpers for backend route handlers and
// services. CJS twin of node/api/public/admin/util.js — keep the two in
// sync if you change one (the frontend ESM module can't be require()'d
// from CJS, so duplicating the helper is the simplest split).
//
// Use these any time you coerce values that came from outside our
// control: req.body / req.query, agent-supplied JSON, third-party API
// payloads. Past code-review feedback has repeatedly flagged places
// where parseInt and `|| ''` silently accepted malformed input —
// '4abc' rendering as '4', or an object rendering as '[object Object]'.
//
// Helpers:
//
//   safeInt(v) -> integer | null
//     Strict integer parsing. Returns null for anything that isn't a
//     clean integer value: NaN, Infinity, '4abc' (parseInt would have
//     accepted '4'), '4.9' (parseInt would have truncated to 4),
//     undefined, null, objects, booleans. Caller picks the fallback:
//
//       const id = safeInt(req.body.actor_id);
//       if (id === null || id <= 0) return res.status(400)...;
//
//       const limit = safeInt(req.body.limit) ?? 50;  // default 50
//
//   stringOrEmpty(v) -> string
//     Returns v if it's a string, otherwise ''. Use this in place of
//     `v || ''` when v could come from an external source — `|| ''`
//     stringifies arrays/objects via JS coercion and surfaces ugly
//     output like '[object Object]' or 'a,b,c'.

// Strict: requires a finite integer. Rejects partial-numeric strings
// ('4abc'), non-integer numerics ('4.9', 4.9), and non-numeric values.
// Number('') is 0, so the leading typeof check rejects '' and undefined
// before they sneak through as zero.
function safeInt(v) {
    if (typeof v !== 'number' && typeof v !== 'string') return null;
    if (typeof v === 'string' && v.trim() === '') return null;
    const n = Number(v);
    // isSafeInteger covers finite + integer + within +/-(2^53 - 1).
    // The safe-integer bound matters because IDs / quotas pulled from the
    // wire can exceed Number.MAX_SAFE_INTEGER, at which point Number(v)
    // silently rounds and downstream comparisons hit the wrong row.
    if (!Number.isSafeInteger(n)) return null;
    return n;
}

// Strict typeof check — `|| ''` would coerce arrays/objects via String()
// and produce '[object Object]' or 'a,b,c'.
function stringOrEmpty(v) {
    return typeof v === 'string' ? v : '';
}

module.exports = { safeInt, stringOrEmpty };
