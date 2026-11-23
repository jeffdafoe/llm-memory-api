// Package events is Node's services/events.js: the admin dashboard's live
// event feed. Agents' API calls (activity start, a chat message, a note save)
// broadcast events, and the Hub pushes each one to every admin browser
// connected to the /admin/ws WebSocket.
package events

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
)

// Broadcaster sends an event to every connected admin dashboard.
type Broadcaster interface {
	Broadcast(event string, data any)
}

// Nop drops every event.
type Nop struct{}

func (Nop) Broadcast(string, any) {}

// PingInterval is events.js's PING_INTERVAL_MS: every interval each client
// that has not answered the previous ping is dropped, and the rest are pinged.
const PingInterval = 30 * time.Second

// maxPayload is ws's default maxPayload (100 MiB): a larger message from a
// client closes the connection with 1009.
const maxPayload = 100 * 1024 * 1024

// sendQueue bounds the messages waiting for one client. Node's ws buffered
// without limit; a client this far behind is dropped instead (Known
// differences in the go-server note).
const sendQueue = 1024

// pingWait bounds one ping: the write, then the wait for its pong. A pong
// that does not come in time only ends the ping's goroutine, and the alive
// flag decides the drop at the next tick. A write that does not finish in
// time closes the connection, as the gorilla port did (Known differences in
// the go-server note).
const pingWait = 10 * time.Second

// TokenValidator is validateAdminToken: the admin user name for a web
// session token, or ok false.
type TokenValidator func(ctx context.Context, token string) (username string, ok bool)

// Hub holds the connected admin clients. Its zero value has no validator and
// refuses every connection; NewHub is the usual constructor.
type Hub struct {
	Validate TokenValidator
	// Errorf receives failures that Node wrote to console.error.
	Errorf func(format string, args ...any)
	// Every replaces PingInterval, for tests.
	Every time.Duration

	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
}

// NewHub returns a hub that validates tokens with validate.
func NewHub(validate TokenValidator, errorf func(format string, args ...any)) *Hub {
	return &Hub{Validate: validate, Errorf: errorf}
}

type client struct {
	conn *websocket.Conn
	send chan []byte
	// alive is the WeakMap of events.js: set on connect and on each pong,
	// cleared when a ping goes out. It is atomic so the reader, which sets
	// it from inside coder/websocket's read lock, never waits for h.mu.
	alive atomic.Bool
	done  chan struct{}
	once  sync.Once
}

// stop closes the connection once; the writer and reader goroutines end.
// CloseNow closes the socket without a close frame, as ws.terminate().
// CloseNow waits for the connection's read lock, so it is never called
// with h.mu held.
func (c *client) stop() {
	c.once.Do(func() {
		close(c.done)
		c.conn.CloseNow()
	})
}

type envelope struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

// Broadcast sends {"event": event, "data": data} as JSON.stringify writes it
// to every open client. With no client connected nothing is encoded, as in
// Node. data must keep JavaScript's key order: a struct, logger.Fields or
// another ordered type, never a Go map.
func (h *Hub) Broadcast(event string, data any) {
	for _, c := range h.broadcast(event, data) {
		c.stop()
	}
}

// broadcast queues the message for every client under h.mu and returns the
// clients whose queue was full, removed from the set, for the caller to stop
// after the lock is released.
func (h *Hub) broadcast(event string, data any) (dropped []*client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) == 0 {
		return nil
	}
	msg, err := jsonutil.Marshal(envelope{Event: event, Data: data})
	if err != nil {
		h.errorf("Broadcast %s: %s", event, err)
		return nil
	}
	for c := range h.clients {
		// The send never blocks; a client whose queue is full is dropped.
		select {
		case c.send <- msg:
		default:
			delete(h.clients, c)
			dropped = append(dropped, c)
		}
	}
	return dropped
}

func (h *Hub) errorf(format string, args ...any) {
	if h.Errorf != nil {
		h.Errorf(format, args...)
	}
}

// add registers a connected client and queues the welcome message first, so
// no broadcast can reach the client before it. It reports false when the hub
// is closed.
func (h *Hub) add(c *client, welcome []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	if h.clients == nil {
		h.clients = map[*client]struct{}{}
	}
	h.clients[c] = struct{}{}
	c.send <- welcome
	return true
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.stop()
}

// Clients is the number of connected clients.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Run is the ping timer: every interval (PingInterval unless Every is set)
// each client that did not answer the last ping is dropped and the others
// are pinged. It returns when ctx ends.
func (h *Hub) Run(ctx context.Context) {
	every := h.Every
	if every <= 0 {
		every = PingInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.pingAll()
		}
	}
}

func (h *Hub) pingAll() {
	h.mu.Lock()
	var ping, dead []*client
	for c := range h.clients {
		// Swap clears the flag and reports whether a pong came since the
		// last tick, in one step, so a pong that lands now is not lost.
		if !c.alive.Swap(false) {
			delete(h.clients, c)
			dead = append(dead, c)
			continue
		}
		ping = append(ping, c)
	}
	h.mu.Unlock()
	for _, c := range dead {
		c.stop()
	}
	// Ping is safe beside the client's writer and blocks until the pong, so
	// each ping gets its own goroutine and one stalled peer does not hold up
	// the rest. Its error is dropped, as events.js drops ws.ping()'s: a pong
	// that never comes leaves alive false for the next tick, and a write
	// that runs out of time closes the connection, which ends the reader.
	for _, c := range ping {
		go func(c *client) {
			ctx, cancel := context.WithTimeout(context.Background(), pingWait)
			defer cancel()
			c.conn.Ping(ctx)
		}(c)
	}
}

// pong runs on the reader goroutine, inside coder/websocket's read lock, and
// takes no hub lock (see client.alive).
func (h *Hub) pong(c *client) {
	c.alive.Store(true)
}

// Close drops every client and refuses new ones. The server calls it at
// shutdown: http.Server.Shutdown does not track hijacked connections.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	clients := h.clients
	h.clients = nil
	h.mu.Unlock()
	for c := range clients {
		c.stop()
	}
}

// serve runs one connection's writer and reader until either ends.
func (h *Hub) serve(c *client) {
	go func() {
		for {
			select {
			case <-c.done:
				return
			case msg := <-c.send:
				// No deadline, as before: a stalled peer blocks here
				// until stop closes the socket.
				if c.conn.Write(context.Background(), websocket.MessageText, msg) != nil {
					h.remove(c)
					return
				}
			}
		}
	}()

	c.conn.SetReadLimit(maxPayload)
	// Node registers no message handler: data frames are read and dropped.
	// Reading also handles the control frames: it answers a ping (ws's
	// autoPong), calls OnPongReceived for a pong, and answers a close frame
	// with the same code and reason (an empty payload for 1005, no status),
	// as ws does.
	for {
		_, r, err := c.conn.Reader(context.Background())
		if err != nil {
			h.remove(c)
			return
		}
		if _, err := discard(r); err != nil {
			h.remove(c)
			return
		}
	}
}
