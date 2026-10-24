// Package events is an in-process pub/sub hub, relayed to browsers as SSE.
package events

import "sync"

// Event names.
const (
	Jobs = "jobs"
	Scan = "scan"
)

// Hub fans out event names. A slow subscriber misses events instead of
// blocking; an event only means "something changed".
type Hub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

// New returns an empty Hub.
func New() *Hub {
	return &Hub{subs: make(map[chan string]struct{})}
}

// Publish sends the event name to every subscriber that has room for it.
func (h *Hub) Publish(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- name:
		default:
		}
	}
}

// Subscribe returns a channel of event names and an unsubscribe func.
func (h *Hub) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}
