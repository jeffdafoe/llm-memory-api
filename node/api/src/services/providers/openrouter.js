// OpenRouter provider — access to 300+ models through a unified OpenAI-compatible API.
// Uses the Chat Completions API (openrouter.ai/api/v1/chat/completions).
//
// Fully dynamic model registry: all models and pricing come from OpenRouter's
// /api/v1/models endpoint, cached in memory with a 4-hour TTL. The admin UI
// fetches the catalog lazily when OpenRouter is selected as a provider, and
// supports typing arbitrary model IDs for models not yet in the catalog.
//
// OpenRouter pricing is per-token in their API; we convert to per-1M for
// consistency with other providers. Cost is computed provider-side in createCall
// using catalog pricing, so index.js calculateCost always gets usage.cost.

const { log } = require('../logger');

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
    }
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

        if (conf.max_tokens) {
            body.max_tokens = conf.max_tokens;
        }

        if (conf.temperature !== undefined) {
            body.temperature = conf.temperature;
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
            throw new Error('OpenRouter API error ' + response.status + ': ' + errorText);
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

        // Compute cost from catalog pricing
        var cost = await computeCost(model, promptTokens, cachedTokens, completionTokens);
        if (cost != null) {
            usage.cost = cost;
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
                return { id: tc.id, name: tc.function.name, input: input };
            });

        logProvider('api-response', {
            provider: 'openrouter', model,
            input: uncachedInput, cached: cachedTokens,
            output: completionTokens, cost: cost != null ? cost.toFixed(6) : 'unknown',
            tool_calls: tool_calls.length
        });

        return { text: choice.message.content || '', tool_calls: tool_calls, usage: usage };
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
    getCapabilities
};
