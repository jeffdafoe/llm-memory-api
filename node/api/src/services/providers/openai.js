// OpenAI provider — GPT and o-series model families.
// Uses the Chat Completions API (api.openai.com/v1/chat/completions).

const { log } = require('../logger');

function logProvider(action, details) {
    log('provider', action, details);
}

// ── Model registry ──────────────────────────────────────────────────────────
// Note: o-series reasoning models do NOT support temperature — they use
// reasoning_effort instead. The capabilities reflect this per model.

const models = {
    'gpt-4o': {
        label: 'GPT-4o',
        configVersion: 1,
        pricing: { input: 2.50, output: 10 },
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
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 4096,
                min: 1,
                max: 16384
            }
        }
    },
    'gpt-4o-mini': {
        label: 'GPT-4o Mini',
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
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 4096,
                min: 1,
                max: 16384
            }
        }
    },
    'gpt-4.1': {
        label: 'GPT-4.1',
        configVersion: 1,
        pricing: { input: 2, output: 8 },
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
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 32768
            }
        }
    },
    'gpt-4.1-mini': {
        label: 'GPT-4.1 Mini',
        configVersion: 1,
        pricing: { input: 0.40, output: 1.60 },
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
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 32768
            }
        }
    },
    'gpt-4.1-nano': {
        label: 'GPT-4.1 Nano',
        configVersion: 1,
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
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 4096,
                min: 1,
                max: 32768
            }
        }
    },
    'gpt-5.4': {
        label: 'GPT-5.4',
        configVersion: 1,
        pricing: { input: 2.50, output: 15 },
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
            reasoning_effort: {
                type: 'select',
                label: 'Reasoning Effort',
                description: 'Controls how much time the model spends thinking. Supports none (default), low, medium, high, and xhigh.',
                default: 'none',
                options: ['none', 'low', 'medium', 'high', 'xhigh']
            },
            max_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 32768
            }
        }
    },
    'gpt-5.4-pro': {
        label: 'GPT-5.4 Pro',
        configVersion: 1,
        pricing: { input: 30, output: 180 },
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
            reasoning_effort: {
                type: 'select',
                label: 'Reasoning Effort',
                description: 'Controls how much time the model spends thinking. Supports none (default), low, medium, high, and xhigh.',
                default: 'none',
                options: ['none', 'low', 'medium', 'high', 'xhigh']
            },
            max_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 32768
            }
        }
    },
    'o3': {
        label: 'o3',
        configVersion: 1,
        pricing: { input: 10, output: 40 },
        capabilities: {
            reasoning_effort: {
                type: 'select',
                label: 'Reasoning Effort',
                description: 'Controls how much time the model spends thinking. Higher effort produces better results on complex tasks but costs more tokens and takes longer.',
                default: 'medium',
                options: ['low', 'medium', 'high']
            },
            max_completion_tokens: {
                type: 'number',
                label: 'Max Completion Tokens',
                description: 'Maximum tokens for the response including reasoning. Use max_completion_tokens instead of max_tokens for reasoning models.',
                default: 16384,
                min: 1,
                max: 100000
            }
        }
    },
    'o3-mini': {
        label: 'o3 Mini',
        configVersion: 1,
        pricing: { input: 1.10, output: 4.40 },
        capabilities: {
            reasoning_effort: {
                type: 'select',
                label: 'Reasoning Effort',
                description: 'Controls how much time the model spends thinking. Higher effort produces better results on complex tasks but costs more tokens and takes longer.',
                default: 'medium',
                options: ['low', 'medium', 'high']
            },
            max_completion_tokens: {
                type: 'number',
                label: 'Max Completion Tokens',
                description: 'Maximum tokens for the response including reasoning. Use max_completion_tokens instead of max_tokens for reasoning models.',
                default: 8192,
                min: 1,
                max: 65536
            }
        }
    },
    'o4-mini': {
        label: 'o4 Mini',
        configVersion: 1,
        pricing: { input: 1.10, output: 4.40 },
        capabilities: {
            reasoning_effort: {
                type: 'select',
                label: 'Reasoning Effort',
                description: 'Controls how much time the model spends thinking. Higher effort produces better results on complex tasks but costs more tokens and takes longer.',
                default: 'medium',
                options: ['low', 'medium', 'high']
            },
            max_completion_tokens: {
                type: 'number',
                label: 'Max Completion Tokens',
                description: 'Maximum tokens for the response including reasoning. Use max_completion_tokens instead of max_tokens for reasoning models.',
                default: 8192,
                min: 1,
                max: 100000
            }
        }
    }
};

// ── Flatten structured system prompts ───────────────────────────────────────

function flattenPrompt(systemPrompt) {
    if (typeof systemPrompt === 'string') return systemPrompt;
    return [systemPrompt.static, systemPrompt.dynamic].filter(Boolean).join('\n\n');
}

// ── Detect reasoning model (o-series) ───────────────────────────────────────

function isReasoningModel(modelId) {
    return modelId.startsWith('o');
}

// ── API call factory ────────────────────────────────────────────────────────

function createCall(model, apiKey, configuration) {
    const conf = configuration || {};

    return async function call(systemPrompt, userMessage) {
        const prompt = flattenPrompt(systemPrompt);
        const reasoning = isReasoningModel(model);

        // Reasoning models use "developer" role instead of "system".
        const systemRole = reasoning ? 'developer' : 'system';

        const body = {
            model: model,
            messages: [
                { role: systemRole, content: prompt },
                { role: 'user', content: userMessage }
            ]
        };

        // O-series reasoning models: use max_completion_tokens, reasoning_effort, no temperature.
        // Standard models: use max_tokens, temperature.
        // GPT-5.4+: supports both temperature and reasoning_effort with max_tokens.
        if (reasoning) {
            if (conf.max_completion_tokens) {
                body.max_completion_tokens = conf.max_completion_tokens;
            }
            if (conf.reasoning_effort) {
                body.reasoning_effort = conf.reasoning_effort;
            }
        } else {
            if (conf.max_tokens) {
                body.max_tokens = conf.max_tokens;
            }
            if (conf.temperature !== undefined) {
                body.temperature = conf.temperature;
            }
            if (conf.reasoning_effort && conf.reasoning_effort !== 'none') {
                body.reasoning_effort = conf.reasoning_effort;
            }
        }

        logProvider('api-call', { provider: 'openai', model, reasoning });

        const response = await fetch('https://api.openai.com/v1/chat/completions', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Authorization': `Bearer ${apiKey}`
            },
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            const errorText = await response.text();
            logProvider('api-error', { provider: 'openai', model, status: response.status, error: errorText });
            throw new Error(`OpenAI API error ${response.status}: ${errorText}`);
        }

        const data = await response.json();
        const choice = data.choices && data.choices[0];
        if (!choice || !choice.message) {
            throw new Error('OpenAI API returned no content');
        }

        const usage = {
            input_tokens: data.usage?.prompt_tokens || 0,
            output_tokens: data.usage?.completion_tokens || 0
        };

        logProvider('api-response', { provider: 'openai', model, ...usage });

        return { text: choice.message.content, usage };
    };
}

module.exports = { name: 'openai', label: 'OpenAI', models, createCall };
