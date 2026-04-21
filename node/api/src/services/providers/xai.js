// xAI provider — Grok model family with optional X (Twitter) and web search.
// Uses the Responses API (api.x.ai/v1/responses) which supports server-side
// tool execution for search. The Responses API uses "input" instead of "messages"
// and returns output in an array of content items.
//
// Search tools (x_search, web_search) are server-side — xAI handles the search
// orchestration automatically. When enabled, the model decides when to search,
// parses results, and may do follow-up queries before composing a response.
// Each search invocation costs $5/1K calls on top of token costs.

const { log } = require('../logger');

function logProvider(action, details) {
    log('provider', action, details);
}

// ── Model registry ──────────────────────────────────────────────────────────
// Pricing is per million tokens. Cached input is 10% of standard input rate.

const models = {
    'grok-4.20-reasoning': {
        label: 'Grok 4.20',
        apiId: 'grok-4.20-0309-reasoning',
        configVersion: 1,
        pricing: { input: 2, output: 6, cache_read: 0.20 },
        capabilities: {
            x_search: {
                type: 'boolean',
                label: 'X Search',
                description: 'Enable searching X (Twitter) posts. $5 per 1,000 search calls.',
                default: false
            },
            web_search: {
                type: 'boolean',
                label: 'Web Search',
                description: 'Enable searching the web. $5 per 1,000 search calls.',
                default: false
            },
            max_output_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 131072
            }
        }
    },
    'grok-4.20-non-reasoning': {
        label: 'Grok 4.20 (non-reasoning)',
        apiId: 'grok-4.20-0309-non-reasoning',
        configVersion: 1,
        pricing: { input: 2, output: 6, cache_read: 0.20 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused, higher values are more creative.',
                default: 1.0,
                min: 0,
                max: 2.0,
                step: 0.1
            },
            x_search: {
                type: 'boolean',
                label: 'X Search',
                description: 'Enable searching X (Twitter) posts. $5 per 1,000 search calls.',
                default: false
            },
            web_search: {
                type: 'boolean',
                label: 'Web Search',
                description: 'Enable searching the web. $5 per 1,000 search calls.',
                default: false
            },
            max_output_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 131072
            }
        }
    },
    'grok-4.1-fast-reasoning': {
        label: 'Grok 4.1 Fast',
        apiId: 'grok-4-1-fast-reasoning',
        configVersion: 1,
        pricing: { input: 0.20, output: 0.50, cache_read: 0.02 },
        capabilities: {
            x_search: {
                type: 'boolean',
                label: 'X Search',
                description: 'Enable searching X (Twitter) posts. $5 per 1,000 search calls.',
                default: false
            },
            web_search: {
                type: 'boolean',
                label: 'Web Search',
                description: 'Enable searching the web. $5 per 1,000 search calls.',
                default: false
            },
            max_output_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 131072
            }
        }
    },
    'grok-4.1-fast-non-reasoning': {
        label: 'Grok 4.1 Fast (non-reasoning)',
        apiId: 'grok-4-1-fast-non-reasoning',
        configVersion: 1,
        pricing: { input: 0.20, output: 0.50, cache_read: 0.02 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused, higher values are more creative.',
                default: 1.0,
                min: 0,
                max: 2.0,
                step: 0.1
            },
            x_search: {
                type: 'boolean',
                label: 'X Search',
                description: 'Enable searching X (Twitter) posts. $5 per 1,000 search calls.',
                default: false
            },
            web_search: {
                type: 'boolean',
                label: 'Web Search',
                description: 'Enable searching the web. $5 per 1,000 search calls.',
                default: false
            },
            max_output_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 131072
            }
        }
    }
};

// ── Flatten structured system prompts ───────────────────────────────────────

function flattenPrompt(systemPrompt) {
    if (typeof systemPrompt === 'string') return systemPrompt;
    return [systemPrompt.static, systemPrompt.dynamic].filter(Boolean).join('\n\n');
}

// ── Provider-side cost calculation ──────────────────────────────────────────
// xAI pricing is straightforward: input, output, cached input at 10% of input.

function computeCost(modelId, promptTokens, cachedTokens, completionTokens) {
    const modelEntry = models[modelId] || Object.values(models).find(m => m.apiId === modelId);
    if (!modelEntry || !modelEntry.pricing) return null;

    const pricing = modelEntry.pricing;
    const uncachedInput = promptTokens - cachedTokens;
    const inputRate = pricing.input || 0;
    const cacheRate = pricing.cache_read || 0;
    const outputRate = pricing.output || 0;

    let cost = 0;
    cost += uncachedInput * inputRate / 1_000_000;
    cost += cachedTokens * cacheRate / 1_000_000;
    cost += completionTokens * outputRate / 1_000_000;

    return cost;
}

// ── API call factory ────────────────────────────────────────────────────────
// Uses the Responses API (/v1/responses) instead of Chat Completions.
// The Responses API uses "input" (array of messages) and returns "output"
// (array of content items) with optional citations.

function createCall(model, apiKey, configuration) {
    const conf = configuration || {};

    return async function call(systemPrompt, userMessage, opts) {
        const prompt = flattenPrompt(systemPrompt);

        // Build input messages — system instruction + user message
        const input = [
            { role: 'system', content: prompt },
            { role: 'user', content: userMessage }
        ];

        const body = {
            model: model,
            input: input
        };

        // Max output tokens
        if (conf.max_output_tokens) {
            body.max_output_tokens = conf.max_output_tokens;
        }

        // Temperature — only for non-reasoning models
        if (conf.temperature !== undefined) {
            body.temperature = conf.temperature;
        }

        // Per-call stop sequences. xAI Responses API accepts `stop` like
        // OpenAI chat completions — cap at 4 to match the family.
        if (opts && Array.isArray(opts.stop) && opts.stop.length > 0) {
            body.stop = opts.stop.slice(0, 4);
        }

        // Build tools array — search tools are server-side, xAI handles execution
        const tools = [];
        if (conf.x_search) {
            tools.push({ type: 'x_search' });
        }
        if (conf.web_search) {
            tools.push({ type: 'web_search' });
        }
        if (tools.length > 0) {
            body.tools = tools;
        }

        const searchEnabled = tools.map(t => t.type);
        logProvider('api-call', {
            provider: 'xai', model,
            search: searchEnabled.length > 0 ? searchEnabled : false
        });

        const response = await fetch('https://api.x.ai/v1/responses', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Authorization': `Bearer ${apiKey}`
            },
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            const errorText = await response.text();
            logProvider('api-error', { provider: 'xai', model, status: response.status, error: errorText });
            throw new Error(`xAI API error ${response.status}: ${errorText}`);
        }

        const data = await response.json();

        // Extract text from the output array.
        // The Responses API returns output as an array of items — we want the
        // message content items with type "output_text".
        let text = '';
        if (data.output && Array.isArray(data.output)) {
            for (const item of data.output) {
                // Direct output_text items
                if (item.type === 'output_text') {
                    text += (text ? '\n' : '') + item.text;
                }
                // Message items contain nested content arrays
                if (item.type === 'message' && item.content) {
                    for (const block of item.content) {
                        if (block.type === 'output_text' || block.type === 'text') {
                            text += (text ? '\n' : '') + (block.text || '');
                        }
                    }
                }
            }
        }

        if (!text) {
            throw new Error('xAI API returned no content');
        }

        // Append citations if present
        if (data.citations && data.citations.length > 0) {
            const citationLines = data.citations.map((c, i) => {
                // Citations may be strings (URLs) or objects with url/title
                const url = typeof c === 'string' ? c : (c.url || c);
                const title = typeof c === 'object' && c.title ? ` — ${c.title}` : '';
                return `[${i + 1}] ${url}${title}`;
            });
            text += '\n\nSources:\n' + citationLines.join('\n');
        }

        // Extract token counts from usage
        const promptTokens = data.usage?.input_tokens || 0;
        const completionTokens = data.usage?.output_tokens || 0;
        const cachedTokens = data.usage?.cache_read_input_tokens || data.usage?.input_tokens_details?.cached_tokens || 0;
        const uncachedInput = Math.max(0, promptTokens - cachedTokens);

        const usage = {
            input_tokens: uncachedInput,
            output_tokens: completionTokens,
            cache_read_input_tokens: cachedTokens
        };

        // Compute cost provider-side
        const cost = computeCost(model, promptTokens, cachedTokens, completionTokens);
        if (cost != null) {
            usage.cost = cost;
        }

        logProvider('api-response', {
            provider: 'xai', model,
            input: uncachedInput, cached: cachedTokens,
            output: completionTokens,
            cost: cost != null ? cost.toFixed(6) : 'unknown',
            search: searchEnabled.length > 0 ? searchEnabled : false
        });

        return { text, usage };
    };
}

// ── Pricing display ─────────────────────────────────────────────────────────

function formatPricing(modelId, config) {
    const modelEntry = models[modelId];
    if (!modelEntry || !modelEntry.pricing) return 'No pricing data';

    const pricing = modelEntry.pricing;
    const parts = [];
    if (pricing.input != null) parts.push('$' + pricing.input + ' in');
    if (pricing.output != null) parts.push('$' + pricing.output + ' out');
    if (pricing.cache_read != null) parts.push('$' + pricing.cache_read + ' cached');

    let result = parts.join(' / ') + ' per 1M tokens';

    // Note search costs if enabled
    const searchTools = [];
    if (config && config.x_search) searchTools.push('X');
    if (config && config.web_search) searchTools.push('web');
    if (searchTools.length > 0) {
        result += ' + $5/1K ' + searchTools.join('+') + ' searches';
    }

    return result;
}

module.exports = { name: 'xai', label: 'xAI', models, createCall, formatPricing };
