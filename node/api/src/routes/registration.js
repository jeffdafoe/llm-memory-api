const express = require('express');
const crypto = require('crypto');
const router = express.Router();
const pool = require('../db');
const config = require('../services/config');
const generatePassphrase = require('eff-diceware-passphrase');
const { generateSalt, hash: hashToken, generateKey, tokenLookupHash } = require('../services/hashing');
const { mailSend } = require('../services/mail');
const { saveNote } = require('../services/documents');
const { checkNameAvailability, moderateActorName } = require('../services/actors');

// Parse YAML-style frontmatter from template content (same logic as admin.js)
function parseTemplateFrontmatter(content = '') {
    const match = content.match(/^---\n([\s\S]*?)\n---\n\n?([\s\S]*)$/);
    if (!match) return { frontmatter: {}, body: content };
    const frontmatter = {};
    for (const line of match[1].split('\n')) {
        const colonIdx = line.indexOf(':');
        if (colonIdx > 0) {
            const key = line.slice(0, colonIdx).trim();
            const val = line.slice(colonIdx + 1).trim();
            frontmatter[key] = val;
        }
    }
    return { frontmatter, body: match[2] };
}

// GET /api/registration-mode — check if open registration is enabled
router.get('/api/registration-mode', (req, res) => {
    const open = config.get('open_registration') === 'true';
    res.json({ open });
});

// POST /api/check-name — check if an agent name is available
router.post('/api/check-name', async (req, res) => {
    const { name } = req.body;
    if (!name || !name.trim()) {
        return res.status(400).json({ error: 'Name is required' });
    }
    const agentName = name.trim().toLowerCase();
    if (!/^[a-z][a-z0-9_-]{1,30}$/.test(agentName)) {
        return res.json({ available: false, reason: 'Name must start with a letter, 2-31 chars, only lowercase letters, numbers, hyphens, underscores.' });
    }
    const result = await checkNameAvailability(agentName);
    res.json(result);
});

// Resolve realm from request host using the realm_host_map config.
// Falls back to 'llm-memory' if no mapping matches.
function realmFromHost(req) {
    const host = (req.get('x-forwarded-host') || req.get('host') || '').split(':')[0];
    try {
        const map = JSON.parse(config.get('realm_host_map') || '{}');
        if (map[host]) return map[host];
    } catch (e) { /* ignore parse errors */ }
    return 'llm-memory';
}

// POST /api/register — create agent via invite code or open registration
router.post('/api/register', async (req, res) => {
    const { code, name } = req.body;
    const openRegistration = config.get('open_registration') === 'true';

    if (!name) {
        return res.status(400).json({ error: 'Agent name is required' });
    }
    if (!code && !openRegistration) {
        return res.status(400).json({ error: 'Invite code is required (open registration is disabled)' });
    }

    const { password, dream_mode } = req.body;
    const minPwLen = parseInt(config.get('minimum_password_length')) || 10;
    if (!password || password.length < minPwLen) {
        return res.status(400).json({ error: 'Password must be at least ' + minPwLen + ' characters' });
    }
    const validDreamModes = ['none', 'companion', 'technical'];
    const dreamMode = validDreamModes.includes(dream_mode) ? dream_mode : 'none';

    const agentName = name.trim().toLowerCase();
    if (!/^[a-z][a-z0-9_-]{1,30}$/.test(agentName)) {
        return res.status(400).json({ error: 'Name must start with a letter, 2-31 chars, only lowercase letters, numbers, hyphens, underscores.' });
    }

    // ── Phase 1: pre-validation (no transaction, no pooled connection held) ──
    // Reject bad invite codes / unavailable names / moderation rejects BEFORE
    // spending any PBKDF2 CPU. The invite check runs first so an invalid code
    // can't reach the moderation VA call (LLM-34). The authoritative invite
    // check happens under FOR UPDATE in Phase 3; this is just the cheap gate.
    let realm;
    let inviteId = null;

    if (code) {
        const invite = await pool.query(
            `SELECT id, used_by, expires_at, realm FROM invite_codes WHERE code = $1`,
            [code.trim()]
        );
        if (invite.rows.length === 0) {
            return res.status(400).json({ error: 'Invalid invite code' });
        }
        const inv = invite.rows[0];
        if (inv.used_by) {
            return res.status(400).json({ error: 'This invite code has already been used' });
        }
        if (inv.expires_at && new Date(inv.expires_at) < new Date()) {
            return res.status(400).json({ error: 'This invite code has expired' });
        }
        realm = inv.realm || 'llm-memory';
        inviteId = inv.id;
    } else {
        // Open registration — derive realm from request host
        realm = realmFromHost(req);
    }

    // Check name availability (existing actors, existing namespaces)
    const availability = await checkNameAvailability(agentName);
    if (!availability.available) {
        return res.status(409).json({ error: availability.reason });
    }

    // Virtual agent moderation check (skips gracefully if VA not configured).
    // Gated behind the invite check above so an invalid code can't invoke the VA.
    const moderation = await moderateActorName(agentName);
    if (!moderation.approved) {
        return res.status(400).json({ error: moderation.reason, field: 'name' });
    }

    // ── Phase 2: hashing (no transaction, no pooled connection held) ──
    // The ~3 PBKDF2 derivations (~30ms each on the libuv threadpool) run here
    // so they never hold the DB connection or the invite FOR UPDATE row lock.
    // Bad codes/names were already rejected in Phase 1, so this CPU is only ever
    // spent on a request that has passed validation — no invite-spam amplification.
    const words = generatePassphrase(3);
    const passphrase = words.join('-');
    const salt = generateSalt();
    const passphraseHash = await hashToken(passphrase, salt);

    const passwordSalt = generateSalt();
    const passwordHash = await hashToken(password, passwordSalt);

    const apiKey = generateKey();
    const apiKeySalt = generateSalt();
    const apiKeyHash = await hashToken(apiKey, apiKeySalt);

    // ── Phase 3: short transaction (inserts only) ──
    // Re-lock and re-validate the invite under FOR UPDATE, then do all the
    // inserts. The lock window now spans only fast index writes — no hashing,
    // no VA call. Re-checking used_by here closes the reuse race opened by
    // validating in Phase 1: two valid concurrent uses of the same code
    // serialize on the row lock, and the loser sees used_by already set.
    let actorId;
    const client = await pool.connect();
    try {
        await client.query('BEGIN');

        if (inviteId !== null) {
            const relock = await client.query(
                `SELECT used_by, expires_at, realm FROM invite_codes WHERE id = $1 FOR UPDATE`,
                [inviteId]
            );
            if (relock.rows.length === 0) {
                await client.query('ROLLBACK');
                return res.status(400).json({ error: 'Invalid invite code' });
            }
            const r = relock.rows[0];
            if (r.used_by) {
                await client.query('ROLLBACK');
                return res.status(400).json({ error: 'This invite code has already been used' });
            }
            if (r.expires_at && new Date(r.expires_at) < new Date()) {
                await client.query('ROLLBACK');
                return res.status(400).json({ error: 'This invite code has expired' });
            }
            // Take realm from the locked row — authoritative if an admin edited
            // the invite between the Phase 1 read and now.
            realm = r.realm || 'llm-memory';
        }

        // Create actor with realm
        await client.query(
            `INSERT INTO actors (name, token_hash, token_salt, password_hash, password_salt, status, realms) VALUES ($1, $2, $3, $4, $5, 'active', $6)`,
            [agentName, passphraseHash, salt, passwordHash, passwordSalt, [realm]]
        );
        // Set created_by to self so the agent owns itself
        await client.query(
            `UPDATE actors SET created_by = id WHERE name = $1`,
            [agentName]
        );

        // Create agent_configuration
        await client.query(
            `INSERT INTO agent_configuration (actor_id, provider, model, dream_mode)
             VALUES ((SELECT id FROM actors WHERE name = $1), NULL, NULL, $2)`,
            [agentName, dreamMode]
        );

        // Mark invite code as used (if one was provided)
        if (inviteId !== null) {
            await client.query(
                `UPDATE invite_codes SET used_by = $1, used_at = NOW() WHERE id = $2`,
                [agentName, inviteId]
            );
        }

        // Get the new actor's ID for permission grants
        const actorResult = await client.query('SELECT id FROM actors WHERE name = $1', [agentName]);
        actorId = actorResult.rows[0].id;

        // Grant all MCP permissions (full tool access)
        await client.query(
            `INSERT INTO agent_permissions (actor_id, permission_id)
             SELECT $1, id FROM permissions`,
            [actorId]
        );

        // Own-namespace access is implicit (enforced in namespace-permissions.js hasAccess).
        // No namespace_permissions row needed.

        // Grant admin UI access (dashboard, agents, communications)
        await client.query(
            `INSERT INTO admin_permissions (actor_id, resource, action) VALUES
             ($1, 'dashboard', 'read'),
             ($1, 'agents', 'read'),
             ($1, 'agents', 'write'),
             ($1, 'comms', 'read'),
             ($1, 'notes', 'read'),
             ($1, 'notes', 'write')`,
            [actorId]
        );

        // Insert the API key hashed in Phase 2. key_lookup_hash (MEM-136) is the
        // deterministic SHA-256 index key that lets auth find this row in one
        // SELECT instead of PBKDF2-scanning the table.
        await client.query(
            `INSERT INTO agent_api_keys (actor_id, key_hash, key_salt, key_lookup_hash, label) VALUES ($1, $2, $3, $4, 'default')`,
            [actorId, apiKeyHash, apiKeySalt, tokenLookupHash(apiKey)]
        );

        await client.query('COMMIT');
    } catch (err) {
        // Rollback can itself throw if BEGIN never succeeded or the connection
        // died; swallow that so it can't mask the original error below.
        try {
            await client.query('ROLLBACK');
        } catch (rollbackErr) {
            console.error('Registration rollback error:', rollbackErr.message);
        }
        // A unique-violation on the name means a concurrent registration claimed
        // it between the Phase 1 availability check and this insert. Return a
        // clean 409 instead of a generic 500.
        if (err.code === '23505' && err.constraint === 'actors_name_key') {
            return res.status(409).json({ error: 'That name was just taken. Please choose another.' });
        }
        console.error('Registration error:', err.message);
        return res.status(500).json({ error: 'Something went wrong' });
    } finally {
        client.release();
    }

    // Apply default welcome template (same as admin-created agents)
    // Runs after commit so the agent exists even if template fails
    try {
        const tplResult = await pool.query(
            "SELECT content FROM templates WHERE kind = 'welcome' ORDER BY id LIMIT 1"
        );
        if (tplResult.rows.length > 0) {
            const rawContent = tplResult.rows[0].content;
            const { frontmatter, body: tplBody } = parseTemplateFrontmatter(rawContent);
            const mailBody = tplBody.replace(/\{agent\}/g, agentName);

            // Copy template body to startup_instructions (persistent)
            await pool.query(
                'UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = (SELECT id FROM actors WHERE name = $2)',
                [mailBody, agentName]
            );

            // Also send as welcome mail
            const mailSubject = (frontmatter.subject || 'Welcome, {agent}').replace(/\{agent\}/g, agentName);
            await mailSend(agentName, 'system', mailSubject, mailBody);
        }
    } catch (tplErr) {
        // Don't fail registration if template application fails
        console.error('Registration welcome template error:', tplErr.message);
    }

    // Save getting-started note from welcome-note template (if one exists)
    try {
        const noteTplResult = await pool.query(
            "SELECT content FROM templates WHERE kind = 'welcome-note' ORDER BY id LIMIT 1"
        );
        if (noteTplResult.rows.length > 0) {
            const rawContent = noteTplResult.rows[0].content;
            const { frontmatter, body: tplBody } = parseTemplateFrontmatter(rawContent);
            const noteBody = tplBody.replace(/\{agent\}/g, agentName).replace(/\{passphrase\}/g, passphrase).replace(/\{api_key\}/g, apiKey);
            const noteTitle = (frontmatter.title || 'Getting Started').replace(/\{agent\}/g, agentName);
            const noteSlug = frontmatter.slug || 'instructions/getting-started';
            await saveNote(agentName, noteTitle, noteBody, noteSlug, actorId);
        }
    } catch (noteErr) {
        // Don't fail registration if note creation fails
        console.error('Registration welcome-note template error:', noteErr.message);
    }

    res.json({
        ok: true,
        agent: agentName,
        passphrase,
        api_key: apiKey,
        message: 'Account created. Save your passphrase and API key — they will not be shown again.'
    });
});

module.exports = router;
