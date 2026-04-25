// Anthropic provider — Claude model family.
// Handles structured system prompts with optional cache_control markers.

const { log } = require('../logger');

function logProvider(action, details) {
    log('provider', action, details);
}

// ── Model registry ──────────────────────────────────────────────────────────

const models = {
    'claude-opus-4-7': {
        label: 'Opus 4.7',
        apiId: 'claude-opus-4-7',
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
                description: 'Controls how much the model thinks before responding. Higher effort produces better results on complex tasks but costs more tokens. "off" disables thinking entirely. Opus 4.7 uses adaptive thinking — extended thinking is not supported on this model.',
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
            max_tokens: conf.max_tokens || 4096,
            system: system,
            messages: messages
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

        logProvider('api-call', { provider: 'anthropic', model, cached: useCache, thinking: !!useThinking, tools: useTools });

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

        logProvider('api-response', { provider: 'anthropic', model, tool_calls: tool_calls.length, ...usage });

        return { text, tool_calls, usage };
    };
}

module.exports = { name: 'anthropic', label: 'Anthropic', models, createCall, flattenPrompt };
