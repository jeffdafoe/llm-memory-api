// Perplexity provider — Sonar model family with built-in web search.
// Uses the Chat Completions API (api.perplexity.ai/chat/completions).
// OpenAI-compatible format with additional response fields (citations, search_results).

const { log } = require('../logger');
const { asNumber } = require('./coerce');

function logProvider(action, details) {
    log('provider', action, details);
}

// ── Model registry ──────────────────────────────────────────────────────────

const models = {
    'sonar': {
        label: 'Sonar',
        configVersion: 1,
        // Per-request fee: $5/1K requests (added per call on top of token costs)
        pricing: { input: 1, output: 1, request: 5 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
                default: 0.2,
                min: 0,
                max: 2.0,
                step: 0.1
            },
            max_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 1024,
                min: 1,
                max: 4096
            },
            search_recency_filter: {
                type: 'select',
                label: 'Search Recency',
                description: 'Filter search results by recency. Leave unset for all time.',
                default: '',
                options: ['', 'hour', 'day', 'week', 'month']
            },
            return_citations: {
                type: 'boolean',
                label: 'Include Citations',
                description: 'Append source URLs to the response text.',
                default: true
            }
        }
    },
    'sonar-pro': {
        label: 'Sonar Pro',
        configVersion: 1,
        pricing: { input: 3, output: 15, request: 6 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
                default: 0.2,
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
                max: 8192
            },
            search_recency_filter: {
                type: 'select',
                label: 'Search Recency',
                description: 'Filter search results by recency. Leave unset for all time.',
                default: '',
                options: ['', 'hour', 'day', 'week', 'month']
            },
            return_citations: {
                type: 'boolean',
                label: 'Include Citations',
                description: 'Append source URLs to the response text.',
                default: true
            }
        }
    },
    'sonar-deep-research': {
        label: 'Sonar Deep Research',
        configVersion: 1,
        pricing: { input: 2, output: 8, request: 5 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
                default: 0.2,
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
                max: 8192
            },
            return_citations: {
                type: 'boolean',
                label: 'Include Citations',
                description: 'Append source URLs to the response text.',
                default: true
            }
        }
    }
};

// ── Flatten structured system prompts ───────────────────────────────────────

function flattenPrompt(systemPrompt) {
    if (typeof systemPrompt === 'string') return systemPrompt;
    return [systemPrompt.static, systemPrompt.dynamic].filter(Boolean).join('\n\n');
}

// ── API call factory ────────────────────────────────────────────────────────

function createCall(model, apiKey, configuration) {
    const conf = configuration || {};

    return async function call(systemPrompt, userMessage, opts) {
        const prompt = flattenPrompt(systemPrompt);

        const body = {
            model: model,
            messages: [
                { role: 'system', content: prompt },
                { role: 'user', content: userMessage }
            ]
        };

        const maxTokens = asNumber(conf.max_tokens);
        if (maxTokens !== undefined) {
            body.max_tokens = maxTokens;
        }
        const temperature = asNumber(conf.temperature);
        if (temperature !== undefined) {
            body.temperature = temperature;
        }
        if (conf.search_recency_filter) {
            body.search_recency_filter = conf.search_recency_filter;
        }

        // Per-call stop sequences. Perplexity is OpenAI-compatible — cap at 4.
        if (opts && Array.isArray(opts.stop) && opts.stop.length > 0) {
            body.stop = opts.stop.slice(0, 4);
        }

        // Sonar models are search-focused and don't support function calling.
        // Drop with a log so callers know their tool spec was ignored.
        if (opts && Array.isArray(opts.tools) && opts.tools.length > 0) {
            logProvider('tools-not-supported', { provider: 'perplexity', model, requested: opts.tools.length });
        }

        logProvider('api-call', { provider: 'perplexity', model });

        const response = await fetch('https://api.perplexity.ai/chat/completions', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Authorization': `Bearer ${apiKey}`
            },
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            const errorText = await response.text();
            logProvider('api-error', { provider: 'perplexity', model, status: response.status, error: errorText });
            // status rides on the error so retryWithBackoff can pick a
            // cadence by error class (deterministic 4xx vs outage/429).
            const apiError = new Error(`Perplexity API error ${response.status}: ${errorText}`);
            apiError.status = response.status;
            throw apiError;
        }

        const data = await response.json();
        const choice = data.choices && data.choices[0];
        if (!choice || !choice.message) {
            throw new Error('Perplexity API returned no content');
        }

        let text = choice.message.content;

        // Append sources if available and configured. Perplexity removed the
        // top-level `citations` array (plain URL strings) in May 2025 in favor
        // of `search_results` — an array of { title, url, date } with richer
        // metadata. Prefer search_results; fall back to the legacy citations
        // field so any model/response still returning it keeps working. The
        // result index maps to the model's inline [n] markers, so numbering
        // must stay 1-based in original order.
        const includeCitations = conf.return_citations !== false;
        if (includeCitations) {
            let sourceLines = null;
            if (Array.isArray(data.search_results) && data.search_results.length > 0) {
                sourceLines = data.search_results.map((r, i) => {
                    const title = r.title ? `${r.title} — ` : '';
                    const date = r.date ? ` (${r.date})` : '';
                    return `[${i + 1}] ${title}${r.url || ''}${date}`;
                });
            } else if (Array.isArray(data.citations) && data.citations.length > 0) {
                sourceLines = data.citations.map((url, i) => `[${i + 1}] ${url}`);
            }
            if (sourceLines) {
                text += '\n\nSources:\n' + sourceLines.join('\n');
            }
        }

        const usage = {
            input_tokens: data.usage?.prompt_tokens || 0,
            output_tokens: data.usage?.completion_tokens || 0
        };

        logProvider('api-response', { provider: 'perplexity', model, ...usage });

        // Empty tool_calls so callers always see a uniform shape across
        // providers — Perplexity Sonar models don't support function calling.
        return { text, tool_calls: [], usage };
    };
}

module.exports = { name: 'perplexity', label: 'Perplexity', models, createCall };
