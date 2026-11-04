// Package events is the hook for live admin-dashboard events. The SSE stream
// and WebSocket that deliver them are ported with the admin API; until then
// the server uses Nop.
package events

// Broadcaster sends an event to every connected admin dashboard.
type Broadcaster interface {
	Broadcast(event string, data any)
}

// Nop drops every event.
type Nop struct{}

func (Nop) Broadcast(string, any) {}
