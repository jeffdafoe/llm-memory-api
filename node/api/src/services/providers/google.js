// Google provider — Gemini model family.
// Uses the Generative Language API (generativelanguage.googleapis.com).

const { log } = require('../logger');

function logProvider(action, details) {
    log('provider', action, details);
}

// ── Model registry ──────────────────────────────────────────────────────────

const models = {
    'gemini-2.5-pro': {
        label: 'Gemini 2.5 Pro',
        configVersion: 1,
        pricing: { input: 1.25, output: 10 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
                default: 1.0,
                min: 0,
                max: 2.0,
                step: 0.1
            },
            max_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate. Thinking tokens count against this limit when thinking is enabled.',
                default: 8192,
                min: 1,
                max: 65536
            },
            thinking_budget: {
                type: 'number',
                label: 'Thinking Budget (tokens)',
                description: 'Maximum tokens for internal reasoning. Set to 0 to disable thinking entirely. The model auto-adjusts within this budget based on task complexity. Thinking tokens count against max output tokens.',
                default: 0,
                min: 0,
                max: 65536
            },
            top_p: {
                type: 'number',
                label: 'Top P',
                description: 'Nucleus sampling threshold. Considers tokens with cumulative probability up to this value. Lower values are more focused.',
                default: 0.95,
                min: 0,
                max: 1.0,
                step: 0.05
            },
            top_k: {
                type: 'number',
                label: 'Top K',
                description: 'Limits token selection to the top K most probable tokens at each step. Lower values are more focused.',
                default: 40,
                min: 1,
                max: 100
            }
        }
    },
    'gemini-2.5-flash': {
        label: 'Gemini 2.5 Flash',
        configVersion: 1,
        pricing: { input: 0.15, output: 0.60 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
                default: 1.0,
                min: 0,
                max: 2.0,
                step: 0.1
            },
            max_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate. Thinking tokens count against this limit when thinking is enabled.',
                default: 8192,
                min: 1,
                max: 65536
            },
            thinking_budget: {
                type: 'number',
                label: 'Thinking Budget (tokens)',
                description: 'Maximum tokens for internal reasoning. Set to 0 to disable thinking entirely. The model auto-adjusts within this budget based on task complexity. Thinking tokens count against max output tokens.',
                default: 0,
                min: 0,
                max: 65536
            },
            top_p: {
                type: 'number',
                label: 'Top P',
                description: 'Nucleus sampling threshold. Considers tokens with cumulative probability up to this value. Lower values are more focused.',
                default: 0.95,
                min: 0,
                max: 1.0,
                step: 0.05
            },
            top_k: {
                type: 'number',
                label: 'Top K',
                description: 'Limits token selection to the top K most probable tokens at each step. Lower values are more focused.',
                default: 40,
                min: 1,
                max: 100
            }
        }
    },
    'gemini-2.5-flash-lite': {
        label: 'Gemini 2.5 Flash-Lite',
        configVersion: 1,
        pricing: { input: 0.075, output: 0.30 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
                default: 1.0,
                min: 0,
                max: 2.0,
                step: 0.1
            },
            max_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate.',
                default: 8192,
                min: 1,
                max: 65536
            },
            top_p: {
                type: 'number',
                label: 'Top P',
                description: 'Nucleus sampling threshold. Considers tokens with cumulative probability up to this value. Lower values are more focused.',
                default: 0.95,
                min: 0,
                max: 1.0,
                step: 0.05
            },
            top_k: {
                type: 'number',
                label: 'Top K',
                description: 'Limits token selection to the top K most probable tokens at each step. Lower values are more focused.',
                default: 40,
                min: 1,
                max: 100
            }
        }
    },
    'gemini-2.0-flash': {
        label: 'Gemini 2.0 Flash',
        configVersion: 1,
        deprecated: 'Retiring June 1, 2026. Migrate to Gemini 2.5 Flash.',
        pricing: { input: 0.10, output: 0.40 },
        capabilities: {
            temperature: {
                type: 'number',
                label: 'Temperature',
                description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
                default: 1.0,
                min: 0,
                max: 2.0,
                step: 0.1
            },
            max_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate.',
                default: 8192,
                min: 1,
                max: 8192
            },
            top_p: {
                type: 'number',
                label: 'Top P',
                description: 'Nucleus sampling threshold. Considers tokens with cumulative probability up to this value. Lower values are more focused.',
                default: 0.95,
                min: 0,
                max: 1.0,
                step: 0.05
            },
            top_k: {
                type: 'number',
                label: 'Top K',
                description: 'Limits token selection to the top K most probable tokens at each step. Lower values are more focused.',
                default: 40,
                min: 1,
                max: 100
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
        const url = `https://generativelanguage.googleapis.com/v1beta/models/${model}:generateContent`;

        const body = {
            system_instruction: {
                parts: { text: prompt }
            },
            contents: [{
                role: 'user',
                parts: [{ text: userMessage }]
            }],
            generationConfig: {}
        };

        if (conf.max_tokens) {
            body.generationConfig.maxOutputTokens = conf.max_tokens;
        }
        if (conf.temperature !== undefined) {
            body.generationConfig.temperature = conf.temperature;
        }
        if (conf.top_p !== undefined) {
            body.generationConfig.topP = conf.top_p;
        }
        if (conf.top_k !== undefined) {
            body.generationConfig.topK = conf.top_k;
        }

        // Thinking budget: 0 = disabled, >0 = enabled with cap.
        // When omitted, model uses its own default thinking behavior.
        if (conf.thinking_budget !== undefined) {
            body.generationConfig.thinkingConfig = {
                thinkingBudget: conf.thinking_budget
            };
        }

        // Per-call stop sequences. Gemini allows up to 5.
        if (opts && Array.isArray(opts.stop) && opts.stop.length > 0) {
            body.generationConfig.stopSequences = opts.stop.slice(0, 5);
        }

        logProvider('api-call', { provider: 'google', model });

        const response = await fetch(url, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'x-goog-api-key': apiKey
            },
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            const errorText = await response.text();
            logProvider('api-error', { provider: 'google', model, status: response.status, error: errorText });
            throw new Error(`Gemini API error ${response.status}: ${errorText}`);
        }

        const data = await response.json();
        const candidate = data.candidates && data.candidates[0];
        if (!candidate || !candidate.content || !candidate.content.parts) {
            throw new Error('Gemini API returned no content');
        }

        const text = candidate.content.parts
            .filter(p => p.text)
            .map(p => p.text)
            .join('\n');

        const usage = {
            input_tokens: data.usageMetadata?.promptTokenCount || 0,
            output_tokens: data.usageMetadata?.candidatesTokenCount || 0
        };

        logProvider('api-response', { provider: 'google', model, ...usage });

        return { text, usage };
    };
}

module.exports = { name: 'google', label: 'Google', aliases: ['gemini'], models, createCall };
