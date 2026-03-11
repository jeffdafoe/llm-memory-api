// Perplexity provider — Sonar model family with built-in web search.
// Uses the Chat Completions API (api.perplexity.ai/chat/completions).
// OpenAI-compatible format with additional response fields (citations, search_results).

const { log } = require('../logger');

function logProvider(action, details) {
    log('provider', action, details);
}

// ── Model registry ──────────────────────────────────────────────────────────

const models = {
    'sonar': {
        label: 'Sonar',
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

    return async function call(systemPrompt, userMessage) {
        const prompt = flattenPrompt(systemPrompt);

        const body = {
            model: model,
            messages: [
                { role: 'system', content: prompt },
                { role: 'user', content: userMessage }
            ]
        };

        if (conf.max_tokens) {
            body.max_tokens = conf.max_tokens;
        }
        if (conf.temperature !== undefined) {
            body.temperature = conf.temperature;
        }
        if (conf.search_recency_filter) {
            body.search_recency_filter = conf.search_recency_filter;
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
            throw new Error(`Perplexity API error ${response.status}: ${errorText}`);
        }

        const data = await response.json();
        const choice = data.choices && data.choices[0];
        if (!choice || !choice.message) {
            throw new Error('Perplexity API returned no content');
        }

        let text = choice.message.content;

        // Append citations if available and configured.
        // Citations come as a top-level array of URLs in the response.
        const includeCitations = conf.return_citations !== false;
        if (includeCitations && data.citations && data.citations.length > 0) {
            const citationLines = data.citations.map((url, i) => `[${i + 1}] ${url}`);
            text += '\n\nSources:\n' + citationLines.join('\n');
        }

        const usage = {
            input_tokens: data.usage?.prompt_tokens || 0,
            output_tokens: data.usage?.completion_tokens || 0
        };

        logProvider('api-response', { provider: 'perplexity', model, ...usage });

        return { text, usage };
    };
}

module.exports = { name: 'perplexity', label: 'Perplexity', models, createCall };
