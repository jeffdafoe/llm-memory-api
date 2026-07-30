// OpenRouter provider — access to 300+ models through a unified OpenAI-compatible API.
// Uses the Chat Completions API (openrouter.ai/api/v1/chat/completions).
//
// Fully dynamic model registry: all models and pricing come from OpenRouter's
// /api/v1/models endpoint, cached in memory with a 4-hour TTL. The admin UI
// fetches the catalog lazily when OpenRouter is selected as a provider, and
// supports typing arbitrary model IDs for models not yet in the catalog.
//
// OpenRouter pricing is per-token in their API; we convert to per-1M for
// consistency with other providers. Cost is resolved provider-side in createCall
// so index.js calculateCost always gets usage.cost — preferring the billed cost
// OpenRouter reports on the response, falling back to catalog pricing only when
// the response omits it. The catalog remains the source for display pricing.

const { log } = require('../logger');
const { asNumber, coerceToolArgs } = require('./coerce');
const { normalizeOpenAIChatFinish, isTruncated } = require('./finish');

function logProvider(action, details) {
    log('provider', action, details);
}

// ── Dynamic model catalog cache ────────────────────────────────────────────
// Fetched from OpenRouter's /api/v1/models on demand, cached with TTL.

let catalogCache = null;
let catalogFetchedAt = 0;
const CATALOG_TTL_MS = 4 * 60 * 60 * 1000; // 4 hours

// Fetch the full model catalog from OpenRouter. Returns a Map of modelId -> info.
// Non-blocking: if the fetch fails, returns the stale cache (or empty map).
async function fetchCatalog() {
    const now = Date.now();
    if (catalogCache && (now - catalogFetchedAt) < CATALOG_TTL_MS) {
        return catalogCache;
    }

    try {
        const response = await fetch('https://openrouter.ai/api/v1/models', {
            headers: { 'Accept': 'application/json' }
        });
        if (!response.ok) {
            logProvider('catalog-fetch-error', { status: response.status });
            return catalogCache || new Map();
        }

        const data = await response.json();
        const catalog = new Map();
        for (const m of (data.data || [])) {
            if (m.id && m.pricing) {
                // OpenRouter pricing is per-token; convert to per-1M for our format.
                var promptPerMillion = parseFloat(m.pricing.prompt || '0') * 1_000_000;
                var completionPerMillion = parseFloat(m.pricing.completion || '0') * 1_000_000;
                var cacheReadPerMillion = m.pricing.input_cache_read
                    ? parseFloat(m.pricing.input_cache_read) * 1_000_000
                    : null;

                catalog.set(m.id, {
                    input: promptPerMillion,
                    output: completionPerMillion,
                    cache_read: cacheReadPerMillion,
                    context_length: m.context_length || null,
                    name: m.name || m.id
                });
            }
        }

        catalogCache = catalog;
        catalogFetchedAt = now;
        logProvider('catalog-fetched', { modelCount: catalog.size });
        return catalog;
    } catch (err) {
        logProvider('catalog-fetch-error', { error: err.message });
        return catalogCache || new Map();
    }
}

// Test seam. The catalog cache is module-level with a 4-hour TTL, so within one
// process the first fetch wins for the rest of the run — a test that needs a
// differently-priced catalog than an earlier test cannot get one. Not called by
// application code.
function _resetCatalogCache() {
    catalogCache = null;
    catalogFetchedAt = 0;
}

// Look up pricing for a model ID from the cached catalog.
async function lookupPricing(modelId) {
    var catalog = await fetchCatalog();
    var entry = catalog.get(modelId);
    if (entry) {
        return {
            input: entry.input,
            output: entry.output,
            cache_read: entry.cache_read
        };
    }
    return null;
}

// ── Model registry ─────────────────────────────────────────────────────────
// Empty — all models come from the dynamic catalog. The models object must
// exist for the provider interface but contains no entries. The admin UI
// fetches the full list via /admin/providers/openrouter/models.

// Default capabilities applied to all OpenRouter models (temperature + max tokens).
// These are universal across OpenRouter's API — the underlying model may ignore
// unsupported params, but OpenRouter accepts them on all requests.
const defaultCapabilities = {
    temperature: {
        type: 'number',
        label: 'Temperature',
        description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
        default: 0.7,
        min: 0,
        max: 2.0,
        step: 0.1
    },
    max_tokens: {
        type: 'number',
        label: 'Max Output Tokens',
        description: 'Maximum number of tokens the model will generate in its response.',
        default: 4096,
        min: 1,
        max: 32768
    },
    thinking_effort: {
        type: 'select',
        label: 'Thinking Effort',
        description: 'Controls how much the model reasons before responding. "off" disables reasoning entirely. Only hybrid reasoning models honour this; others ignore it. LEAVING THIS UNSET IS NOT THE SAME AS "off" — unset sends nothing and the model keeps its own default, which for deepseek-v4-flash and Gemini 2.5 means it reasons. Reasoning tokens are billed as output tokens but are NOT stored in the response, so any setting other than "off" pays for text that is never kept (LLM-570 follow-up).',
        default: 'off',
        options: ['off', 'low', 'medium', 'high', 'max']
    }
};

// Our thinking_effort vocabulary (shared with the Anthropic provider) mapped to
// OpenRouter's `reasoning.effort` scale. OpenRouter's scale is a superset —
// it also accepts 'xhigh' and 'minimal', which we deliberately do not expose so
// the setting means the same thing whichever provider an agent is pointed at.
// 'off' maps to 'none', which suppresses reasoning rather than merely hiding it
// (`reasoning.exclude` hides the output but still generates and bills it).
const REASONING_EFFORT_BY_THINKING_EFFORT = {
    off: 'none',
    low: 'low',
    medium: 'medium',
    high: 'high',
    max: 'max'
};

const models = {};

// ── Flatten structured system prompts ───────────────────────────────────────

function flattenPrompt(systemPrompt) {
    if (typeof systemPrompt === 'string') return systemPrompt;
    return [systemPrompt.static, systemPrompt.dynamic].filter(Boolean).join('\n\n');
}

// ── Cost calculation ────────────────────────────────────────────────────────
// All pricing comes from the dynamic catalog. computeCost is async because
// it may need to fetch the catalog. Called from createCall where await is fine.

async function computeCost(modelId, promptTokens, cachedTokens, completionTokens) {
    var pricing = await lookupPricing(modelId);
    if (!pricing) return null;

    var uncachedInput = promptTokens - cachedTokens;
    var cost = 0;
    cost += uncachedInput * (pricing.input || 0) / 1_000_000;
    cost += cachedTokens * (pricing.cache_read || pricing.input || 0) / 1_000_000;
    cost += completionTokens * (pricing.output || 0) / 1_000_000;
    return cost;
}

// ── API call factory ────────────────────────────────────────────────────────

function createCall(model, apiKey, configuration) {
    var conf = configuration || {};

    return async function call(systemPrompt, userMessage, opts) {
        var prompt = flattenPrompt(systemPrompt);

        // Same passthrough as openai.js — neutral message shape is OpenAI's,
        // and OpenRouter's API matches.
        var userMessages = (opts && Array.isArray(opts.messages) && opts.messages.length > 0)
            ? opts.messages
            : [{ role: 'user', content: userMessage }];

        var body = {
            model: model,
            messages: [
                { role: 'system', content: prompt }
            ].concat(userMessages)
        };

        const maxTokens = asNumber(conf.max_tokens);
        if (maxTokens !== undefined) {
            body.max_tokens = maxTokens;
        }

        const temperature = asNumber(conf.temperature);
        if (temperature !== undefined) {
            body.temperature = temperature;
        }

        // Reasoning control (LLM-570). Until this landed the provider sent no
        // reasoning field at all, so every model ran at its own default — and a
        // hybrid reasoning model like deepseek-v4-flash reasons by default. Those
        // tokens arrive on `choice.message.reasoning`, which the response
        // extractor below does not store, but they ARE counted in
        // usage.completion_tokens and billed. The observed result was calls
        // charged more output tokens than the stored response had characters.
        // ABSENT IS NOT 'off', deliberately. An absent key sends no reasoning
        // field, leaving the model's own default — the pre-LLM-570 behaviour.
        // Defaulting absence to 'none' would silently disable thinking on the
        // two Gemini dream agents (dream-technical, dream-technical-soul), which
        // is a change to home's and work's soul documents that nobody asked for.
        // Opting an agent out is therefore an explicit "thinking_effort":"off".
        //
        // An unrecognized value is ignored rather than passed through, so a
        // typo in agent configuration cannot ship a malformed reasoning field.
        // hasOwnProperty rather than a bare lookup: configuration is operator-
        // supplied, and a value like "constructor" or "toString" would otherwise
        // resolve up the prototype chain to a function and ship as the effort.
        const configuredEffort = conf.thinking_effort;
        if (typeof configuredEffort === 'string'
            && Object.prototype.hasOwnProperty.call(REASONING_EFFORT_BY_THINKING_EFFORT, configuredEffort)) {
            body.reasoning = { effort: REASONING_EFFORT_BY_THINKING_EFFORT[configuredEffort] };
        }

        // Per-call stop sequences. OpenRouter proxies to many upstreams;
        // most OpenAI-compatible upstreams allow 4, so cap at 4.
        if (opts && Array.isArray(opts.stop) && opts.stop.length > 0) {
            body.stop = opts.stop.slice(0, 4);
        }

        // Per-call tool definitions — translate the neutral shape to the OpenAI
        // function-tool wrapper that OpenRouter passes through to upstream
        // providers. Tool support varies by upstream model; OpenRouter's API
        // documentation calls out which models support tools. For ones that
        // don't, the upstream typically just ignores the field and returns a
        // text response — we tolerate that case (empty tool_calls in response).
        var useTools = opts && Array.isArray(opts.tools) && opts.tools.length > 0;
        if (useTools) {
            body.tools = opts.tools.map(function (tool) {
                return {
                    type: 'function',
                    function: {
                        name: tool.name,
                        description: tool.description,
                        parameters: tool.parameters || { type: 'object', properties: {} }
                    }
                };
            });
        }

        // OpenRouter provider routing (LLM-328). Passed through verbatim from the
        // agent's configuration JSON (`configuration.provider`), which mirrors
        // OpenRouter's own `provider` request field — e.g.
        // { "order": ["deepinfra"], "allow_fallbacks": false }. OpenRouter fronts
        // many upstream hosts for a single model id (deepseek-v4-flash alone has
        // 10+), and prompt-prefix caching is per-host: without pinning, OpenRouter
        // load-balances consecutive requests across hosts, so a prefix warmed on
        // host A is a cold miss on host B and cross-tick/cross-actor cache hits
        // almost never land. Pinning the upstream is what lets the invariant
        // prefix stay warm across a burst of NPC ticks. Behavior-neutral — same
        // prompt, same tools, only which host serves it. Require a plain object
        // (config is parsed JSON in normal use) so a stray scalar/array — or an
        // exotic object — can't ship a garbage routing field.
        if (conf.provider && Object.prototype.toString.call(conf.provider) === '[object Object]') {
            body.provider = conf.provider;
        }

        logProvider('api-call', { provider: 'openrouter', model, tools: useTools });

        var response = await fetch('https://openrouter.ai/api/v1/chat/completions', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Authorization': 'Bearer ' + apiKey,
                'HTTP-Referer': 'https://memory.jeffdafoe.com',
                'X-Title': 'llm-memory'
            },
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            var errorText = await response.text();
            logProvider('api-error', { provider: 'openrouter', model, status: response.status, error: errorText });
            // status rides on the error so retryWithBackoff can pick a
            // cadence by error class (deterministic 4xx vs outage/429).
            var apiError = new Error('OpenRouter API error ' + response.status + ': ' + errorText);
            apiError.status = response.status;
            throw apiError;
        }

        var data = await response.json();
        var choice = data.choices && data.choices[0];
        if (!choice || !choice.message) {
            throw new Error('OpenRouter API returned no content');
        }

        // Extract token counts from OpenAI-compatible usage block
        var promptTokens = (data.usage && data.usage.prompt_tokens) || 0;
        var completionTokens = (data.usage && data.usage.completion_tokens) || 0;
        var cachedTokens = 0;
        if (data.usage && data.usage.prompt_tokens_details) {
            cachedTokens = Math.max(0, Math.min(
                data.usage.prompt_tokens_details.cached_tokens || 0,
                promptTokens
            ));
        }
        var uncachedInput = Math.max(0, promptTokens - cachedTokens);

        var usage = {
            input_tokens: uncachedInput,
            output_tokens: completionTokens,
            cache_read_input_tokens: cachedTokens
        };

        // Cost: prefer OpenRouter's own billed figure over the catalog estimate
        // (LLM-560). Every chat completion carries usage.cost — what our account
        // was actually charged — with no request flag needed. The catalog holds
        // one listed price per model id, but OpenRouter fronts many upstream
        // hosts per model at differing prices, and a listed price can step while
        // a pinned upstream keeps serving at the old one. That is exactly what
        // happened on 2026-07-26: the deepseek-v4-flash listing went ~$0.096 →
        // $0.14/Mtok, logged village spend jumped ~50% overnight, and none of it
        // was real — the pinned traffic was still being billed at Baidu's rate.
        // Catalog pricing is now only the fallback, for responses omitting cost.
        var billedCost = null;
        if (data.usage && data.usage.cost != null) {
            var reportedCost = Number(data.usage.cost);
            if (Number.isFinite(reportedCost) && reportedCost >= 0) {
                billedCost = reportedCost;
            }
        }
        var costSource = 'billed';
        var cost = billedCost;
        if (cost == null) {
            costSource = 'catalog-estimate';
            cost = await computeCost(model, promptTokens, cachedTokens, completionTokens);
        }
        if (cost != null) {
            usage.cost = cost;
        }

        // Which upstream host actually served the request, persisted to
        // virtual_agent_calls.served_by (MEM-144) so a future price or routing
        // incident is diagnosable from our own DB instead of live test calls.
        // Carried on `usage` because that object is the one thing every logCall
        // site already forwards from the provider unchanged.
        // Trimmed and capped because this is provider-controlled text landing in
        // a column meant for grouping spend queries: a whitespace-only name would
        // persist as a distinct non-NULL bucket that reads as blank, and an
        // overlong one would bloat every row for a value that is in practice a
        // short display name ("Baidu", "DeepInfra"). Left unset rather than
        // stored empty, so logCall writes NULL.
        if (typeof data.provider === 'string') {
            var servedBy = data.provider.trim();
            if (servedBy !== '') {
                usage.served_by = servedBy.slice(0, 100);
            }
        }

        // Tool calls in OpenAI-compatible shape on choice.message.tool_calls.
        // Normalize to neutral [{ id, name, input }]. Args parsed from JSON
        // string; malformed JSON falls back to {} and gets logged.
        var tool_calls = ((choice.message && choice.message.tool_calls) || [])
            .filter(function (tc) {
                return tc.type === 'function' && tc.function && tc.function.name;
            })
            .map(function (tc) {
                var input = {};
                if (tc.function.arguments) {
                    try {
                        input = JSON.parse(tc.function.arguments);
                    } catch (e) {
                        logProvider('tool-args-parse-error', { provider: 'openrouter', model, error: e.message });
                    }
                }
                // Coerce string-encoded scalars against the offered tool schema.
                // OpenRouter fronts many models (Llama 3.x especially) that emit
                // numeric/boolean tool args as JSON strings ({"qty":"1"}) despite
                // the schema's declared type; strictly typed downstream decoders
                // (the Salem engine) reject those as malformed. The schema travels
                // on opts.tools[].parameters, the same defs sent to the wire above.
                if (Array.isArray(opts.tools)) {
                    var spec = opts.tools.find(function (t) { return t.name === tc.function.name; });
                    if (spec) {
                        input = coerceToolArgs(input, spec.parameters);
                    }
                }
                return { id: tc.id, name: tc.function.name, input: input };
            });

        // finish_reason "length" means the upstream model hit its token cap.
        // OpenRouter fronts the affected Gemini soul agent, so this is a live
        // truncation path — surface it for the persist guards.
        const finish_reason = normalizeOpenAIChatFinish(choice.finish_reason);

        // reasoning_chars makes discarded reasoning visible (LLM-570). The
        // response body below keeps only content + tool_calls, so reasoning the
        // model returned is billed inside completion_tokens and then dropped.
        // Storing it properly is a follow-up; logging its size means an agent
        // configured to a non-off effort is at least not silently burning
        // output tokens. 0 on the expected path, where effort is 'none'.
        const reasoningText = typeof choice.message.reasoning === 'string' ? choice.message.reasoning : '';

        logProvider('api-response', {
            provider: 'openrouter', model,
            input: uncachedInput, cached: cachedTokens,
            output: completionTokens, cost: cost != null ? cost.toFixed(8) : 'unknown',
            cost_source: cost != null ? costSource : 'unknown',
            served_by: usage.served_by || null,
            reasoning_chars: reasoningText.length,
            tool_calls: tool_calls.length, finish_reason
        });

        return { text: choice.message.content || '', tool_calls: tool_calls, usage: usage, finish_reason, truncated: isTruncated(finish_reason) };
    };
}

// ── Pricing display ─────────────────────────────────────────────────────────

function formatPricing(modelId, config) {
    // Use cached catalog (synchronous — only uses what's already fetched)
    if (catalogCache) {
        var entry = catalogCache.get(modelId);
        if (entry) {
            var parts = [];
            if (entry.input != null) parts.push('$' + Number(entry.input.toFixed(4)) + ' in');
            if (entry.output != null) parts.push('$' + Number(entry.output.toFixed(4)) + ' out');
            if (entry.cache_read != null) parts.push('$' + Number(entry.cache_read.toFixed(4)) + ' cached');
            return parts.join(' / ') + ' per 1M tokens';
        }
    }

    return 'Pricing loads with model catalog';
}

// ── Capabilities for arbitrary models ───────────────────────────────────────
// Since models{} is empty, capabilitiesFor() in the admin UI would return {}.
// Override getCapabilities so any OpenRouter model gets the default caps.

function getCapabilities(modelId) {
    return defaultCapabilities;
}

module.exports = {
    name: 'openrouter',
    label: 'OpenRouter',
    aliases: ['open-router', 'open_router'],
    models,
    createCall,
    formatPricing,
    lookupPricing,
    fetchCatalog,
    getCapabilities,
    _resetCatalogCache
};
