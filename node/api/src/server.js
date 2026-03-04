const express = require('express');
const path = require('path');
const config = require('./services/config');
const auth = require('./middleware/auth');
const opportunisticHeartbeat = require('./middleware/heartbeat');
const { requestLog } = require('./middleware/request-log');
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
app.use('/admin', express.static(path.join(__dirname, '..', 'public', 'admin')));

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
    app.listen(port, () => {
        console.log(`Memory API listening on port ${port}`);
    });
}).catch(err => {
    console.error('Failed to load config:', err.message);
    process.exit(1);
});
