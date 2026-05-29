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
// field, garbage for a boolean) is returned untouched so downstream validation
// still rejects it with a precise error rather than this layer masking a real
// problem. Only the observed failure direction (model stringifies numbers/bools)
// is handled; string fields are left alone.
//
// Recurses through declared object properties and array items so nested
// argument shapes coerce too. `value` is a freshly parsed throwaway object, so
// in-place mutation of objects/arrays is safe.
function coerceToSchema(value, schema) {
    if (!schema || typeof schema !== 'object') return value;
    // JSON Schema permits `type` to be an array (a union); a union is ambiguous
    // to coerce toward, so only single string types are handled.
    const type = typeof schema.type === 'string' ? schema.type : null;

    if (type === 'integer' || type === 'number') {
        if (typeof value !== 'string') return value;
        const trimmed = value.trim();
        // An integer field must receive a whole number — no fractional or
        // exponent noise. Leave anything else for downstream validation.
        if (type === 'integer' && !/^[+-]?\d+$/.test(trimmed)) return value;
        const n = asNumber(trimmed);
        return n === undefined ? value : n;
    }

    if (type === 'boolean') {
        if (typeof value !== 'string') return value;
        const lowered = value.trim().toLowerCase();
        if (lowered === 'true') return true;
        if (lowered === 'false') return false;
        return value;
    }

    if (type === 'object' && value && typeof value === 'object'
        && !Array.isArray(value) && schema.properties) {
        for (const key of Object.keys(value)) {
            const propSchema = schema.properties[key];
            if (propSchema) {
                value[key] = coerceToSchema(value[key], propSchema);
            }
        }
        return value;
    }

    if (type === 'array' && Array.isArray(value) && schema.items) {
        for (let i = 0; i < value.length; i++) {
            value[i] = coerceToSchema(value[i], schema.items);
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
