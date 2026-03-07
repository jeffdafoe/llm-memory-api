// Provider abstraction — factory that returns a call(systemPrompt, userMessage) function.
// Each provider translates to its native API format.

const { log } = require('./logger');
const config = require('./config');
const crypto = require('crypto');

function logProvider(action, details) {
    log('provider', action, details);
}

// Decrypt an API key stored with AES-256-GCM.
function decryptApiKey(encrypted) {
    const encKey = config.get('virtual_agent_encryption_key');
    if (!encKey) {
        throw new Error('virtual_agent_encryption_key not configured');
    }
    // Format: iv:authTag:ciphertext (all hex)
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

// Encrypt an API key for storage.
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

// Build a provider call function from agent config.
function createProvider(provider, model, apiKey, configuration) {
    const providerLower = (provider || '').toLowerCase();

    if (providerLower === 'anthropic') {
        return createAnthropicProvider(model, apiKey, configuration);
    }

    throw new Error(`Unsupported provider: ${provider}`);
}

function createAnthropicProvider(model, apiKey, configuration) {
    const conf = configuration || {};

    return async function call(systemPrompt, userMessage) {
        const headers = {
            'Content-Type': 'application/json',
            'x-api-key': apiKey,
            'anthropic-version': '2023-06-01',
            ...(conf.headers || {})
        };

        const body = {
            model: model,
            max_tokens: conf.max_tokens || 4096,
            system: systemPrompt,
            messages: [{ role: 'user', content: userMessage }]
        };

        if (conf.temperature !== undefined) {
            body.temperature = conf.temperature;
        }

        logProvider('api-call', { provider: 'anthropic', model });

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

        logProvider('api-response', {
            provider: 'anthropic', model,
            input_tokens: data.usage?.input_tokens,
            output_tokens: data.usage?.output_tokens
        });

        return text;
    };
}

module.exports = { createProvider, encryptApiKey, decryptApiKey };
