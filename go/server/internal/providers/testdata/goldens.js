// Writes goldens.json beside this file: what the Node server's provider
// adapters send and return for a set of inputs, so the Go port's tests
// compare against Node itself. Run from the repository root, after `npm ci`
// in node/api:
//
//     node go/server/internal/providers/testdata/goldens.js
//
// config and the logger are stubbed, and globalThis.fetch is replaced by a
// stub that records each request and answers from the case's queue; nothing
// leaves the process.

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

const api = path.resolve(__dirname, '../../../../../node/api/src');

// Config: a plain map, with config.get's error for a missing key.
let configValues = {};
const configPath = require.resolve(path.join(api, 'services/config.js'));
require.cache[configPath] = {
    id: configPath, filename: configPath, loaded: true,
    exports: {
        get(key) {
            if (!(key in configValues)) {
                throw new Error(`Config key "${key}" not found. Was config.init() called?`);
            }
            return configValues[key];
        },
        set(key, value) { configValues[key] = value; },
    },
};
const loggerPath = require.resolve(path.join(api, 'services/logger.js'));
require.cache[loggerPath] = { id: loggerPath, filename: loggerPath, loaded: true, exports: { log() {}, logError() {} } };

// Every upstream goes to the same fake base, with a path prefix so the
// join of base and path is checked.
const BASE = 'http://upstream.test/prefix';
for (const name of ['OPENAI_API_BASE', 'ANTHROPIC_API_BASE', 'GOOGLE_API_BASE', 'OPENROUTER_API_BASE', 'PERPLEXITY_API_BASE', 'XAI_API_BASE']) {
    process.env[name] = BASE;
}

const providers = require(path.join(api, 'services/providers/index.js'));
const openrouter = require(path.join(api, 'services/providers/openrouter.js'));
const { coerceToolArgs, asNumber } = require(path.join(api, 'services/providers/coerce.js'));
const finish = require(path.join(api, 'services/providers/finish.js'));

const ENC_KEY = '000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f';
const BASE_CONFIG = { virtual_agent_request_timeout: '30', virtual_agent_encryption_key: ENC_KEY };

// The fetch stub. Each request is recorded as undici would put it on the
// wire: the headers object through `new Headers` (names lowercased, values
// combined), the body text as sent.
let queue = [];
let recorded = [];
globalThis.fetch = async function (url, init) {
    init = init || {};
    const headers = [];
    for (const [k, v] of new Headers(init.headers || {})) {
        headers.push([k, v]);
    }
    recorded.push({ method: init.method || 'GET', url: String(url), headers, body: init.body === undefined ? null : init.body });
    const reply = queue.shift() || { status: 599, body: 'no reply queued' };
    if (reply.delay) {
        await new Promise(resolve => setTimeout(resolve, reply.delay));
    }
    const text = typeof reply.body === 'string' ? reply.body : JSON.stringify(reply.body);
    return {
        ok: reply.status >= 200 && reply.status <= 299,
        status: reply.status,
        text: async () => text,
        json: async () => JSON.parse(text),
    };
};

function ok(body) {
    return { status: 200, body };
}

// ── Canned answers ─────────────────────────────────────────────────────────

const anthropicReply = ok({
    content: [
        { type: 'thinking', thinking: '' },
        { type: 'text', text: 'first' },
        { type: 'tool_use', id: 'tu_1', name: 'look', input: { dir: 'north' } },
        { type: 'text', text: 'second' },
    ],
    stop_reason: 'tool_use',
    usage: { input_tokens: 10, output_tokens: 2, cache_creation_input_tokens: 5, cache_read_input_tokens: 7 },
});

const chatReply = ok({
    choices: [{
        message: {
            content: 'hi',
            tool_calls: [
                { id: 'c1', type: 'function', function: { name: 'look', arguments: '{"dir":"north","n":"3"}' } },
                { id: 'c2', type: 'function', function: { name: 'bad', arguments: '{not json' } },
                { id: 'c3', type: 'other', function: { name: 'skip' } },
                { id: 'c4', type: 'function', function: { name: '' } },
            ],
        },
        finish_reason: 'tool_calls',
    }],
    usage: { prompt_tokens: 1000, completion_tokens: 50, prompt_tokens_details: { cached_tokens: 200 } },
    service_tier: 'flex',
});

const responsesReply = ok({
    status: 'completed',
    output: [
        { type: 'reasoning' },
        { type: 'message', content: [{ type: 'output_text', text: 'a' }, { type: 'refusal', text: 'x' }, { type: 'output_text', text: 'b' }] },
        { type: 'function_call', call_id: 'fc1', name: 'look', arguments: '{"x":1}' },
        { type: 'function_call', call_id: 'fc2', name: 'look', arguments: 'nope' },
    ],
    usage: { input_tokens: 300000, output_tokens: 10, input_tokens_details: { cached_tokens: 100 } },
});

const googleReply = ok({
    candidates: [{
        content: { parts: [{ text: 'a' }, { text: '' }, { text: 'b' }, { functionCall: { name: 'look', args: { x: 1 } } }, { functionCall: { name: 'wait' } }] },
        finishReason: 'MAX_TOKENS',
    }],
    usageMetadata: { promptTokenCount: 12, candidatesTokenCount: 3 },
});

const catalogReply = ok({
    data: [
        { id: 'deepseek/deepseek-v4-flash', name: 'DeepSeek V4 Flash', context_length: 64000, pricing: { prompt: '0.00000014', completion: '0.00000028', input_cache_read: '0.000000028' } },
        { id: 'x/no-cache', pricing: { prompt: '0.000001', completion: '0.000002' } },
        { id: 'x/garbage', pricing: { prompt: 'abc', completion: '1e-6xyz', input_cache_read: '.5e-7' } },
        { id: 'x/no-pricing' },
        { id: 7, pricing: { prompt: '0.1' } },
        { id: 'x/no-cache', name: 'second copy', pricing: { prompt: '0.000003' } },
    ],
});

const openrouterReply = ok({
    choices: [{
        message: {
            content: 'ok',
            reasoning: 'thinking out loud',
            tool_calls: [{ id: 'c1', type: 'function', function: { name: 'look', arguments: '{"dir":"north","n":"3","flag":"TRUE","list":"[\\"1\\",2]"}' } }],
        },
        finish_reason: 'length',
    }],
    usage: { prompt_tokens: 1000, completion_tokens: 20, prompt_tokens_details: { cached_tokens: 400 } },
    provider: '  DeepInfra \n',
});

const perplexityReply = ok({
    choices: [{ message: { content: 'answer [1]' }, finish_reason: 'stop' }],
    search_results: [
        { title: 'First', url: 'https://a.example/1', date: '2026-07-01' },
        { url: 'https://b.example/2' },
        { title: 'No url' },
    ],
    citations: ['https://old.example'],
    usage: { prompt_tokens: 10, completion_tokens: 5 },
});

const xaiReply = ok({
    status: 'incomplete',
    incomplete_details: { reason: 'max_output_tokens' },
    output: [
        { type: 'output_text', text: 'one' },
        { type: 'message', content: [{ type: 'text', text: 'two' }, { type: 'output_text' }, { type: 'image' }] },
        { type: 'output_text' },
    ],
    citations: ['https://a.example', { url: 'https://b.example', title: 'B' }, { title: 'no url' }, 42],
    usage: { input_tokens: 100, output_tokens: 5, input_tokens_details: { cached_tokens: 10 } },
});

// ── Inputs ─────────────────────────────────────────────────────────────────

const TOOLS = [
    { name: 'look', description: 'Look around', parameters: { type: 'object', properties: { dir: { type: 'string' }, n: { type: 'integer' }, flag: { type: 'boolean' }, list: { type: 'array', items: { type: 'integer' } } } } },
    { name: 'wait' },
    { name: 5 },
    null,
    ['not', 'a', 'tool'],
];

// A tool loop in the neutral shape: opener, two rounds of calls and results.
function history(rounds) {
    const messages = [{ role: 'system', content: 'ignored system row' }, { role: 'user', content: 'The court is in session.' }];
    for (let i = 1; i <= rounds; i++) {
        messages.push({
            role: 'assistant',
            content: i === 1 ? 'Reading record.' : '',
            tool_calls: [
                { id: 'call_' + i, type: 'function', function: { name: 'read_record', arguments: '{"n":' + i + '}' } },
                { id: 'gemini-look-' + i, type: 'function', function: { name: 'look', arguments: { dir: 'east' } } },
            ],
        });
        messages.push({ role: 'tool', tool_call_id: 'call_' + i, content: 'record ' + i });
        messages.push({ role: 'tool', tool_call_id: 'gemini-look-' + i, content: { seen: ['a door'] } });
    }
    return messages;
}

// Orphans on both sides, for /v1/responses pairing.
const ORPHANS = [
    { role: 'user', content: 'start' },
    { role: 'assistant', content: '  ', tool_calls: [{ id: 'paired', type: 'function', function: { name: 'a', arguments: '' } }, { id: 'lonely', type: 'function', function: { name: 'b', arguments: '{}' } }, { id: 'paired2', type: 'nope', function: { name: 'c' } }, { id: 'paired3', type: 'function' }, { id: 'paired4', type: 'function', function: 0 }] },
    { role: 'tool', tool_call_id: 'paired', content: 'ok' },
    { role: 'tool', tool_call_id: 'paired2', content: 'output of a skipped call' },
    { role: 'tool', tool_call_id: 'paired3', content: 'output of a call with no function' },
    { role: 'tool', tool_call_id: 'paired4', content: 'output of a call with a falsy function' },
    { role: 'tool', tool_call_id: 'stranger', content: 'orphan output' },
    { role: 'tool', content: 'no id' },
    { role: 'developer', content: 'dev note', extra: 1 },
    { role: 'user' },
];

const SYS = 'You are a test.';
const STRUCT = { static: 'static rules', dynamic: 'dynamic part' };
const STOPS = ['END', '', 7, 'STOP', 'HALT', 'QUIT', 'EXTRA'];

const cases = [];
function add(c) {
    cases.push(Object.assign({ apiKey: 'sk-test', system: SYS, user: 'hello', opts: {} }, c));
}

// Anthropic
const anthropicModels = ['claude-opus-4-6', 'claude-opus-4-7', 'claude-opus-4-8', 'claude-opus-5', 'claude-sonnet-5', 'claude-opus-5-5', 'claude-fable-5-1', 'claude-haiku-5-5', 'claude-haiku-4-5', 'claude-unknown'];
const efforts = [undefined, 'off', 'high', 'xhigh', '', 3];
for (const model of anthropicModels) {
    for (const effort of efforts) {
        const conf = { temperature: '0.4', max_tokens: '2048' };
        if (effort !== undefined) conf.thinking_effort = effort;
        add({ name: `anthropic ${model} effort ${JSON.stringify(effort)}`, provider: 'anthropic', model, conf, responses: [anthropicReply] });
    }
}
add({ name: 'anthropic cache, structured, tools, stop, history, turn context', provider: 'Anthropic', model: 'claude-opus-5-5', conf: { cache_prompts: true, max_tokens: 0 }, system: STRUCT, opts: { cache: true, tools: TOOLS, stop: STOPS, messages: history(2), turnContext: 'volatile now' }, responses: [anthropicReply] });
add({ name: 'anthropic cache with plain prompt', provider: 'anthropic', model: 'claude-opus-5-5', conf: { cache_prompts: true }, opts: { cache: true, messages: [{ role: 'user', content: 'perception' }], turnContext: 'ctx' }, responses: [anthropicReply] });
add({ name: 'anthropic cache, empty static', provider: 'anthropic', model: 'claude-opus-5-5', conf: { cache_prompts: 1 }, system: { static: '', dynamic: 'dyn' }, opts: { cache: true }, responses: [anthropicReply] });
add({ name: 'anthropic cache off, turn context', provider: 'anthropic', model: 'claude-opus-5-5', conf: {}, opts: { cache: true, messages: [{ role: 'user', content: 'A perception.' }], turnContext: 'volatile' }, responses: [anthropicReply] });
add({ name: 'anthropic cache, empty last text', provider: 'anthropic', model: 'claude-opus-5-5', conf: { cache_prompts: true }, system: STRUCT, opts: { cache: true, messages: [{ role: 'user', content: '' }], turnContext: 'ctx' }, responses: [anthropicReply] });
add({ name: 'anthropic cache, array content', provider: 'anthropic', model: 'claude-opus-5-5', conf: { cache_prompts: true }, system: STRUCT, opts: { cache: true, messages: [{ role: 'user', content: [{ type: 'text', text: 'a' }, { type: 'image', source: {} }] }], turnContext: 'ctx' }, responses: [anthropicReply] });
add({ name: 'anthropic cache, array content empty text last', provider: 'anthropic', model: 'claude-opus-5-5', conf: { cache_prompts: true }, system: STRUCT, opts: { cache: true, messages: [{ role: 'user', content: [{ type: 'text', text: '' }] }] }, responses: [anthropicReply] });
add({ name: 'anthropic cache, primitive last block', provider: 'anthropic', model: 'claude-opus-5-5', conf: { cache_prompts: true }, system: STRUCT, opts: { cache: true, messages: [{ role: 'user', content: ['plain', 5] }] }, responses: [anthropicReply] });
add({ name: 'anthropic cache, null last block', provider: 'anthropic', model: 'claude-opus-5-5', conf: { cache_prompts: true }, system: STRUCT, opts: { cache: true, messages: [{ role: 'user', content: [null] }] }, responses: [anthropicReply] });
add({ name: 'anthropic turn context after assistant', provider: 'anthropic', model: 'claude-opus-5-5', conf: {}, opts: { messages: [{ role: 'user', content: 'p' }, { role: 'assistant', content: 'said' }], turnContext: 'ctx' }, responses: [anthropicReply] });
add({ name: 'anthropic turn context object content', provider: 'anthropic', model: 'claude-opus-5-5', conf: {}, opts: { messages: [{ role: 'user', content: { odd: true } }], turnContext: 'ctx' }, responses: [anthropicReply] });
add({ name: 'anthropic custom headers', provider: 'anthropic', model: 'claude-opus-4-6', conf: { headers: { 'anthropic-beta': 'x', 'x-api-key': 'override', 'Content-type': 'text/plain', n: 5 } }, responses: [anthropicReply] });
add({ name: 'anthropic header above latin-1', provider: 'anthropic', model: 'claude-opus-4-6', conf: { headers: { 'x-ok': 'café', 'x-snow': 'a ☃' } }, responses: [anthropicReply] });
add({ name: 'anthropic header astral', provider: 'anthropic', model: 'claude-opus-4-6', conf: { headers: { 'x-e': '😀' } }, responses: [anthropicReply] });
add({ name: 'anthropic bad header name', provider: 'anthropic', model: 'claude-opus-4-6', conf: { headers: { 'bad name': 'v' } }, responses: [anthropicReply] });
add({ name: 'anthropic bad header value', provider: 'anthropic', model: 'claude-opus-4-6', conf: { headers: { 'x-a': ' a\nb ' } }, responses: [anthropicReply] });
add({ name: 'anthropic header record converted first', provider: 'anthropic', model: 'claude-opus-4-6', conf: { headers: { 'bad name': 'v', 'x-b': '☃' } }, responses: [anthropicReply] });
add({ name: 'anthropic headers string', provider: 'anthropic', model: 'claude-opus-4-6', conf: { headers: 'ab' }, responses: [anthropicReply] });
add({ name: 'anthropic assistant odd tool calls', provider: 'anthropic', model: 'claude-opus-4-6', conf: {}, opts: { messages: [{ role: 'user', content: 'p' }, { role: 'assistant', content: '', tool_calls: [{ id: 1, function: { name: 'n', arguments: '[1,2]' } }, { function: { name: 'm', arguments: 'bad' } }, { function: { name: 'k', arguments: 7 } }, { function: 'str' }, 5, { function: { name: 'o', arguments: [1] } }] }, { role: 'assistant' }, { role: 'tool', content: null }] }, responses: [anthropicReply] });
add({ name: 'anthropic null tool call', provider: 'anthropic', model: 'claude-opus-4-6', conf: {}, opts: { messages: [{ role: 'assistant', tool_calls: [null] }] }, responses: [anthropicReply] });
add({ name: 'anthropic max_tokens toString object', provider: 'anthropic', model: 'claude-opus-4-6', conf: { max_tokens: { toString: 1 } }, responses: [anthropicReply] });
add({ name: 'anthropic content not array', provider: 'anthropic', model: 'claude-opus-4-6', conf: {}, responses: [ok({ content: {} })] });
add({ name: 'anthropic content missing', provider: 'anthropic', model: 'claude-opus-4-6', conf: {}, responses: [ok({})] });
add({ name: 'anthropic null body', provider: 'anthropic', model: 'claude-opus-4-6', conf: {}, responses: [ok(null)] });
add({ name: 'anthropic odd blocks', provider: 'anthropic', model: 'claude-opus-4-6', conf: {}, responses: [ok({ content: [{ type: 'text', text: 5 }, { type: 'text' }, { type: 'text', text: null }, { type: 'tool_use', name: 'x' }], usage: { input_tokens: '12', output_tokens: 0 }, stop_reason: 'max_tokens' })] });
add({ name: 'anthropic null block', provider: 'anthropic', model: 'claude-opus-4-6', conf: {}, responses: [ok({ content: [null] })] });
add({ name: 'anthropic api error', provider: 'anthropic', model: 'claude-opus-4-6', conf: {}, responses: [{ status: 429, body: '{"error":"rate limited"}' }] });
add({ name: 'anthropic no configuration', provider: 'anthropic', model: 'claude-opus-4-6', responses: [anthropicReply] });
add({ name: 'anthropic opts not object', provider: 'anthropic', model: 'claude-opus-4-6', conf: {}, opts: 'cache', responses: [anthropicReply] });

// OpenAI chat completions and responses
const openaiConfs = [
    {},
    { temperature: 0.3, max_tokens: '100' },
    { reasoning_effort: 'none', temperature: '0.3', max_completion_tokens: 50, service_tier: 'flex' },
    { reasoning_effort: 'high', temperature: 0.3, service_tier: 'auto' },
    { reasoning_effort: '', service_tier: 'priority' },
];
for (const model of ['gpt-4o-mini', 'o3', 'gpt-5.5', 'gpt-6.1-sol', 'gpt-6-luna']) {
    for (const conf of openaiConfs) {
        add({ name: `openai ${model} ${JSON.stringify(conf)}`, provider: 'openai', model, conf, responses: [model.startsWith('gpt-5') || model.startsWith('gpt-6') ? responsesReply : chatReply] });
    }
}
add({ name: 'openai chat history tools stop turn context', provider: 'openai', model: 'gpt-4o-mini', conf: {}, system: STRUCT, opts: { tools: TOOLS, stop: STOPS, messages: history(1), turnContext: 'ctx' }, responses: [chatReply] });
add({ name: 'openai responses history tools', provider: 'openai', model: 'gpt-5.4-mini', conf: {}, opts: { tools: TOOLS, stop: STOPS, messages: history(2), turnContext: 'ctx' }, responses: [responsesReply] });
add({ name: 'openai responses orphans', provider: 'openai', model: 'gpt-5.4', conf: {}, opts: { messages: ORPHANS }, responses: [responsesReply] });
add({ name: 'openai responses null tool call', provider: 'openai', model: 'gpt-5.4', conf: {}, opts: { messages: [{ role: 'assistant', tool_calls: [null] }] }, responses: [responsesReply] });
add({ name: 'openai responses incomplete', provider: 'openai', model: 'gpt-5.4', conf: {}, responses: [ok({ status: 'incomplete', incomplete_details: { reason: 'content_filter' }, output: [], usage: { input_tokens: 5, output_tokens: 1, input_tokens_details: { cached_tokens: 9 } }, service_tier: 'flex' })] });
add({ name: 'openai responses flex, answer reports default', provider: 'openai', model: 'gpt-5.4-nano', conf: { service_tier: 'flex' }, responses: [ok({ status: 'completed', output: [], usage: { input_tokens: 1000, output_tokens: 10 }, service_tier: 'default' })] });
add({ name: 'openai responses flex reported', provider: 'openai', model: 'gpt-6.1-terra', conf: { service_tier: 'flex' }, responses: [ok({ status: 'completed', output: [], usage: { input_tokens: 1000, output_tokens: 10 }, service_tier: 'flex' })] });
add({ name: 'openai responses service_tier non-string', provider: 'openai', model: 'gpt-5.4', conf: { service_tier: 7 }, responses: [responsesReply] });
add({ name: 'openai responses no output', provider: 'openai', model: 'gpt-5.4', conf: {}, responses: [ok({ status: 'completed' })] });
add({ name: 'openai responses unknown model', provider: 'openai', model: 'gpt-5.9-new', conf: {}, responses: [responsesReply] });
add({ name: 'openai chat no choices', provider: 'openai', model: 'gpt-4o-mini', conf: {}, responses: [ok({ choices: [] })] });
add({ name: 'openai chat choices string', provider: 'openai', model: 'gpt-4o-mini', conf: {}, responses: [ok({ choices: 'abc' })] });
add({ name: 'openai chat tool_calls object', provider: 'openai', model: 'gpt-4o-mini', conf: {}, responses: [ok({ choices: [{ message: { content: null, tool_calls: {} } }] })] });
add({ name: 'openai chat odd content and usage', provider: 'openai', model: 'gpt-4o-mini', conf: {}, responses: [ok({ choices: [{ message: { content: 5 } }], usage: { prompt_tokens: 10, prompt_tokens_details: { cached_tokens: 50 } } })] });
add({ name: 'openai chat null tool call', provider: 'openai', model: 'gpt-4o-mini', conf: {}, responses: [ok({ choices: [{ message: { tool_calls: [null] } }] })] });
add({ name: 'openai chat usage toString object', provider: 'openai', model: 'gpt-4o-mini', conf: {}, responses: [ok({ choices: [{ message: { content: 'x' } }], usage: { prompt_tokens: { toString: 1 } } })] });
add({ name: 'openrouter usage toString object', provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', conf: {}, responses: [ok({ choices: [{ message: { content: 'x' } }], usage: { prompt_tokens: 5, completion_tokens: { toString: 1 } } }), catalogReply] });
add({ name: 'xai citations length object', provider: 'xai', model: 'grok-4.1-fast-non-reasoning', conf: {}, responses: [ok({ output: [{ type: 'output_text', text: 'x' }], citations: { length: { toString: 1 } } })] });
add({ name: 'openai api error', provider: 'openai', model: 'gpt-5.5', conf: {}, responses: [{ status: 500, body: 'upstream broke' }] });

// Google
add({ name: 'google plain', provider: 'google', model: 'gemini-2.5-pro', conf: { max_tokens: '8192', temperature: 0, top_p: '0.9', top_k: 40, thinking_budget: '0' }, responses: [googleReply] });
add({ name: 'google alias, no conf numbers', provider: 'GEMINI', model: 'gemini-2.5-flash', conf: { temperature: '', top_k: 'abc' }, system: STRUCT, responses: [googleReply] });
add({ name: 'google history tools stop turn context', provider: 'google', model: 'gemini-2.5-flash', conf: {}, opts: { tools: TOOLS, stop: STOPS, messages: history(2).concat([{ role: 'tool', tool_call_id: 'gemini-', content: 'x' }, { role: 'tool', tool_call_id: '', content: 'y' }, { role: 'assistant' }, { role: 'user' }]), turnContext: 'ctx' }, responses: [googleReply] });
add({ name: 'google numeric tool_call_id', provider: 'google', model: 'gemini-2.5-flash', conf: {}, opts: { messages: [{ role: 'tool', tool_call_id: 5, content: 'x' }] }, responses: [googleReply] });
add({ name: 'google thinking tokens', provider: 'google', model: 'gemini-2.5-flash', conf: {}, responses: [ok({ candidates: [{ content: { parts: [{ text: 'a' }] }, finishReason: 'STOP' }], usageMetadata: { promptTokenCount: 12, candidatesTokenCount: 3, thoughtsTokenCount: 900, totalTokenCount: 915 } })] });
add({ name: 'google thinking tokens, no candidates count', provider: 'google', model: 'gemini-2.5-pro', conf: {}, responses: [ok({ candidates: [{ content: { parts: [{ text: '' }] }, finishReason: 'MAX_TOKENS' }], usageMetadata: { promptTokenCount: 12, thoughtsTokenCount: 8192 } })] });
add({ name: 'google thinking tokens, string counts', provider: 'google', model: 'gemini-2.5-flash', conf: {}, responses: [ok({ candidates: [{ content: { parts: [{ text: 'a' }] } }], usageMetadata: { candidatesTokenCount: '3', thoughtsTokenCount: 5 } })] });
add({ name: 'google thinking tokens, string thoughts', provider: 'google', model: 'gemini-2.5-flash', conf: {}, responses: [ok({ candidates: [{ content: { parts: [{ text: 'a' }] } }], usageMetadata: { candidatesTokenCount: 3, thoughtsTokenCount: '5' } })] });
add({ name: 'google thinking tokens, usageMetadata string', provider: 'google', model: 'gemini-2.5-flash', conf: {}, responses: [ok({ candidates: [{ content: { parts: [{ text: 'a' }] } }], usageMetadata: 'abc' })] });
add({ name: 'google no candidates', provider: 'google', model: 'gemini-2.5-flash', conf: {}, responses: [ok({ candidates: [] })] });
add({ name: 'google parts object', provider: 'google', model: 'gemini-2.5-flash', conf: {}, responses: [ok({ candidates: [{ content: { parts: {} } }] })] });
add({ name: 'google null part', provider: 'google', model: 'gemini-2.5-flash', conf: {}, responses: [ok({ candidates: [{ content: { parts: [null] } }] })] });
add({ name: 'google api error', provider: 'google', model: 'gemini-2.5-flash', conf: {}, responses: [{ status: 400, body: 'bad' }] });

// OpenRouter
add({ name: 'openrouter billed cost', provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', conf: { thinking_effort: 'off', provider: { order: ['deepinfra'], allow_fallbacks: false }, max_tokens: 512, temperature: '0.7' }, opts: { tools: TOOLS, stop: STOPS }, responses: [ok(Object.assign({}, openrouterReply.body, { usage: { prompt_tokens: 1000, completion_tokens: 20, cost: 0.000123 } }))] });
add({ name: 'openrouter catalog cost, coercion, served_by', provider: 'open-router', model: 'deepseek/deepseek-v4-flash', conf: { thinking_effort: 'high' }, opts: { tools: TOOLS, messages: history(1), turnContext: 'ctx' }, responses: [openrouterReply, catalogReply] });
add({ name: 'openrouter catalog garbage prices', provider: 'openrouter', model: 'x/garbage', conf: {}, responses: [ok({ choices: [{ message: { content: 'ok' } }], usage: { prompt_tokens: 10, completion_tokens: 2 } }), catalogReply] });
add({ name: 'openrouter catalog duplicate id', provider: 'openrouter', model: 'x/no-cache', conf: {}, responses: [ok({ choices: [{ message: { content: 'ok' } }], usage: { prompt_tokens: 10, completion_tokens: 2, prompt_tokens_details: {} } }), catalogReply] });
add({ name: 'openrouter catalog failure', provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', conf: {}, responses: [ok({ choices: [{ message: { content: 'ok' } }], usage: { prompt_tokens: 10, completion_tokens: 2, cost: -1 } }), { status: 503, body: 'down' }] });
add({ name: 'openrouter catalog data object', provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', conf: {}, responses: [ok({ choices: [{ message: { content: 'ok' } }], usage: { prompt_tokens: 10, completion_tokens: 2, cost: 'n/a' } }), ok({ data: {} })] });
add({ name: 'openrouter catalog null model', provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', conf: {}, responses: [ok({ choices: [{ message: { content: 'ok' } }], usage: { prompt_tokens: 10, completion_tokens: 2 } }), ok({ data: [null] })] });
add({ name: 'openrouter unknown efforts and routing', provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', conf: { thinking_effort: 'constructor', provider: ['deepinfra'] }, responses: [ok({ choices: [{ message: { content: 'ok' } }], usage: { cost: 0 }, provider: 42 })] });
add({ name: 'openrouter tool calls without opts', provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', conf: {}, opts: null, responses: [ok({ choices: [{ message: { tool_calls: [{ id: 'c', type: 'function', function: { name: 'x' } }] } }], usage: { cost: 0 } })] });
add({ name: 'openrouter tool_calls object', provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', conf: {}, responses: [ok({ choices: [{ message: { tool_calls: 5 } }], usage: { cost: 0 } })] });
add({ name: 'openrouter long served_by', provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', conf: {}, responses: [ok({ choices: [{ message: { content: 'ok' } }], usage: { cost: 0 }, provider: 'y'.repeat(150) })] });

// Perplexity
add({ name: 'perplexity search results', provider: 'perplexity', model: 'sonar', conf: { max_tokens: 1024, temperature: 0.2, search_recency_filter: 'week' }, opts: { tools: TOOLS, stop: STOPS, messages: history(1) }, responses: [perplexityReply] });
add({ name: 'perplexity legacy citations', provider: 'perplexity', model: 'sonar-pro', conf: { search_recency_filter: '' }, responses: [ok({ choices: [{ message: { content: 'a' } }], citations: ['https://x', null, 3], search_results: [] })] });
add({ name: 'perplexity citations off', provider: 'perplexity', model: 'sonar', conf: { return_citations: false }, responses: [perplexityReply] });
add({ name: 'perplexity null content with sources', provider: 'perplexity', model: 'sonar', conf: { return_citations: 0 }, responses: [ok({ choices: [{ message: { content: null } }], citations: ['https://x'] })] });
add({ name: 'perplexity undefined content no sources', provider: 'perplexity', model: 'sonar', conf: {}, responses: [ok({ choices: [{ message: {} }] })] });

// xAI
add({ name: 'xai search tools', provider: 'xai', model: 'grok-4.20-reasoning', conf: { x_search: true, web_search: 1, max_output_tokens: '4096', temperature: 0.5 }, opts: { tools: TOOLS, stop: STOPS, messages: history(1) }, responses: [xaiReply] });
add({ name: 'xai plain', provider: 'xai', model: 'grok-4.1-fast-non-reasoning', conf: {}, responses: [ok({ output: [{ type: 'message', content: 'ab' }, { type: 'message', content: [{ type: 'text', text: 'hi' }] }], usage: { input_tokens: 10, output_tokens: 2, cache_read_input_tokens: 20 } })] });
add({ name: 'xai no text', provider: 'xai', model: 'grok-4.1-fast-non-reasoning', conf: {}, responses: [ok({ output: [] })] });
add({ name: 'xai content object', provider: 'xai', model: 'grok-4.1-fast-non-reasoning', conf: {}, responses: [ok({ output: [{ type: 'message', content: {} }] })] });
add({ name: 'xai citations string', provider: 'xai', model: 'grok-4.1-fast-non-reasoning', conf: {}, responses: [ok({ output: [{ type: 'output_text', text: 'x' }], citations: 'abc' })] });
add({ name: 'xai unknown model', provider: 'xai', model: 'grok-9', conf: {}, responses: [ok({ output: [{ type: 'output_text', text: 'x' }] })] });

// The wrapper
add({ name: 'unsupported provider', provider: 'nope', model: 'x', conf: {}, responses: [] });
add({ name: 'prototype provider', provider: 'Constructor', model: 'gpt-x', conf: {}, responses: [] });
add({ name: 'timeout', provider: 'openai', model: 'gpt-4o-mini', conf: {}, config: { virtual_agent_request_timeout: '-5' }, responses: [Object.assign({ delay: 200 }, chatReply)] });
add({ name: 'timeout config missing', provider: 'openai', model: 'gpt-4o-mini', conf: {}, config: {}, responses: [chatReply] });
add({ name: 'opts sanitized away', provider: 'openai', model: 'gpt-4o-mini', conf: {}, opts: { cache: 'yes', stop: [], tools: [], messages: [{ content: 'no role' }], turnContext: 'ctx', other: 1 }, responses: [chatReply] });
add({ name: 'turn context without messages', provider: 'openai', model: 'gpt-4o-mini', conf: {}, opts: { cache: true, turnContext: 'ctx' }, responses: [chatReply] });
add({ name: 'turn context object content', provider: 'openai', model: 'gpt-4o-mini', conf: {}, opts: { messages: [{ role: 'user', content: { toString: 1 } }], turnContext: 'ctx' }, responses: [chatReply] });

// ── Run ────────────────────────────────────────────────────────────────────

function opaque(v) {
    return v === undefined ? null : v;
}

async function runCase(c) {
    configValues = Object.assign({}, BASE_CONFIG, c.config === undefined ? {} : c.config);
    if (c.config && Object.keys(c.config).length === 0) {
        configValues = {};
    }
    openrouter._resetCatalogCache();
    queue = c.responses.map(r => Object.assign({}, r));
    recorded = [];
    const out = { requests: null, result: null, error: null };
    try {
        const call = providers.createProvider(c.provider, c.model, c.apiKey, c.conf);
        const res = await call(c.system, c.user, c.opts === null ? undefined : c.opts);
        out.result = JSON.stringify(res);
    } catch (err) {
        out.error = { message: err.message, status: opaque(err.status) };
    }
    // A timed-out call's fetch is still pending; let it finish before the
    // next case swaps the stub's queue.
    await new Promise(resolve => setTimeout(resolve, c.responses.some(r => r.delay) ? 300 : 0));
    out.requests = recorded.slice();
    return out;
}

function catching(fn) {
    try {
        const v = fn();
        return { out: v === undefined ? null : JSON.stringify(v) };
    } catch (err) {
        return { error: err.message };
    }
}

async function main() {
    const calls = [];
    for (const c of cases) {
        // Node changes the caller's messages in place (Anthropic's cache
        // marker and turn context on array content), so the case is
        // recorded as it was before the call.
        const snapshot = JSON.parse(JSON.stringify(c));
        const result = await runCase(c);
        Object.assign(c, { opts: snapshot.opts, conf: snapshot.conf });
        calls.push(Object.assign({ case: Object.assign({}, c, { responses: c.responses.map(r => ({ status: r.status, delay: r.delay || 0, body: typeof r.body === 'string' ? r.body : JSON.stringify(r.body) })), conf: c.conf === undefined ? null : c.conf, has_conf: c.conf !== undefined, opts: c.opts === undefined ? null : c.opts }) }, result));
    }

    configValues = Object.assign({}, BASE_CONFIG);
    const registry = JSON.stringify(providers.getRegistry());

    const lookups = [];
    const pairs = [];
    for (const p of providers.getRegistry().providers) {
        for (const m of Object.keys(p.models)) {
            pairs.push([p.name, m]);
        }
    }
    pairs.push(['anthropic', 'nope'], ['ANTHROPIC', 'claude-opus-4-6'], ['gemini', 'gemini-2.5-pro'], ['openrouter', 'deepseek/deepseek-v4-flash'], ['nope', 'x'], ['constructor', 'm'], ['__proto__', 'm'], ['anthropic', 'constructor'], ['anthropic', '__proto__'], ['xai', 'grok-4.20-0309-reasoning']);
    for (const [provider, model] of pairs) {
        lookups.push({
            provider, model,
            defaults: catching(() => providers.getDefaultConfiguration(provider, model)),
            version: catching(() => providers.getModelConfigVersion(provider, model)),
            pricing: catching(() => providers.getModelPricing(provider, model)),
        });
    }

    const usages = [
        { input_tokens: 1000, output_tokens: 500 },
        { input_tokens: 1000, output_tokens: 500, cache_creation_input_tokens: 300, cache_read_input_tokens: 200 },
        { input_tokens: '1000', output_tokens: null },
        { input_tokens: 10, output_tokens: 1, cost: 0.5 },
        { input_tokens: 10, output_tokens: 1, cost: -1 },
        { input_tokens: 10, output_tokens: 1, cost: NaN },
        { input_tokens: 10, output_tokens: 1, cost: 0 },
        {},
        { input_tokens: { toString: 0 }, output_tokens: 1 },
    ];
    const costs = [];
    for (const [provider, model] of pairs) {
        for (const usage of usages) {
            costs.push({ provider, model, usage: JSON.stringify(usage), cost_nan: Number.isNaN(usage.cost), out: catching(() => providers.calculateCost(provider, model, usage)) });
        }
    }

    const formatConfs = [undefined, {}, { service_tier: 'flex' }, { x_search: true, web_search: true }, { web_search: 1 }];
    const formats = [];
    for (const [provider, model] of pairs) {
        for (const conf of formatConfs) {
            formats.push({ provider, model, conf: conf === undefined ? null : conf, out: catching(() => providers.formatPricing(provider, model, conf)) });
        }
    }
    // OpenRouter pricing once its catalog is loaded.
    openrouter._resetCatalogCache();
    queue = [catalogReply];
    await openrouter.fetchCatalog();
    const catalogFormats = ['deepseek/deepseek-v4-flash', 'x/no-cache', 'x/garbage', 'x/missing', '7'].map(model => ({ model, out: providers.formatPricing('openrouter', model, {}) }));
    const catalogEntries = [];
    for (const [id, entry] of await openrouter.fetchCatalog()) {
        catalogEntries.push(JSON.stringify({ id, entry }));
    }

    const schemas = [
        { type: 'object', properties: { n: { type: 'integer' }, x: { type: 'number' }, b: { type: 'boolean' }, s: { type: 'string' }, u: { type: ['integer', 'null'] }, amb: { type: ['integer', 'string'] }, o: { type: 'object', properties: { k: { type: 'integer' } } }, a: { type: 'array', items: { type: 'number' } }, __proto__x: { type: 'integer' } } },
        '{"type":"object","properties":{"n":{"type":"integer"}}}',
        'not json',
        null,
        [],
        { type: 'array', items: { type: 'boolean' } },
        { type: 'object' },
    ];
    const inputs = [
        { n: '5', x: ' 2.5 ', b: 'TRUE', s: '7', u: '-3', amb: '1', o: '{"k":"4"}', a: '["1","x",2]', extra: '9' },
        { n: '9007199254740993', x: '1abc', b: 'yes', o: '[1]', a: '{}' },
        { n: '+0', x: '', b: ' false ', o: { k: ' 12 ' }, a: ['0x10', '1e3', 'Infinity'] },
        { n: '1.5', x: '0x1f', u: null },
        '{"n":"2"}',
        ['true', 'false', 'x'],
        '["true"]',
        5,
        null,
    ];
    const coerce = [];
    for (const schema of schemas) {
        for (const input of inputs) {
            const copy = JSON.parse(JSON.stringify(input));
            coerce.push({ schema: JSON.stringify(schema), input: JSON.stringify(input), out: JSON.stringify(coerceToolArgs(copy, schema)) });
        }
    }

    const numbers = [null, '', '0', '12', ' 7 ', '1e3', '0x10', 'abc', 'Infinity', true, false, [], [5], ['6'], [1, 2], {}, 1.5, -0, '1_000'];
    const asNumbers = numbers.map(v => ({ in: JSON.stringify(v), out: catching(() => asNumber(v)) }));

    const sanitize = [
        { cache: true, stop: ['a', '', 'b', 5], tools: [{ name: 'x' }, { name: 1 }, null], messages: [{ role: 'user', content: 'p' }, { role: 5 }], turnContext: 'ctx' },
        { cache: 1 },
        { messages: [], turnContext: 'ctx' },
        { stop: 'abc' },
        [],
        'str',
        null,
    ].map(o => ({ in: JSON.stringify(o), out: catching(() => providers.sanitizeOpts(o)) }));

    const attach = [
        [[{ role: 'user', content: 'perception', extra: 1 }], 'ctx'],
        [[{ role: 'user', content: '' }], 'ctx'],
        [[{ role: 'user', content: 5 }], 'ctx'],
        [[{ role: 'user', content: [1, [2, 3]] }], 'ctx'],
        [[{ role: 'user' }], 'ctx'],
        [[{ role: 'assistant', content: 'x' }], 'ctx'],
        [[], 'ctx'],
        [[{ role: 'user', content: 'p' }], ''],
    ].map(([m, t]) => ({ messages: JSON.stringify(m), context: t, out: catching(() => providers.attachTurnContext(m, t)) }));

    const reasons = ['stop', 'length', 'tool_calls', 'function_call', 'content_filter', 'end_turn', 'stop_sequence', 'max_tokens', 'tool_use', 'refusal', 'pause_turn', 'STOP', 'MAX_TOKENS', 'SAFETY', 'RECITATION', 'BLOCKLIST', 'PROHIBITED_CONTENT', 'SPII', 'OTHER', 'completed', 'incomplete', 'failed', null, 5];
    const incomplete = ['max_output_tokens', 'content_filter', 'other', null];
    const finishes = [];
    for (const r of reasons) {
        finishes.push({ in: JSON.stringify(r), chat: finish.normalizeOpenAIChatFinish(r), anthropic: finish.normalizeAnthropicStop(r), google: finish.normalizeGoogleFinish(r) });
    }
    const statuses = [];
    for (const s of reasons) {
        for (const i of incomplete) {
            for (const t of [false, true]) {
                statuses.push({ status: JSON.stringify(s), reason: JSON.stringify(i), tool: t, out: finish.normalizeResponsesStatus(s, i, t) });
            }
        }
    }

    // Ciphertexts made by Node with a fixed IV, and broken variants.
    function encryptWith(iv, plaintext) {
        const cipher = crypto.createCipheriv('aes-256-gcm', Buffer.from(ENC_KEY, 'hex'), iv);
        let ct = cipher.update(plaintext, 'utf8', 'hex');
        ct += cipher.final('hex');
        return `${iv.toString('hex')}:${cipher.getAuthTag().toString('hex')}:${ct}`;
    }
    const good = encryptWith(Buffer.from('0102030405060708090a0b0c', 'hex'), 'sk-ant-test-key é');
    const [gIv, gTag, gCt] = good.split(':');
    const decryptInputs = [
        good,
        encryptWith(Buffer.from('0102030405060708090a0b0c0d0e0f10', 'hex'), 'sixteen-byte iv'),
        providers.encryptApiKey(''),
        `${gIv}:${gTag}:${gCt}zz`,
        `${gIv}:${gTag}:${gCt}0`,
        `${gIv}:${gTag.slice(0, 24)}:${gCt}`,
        `${gIv}:${gTag.slice(0, 6)}:${gCt}`,
        `${gIv}:${gTag.replace(/^./, c => (c === '0' ? '1' : '0'))}:${gCt}`,
        `:${gTag}:${gCt}`,
        `${gIv}:${gTag}`,
        `${gIv}:${gTag}:${gCt}:extra`,
        '',
    ];
    const decrypt = [];
    for (const keyConfig of [ENC_KEY, '', ENC_KEY.slice(0, 62), undefined]) {
        configValues = keyConfig === undefined ? {} : { virtual_agent_encryption_key: keyConfig };
        for (const input of decryptInputs) {
            decrypt.push({ key: keyConfig === undefined ? null : keyConfig, in: input, out: catching(() => providers.decryptApiKey(input)) });
        }
    }

    const floats = ['0.00000014', '1e-6xyz', 'abc', '.5e-7', '  -3.5e2rest', '+.5', '5.', '1e', '1e+', 'Infinityx', '-Infinity', '0x10', '', '.', '-.e1', '00012', '1.2.3'];
    const parseFloats = floats.map(s => ({ in: s, out: JSON.stringify(parseFloat(s)), nan: Number.isNaN(parseFloat(s)) }));

    const out = {
        base: BASE,
        calls,
        registry,
        lookups,
        costs,
        formats,
        catalog_formats: catalogFormats,
        catalog_entries: catalogEntries,
        coerce,
        as_number: asNumbers,
        sanitize,
        attach,
        finishes,
        statuses,
        enc_key: ENC_KEY,
        decrypt,
        parse_float: parseFloats,
    };
    fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 1) + '\n');
}

main().then(() => process.exit(0), err => {
    console.error(err);
    process.exit(1);
});
