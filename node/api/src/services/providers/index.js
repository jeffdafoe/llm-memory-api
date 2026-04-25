// Provider registry — aggregates all provider modules.
// Single source of truth for supported providers, models, and capabilities.

const crypto = require('crypto');
const config = require('../config');

// ── Load provider modules ───────────────────────────────────────────────────

const anthropic = require('./anthropic');
const google = require('./google');
const openai = require('./openai');
const openrouter = require('./openrouter');
const perplexity = require('./perplexity');
const xai = require('./xai');

const providerModules = [anthropic, google, openai, openrouter, perplexity, xai];

// Build lookup maps: by name and by alias.
const providersByName = {};
const providersByAlias = {};

for (const mod of providerModules) {
    providersByName[mod.name] = mod;
    if (mod.aliases) {
        for (const alias of mod.aliases) {
            providersByAlias[alias] = mod;
        }
    }
}

// ── Resolve provider module from name string ────────────────────────────────

function resolveProvider(providerName) {
    const lower = (providerName || '').toLowerCase();
    return providersByName[lower] || providersByAlias[lower] || null;
}

// ── Public API ──────────────────────────────────────────────────────────────

// Create a call function for a given provider/model/apiKey/configuration.
// This is the main entry point used by virtual-agent.js.
// The returned function is wrapped with a request timeout (from config) so that
// hung API calls don't block the retry pipeline indefinitely.
//
// Signature: (systemPrompt, userMessage, opts?) -> { text, tool_calls, usage }
//
// Per-call opts contract (all fields optional):
//   cache: boolean    — request Anthropic prompt caching if agent has cache_prompts=true
//   stop:  string[]   — provider-agnostic stop sequences, translated per-provider
//                       (Anthropic stop_sequences / OpenAI-family stop / Google stopSequences)
//   tools: object[]   — tool/function definitions. Neutral shape:
//                       { name: string, description: string, parameters: object }
//                       where `parameters` is a JSON Schema. Each provider translates
//                       to its native format (Anthropic input_schema, OpenAI/xai/
//                       OpenRouter "function" wrapper, Google functionDeclarations).
//                       Returned tool_calls is normalized to [{ id, name, input }]
//                       across providers; empty array when no tools were called.
//                       Perplexity Sonar models don't support tools and silently
//                       drop them.
//   messages: object[] — full conversation history, OpenAI-shape:
//                       { role: "user"|"assistant"|"tool", content: string,
//                         tool_calls?: [{id, type:"function", function:{name, arguments}}],
//                         tool_call_id?: string }
//                       When provided, overrides the default single-user-message
//                       built from `userMessage`. Anthropic/Google translate to
//                       their native formats. Use to continue a tool-use session
//                       across calls (engine appends prior assistant tool_use +
//                       user tool_result messages and re-calls the provider).
//
// Unknown opts fields are dropped at this boundary so providers never see them.
function createProvider(provider, model, apiKey, configuration) {
    const mod = resolveProvider(provider);
    if (!mod) {
        throw new Error(`Unsupported provider: ${provider}`);
    }
    // Resolve apiId if the model entry defines one (e.g. Anthropic short names → dated API IDs)
    const modelEntry = mod.models[model];
    const apiModel = (modelEntry && modelEntry.apiId) || model;
    const callFn = mod.createCall(apiModel, apiKey, configuration);

    // Wrap with request timeout. If the provider call doesn't resolve within
    // the configured limit, reject with a timeout error so retryWithBackoff
    // can kick in. The abandoned fetch response is silently discarded by GC.
    return function callWithTimeout(systemPrompt, userMessage, opts) {
        const timeoutSeconds = parseInt(config.get('virtual_agent_request_timeout')) || 120;
        const timeoutMs = timeoutSeconds * 1000;

        const sanitizedOpts = sanitizeOpts(opts);

        return Promise.race([
            callFn(systemPrompt, userMessage, sanitizedOpts),
            new Promise((_, reject) => {
                setTimeout(() => {
                    reject(new Error(`Provider request timed out after ${timeoutSeconds}s (${provider}/${model})`));
                }, timeoutMs);
            })
        ]);
    };
}

// Narrow the opts bag to the documented contract. Silently drops unknown keys
// so a typo in one call site can't leak through to provider request bodies.
// Returns undefined when there's nothing useful to forward — lets providers
// keep cheap `opts && opts.x` guards.
function sanitizeOpts(opts) {
    if (!opts || typeof opts !== 'object') return undefined;
    const out = {};
    if (opts.cache === true) {
        out.cache = true;
    }
    if (Array.isArray(opts.stop)) {
        const stops = opts.stop.filter(s => typeof s === 'string' && s.length > 0);
        if (stops.length > 0) {
            out.stop = stops;
        }
    }
    if (Array.isArray(opts.tools) && opts.tools.length > 0) {
        // Light shape check — keep only entries that look like tool defs.
        // Provider does deeper validation as the API rejects malformed bodies.
        const tools = opts.tools.filter(t => t && typeof t === 'object' && typeof t.name === 'string');
        if (tools.length > 0) {
            out.tools = tools;
        }
    }
    if (Array.isArray(opts.messages) && opts.messages.length > 0) {
        // Light shape check — drop entries without a string role + content.
        // Tool-call message variants get richer validation provider-side.
        const messages = opts.messages.filter(m => m && typeof m === 'object' && typeof m.role === 'string');
        if (messages.length > 0) {
            out.messages = messages;
        }
    }
    if (Object.keys(out).length === 0) return undefined;
    return out;
}

// Flatten a structured system prompt into a string.
// Delegates to the Anthropic module (which defines the canonical implementation),
// but all providers have their own copy for internal use.
function flattenPrompt(systemPrompt) {
    return anthropic.flattenPrompt(systemPrompt);
}

// Get the full registry for the admin UI.
// Returns { providers: [ { name, label, models: { id: { label, capabilities, deprecated? } } } ] }
function getRegistry() {
    const providers = providerModules.map(mod => ({
        name: mod.name,
        label: mod.label,
        models: mod.models
    }));
    return { providers };
}

// Get pricing data for a specific provider + model.
// Returns the pricing object (e.g. { input, output, cache_write, cache_read, request }), or null.
function getModelPricing(providerName, modelId) {
    const mod = resolveProvider(providerName);
    if (!mod) return null;
    const model = mod.models[modelId];
    if (!model) return null;
    return model.pricing || null;
}

// Calculate cost in dollars for a single API call.
// If the provider already computed cost (usage.cost), use that directly.
// Otherwise fall back to the static formula using model pricing data.
// usage: { input_tokens, output_tokens, cache_creation_input_tokens?, cache_read_input_tokens?, cost? }
function calculateCost(providerName, modelId, usage) {
    // Provider-computed cost takes precedence — the provider knows its own
    // pricing quirks (service tiers, caching discounts, per-request fees, etc.)
    // Validate before trusting: must be a finite non-negative number.
    if (usage.cost != null) {
        const providerCost = Number(usage.cost);
        if (Number.isFinite(providerCost) && providerCost >= 0) {
            return providerCost;
        }
    }

    // Fallback: static formula from model pricing registry
    const pricing = getModelPricing(providerName, modelId);
    if (!pricing) {
        throw new Error(`No pricing data for ${providerName}/${modelId} — cannot calculate cost`);
    }

    const inputTokens = usage.input_tokens || 0;
    const outputTokens = usage.output_tokens || 0;
    const cacheWriteTokens = usage.cache_creation_input_tokens || 0;
    const cacheReadTokens = usage.cache_read_input_tokens || 0;

    // All pricing is per million tokens — divide by 1,000,000
    let cost = 0;
    cost += inputTokens * (pricing.input || 0) / 1_000_000;
    cost += outputTokens * (pricing.output || 0) / 1_000_000;

    // Anthropic cache tiers: separate from input_tokens, priced independently
    if (pricing.cache_write) {
        cost += cacheWriteTokens * pricing.cache_write / 1_000_000;
    }
    if (pricing.cache_read) {
        cost += cacheReadTokens * pricing.cache_read / 1_000_000;
    }

    // Perplexity per-request fee: $/1K requests = $/1000 per request = price/1000 per call
    if (pricing.request) {
        cost += pricing.request / 1000;
    }

    return cost;
}

// Format pricing as a human-readable string for the admin UI.
// Delegates to the provider's formatPricing if it defines one,
// otherwise falls back to a generic format from the static pricing object.
function formatPricing(providerName, modelId, config) {
    const mod = resolveProvider(providerName);

    // Provider-specific formatting — the provider knows its own pricing dimensions
    if (mod && mod.formatPricing) {
        return mod.formatPricing(modelId, config);
    }

    // Generic fallback from static pricing data
    const pricing = getModelPricing(providerName, modelId);
    if (!pricing) return 'No pricing data';

    const parts = [];
    if (pricing.input != null) parts.push('$' + pricing.input + ' in');
    if (pricing.output != null) parts.push('$' + pricing.output + ' out');
    if (pricing.cache_write != null) parts.push('$' + pricing.cache_write + ' cache write');
    if (pricing.cache_read != null) parts.push('$' + pricing.cache_read + ' cache read');
    if (pricing.request != null) parts.push('$' + pricing.request + '/1K requests');

    return parts.join(' / ') + ' per 1M tokens';
}

// Get the configVersion for a specific provider + model.
// Returns the version integer, or null if provider/model not found.
function getModelConfigVersion(providerName, modelId) {
    const mod = resolveProvider(providerName);
    if (!mod) return null;
    const model = mod.models[modelId];
    if (!model) return null;
    return model.configVersion || null;
}

// Get capabilities for a specific provider + model.
// Returns the capabilities object, or null if not found.
function getModelCapabilities(providerName, modelId) {
    const mod = resolveProvider(providerName);
    if (!mod) return null;
    const model = mod.models[modelId];
    if (!model) return null;
    return model.capabilities;
}

// Build default configuration from a model's capability defaults.
// Used when creating a new virtual agent to seed the configuration JSON.
// Includes _configVersion so the stored config is immediately valid.
function getDefaultConfiguration(providerName, modelId) {
    const capabilities = getModelCapabilities(providerName, modelId);
    if (!capabilities) return {};

    const defaults = {};
    for (const [key, cap] of Object.entries(capabilities)) {
        if (cap.default !== undefined) {
            defaults[key] = cap.default;
        }
    }

    const version = getModelConfigVersion(providerName, modelId);
    if (version != null) {
        defaults._configVersion = version;
    }

    return defaults;
}

// ── Encryption helpers (unchanged from original provider.js) ────────────────

function decryptApiKey(encrypted) {
    const encKey = config.get('virtual_agent_encryption_key');
    if (!encKey) {
        throw new Error('virtual_agent_encryption_key not configured');
    }
    const parts = encrypted.split(':');
    if (parts.length !== 3) {
        throw new Error('Invalid encrypted API key format');
    }
    const iv = Buffer.from(parts[0], 'hex');
    const authTag = Buffer.from(parts[1], 'hex');
    const ciphertext = Buffer.from(parts[2], 'hex');
    const key = Buffer.from(encKey, 'hex');
    const decipher = crypto.createDecipheriv('aes-256-gcm', key, iv);
    decipher.setAuthTag(authTag);
    let decrypted = decipher.update(ciphertext, null, 'utf8');
    decrypted += decipher.final('utf8');
    return decrypted;
}

function encryptApiKey(plaintext) {
    const encKey = config.get('virtual_agent_encryption_key');
    if (!encKey) {
        throw new Error('virtual_agent_encryption_key not configured');
    }
    const key = Buffer.from(encKey, 'hex');
    const iv = crypto.randomBytes(12);
    const cipher = crypto.createCipheriv('aes-256-gcm', key, iv);
    let ciphertext = cipher.update(plaintext, 'utf8', 'hex');
    ciphertext += cipher.final('hex');
    const authTag = cipher.getAuthTag().toString('hex');
    return `${iv.toString('hex')}:${authTag}:${ciphertext}`;
}

module.exports = {
    createProvider,
    flattenPrompt,
    encryptApiKey,
    decryptApiKey,
    getRegistry,
    getModelConfigVersion,
    getModelCapabilities,
    getDefaultConfiguration,
    getModelPricing,
    calculateCost,
    formatPricing
};
