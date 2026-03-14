// OpenAI provider — GPT and o-series model families.
// Uses the Chat Completions API (api.openai.com/v1/chat/completions).
//
// Cost calculation is done provider-side because OpenAI has pricing dimensions
// that depend on configuration (service_tier: flex halves all rates) and on
// response data (prompt caching at 1/10th input price). The generic fallback
// formula in index.js can't account for these without becoming OpenAI-aware.

const { log } = require('../logger');

function logProvider(action, details) {
    log('provider', action, details);
}

// ── Model registry ──────────────────────────────────────────────────────────
// Note: o-series reasoning models do NOT support temperature — they use
// reasoning_effort instead. The capabilities reflect this per model.
//
// Pricing is per million tokens at standard rates. Flex tier is always
// exactly half of standard across all rate types. Cached input is 1/10th
// of the standard input rate (applied automatically by OpenAI when it
// detects repeated prompt prefixes).

const models = {
    'gpt-4o': {
        label: 'GPT-4o',
        configVersion: 2,
        pricing: { input: 2.50, output: 10, cache_read: 0.625 },
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
            max_completion_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 4096,
                min: 1,
                max: 16384
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
            }
        }
    },
    'gpt-4o-mini': {
        label: 'GPT-4o Mini',
        configVersion: 2,
        pricing: { input: 0.15, output: 0.60, cache_read: 0.075 },
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
            max_completion_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 4096,
                min: 1,
                max: 16384
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
            }
        }
    },
    'gpt-4.1': {
        label: 'GPT-4.1',
        configVersion: 2,
        pricing: { input: 2, output: 8, cache_read: 0.50 },
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
            max_completion_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 32768
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
            }
        }
    },
    'gpt-4.1-mini': {
        label: 'GPT-4.1 Mini',
        configVersion: 2,
        pricing: { input: 0.40, output: 1.60, cache_read: 0.10 },
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
            max_completion_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 32768
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
            }
        }
    },
    'gpt-4.1-nano': {
        label: 'GPT-4.1 Nano',
        configVersion: 2,
        pricing: { input: 0.10, output: 0.40, cache_read: 0.025 },
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
            max_completion_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 4096,
                min: 1,
                max: 32768
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
            }
        }
    },
    'gpt-5.4': {
        label: 'GPT-5.4',
        configVersion: 2,
        pricing: { input: 2.50, output: 15, cache_read: 0.25 },
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
            max_completion_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 32768
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
            }
        }
    },
    'gpt-5.4-pro': {
        label: 'GPT-5.4 Pro',
        configVersion: 2,
        pricing: { input: 30, output: 180, cache_read: 3 },
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
            max_completion_tokens: {
                type: 'number',
                label: 'Max Output Tokens',
                description: 'Maximum number of tokens the model will generate in its response.',
                default: 8192,
                min: 1,
                max: 32768
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
            }
        }
    },
    'o3': {
        label: 'o3',
        configVersion: 2,
        pricing: { input: 10, output: 40, cache_read: 2.50 },
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
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
            }
        }
    },
    'o3-mini': {
        label: 'o3 Mini',
        configVersion: 2,
        pricing: { input: 1.10, output: 4.40, cache_read: 0.275 },
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
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
            }
        }
    },
    'o4-mini': {
        label: 'o4 Mini',
        configVersion: 2,
        pricing: { input: 1.10, output: 4.40, cache_read: 0.275 },
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
            },
            service_tier: {
                type: 'select',
                label: 'Service Tier',
                description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
                default: 'auto',
                options: ['auto', 'flex']
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

// ── Provider-side cost calculation ──────────────────────────────────────────
// Accounts for service tier (flex = half price) and prompt caching
// (cached input at 1/10th of standard input rate).

function computeCost(modelId, serviceTier, promptTokens, cachedTokens, completionTokens) {
    const modelEntry = models[modelId];
    if (!modelEntry || !modelEntry.pricing) {
        return null;
    }

    const pricing = modelEntry.pricing;
    const flexMultiplier = (serviceTier === 'flex') ? 0.5 : 1.0;

    const uncachedInput = promptTokens - cachedTokens;
    const inputRate = (pricing.input || 0) * flexMultiplier;
    const cacheRate = (pricing.cache_read || 0) * flexMultiplier;
    const outputRate = (pricing.output || 0) * flexMultiplier;

    let cost = 0;
    cost += uncachedInput * inputRate / 1_000_000;
    cost += cachedTokens * cacheRate / 1_000_000;
    cost += completionTokens * outputRate / 1_000_000;

    return cost;
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

        // All OpenAI models now use max_completion_tokens (max_tokens is deprecated).
        // Accept either key from stored config for backwards compatibility.
        const maxTokens = conf.max_completion_tokens || conf.max_tokens;
        if (maxTokens) {
            body.max_completion_tokens = maxTokens;
        }

        if (reasoning) {
            if (conf.reasoning_effort) {
                body.reasoning_effort = conf.reasoning_effort;
            }
        } else {
            if (conf.temperature !== undefined) {
                body.temperature = conf.temperature;
            }
            if (conf.reasoning_effort && conf.reasoning_effort !== 'none') {
                body.reasoning_effort = conf.reasoning_effort;
            }
        }

        // Service tier: "flex" gives half-price processing at higher latency.
        // Only send if explicitly set — omitting lets OpenAI use the default ("auto").
        const serviceTier = conf.service_tier || 'auto';
        if (serviceTier && serviceTier !== 'auto') {
            body.service_tier = serviceTier;
        }

        logProvider('api-call', { provider: 'openai', model, reasoning, serviceTier });

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

        // Extract token counts. OpenAI includes cached tokens within prompt_tokens,
        // so we subtract them to get the uncached count for accurate cost tracking.
        const promptTokens = data.usage?.prompt_tokens || 0;
        const completionTokens = data.usage?.completion_tokens || 0;
        const cachedTokens = data.usage?.prompt_tokens_details?.cached_tokens || 0;

        // Store input_tokens as uncached only (matching Anthropic convention where
        // input_tokens and cache tokens are separate). This keeps the DB columns
        // consistent across providers.
        const usage = {
            input_tokens: promptTokens - cachedTokens,
            output_tokens: completionTokens,
            cache_read_input_tokens: cachedTokens
        };

        // Compute cost provider-side, accounting for service tier and caching.
        const cost = computeCost(model, serviceTier, promptTokens, cachedTokens, completionTokens);
        if (cost != null) {
            usage.cost = cost;
        }

        logProvider('api-response', {
            provider: 'openai', model, serviceTier,
            input: usage.input_tokens, cached: cachedTokens,
            output: completionTokens, cost: cost != null ? cost.toFixed(6) : 'unknown'
        });

        return { text: choice.message.content, usage };
    };
}

// ── Pricing display ─────────────────────────────────────────────────────────
// Returns a human-readable pricing string for the admin UI.
// Accounts for service tier (flex = half price on all rates).

function formatPricing(modelId, config) {
    const modelEntry = models[modelId];
    if (!modelEntry || !modelEntry.pricing) return 'No pricing data';

    const pricing = modelEntry.pricing;
    const isFlex = config && config.service_tier === 'flex';
    const m = isFlex ? 0.5 : 1.0;

    const parts = [];
    if (pricing.input != null) parts.push('$' + (pricing.input * m) + ' in');
    if (pricing.output != null) parts.push('$' + (pricing.output * m) + ' out');
    if (pricing.cache_read != null) parts.push('$' + (pricing.cache_read * m) + ' cached');

    let result = parts.join(' / ') + ' per 1M tokens';
    if (isFlex) {
        result += ' (flex tier)';
    }
    return result;
}

module.exports = { name: 'openai', label: 'OpenAI', models, createCall, formatPricing };
