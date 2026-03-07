// WebSocket event broadcast service for the admin dashboard.
// Agents trigger events via API calls (activity_start, etc.); this service
// pushes them to connected admin browsers in real time.

const { WebSocketServer } = require('ws');
const pool = require('../db');

// Auth cache — reuse the same cache from the auth middleware so we don't
// double-query the DB for tokens we've already validated.
const authMiddleware = require('../middleware/auth');

const wss = new WebSocketServer({ noServer: true });
const clients = new Set();

// Ping interval — detect and clean up dead connections every 30s
const PING_INTERVAL_MS = 30000;

// Track liveness per client via a WeakMap so we don't pollute the ws object
const alive = new WeakMap();

const pingTimer = setInterval(() => {
    for (const ws of clients) {
        if (!alive.get(ws)) {
            // Didn't respond to last ping — terminate
            ws.terminate();
            clients.delete(ws);
            continue;
        }
        alive.set(ws, false);
        ws.ping();
    }
}, PING_INTERVAL_MS);

// Don't let the timer keep the process alive on shutdown
pingTimer.unref();

// Validate an admin session token against the user_sessions table.
// Returns the user object if valid, null otherwise.
async function validateAdminToken(token) {
    if (!token) return null;

    // Check auth middleware cache first
    const cached = authMiddleware.sessionCache.get(token);
    if (cached && cached.expires > Date.now() && cached.type === 'user') {
        return cached.user;
    }

    // Cache miss — check DB
    try {
        const result = await pool.query(
            `SELECT us.session_token, us.expires_at, u.id, u.username
             FROM user_sessions us JOIN users u ON u.id = us.user_id
             WHERE us.session_token = $1 AND us.expires_at > NOW()`,
            [token]
        );
        if (result.rows.length > 0) {
            const row = result.rows[0];
            return { id: row.id, username: row.username };
        }
    } catch (err) {
        console.error('WebSocket auth error:', err.message);
    }
    return null;
}

// Handle the HTTP upgrade request. Called from server.js when a request
// comes in for the WebSocket path.
async function handleUpgrade(req, socket, head) {
    // Extract token from Sec-WebSocket-Protocol header.
    // Client sends: new WebSocket(url, ['bearer', '<token>'])
    // Browser sends: Sec-WebSocket-Protocol: bearer, <token>
    const protocols = (req.headers['sec-websocket-protocol'] || '').split(',').map(s => s.trim());
    const bearerIndex = protocols.indexOf('bearer');
    const token = bearerIndex >= 0 ? protocols[bearerIndex + 1] : null;

    const user = await validateAdminToken(token);
    if (!user) {
        socket.write('HTTP/1.1 401 Unauthorized\r\n\r\n');
        socket.destroy();
        return;
    }

    // Complete the WebSocket handshake — echo back the 'bearer' subprotocol
    wss.handleUpgrade(req, socket, head, (ws) => {
        alive.set(ws, true);
        clients.add(ws);

        ws.on('pong', () => {
            alive.set(ws, true);
        });

        ws.on('close', () => {
            clients.delete(ws);
            alive.delete(ws);
        });

        ws.on('error', () => {
            clients.delete(ws);
            alive.delete(ws);
        });

        // Send a welcome message so the client knows the connection is live
        ws.send(JSON.stringify({ event: 'connected', data: { user: user.username } }));
    });
}

// Broadcast an event to all connected admin clients.
// eventType: string (e.g., 'agent_activity')
// data: object (serializable)
function broadcast(eventType, data) {
    if (clients.size === 0) return;

    const message = JSON.stringify({ event: eventType, data });
    for (const ws of clients) {
        if (ws.readyState === 1) { // WebSocket.OPEN
            ws.send(message);
        }
    }
}

module.exports = { handleUpgrade, broadcast };
