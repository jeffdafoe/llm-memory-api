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

app.get('/health', (req, res) => {
    res.json({ status: 'ok' });
});

// Load config from DB before accepting requests
config.init().then(() => {
    // Register virtual agent handler (depends on config being loaded)
    require('./services/virtual-agent');

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
