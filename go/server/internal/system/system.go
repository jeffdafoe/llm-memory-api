// Package system is Node's services/system-handler.js and the notifySystem
// part of services/system-notify.js: chat messages addressed to the "system"
// agent carry a JSON payload whose "type" picks a handler.
package system

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// Meta is what a handler learns about the message besides its payload.
type Meta struct {
	MessageID int
	FromAgent string
	Channel   *string
}

// HandlerFunc handles one payload type.
type HandlerFunc func(ctx context.Context, payload json.RawMessage, meta Meta) error

// Handler routes system messages to the handler registered for their type.
type Handler struct {
	Log *logger.Logger

	mu       sync.RWMutex
	handlers map[string]HandlerFunc
}

// Register sets the handler for a payload type.
func (h *Handler) Register(typ string, fn HandlerFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handlers == nil {
		h.handlers = map[string]HandlerFunc{}
	}
	h.handlers[typ] = fn
}

func (h *Handler) lookup(typ string) (HandlerFunc, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	fn, ok := h.handlers[typ]
	return fn, ok
}

func (h *Handler) log(action string, fields logger.Fields) {
	h.Log.Log("system-handler", action, fields)
}

// HandleMessage parses a message sent to system and runs its handler.
// message nil is a message with no text (Node passed undefined). Callers run
// it in the background, as Node did not await it; every failure is only
// logged.
func (h *Handler) HandleMessage(ctx context.Context, messageID int, fromAgent string, message *string, channel *string) {
	var payload any
	err := errNoText
	if message != nil {
		err = json.Unmarshal([]byte(*message), &payload)
	}
	if err != nil {
		h.log("parse-error", logger.Fields{{Key: "messageId", Value: messageID}, {Key: "fromAgent", Value: fromAgent}, {Key: "error", Value: err.Error()}})
		return
	}
	// `const { type } = payload` threw for null, and the caller dropped the
	// rejection without a log line.
	if payload == nil {
		return
	}
	var typ any
	if obj, ok := payload.(map[string]any); ok {
		typ = obj["type"]
	}
	if !truthy(typ) {
		h.log("missing-type", logger.Fields{{Key: "messageId", Value: messageID}, {Key: "fromAgent", Value: fromAgent}})
		return
	}
	name, isString := typ.(string)
	var fn HandlerFunc
	found := false
	if isString {
		fn, found = h.lookup(name)
	}
	if !found {
		h.log("unknown-type", logger.Fields{{Key: "messageId", Value: messageID}, {Key: "fromAgent", Value: fromAgent}, {Key: "type", Value: typ}})
		return
	}
	h.log("dispatch", logger.Fields{{Key: "messageId", Value: messageID}, {Key: "fromAgent", Value: fromAgent}, {Key: "type", Value: name}})
	if err := fn(ctx, json.RawMessage(*message), Meta{MessageID: messageID, FromAgent: fromAgent, Channel: channel}); err != nil {
		h.log("handler-error", logger.Fields{{Key: "messageId", Value: messageID}, {Key: "fromAgent", Value: fromAgent}, {Key: "type", Value: name}, {Key: "error", Value: err.Error()}})
	}
}

type noTextError struct{}

func (noTextError) Error() string { return `"undefined" is not valid JSON` }

// errNoText is V8's JSON.parse(undefined) message.
var errNoText error = noTextError{}

// truthy is JavaScript truthiness for a value encoding/json decoded.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

// Notifier sends messages to the system agent itself.
type Notifier struct {
	DB      db.DB
	Actors  *actors.Cache
	Handler *Handler
}

// Notified is the delivery row a notification wrote.
type Notified struct {
	ID     int
	SentAt time.Time
}

// NotifySystem stores payload, as JSON.stringify writes it, as a message
// from system to system, then runs its handler in the background. The two
// inserts are separate statements, as in Node. The handler has no time
// bound: the virtual-agent handler waits out the response pacing and slow
// provider calls.
func (n *Notifier) NotifySystem(ctx context.Context, payload any) (*Notified, error) {
	text, err := jsonutil.Marshal(payload)
	if err != nil {
		return nil, err
	}
	message := string(text)
	systemActor, err := n.Actors.RequireByName(ctx, "system")
	if err != nil {
		return nil, err
	}
	sent, err := n.insert(ctx, message, systemActor.ID, nil, systemActor.ID)
	if err != nil {
		return nil, err
	}
	db.BackgroundUnbounded(func(ctx context.Context) {
		n.Handler.HandleMessage(ctx, sent.ID, "system", &message, nil)
	})
	return sent, nil
}
