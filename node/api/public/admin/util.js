// util.js — shared safe-coercion helpers for the admin frontend.
//
// These exist so that any place we coerce values that came from outside
// our control (LLM tool-call inputs, form fields, JSON we didn't build
// ourselves) can do it the same strict way. Past code-review feedback
// has repeatedly flagged places where parseInt and `|| ''` silently
// accepted malformed input — '4abc' rendering as '4', or an object
// rendering as '[object Object]' — so the rule going forward is:
//
//   - If you want an integer, use safeInt and treat null as "skip /
//     use my own default via ??".
//   - If you want a string, use stringOrEmpty and let arrays / objects
//     fall through to ''.
//
// Helpers:
//
//   safeInt(v) -> integer | null
//     Strict integer parsing. Returns null for anything that isn't a
//     clean integer value: NaN, Infinity, '4abc' (parseInt would have
//     accepted '4'), '4.9' (parseInt would have truncated to 4),
//     undefined, null, objects, booleans. Caller picks the fallback:
//
//       const q   = safeInt(qty) ?? 1;           // schema default = 1
//       const amt = safeInt(amount);              // null = skip render
//       if (amt !== null) parts.push(amt + 'c');
//
//   stringOrEmpty(v) -> string
//     Returns v if it's a string, otherwise ''. Use this in place of
//     `v || ''` when v could come from an external source — `|| ''`
//     stringifies arrays/objects via JS coercion and surfaces ugly
//     output like '[object Object]' or 'a,b,c' in the UI.

// Strict: requires a finite integer. Rejects partial-numeric strings
// ('4abc'), non-integer numerics ('4.9', 4.9), and non-numeric values.
// Number('') is 0, so the leading typeof check rejects '' and undefined
// before they sneak through as zero.
export function safeInt(v) {
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
// and produce '[object Object]' or 'a,b,c'. Returning '' lets the caller
// hide the field with a v-if rather than render garbage.
export function stringOrEmpty(v) {
    return typeof v === 'string' ? v : '';
}
