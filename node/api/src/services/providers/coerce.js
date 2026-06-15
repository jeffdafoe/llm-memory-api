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

// coerceToSchema walks a parsed tool-call argument value and converts
// string-encoded scalars to the JSON-Schema-declared type. LLM function
// calling is unreliable about argument types — Llama 3.x (and other models
// routed via OpenRouter) routinely emit every scalar as a JSON string
// ({"qty":"1","consume_now":"true"}) regardless of the schema's
// `type: integer/boolean`. Downstream consumers (notably the Salem engine)
// decode tool args into strictly typed structs and hard-reject a string where
// they expect an int/bool, so the call fails as malformed_args even though the
// model's intent was correct.
//
// Coercion is conservative: a value is converted only when it cleanly matches
// the declared type. Anything that doesn't (a non-numeric string for an integer
// field, garbage for a boolean, a string that isn't valid JSON for an
// array/object field) is returned untouched so downstream validation still
// rejects it with a precise error rather than this layer masking a real
// problem. The handled failure direction is over-stringification: the model
// emits a scalar (number/bool) — or, for an array/object field, the entire
// structure — as a JSON string despite the declared type. A field whose schema
// type is `string` is never touched (the model over-stringifies; it never
// under-stringifies).
//
// Recurses through declared object properties and array items so nested
// argument shapes coerce too. `value` is a freshly parsed throwaway object, so
// in-place mutation of objects/arrays is safe.

// singleNonNullType resolves a JSON Schema `type` to a single coercible type
// name. A plain string type passes through; a union array resolves only when it
// names exactly one non-"null" type — the common nullable case, e.g.
// ["integer","null"]. Genuinely ambiguous unions (two real types) return null
// and are left untouched.
function singleNonNullType(type) {
    if (typeof type === 'string') return type;
    if (Array.isArray(type)) {
        const nonNull = type.filter(function (t) { return t !== 'null'; });
        if (nonNull.length === 1 && typeof nonNull[0] === 'string') return nonNull[0];
    }
    return null;
}

// tryParseJSON returns the parsed value, or undefined when `s` isn't valid
// JSON. Used to recover a whole array/object argument the model
// over-stringified (emitted the entire structure as a JSON string rather than
// inline JSON). A non-throwing parse so callers can fall back to leaving the
// original string in place for strict downstream validation.
function tryParseJSON(s) {
    try {
        return JSON.parse(s);
    } catch (e) {
        return undefined;
    }
}

function coerceToSchema(value, schema) {
    if (!schema || typeof schema !== 'object') return value;
    const type = singleNonNullType(schema.type);
    if (type === null) return value;

    if (type === 'integer') {
        if (typeof value !== 'string') return value;
        const trimmed = value.trim();
        // Whole number only — no fractional/exponent noise. And only within JS's
        // exact-integer range: Number("9007199254740993") silently rounds, so an
        // out-of-range id/amount is left for downstream validation rather than
        // corrupted here.
        if (!/^[+-]?\d+$/.test(trimmed)) return value;
        const n = Number(trimmed);
        return Number.isSafeInteger(n) ? n : value;
    }

    if (type === 'number') {
        if (typeof value !== 'string') return value;
        const trimmed = value.trim();
        if (trimmed === '') return value;
        // Number() (not parseFloat) so junk suffixes fail cleanly: "1abc" -> NaN.
        const n = Number(trimmed);
        return Number.isFinite(n) ? n : value;
    }

    if (type === 'boolean') {
        if (typeof value !== 'string') return value;
        const lowered = value.trim().toLowerCase();
        if (lowered === 'true') return true;
        if (lowered === 'false') return false;
        return value;
    }

    if (type === 'object') {
        // Llama (via OpenRouter) sometimes over-stringifies a whole object
        // argument: the entire {...} arrives as a JSON string instead of an
        // object. Recover it before coercing; a string that doesn't parse to a
        // plain object is left untouched for downstream validation to reject.
        if (typeof value === 'string') {
            const parsed = tryParseJSON(value);
            if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return value;
            value = parsed;
        }
        if (value && typeof value === 'object' && !Array.isArray(value)
            && schema.properties && typeof schema.properties === 'object' && !Array.isArray(schema.properties)) {
            for (const key of Object.keys(value)) {
                // Never assign through a prototype-poisoning key, even if a schema
                // somehow declared one: value["__proto__"] = ... can invoke the
                // legacy prototype setter rather than set an own data property.
                if (key === '__proto__' || key === 'constructor' || key === 'prototype') continue;
                // Own-property check only — never read through to Object.prototype for
                // a model-supplied key like "__proto__".
                if (!Object.prototype.hasOwnProperty.call(schema.properties, key)) continue;
                value[key] = coerceToSchema(value[key], schema.properties[key]);
            }
        }
        return value;
    }

    if (type === 'array') {
        // Same over-stringification failure mode: the whole array arrives as a
        // JSON string. Recover it before coercing items; a string that doesn't
        // parse to an array is left untouched for downstream validation to reject.
        if (typeof value === 'string') {
            const parsed = tryParseJSON(value);
            if (!Array.isArray(parsed)) return value;
            value = parsed;
        }
        if (Array.isArray(value) && schema.items) {
            for (let i = 0; i < value.length; i++) {
                value[i] = coerceToSchema(value[i], schema.items);
            }
        }
        return value;
    }

    return value;
}

// coerceToolArgs is the entry point for a tool call's top-level arguments. The
// `parameters` argument is the tool's offered JSON Schema (itself a
// `type: object` schema); it may arrive as an object or, defensively, as a JSON
// string. Returns the input unchanged when there is no usable schema to coerce
// against.
function coerceToolArgs(input, parameters) {
    let schema = parameters;
    if (typeof schema === 'string') {
        try {
            schema = JSON.parse(schema);
        } catch (e) {
            return input;
        }
    }
    if (!schema || typeof schema !== 'object') return input;
    return coerceToSchema(input, schema);
}

module.exports = { asNumber, coerceToSchema, coerceToolArgs };
