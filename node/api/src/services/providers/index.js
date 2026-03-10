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
    getDefaultConfiguration
};
