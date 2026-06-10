package server

import (
	"fmt"
	"net/http"
	"sync"
)

// SSEEvent is a server-sent event.
type SSEEvent struct {
	Type string // event type: model_checked, provider_done, check_completed, event_created
	Data string // payload (HTML fragment or JSON string)
}

// SSEHub manages SSE client connections and broadcasts events.
type SSEHub struct {
	mu      sync.RWMutex
	clients map[chan SSEEvent]struct{}
}

func NewSSEHub() *SSEHub {
	return &SSEHub{
		clients: make(map[chan SSEEvent]struct{}),
	}
}

func (h *SSEHub) Subscribe() (chan SSEEvent, func()) {
	ch := make(chan SSEEvent, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()

	unsub := func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
		// No drain needed: Publish uses non-blocking sends, so no goroutine is ever
		// parked waiting to send on ch. Once removed from h.clients (under the write
		// lock, which waits out any in-flight Publish), nothing else references ch.
		// A `for range ch` here would block forever — nobody closes ch — leaking the
		// HandleSSE goroutine and the channel on every client disconnect.
	}
	return ch, unsub
}

func (h *SSEHub) Publish(event SSEEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for ch := range h.clients {
		select {
		case ch <- event:
		default:
			// Client buffer full, skip (don't block other clients)
		}
	}
}

func (h *SSEHub) HandleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := h.Subscribe()
	defer unsub()

	// Send initial keepalive
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, event.Data)
			flusher.Flush()
		}
	}
}
