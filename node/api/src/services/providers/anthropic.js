// Anthropic provider — Claude model family.
// Handles structured system prompts with optional cache_control markers.

const { log } = require('../logger');
const { asNumber } = require('./coerce');
const { normalizeAnthropicStop, isTruncated } = require('./finish');

function logProvider(action, details) {
    log('provider', action, details);
}

// Models that reject sampling parameters (temperature/top_p/top_k) with a 400.
const SAMPLING_PARAMS_REJECTED = new Set([
    'claude-opus-4-7', 'claude-opus-4-8', 'claude-opus-5', 'claude-opus-5-5',
    'claude-sonnet-5', 'claude-fable-5-1'
]);

// How a model treats an OMITTED `thinking` parameter differs by generation
// (LLM-672). Older models (Opus 4.6-4.8, Sonnet 4.6) run WITHOUT thinking when
// it is omitted, so "off" means leaving it out. These two run adaptive
// thinking when it is omitted, so "off" must send {type: "disabled"}:
const THINKING_ON_BY_DEFAULT = new Set(['claude-opus-5', 'claude-sonnet-5']);
// And these cannot turn thinking off at all — {type: "disabled"} is a 400.
// Effort is the only control, so they offer no "off" option.
const THINKING_ALWAYS_ON = new Set(['claude-opus-5-5', 'claude-fable-5-1']);

// Shared capability blocks for the Claude 5-generation entries below.
const CACHE_PROMPTS_CAPABILITY = {
    type: 'boolean',
    label: 'Prompt Caching',
    description: 'Caches the static portion of the system prompt across calls. 5-minute TTL, 25% write premium, 90% read discount.',
    default: false
};

function maxTokensCapability() {
    return {
        type: 'number',
        label: 'Max Output Tokens',
        description: 'Maximum number of tokens the model will generate (thinking + response combined). Thinking counts against this, so leave headroom.',
        default: 16384,
        min: 1,
        max: 128000
    };
}

function thinkingEffortCapability(canTurnOff, defaultEffort, note) {
    const levels = ['low', 'medium', 'high', 'xhigh', 'max'];
    return {
        type: 'select',
        label: 'Thinking Effort',
        description: 'Controls how much the model thinks before responding. Higher effort produces better results on complex tasks but costs more tokens and time. ' + note,
        default: defaultEffort,
        options: canTurnOff ? ['off'].concat(levels) : levels
    };
}

// ── Model registry ──────────────────────────────────────────────────────────

const models = {
    // Claude 5 generation (LLM-672). None accept temperature. Pricing: dollars
    // per million tokens from the Anthropic model table, 2026-09-24; cache_write
    // is the 5-minute rate (1.25x input).
    'claude-fable-5-1': {
        label: 'Fable 5.1',
        apiId: 'claude-fable-5-1',
        configVersion: 1,
        pricing: { input: 10, output: 50, cache_write: 12.50, cache_read: 0.25 },
        capabilities: {
            max_tokens: maxTokensCapability(),
            thinking_effort: thinkingEffortCapability(false, 'medium',
                'Thinking is always on for Fable 5.1 and cannot be turned off. Requires an Anthropic organization with 30-day data retention; long requests can exceed the request timeout at high effort.'),
            cache_prompts: CACHE_PROMPTS_CAPABILITY
        }
    },
    'claude-opus-5-5': {
        label: 'Opus 5.5',
        apiId: 'claude-opus-5-5',
        configVersion: 1,
        pricing: { input: 4, output: 20, cache_write: 5, cache_read: 0.20 },
        capabilities: {
            max_tokens: maxTokensCapability(),
            thinking_effort: thinkingEffortCapability(false, 'medium',
                'Thinking is always on for Opus 5.5 and cannot be turned off; "medium" matches Anthropic\'s own default for this model.'),
            cache_prompts: CACHE_PROMPTS_CAPABILITY
        }
    },
    'claude-opus-5': {
        label: 'Opus 5',
        apiId: 'claude-opus-5',
        configVersion: 1,
        pricing: { input: 5, output: 25, cache_write: 6.25, cache_read: 0.50 },
        capabilities: {
            max_tokens: maxTokensCapability(),
            thinking_effort: thinkingEffortCapability(true, 'low',
                'Opus 5 thinks by default. "off" disables it, but Anthropic recommends low effort over off: with thinking off the model sometimes writes a tool call as text instead of calling it.'),
            cache_prompts: CACHE_PROMPTS_CAPABILITY
        }
    },
    'claude-sonnet-5': {
        label: 'Sonnet 5',
        apiId: 'claude-sonnet-5',
        configVersion: 1,
        pricing: { input: 2, output: 10, cache_write: 2.50, cache_read: 0.20 },
        capabilities: {
            max_tokens: maxTokensCapability(),
            thinking_effort: thinkingEffortCapability(true, 'low',
                'Sonnet 5 thinks by default. "off" disables it; the model is then less eager to use tools.'),
            cache_prompts: CACHE_PROMPTS_CAPABILITY
        }
    },
    'claude-opus-4-8': {
        label: 'Opus 4.8',
        apiId: 'claude-opus-4-8',
        configVersion: 1,
        pricing: { input: 5, output: 25, cache_write: 6.25, cache_read: 0.50 },
        capabilities: {
            max_tokens: maxTokensCapability(),
            thinking_effort: thinkingEffortCapability(true, 'off',
                '"off" disables thinking entirely. Opus 4.8 does not accept temperature.'),
            cache_prompts: CACHE_PROMPTS_CAPABILITY
        }
    },
    'claude-opus-4-7': {
        label: 'Opus 4.7',
        apiId: 'claude-opus-4-7',
        configVersion: 2,
        // Pricing: dollars per million tokens. Source: claude.com/pricing, 2026-04-20.
        pricing: { input: 5, output: 25, cache_write: 6.25, cache_read: 0.50 },
        capabilities: {
            // No temperature: Opus 4.7 rejects temperature/top_p/top_k with a 400.
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
                description: 'Controls how much the model thinks before responding. Higher effort produces better results on complex tasks but costs more tokens. "off" disables thinking entirely. Opus 4.7 uses adaptive thinking — extended thinking is not supported on this model.',
                default: 'off',
                // xhigh arrived with Opus 4.7; adding an option keeps stored configs valid, so no configVersion bump.
                options: ['off', 'low', 'medium', 'high', 'xhigh', 'max']
            },
            cache_prompts: {
                type: 'boolean',
                label: 'Prompt Caching',
                description: 'Caches the static portion of the system prompt across calls. 5-minute TTL, 25% write premium, 90% read discount.',
                default: false
            }
        }
    },
    'claude-opus-4-6': {
        label: 'Opus 4.6',
        apiId: 'claude-opus-4-6',
        configVersion: 2,
        // Pricing: dollars per million tokens. Source: claude.com/pricing, 2026-04-20.
        pricing: { input: 5, output: 25, cache_write: 6.25, cache_read: 0.50 },
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
        apiId: 'claude-sonnet-4-6',
        configVersion: 2,
        // Pricing: dollars per million tokens. Source: claude.com/pricing, 2026-04-20.
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
        configVersion: 1,
        // Pricing: dollars per million tokens. Source: claude.com/pricing, 2026-04-20.
        pricing: { input: 1, output: 5, cache_write: 1.25, cache_read: 0.10 },
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
                max: 64000
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

// ── Translate OpenAI-shape messages to Anthropic-shape ──────────────────────
// The neutral message shape is OpenAI's. Anthropic differs in two ways:
//   1. Tool calls are content blocks inside the assistant message
//      (type: "tool_use") rather than a separate tool_calls field.
//   2. Tool results are content blocks (type: "tool_result") inside a USER
//      message rather than a separate role:"tool" message. Consecutive
//      role:"tool" messages must be merged into a single user message
//      with multiple tool_result blocks.
function translateMessagesToAnthropic(openaiMessages) {
    const out = [];
    let pendingToolResults = null;

    function flushToolResults() {
        if (pendingToolResults && pendingToolResults.length > 0) {
            out.push({ role: 'user', content: pendingToolResults });
            pendingToolResults = null;
        }
    }

    for (const msg of openaiMessages) {
        if (msg.role === 'tool') {
            // Accumulate tool_result blocks; flush when next non-tool message lands.
            if (!pendingToolResults) pendingToolResults = [];
            pendingToolResults.push({
                type: 'tool_result',
                tool_use_id: msg.tool_call_id,
                content: typeof msg.content === 'string' ? msg.content : JSON.stringify(msg.content)
            });
            continue;
        }
        flushToolResults();

        if (msg.role === 'user') {
            out.push({ role: 'user', content: msg.content });
        } else if (msg.role === 'assistant') {
            // Assistant may carry tool_calls alongside text. Build a content
            // array when both are present; plain string when only text.
            const blocks = [];
            if (msg.content) {
                blocks.push({ type: 'text', text: msg.content });
            }
            if (Array.isArray(msg.tool_calls)) {
                for (const tc of msg.tool_calls) {
                    if (!tc.function || !tc.function.name) continue;
                    let input = {};
                    if (typeof tc.function.arguments === 'string') {
                        try { input = JSON.parse(tc.function.arguments); }
                        catch (e) { /* fall through with {} */ }
                    } else if (tc.function.arguments && typeof tc.function.arguments === 'object') {
                        input = tc.function.arguments;
                    }
                    blocks.push({
                        type: 'tool_use',
                        id: tc.id,
                        name: tc.function.name,
                        input: input
                    });
                }
            }
            out.push({
                role: 'assistant',
                content: blocks.length === 1 && blocks[0].type === 'text' ? blocks[0].text : blocks
            });
        }
        // role:"system" intentionally dropped — Anthropic uses a separate
        // top-level system field, not a system role in messages. Callers
        // pass system content via the systemPrompt argument.
    }
    flushToolResults();
    return out;
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

        // When opts.messages is provided, translate the OpenAI-shape array to
        // Anthropic's native shape. Otherwise fall back to the single-user-
        // message default. The provided messages are the FULL conversation —
        // engine includes the original perception as the first user message.
        const messages = (opts && Array.isArray(opts.messages) && opts.messages.length > 0)
            ? translateMessagesToAnthropic(opts.messages)
            : [{ role: 'user', content: userMessage }];

        const body = {
            model: model,
            max_tokens: asNumber(conf.max_tokens) || 4096,
            system: system,
            messages: messages
        };

        // Adaptive thinking — omit temperature entirely when thinking is active.
        // Effort is a sibling of thinking, under output_config — not a key inside it.
        if (THINKING_ALWAYS_ON.has(model)) {
            // Cannot be disabled. A stored "off" (or no setting) becomes the
            // lowest effort rather than a 400.
            body.thinking = { type: 'adaptive' };
            body.output_config = { effort: useThinking ? conf.thinking_effort : 'low' };
        } else if (useThinking) {
            body.thinking = { type: 'adaptive' };
            body.output_config = { effort: conf.thinking_effort };
        } else if (THINKING_ON_BY_DEFAULT.has(model)) {
            // Omitting `thinking` would run adaptive thinking on these models.
            body.thinking = { type: 'disabled' };
        } else if (!SAMPLING_PARAMS_REJECTED.has(model)) {
            const t = asNumber(conf.temperature);
            if (t !== undefined) {
                body.temperature = t;
            }
        }

        // Per-call stop sequences. Anthropic allows up to 4.
        if (opts && Array.isArray(opts.stop) && opts.stop.length > 0) {
            body.stop_sequences = opts.stop.slice(0, 4);
        }

        // Per-call tool definitions. Translate the neutral
        // { name, description, parameters } shape (per the providers/index.js
        // contract) to Anthropic's { name, description, input_schema } shape.
        const useTools = opts && Array.isArray(opts.tools) && opts.tools.length > 0;
        if (useTools) {
            body.tools = opts.tools.map(tool => ({
                name: tool.name,
                description: tool.description,
                input_schema: tool.parameters || { type: 'object', properties: {} }
            }));
        }

        logProvider('api-call', { provider: 'anthropic', model, cached: useCache, thinking: !!(body.thinking && body.thinking.type === 'adaptive'), tools: useTools });

        const response = await fetch('https://api.anthropic.com/v1/messages', {
            method: 'POST',
            headers,
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            const errorText = await response.text();
            logProvider('api-error', { provider: 'anthropic', model, status: response.status, error: errorText });
            // status rides on the error so retryWithBackoff can pick a
            // cadence by error class (deterministic 4xx vs outage/429).
            const apiError = new Error(`Anthropic API error ${response.status}: ${errorText}`);
            apiError.status = response.status;
            throw apiError;
        }

        const data = await response.json();
        const text = data.content
            .filter(block => block.type === 'text')
            .map(block => block.text)
            .join('\n');

        // Tool use blocks come back interleaved with text blocks. Hoist them
        // into a top-level array so callers don't have to re-walk content.
        // Empty array (not undefined) when no tools were called — keeps the
        // shape uniform whether or not tools were requested.
        const tool_calls = data.content
            .filter(block => block.type === 'tool_use')
            .map(block => ({ id: block.id, name: block.name, input: block.input }));

        const usage = {
            input_tokens: data.usage?.input_tokens || 0,
            output_tokens: data.usage?.output_tokens || 0,
            cache_creation_input_tokens: data.usage?.cache_creation_input_tokens || 0,
            cache_read_input_tokens: data.usage?.cache_read_input_tokens || 0
        };

        // stop_reason "max_tokens" means the response was cut at the token
        // ceiling — surface it so callers that persist output don't save a
        // partial document as if it were whole.
        const finish_reason = normalizeAnthropicStop(data.stop_reason);

        logProvider('api-response', { provider: 'anthropic', model, tool_calls: tool_calls.length, finish_reason, ...usage });

        return { text, tool_calls, usage, finish_reason, truncated: isTruncated(finish_reason) };
    };
}

module.exports = { name: 'anthropic', label: 'Anthropic', models, createCall, flattenPrompt };
