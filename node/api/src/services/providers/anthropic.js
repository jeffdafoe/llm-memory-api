// Anthropic provider — Claude model family.
// Handles structured system prompts with optional cache_control markers.

const { log } = require('../logger');

function logProvider(action, details) {
    log('provider', action, details);
}

// ── Model registry ──────────────────────────────────────────────────────────

const models = {
    'claude-opus-4-6': {
        label: 'Opus 4.6',
        apiId: 'claude-opus-4-20250514',
        // Pricing: dollars per million tokens
        pricing: { input: 15, output: 75, cache_write: 18.75, cache_read: 1.50 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative. Ignored when thinking is enabled.',
                default: 1.0,
                min: 0,
                max: 1.0,
                step: 0.1
            },
            max_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate (thinking + response combined).',
                default: 16384,
                min: 1,
                max: 128000
            },
            thinking_effort: {
                type: 'select',
                label: 'Thinking Effort',
                description: 'Controls how much the model thinks before responding. Higher effort produces better results on complex tasks but costs more tokens. "off" disables thinking entirely.',
                default: 'off',
                options: ['off', 'low', 'medium', 'high', 'max']
            },
            cache_prompts: {
                type: 'boolean',
                label: 'Prompt Caching',
                description: 'Caches the static portion of the system prompt across calls. 5-minute TTL, 25% write premium, 90% read discount.',
                default: false
            }
        }
    },
    'claude-sonnet-4-6': {
        label: 'Sonnet 4.6',
        apiId: 'claude-sonnet-4-20250514',
        pricing: { input: 3, output: 15, cache_write: 3.75, cache_read: 0.30 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative. Ignored when thinking is enabled.',
                default: 1.0,
                min: 0,
                max: 1.0,
                step: 0.1
            },
            max_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate (thinking + response combined).',
                default: 8192,
                min: 1,
                max: 64000
            },
            thinking_effort: {
                type: 'select',
                label: 'Thinking Effort',
                description: 'Controls how much the model thinks before responding. Higher effort produces better results on complex tasks but costs more tokens. "off" disables thinking entirely.',
                default: 'off',
                options: ['off', 'low', 'medium', 'high', 'max']
            },
            cache_prompts: {
                type: 'boolean',
                label: 'Prompt Caching',
                description: 'Caches the static portion of the system prompt across calls. 5-minute TTL, 25% write premium, 90% read discount.',
                default: false
            }
        }
    },
    'claude-haiku-4-5': {
        label: 'Haiku 4.5',
        apiId: 'claude-haiku-4-5-20251001',
        pricing: { input: 0.80, output: 4, cache_write: 1.00, cache_read: 0.08 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
                default: 1.0,
                min: 0,
                max: 1.0,
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
            cache_prompts: {
                type: 'boolean',
                label: 'Prompt Caching',
                description: 'Caches the static portion of the system prompt across calls. 5-minute TTL, 25% write premium, 90% read discount.',
                default: false
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
        const useCache = conf.cache_prompts && opts && opts.cache && typeof systemPrompt !== 'string';
        const useThinking = conf.thinking_effort && conf.thinking_effort !== 'off';

        let system;
        if (useCache) {
            // Structured system prompt with cache_control on the static prefix.
            // Anthropic caches the longest prefix marked with cache_control.
            const parts = [];
            if (systemPrompt.static) {
                parts.push({ type: 'text', text: systemPrompt.static, cache_control: { type: 'ephemeral' } });
            }
            if (systemPrompt.dynamic) {
                parts.push({ type: 'text', text: systemPrompt.dynamic });
            }
            system = parts;
        } else {
            system = flattenPrompt(systemPrompt);
        }

        const headers = {
            'Content-Type': 'application/json',
            'x-api-key': apiKey,
            'anthropic-version': '2023-06-01',
            ...(conf.headers || {})
        };

        const body = {
            model: model,
            max_tokens: conf.max_tokens || 4096,
            system: system,
            messages: [{ role: 'user', content: userMessage }]
        };

        // Adaptive thinking — omit temperature entirely when thinking is active.
        if (useThinking) {
            body.thinking = {
                type: 'adaptive',
                effort: conf.thinking_effort
            };
        } else if (conf.temperature !== undefined) {
            body.temperature = conf.temperature;
        }

        logProvider('api-call', { provider: 'anthropic', model, cached: useCache, thinking: !!useThinking });

        const response = await fetch('https://api.anthropic.com/v1/messages', {
            method: 'POST',
            headers,
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            const errorText = await response.text();
            logProvider('api-error', { provider: 'anthropic', model, status: response.status, error: errorText });
            throw new Error(`Anthropic API error ${response.status}: ${errorText}`);
        }

        const data = await response.json();
        const text = data.content
            .filter(block => block.type === 'text')
            .map(block => block.text)
            .join('\n');

        const usage = {
            input_tokens: data.usage?.input_tokens || 0,
            output_tokens: data.usage?.output_tokens || 0,
            cache_creation_input_tokens: data.usage?.cache_creation_input_tokens || 0,
            cache_read_input_tokens: data.usage?.cache_read_input_tokens || 0
        };

        logProvider('api-response', { provider: 'anthropic', model, ...usage });

        return { text, usage };
    };
}

module.exports = { name: 'anthropic', label: 'Anthropic', models, createCall, flattenPrompt };
