const express = require('express');
const path = require('path');
const config = require('./services/config');
const auth = require('./middleware/auth');
const opportunisticHeartbeat = require('./middleware/heartbeat');
const { requestLog } = require('./middleware/request-log');
const { handleUpgrade, broadcast } = require('./services/events');
const pool = require('./db');
const oauthRoutes = require('./routes/oauth');
const mcpRoutes = require('./routes/mcp');
const agentRoutes = require('./routes/agent');
const chatRoutes = require('./routes/chat');
const discussionRoutes = require('./routes/discussion');
const mailRoutes = require('./routes/mail');
const memoryRoutes = require('./routes/memory');
const documentRoutes = require('./routes/documents');
const systemRoutes = require('./routes/system');
const adminRoutes = require('./routes/admin');
const registrationRoutes = require('./routes/registration');
const authRoutes = require('./routes/auth');

const app = express();
const port = process.env.PORT || 3100;

app.use(express.json({ limit: '5mb' }));
app.use('/admin', express.static(path.join(__dirname, '..', 'public', 'admin', 'dist'), {
    setHeaders: (res, filePath) => {
        if (filePath.endsWith('.html')) {
            res.setHeader('Cache-Control', 'no-cache, no-store, must-revalidate');
        } else if (filePath.includes('assets')) {
            res.setHeader('Cache-Control', 'public, max-age=31536000, immutable');
        }
    }
}));

// Log all API requests to in-memory ring buffer (admin dashboard live view)
app.use(requestLog);

// OAuth discovery + token endpoint (no auth required, root-level)
app.use(oauthRoutes);

// MCP Streamable HTTP endpoint (HMAC auth via mcp-auth middleware)
app.use(mcpRoutes);

// Auth verification (public, no auth — the token being verified IS the credential)
app.use('/v1', authRoutes);

app.use('/v1', auth);
app.use('/v1', opportunisticHeartbeat);
app.use('/v1', agentRoutes);
app.use('/v1', chatRoutes);
app.use('/v1', discussionRoutes);
app.use('/v1', mailRoutes);
app.use('/v1', memoryRoutes);
app.use('/v1', documentRoutes);
app.use('/v1', systemRoutes);
app.use('/v1', adminRoutes);

// Registration endpoints (public, no auth)
app.use(registrationRoutes);

// Registration page
app.get('/register', (req, res) => {
    res.sendFile(path.join(__dirname, '..', 'public', 'landing', 'register.html'));
});

// Agents detail page
app.get('/agents', (req, res) => {
    res.sendFile(path.join(__dirname, '..', 'public', 'landing', 'agents.html'));
});

// Landing page for public domains, admin redirect for internal domains
app.get('/', (req, res) => {
    // Prevent browsers from caching the landing page (avoids stale redirect issues)
    res.set('Cache-Control', 'no-store, no-cache, must-revalidate');
    res.set('Pragma', 'no-cache');
    const landingPage = path.join(__dirname, '..', 'public', 'landing', 'index.html');
    res.sendFile(landingPage, (err) => {
        if (err) res.redirect('/admin/');
    });
});

// Access request form submission (public, no auth)
app.post('/api/access-request', async (req, res) => {
    const { email, usage } = req.body;
    if (!email || !usage) {
        return res.status(400).json({ error: 'Email and usage description are required' });
    }
    if (email.length > 255 || usage.length > 5000) {
        return res.status(400).json({ error: 'Input too long' });
    }
    try {
        const config = require('./services/config');
        const openRegistration = config.get('open_registration') === 'true';

        if (openRegistration) {
            // Auto-approve: insert request as approved, generate invite code, return it
            const crypto = require('crypto');
            const code = crypto.randomBytes(16).toString('hex');
            const client = await pool.connect();
            try {
                await client.query('BEGIN');
                const reqResult = await client.query(
                    `INSERT INTO access_requests (email, usage_description, status, reviewed_at)
                     VALUES ($1, $2, 'approved', NOW()) RETURNING id`,
                    [email.trim(), usage.trim()]
                );
                await client.query(
                    `INSERT INTO invite_codes (code, created_by, access_request_id, expires_at)
                     VALUES ($1, 'system', $2, NOW() + INTERVAL '7 days')`,
                    [code, reqResult.rows[0].id]
                );
                await client.query('COMMIT');
            } catch (err) {
                await client.query('ROLLBACK');
                throw err;
            } finally {
                client.release();
            }
            return res.json({ ok: true, auto_approved: true, code });
        }

        // Normal flow: save as pending for manual review
        await pool.query(
            'INSERT INTO access_requests (email, usage_description) VALUES ($1, $2)',
            [email.trim(), usage.trim()]
        );
        res.json({ ok: true });
    } catch (err) {
        console.error('Access request error:', err.message);
        res.status(500).json({ error: 'Something went wrong' });
    }
});

app.get('/health', (req, res) => {
    res.json({ status: 'ok' });
});

app.get('/llms.txt', (req, res) => {
    res.type('text/plain');
    res.sendFile(path.join(__dirname, '..', 'public', 'llms.txt'));
});

app.get('/openapi.yaml', (req, res) => {
    res.type('text/yaml');
    res.sendFile(path.join(__dirname, '..', 'public', 'openapi.yaml'));
});

// Global error handler — catches unhandled errors (JSON parse failures, etc.)
// and returns a clean JSON response instead of Express's default HTML stack trace.
app.use((err, req, res, next) => {
    console.error('Unhandled error:', err.stack || err.message || err);

    const status = err.status || err.statusCode || 500;
    res.status(status).json({
        error: {
            code: status === 500 ? 'INTERNAL_ERROR' : 'BAD_REQUEST',
            message: err.message || 'An internal error occurred'
        }
    });
});

// Register pgvector types, then load config, then start server
pool.init().then(() => config.init()).then(() => {
    // Register virtual agent handler (depends on config being loaded)
    const { startErrorPing } = require('./services/virtual-agent');
    startErrorPing();

    const { startStaleVoteScanner } = require('./services/discussion');
    startStaleVoteScanner();

    const { startDreamScheduler } = require('./services/dream');
    startDreamScheduler();

    const { startCleanupScheduler } = require('./services/cleanup');
    startCleanupScheduler();

    const server = app.listen(port, () => {
        console.log(`Memory API listening on port ${port}`);
    });

    // Clear stale activity spinners — if an agent hasn't made any API request
    // in 5 minutes, they've disconnected (closed session, crashed, etc.)
    const STALE_ACTIVITY_INTERVAL_MS = 60000;
    const STALE_ACTIVITY_THRESHOLD = '5 minutes';
    const staleActivityTimer = setInterval(async () => {
        try {
            const { rows } = await pool.query(
                `UPDATE actors SET active_since = NULL
                 WHERE active_since IS NOT NULL
                   AND last_seen < NOW() - INTERVAL '${STALE_ACTIVITY_THRESHOLD}'
                 RETURNING id, name`
            );
            for (const row of rows) {
                broadcast('agent_activity', { agent: row.name, active: false });
            }
        } catch (err) {
            // Don't crash the server if this housekeeping query fails
            console.error('Stale activity cleanup error:', err.message);
        }
    }, STALE_ACTIVITY_INTERVAL_MS);
    staleActivityTimer.unref();

    // WebSocket upgrade handler for admin real-time events
    server.on('upgrade', (req, socket, head) => {
        if (req.url === '/admin/ws') {
            handleUpgrade(req, socket, head);
        } else {
            socket.destroy();
        }
    });
}).catch(err => {
    console.error('Failed to load config:', err.message);
    process.exit(1);
});
