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

// Reusable capability fragments — every gpt-5.x entry uses the same
// temperature/reasoning_effort/max-tokens/service-tier shape. Defining
// them once avoids the long copy-paste blocks that crept in across
// previous additions.

const TEMPERATURE_CAP = {
    type: 'number',
    label: 'Temperature',
    description: 'Controls randomness. Lower values are more focused and deterministic, higher values are more creative.',
    default: 1.0,
    min: 0,
    max: 2.0,
    step: 0.1,
    disabledWhen: {
        field: 'reasoning_effort',
        condition: 'notEquals',
        value: 'none',
        message: 'Not supported when reasoning is active'
    }
};

const REASONING_EFFORT_CAP = {
    type: 'select',
    label: 'Reasoning Effort',
    description: 'Controls how much time the model spends thinking. Supports none (default), low, medium, high, and xhigh.',
    default: 'none',
    options: ['none', 'low', 'medium', 'high', 'xhigh']
};

const MAX_OUTPUT_TOKENS_CAP = {
    type: 'number',
    label: 'Max Output Tokens',
    description: 'Maximum number of tokens the model will generate in its response.',
    default: 8192,
    min: 1,
    max: 32768
};

const SERVICE_TIER_CAP = {
    type: 'select',
    label: 'Service Tier',
    description: 'Processing tier. "auto" uses standard pricing. "flex" is half the cost but higher latency — good for background/batch work.',
    default: 'auto',
    options: ['auto', 'flex']
};

// Returns a fresh capabilities object per call. Caller-side normalizers
// (admin UI defaults, option filtering, label localization, etc.) can
// mutate fields without leaking the change across every model in the
// registry. Nested arrays and the disabledWhen object are spread/cloned
// for the same reason.
function gpt5Capabilities() {
    return {
        temperature: {
            ...TEMPERATURE_CAP,
            disabledWhen: { ...TEMPERATURE_CAP.disabledWhen }
        },
        reasoning_effort: {
            ...REASONING_EFFORT_CAP,
            options: [...REASONING_EFFORT_CAP.options]
        },
        max_completion_tokens: { ...MAX_OUTPUT_TOKENS_CAP },
        service_tier: {
            ...SERVICE_TIER_CAP,
            options: [...SERVICE_TIER_CAP.options]
        }
    };
}

// Lineup last reconciled against https://developers.openai.com/api/docs/pricing
// on 2026-05-01. The gpt-4.x family and the o-series (o3, o3-mini, o4-mini)
// were dropped in that pass — none were in active use and OpenAI no longer
// lists them on the public pricing page; reasoning is now integrated into
// the gpt-5.x family. cache_read for the *-pro tiers follows the standard
// 1/10-of-input convention since the pricing page omits an explicit cached
// rate for them — conservative for caching that does happen, and consistent
// with the rest of the family.

const models = {
    'gpt-5.5': {
        label: 'GPT-5.5',
        configVersion: 2,
        pricing: { input: 5.00, output: 30, cache_read: 0.50 },
        capabilities: gpt5Capabilities()
    },
    'gpt-5.5-pro': {
        label: 'GPT-5.5 Pro',
        configVersion: 2,
        pricing: { input: 30, output: 180, cache_read: 3 },
        capabilities: gpt5Capabilities()
    },
    'gpt-5.4': {
        label: 'GPT-5.4',
        configVersion: 2,
        pricing: { input: 2.50, output: 15, cache_read: 0.25 },
        capabilities: gpt5Capabilities()
    },
    'gpt-5.4-mini': {
        label: 'GPT-5.4 Mini',
        configVersion: 2,
        pricing: { input: 0.75, output: 4.50, cache_read: 0.075 },
        capabilities: gpt5Capabilities()
    },
    'gpt-5.4-nano': {
        label: 'GPT-5.4 Nano',
        configVersion: 2,
        pricing: { input: 0.20, output: 1.25, cache_read: 0.02 },
        capabilities: gpt5Capabilities()
    },
    'gpt-5.4-pro': {
        label: 'GPT-5.4 Pro',
        configVersion: 2,
        pricing: { input: 30, output: 180, cache_read: 3 },
        capabilities: gpt5Capabilities()
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

// ── Detect gpt-5.x family (uses /v1/responses) ──────────────────────────────
// gpt-5.x models reject function tools on /v1/chat/completions with the error:
//   "Function tools with reasoning_effort are not supported for gpt-5.5 in
//    /v1/chat/completions. Please use /v1/responses instead."
// Confirmed by direct probe (2026-04-29). The error mentions reasoning_effort
// because gpt-5.x has implicit reasoning enabled by default — sending tools
// triggers the constraint regardless of whether reasoning_effort is set in
// the request body. Route gpt-5.x calls through /v1/responses unconditionally;
// the endpoint accepts the same calls without tools too, so there's no need
// to branch on tool presence per call.
function usesResponsesEndpoint(modelId) {
    return modelId.startsWith('gpt-5.');
}

// ── Convert chat-completions messages to /v1/responses input ────────────────
// /v1/chat/completions uses messages: [{role, content, tool_calls?, tool_call_id?}]
// /v1/responses uses input: a flat list mixing role-content messages with
// {type:"function_call", call_id, name, arguments} and
// {type:"function_call_output", call_id, output} entries.
//
// Translation:
//   {role: "user", content: "..."}                      → as-is
//   {role: "assistant", content: "..."}                 → as-is (when no tool_calls)
//   {role: "assistant", content: "", tool_calls: [...]} → expand each tool_call
//                                                          into a function_call entry,
//                                                          dropping the assistant row
//                                                          itself (responses input
//                                                          doesn't carry an empty
//                                                          assistant message).
//   {role: "tool", content, tool_call_id}               → {type:"function_call_output",
//                                                          call_id: tool_call_id,
//                                                          output: content}
function messagesToResponsesInput(messages) {
    // First pass: collect call_ids referenced by both sides of the
    // tool-call/tool-result pairing. /v1/responses rejects input that
    // contains a function_call without a paired function_call_output
    // OR a function_call_output without a paired function_call —
    // strict pairing in both directions. /v1/chat/completions tolerates
    // either mismatch silently; chat history reconstructed for
    // chronicler-style multi-iteration harnesses can produce orphans
    // when an earlier fire bailed mid-loop or when truncation drops
    // one side of a pair, so this guard is necessary even for healthy
    // callers.
    const pairedCallIds = new Set();         // call_ids that have a tool-result row
    const assistantToolCallIds = new Set();  // call_ids that have an assistant tool_call row
    for (const msg of messages) {
        if (msg.role === 'tool' && msg.tool_call_id) {
            pairedCallIds.add(msg.tool_call_id);
        }
        if (msg.role === 'assistant' && Array.isArray(msg.tool_calls)) {
            for (const tc of msg.tool_calls) {
                if (tc && tc.id) assistantToolCallIds.add(tc.id);
            }
        }
    }

    const input = [];
    for (const msg of messages) {
        if (msg.role === 'tool') {
            // Drop orphan function_call_outputs — symmetric to the
            // function_call drop below. Without a matching assistant
            // tool_call earlier in the input, /v1/responses returns
            // "No tool call found for function call output with call_id <id>".
            // Also drops rows with missing/empty tool_call_id (malformed
            // history): the set won't contain `undefined` either, but
            // the explicit check makes the intent obvious.
            if (!msg.tool_call_id || !assistantToolCallIds.has(msg.tool_call_id)) continue;
            input.push({
                type: 'function_call_output',
                call_id: msg.tool_call_id,
                output: typeof msg.content === 'string' ? msg.content : JSON.stringify(msg.content),
            });
            continue;
        }
        if (msg.role === 'assistant' && Array.isArray(msg.tool_calls) && msg.tool_calls.length > 0) {
            // Emit assistant text content first if non-empty, then expand
            // each tool_call into a function_call entry. Most assistant
            // tool-call messages from chat-completions have empty content
            // ("" or null); skip the message row in that case to avoid
            // sending a content-less assistant turn that the API may reject.
            if (msg.content && typeof msg.content === 'string' && msg.content.trim() !== '') {
                input.push({ role: 'assistant', content: msg.content });
            }
            for (const tc of msg.tool_calls) {
                if (tc.type !== 'function' || !tc.function) continue;
                // Drop orphan function_calls — see the pairedCallIds
                // comment above. Without a matching tool result later in
                // the conversation, /v1/responses returns
                // "No tool output found for function call <id>".
                if (!pairedCallIds.has(tc.id)) continue;
                input.push({
                    type: 'function_call',
                    call_id: tc.id,
                    name: tc.function.name,
                    arguments: tc.function.arguments || '{}',
                });
            }
            continue;
        }
        // Plain role-content message (system/developer/user/assistant-text).
        input.push({ role: msg.role, content: msg.content });
    }
    return input;
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

// ── /v1/responses call path ─────────────────────────────────────────────────
// Used for gpt-5.x. Different request body (input vs messages, max_output_tokens
// vs max_completion_tokens, reasoning.effort vs reasoning_effort, top-level tool
// shape) and different response shape (output[] of typed items vs choices[]).
//
// Returns the same neutral { text, tool_calls, usage } the rest of the API
// expects, so callers don't have to know which endpoint was used.

async function callResponses(model, apiKey, conf, fullMessages, opts) {
    const body = {
        model: model,
        input: messagesToResponsesInput(fullMessages),
    };

    // /v1/responses uses max_output_tokens. Accept either stored config key.
    const maxTokens = conf.max_completion_tokens || conf.max_tokens;
    if (maxTokens) {
        body.max_output_tokens = maxTokens;
    }

    // reasoning.effort is the gpt-5.x equivalent of the o-series
    // reasoning_effort top-level field. Send only when the operator
    // explicitly opted in to a non-default level.
    if (conf.reasoning_effort && conf.reasoning_effort !== 'none') {
        body.reasoning = { effort: conf.reasoning_effort };
    }

    // Per-call tool definitions. /v1/responses uses a flat tool shape
    // — name/description/parameters at the top level of the tool object,
    // no nested "function" wrapper.
    const useTools = opts && Array.isArray(opts.tools) && opts.tools.length > 0;
    if (useTools) {
        body.tools = opts.tools.map(tool => ({
            type: 'function',
            name: tool.name,
            description: tool.description,
            parameters: tool.parameters || { type: 'object', properties: {} },
        }));
    }

    logProvider('api-call', { provider: 'openai', model, endpoint: 'responses', tools: useTools });

    const response = await fetch('https://api.openai.com/v1/responses', {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json',
            'Authorization': `Bearer ${apiKey}`,
        },
        body: JSON.stringify(body),
    });

    if (!response.ok) {
        const errorText = await response.text();
        logProvider('api-error', { provider: 'openai', model, endpoint: 'responses', status: response.status, error: errorText });
        throw new Error(`OpenAI API error ${response.status}: ${errorText}`);
    }

    const data = await response.json();
    if (!Array.isArray(data.output)) {
        throw new Error('OpenAI /v1/responses returned no output array');
    }

    // Aggregate text from "message" outputs and tool calls from "function_call"
    // outputs. The output array can contain both — a model that emits a tool
    // call AND a final-answer text gets two items. Tool calls in the neutral
    // shape are { id, name, input } where input is the JSON-parsed arguments.
    let text = '';
    const tool_calls = [];
    for (const item of data.output) {
        if (item.type === 'message' && Array.isArray(item.content)) {
            for (const part of item.content) {
                if (part.type === 'output_text' && typeof part.text === 'string') {
                    text += part.text;
                }
            }
        } else if (item.type === 'function_call' && item.name) {
            let input = {};
            if (item.arguments) {
                try {
                    input = JSON.parse(item.arguments);
                } catch (e) {
                    logProvider('tool-args-parse-error', { provider: 'openai', model, error: e.message });
                }
            }
            tool_calls.push({ id: item.call_id, name: item.name, input });
        }
    }

    // Usage shape on /v1/responses uses input_tokens / output_tokens
    // (not prompt_tokens / completion_tokens), and cached_tokens is nested
    // under input_tokens_details. output_tokens already includes any
    // reasoning_tokens — those are billed at the output rate, so the
    // existing per-model pricing math gives the right cost without
    // separate accounting.
    const promptTokens = data.usage?.input_tokens ?? 0;
    const completionTokens = data.usage?.output_tokens ?? 0;
    const rawCachedTokens = data.usage?.input_tokens_details?.cached_tokens ?? 0;
    const cachedTokens = Math.max(0, Math.min(rawCachedTokens, promptTokens));
    const uncachedInput = Math.max(0, promptTokens - cachedTokens);

    const appliedServiceTier = data.service_tier ?? 'default';

    const usage = {
        input_tokens: uncachedInput,
        output_tokens: completionTokens,
        cache_read_input_tokens: cachedTokens,
    };

    const cost = computeCost(model, appliedServiceTier, promptTokens, cachedTokens, completionTokens);
    if (cost != null) {
        usage.cost = cost;
    }

    logProvider('api-response', {
        provider: 'openai', model,
        endpoint: 'responses',
        serviceTier: appliedServiceTier,
        input: uncachedInput, cached: cachedTokens,
        output: completionTokens, cost: cost != null ? cost.toFixed(6) : 'unknown',
        tool_calls: tool_calls.length,
    });

    return { text, tool_calls, usage };
}

// ── API call factory ────────────────────────────────────────────────────────

function createCall(model, apiKey, configuration) {
    const conf = configuration || {};

    return async function call(systemPrompt, userMessage, opts) {
        const prompt = flattenPrompt(systemPrompt);
        const reasoning = isReasoningModel(model);

        // Reasoning models use "developer" role instead of "system".
        const systemRole = reasoning ? 'developer' : 'system';

        // OpenAI-shape messages array passes through directly. The engine sends
        // the full conversation (prior assistant tool_calls + user tool_result
        // messages) so the model sees its own observation history. System
        // prompt is always pre-pended separately.
        const userMessages = (opts && Array.isArray(opts.messages) && opts.messages.length > 0)
            ? opts.messages
            : [{ role: 'user', content: userMessage }];

        const fullMessages = [
            { role: systemRole, content: prompt },
            ...userMessages,
        ];

        // gpt-5.x family routes through /v1/responses — see usesResponsesEndpoint
        // comment for the rationale and the source error.
        if (usesResponsesEndpoint(model)) {
            return await callResponses(model, apiKey, conf, fullMessages, opts);
        }

        const body = {
            model: model,
            messages: fullMessages,
        };

        // All OpenAI models now use max_completion_tokens (max_tokens is deprecated).
        // Accept either key from stored config for backwards compatibility.
        const maxTokens = conf.max_completion_tokens || conf.max_tokens;
        if (maxTokens) {
            body.max_completion_tokens = maxTokens;
        }

        // Reasoning models (o-series) always use reasoning_effort, never temperature.
        // Non-reasoning models (gpt-5.x) support both, but temperature is only allowed
        // when reasoning_effort is "none" or unset — OpenAI rejects the combination.
        if (reasoning) {
            if (conf.reasoning_effort) {
                body.reasoning_effort = conf.reasoning_effort;
            }
        } else {
            var activeReasoning = conf.reasoning_effort && conf.reasoning_effort !== 'none';
            if (activeReasoning) {
                body.reasoning_effort = conf.reasoning_effort;
            } else if (conf.temperature !== undefined) {
                body.temperature = conf.temperature;
            }
        }

        // Service tier: "flex" gives half-price processing at higher latency.
        // Only send if explicitly set — omitting lets OpenAI use the default ("auto").
        const requestedServiceTier = conf.service_tier || 'auto';
        if (requestedServiceTier && requestedServiceTier !== 'auto') {
            body.service_tier = requestedServiceTier;
        }

        // Per-call stop sequences. OpenAI chat completions accept up to 4.
        if (opts && Array.isArray(opts.stop) && opts.stop.length > 0) {
            body.stop = opts.stop.slice(0, 4);
        }

        // Per-call tool definitions. Translate neutral shape to OpenAI's
        // function-tool wrapper. Empty parameters becomes an empty object schema
        // because OpenAI rejects function defs without a parameters field.
        const useTools = opts && Array.isArray(opts.tools) && opts.tools.length > 0;
        if (useTools) {
            body.tools = opts.tools.map(tool => ({
                type: 'function',
                function: {
                    name: tool.name,
                    description: tool.description,
                    parameters: tool.parameters || { type: 'object', properties: {} }
                }
            }));
        }

        logProvider('api-call', { provider: 'openai', model, reasoning, serviceTier: requestedServiceTier, tools: useTools });

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
        // Clamp cached to never exceed prompt to avoid negative values from bad data.
        const promptTokens = data.usage?.prompt_tokens ?? 0;
        const completionTokens = data.usage?.completion_tokens ?? 0;
        const rawCachedTokens = data.usage?.prompt_tokens_details?.cached_tokens ?? 0;
        const cachedTokens = Math.max(0, Math.min(rawCachedTokens, promptTokens));
        const uncachedInput = Math.max(0, promptTokens - cachedTokens);

        // Use the tier OpenAI actually applied, not just what we requested.
        // "auto" requests may resolve to a specific tier; flex requests confirm flex.
        const appliedServiceTier = data.service_tier ?? requestedServiceTier;

        // Store input_tokens as uncached only (matching Anthropic convention where
        // input_tokens and cache tokens are separate). This keeps the DB columns
        // consistent across providers.
        const usage = {
            input_tokens: uncachedInput,
            output_tokens: completionTokens,
            cache_read_input_tokens: cachedTokens
        };

        // Compute cost provider-side, accounting for actual service tier and caching.
        const cost = computeCost(model, appliedServiceTier, promptTokens, cachedTokens, completionTokens);
        if (cost != null) {
            usage.cost = cost;
        }

        // Tool calls come back on choice.message.tool_calls in OpenAI's shape:
        //   [{ id, type: "function", function: { name, arguments: "JSON STRING" } }]
        // Normalize to the neutral [{ id, name, input }] shape, parsing the
        // arguments string into an object. Malformed JSON falls back to {} so
        // the caller still sees the call (and can decide how to handle it).
        const tool_calls = (choice.message.tool_calls || [])
            .filter(tc => tc.type === 'function' && tc.function && tc.function.name)
            .map(tc => {
                let input = {};
                if (tc.function.arguments) {
                    try {
                        input = JSON.parse(tc.function.arguments);
                    } catch (e) {
                        logProvider('tool-args-parse-error', { provider: 'openai', model, error: e.message });
                    }
                }
                return { id: tc.id, name: tc.function.name, input };
            });

        logProvider('api-response', {
            provider: 'openai', model,
            serviceTier: appliedServiceTier,
            input: uncachedInput, cached: cachedTokens,
            output: completionTokens, cost: cost != null ? cost.toFixed(6) : 'unknown',
            tool_calls: tool_calls.length
        });

        return { text: choice.message.content || '', tool_calls, usage };
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
