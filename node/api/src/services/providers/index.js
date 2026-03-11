// Provider registry — aggregates all provider modules.
// Single source of truth for supported providers, models, and capabilities.

const crypto = require('crypto');
const config = require('../config');

// ── Load provider modules ───────────────────────────────────────────────────

const anthropic = require('./anthropic');
const google = require('./google');
const openai = require('./openai');
const perplexity = require('./perplexity');

const providerModules = [anthropic, google, openai, perplexity];

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
function createProvider(provider, model, apiKey, configuration) {
    const mod = resolveProvider(provider);
    if (!mod) {
        throw new Error(`Unsupported provider: ${provider}`);
    }
    // Resolve apiId if the model entry defines one (e.g. Anthropic short names → dated API IDs)
    const modelEntry = mod.models[model];
    const apiModel = (modelEntry && modelEntry.apiId) || model;
    return mod.createCall(apiModel, apiKey, configuration);
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
// usage: { input_tokens, output_tokens, cache_creation_input_tokens?, cache_read_input_tokens? }
// Throws if pricing data is missing (fail closed — unknown cost must not bypass budgets).
function calculateCost(providerName, modelId, usage) {
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
function formatPricing(providerName, modelId) {
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
function getDefaultConfiguration(providerName, modelId) {
    const capabilities = getModelCapabilities(providerName, modelId);
    if (!capabilities) return {};

    const defaults = {};
    for (const [key, cap] of Object.entries(capabilities)) {
        if (cap.default !== undefined) {
            defaults[key] = cap.default;
        }
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
    getModelCapabilities,
    getDefaultConfiguration,
    getModelPricing,
    calculateCost,
    formatPricing
};
