// events.js — WebSocket client for real-time admin events.
// Connects to /admin/ws with Bearer token via Sec-WebSocket-Protocol.
// Auto-reconnects with exponential backoff on disconnect.

function createEventsModule() {
    let ws = null;
    let token = null;
    let intentionalClose = false;
    let reconnectDelay = 1000;
    let reconnectTimer = null;
    const MAX_RECONNECT_DELAY = 30000;

    // Event handlers registered by other modules
    const handlers = {};

    // Build the WebSocket URL from the current page location
    function getWsUrl() {
        const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
        return protocol + '//' + location.host + '/admin/ws';
    }

    function connect(sessionToken) {
        token = sessionToken;
        intentionalClose = false;
        openConnection();
    }

    function disconnect() {
        intentionalClose = true;
        clearReconnectTimer();
        if (ws) {
            ws.close();
            ws = null;
        }
    }

    function openConnection() {
        if (ws) {
            ws.close();
            ws = null;
        }

        if (!token) return;

        // Pass the admin token via the Sec-WebSocket-Protocol header.
        // The server expects ['bearer', '<token>'] as subprotocols.
        ws = new WebSocket(getWsUrl(), ['bearer', token]);

        ws.onopen = () => {
            // Reset backoff on successful connection
            reconnectDelay = 1000;
            console.log('[events] WebSocket connected');
        };

        ws.onmessage = (event) => {
            try {
                const msg = JSON.parse(event.data);
                if (msg.event && handlers[msg.event]) {
                    for (const handler of handlers[msg.event]) {
                        handler(msg.data);
                    }
                }
            } catch (err) {
                console.error('[events] Failed to parse message:', err);
            }
        };

        ws.onclose = () => {
            ws = null;
            if (!intentionalClose) {
                scheduleReconnect();
            }
        };

        ws.onerror = () => {
            // onclose fires after onerror, so reconnect is handled there
        };
    }

    function scheduleReconnect() {
        clearReconnectTimer();

        // Don't reconnect if the tab is hidden — we'll reconnect on visibility change
        if (document.hidden) return;

        reconnectTimer = setTimeout(() => {
            console.log('[events] Reconnecting (delay: ' + reconnectDelay + 'ms)');
            openConnection();
            // Exponential backoff with cap
            reconnectDelay = Math.min(reconnectDelay * 2, MAX_RECONNECT_DELAY);
        }, reconnectDelay);
    }

    function clearReconnectTimer() {
        if (reconnectTimer) {
            clearTimeout(reconnectTimer);
            reconnectTimer = null;
        }
    }

    // Register a handler for a specific event type.
    // Multiple handlers per event type are supported.
    function onEvent(eventType, callback) {
        if (!handlers[eventType]) {
            handlers[eventType] = [];
        }
        handlers[eventType].push(callback);
    }

    // Handle tab visibility changes — pause reconnect when hidden, resume when visible
    document.addEventListener('visibilitychange', () => {
        if (document.hidden) {
            // Tab hidden — if we're waiting to reconnect, clear the timer
            clearReconnectTimer();
        } else {
            // Tab visible — if we're disconnected and should be connected, reconnect now
            if (!ws && token && !intentionalClose) {
                reconnectDelay = 1000; // Reset backoff on visibility restore
                openConnection();
            }
        }
    });

    return { connect, disconnect, onEvent };
}
