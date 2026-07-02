// Package web implements the browser simulator device: it renders the round
// screen in a web page and feeds the daemon decisions from on-screen buttons.
//
// It satisfies the daemon's Device interface, so from the daemon's point of
// view the simulator and the real BLE Dial are interchangeable. The simulator
// uses Server-Sent Events (host -> browser) and a plain POST (browser -> host),
// keeping the daemon dependency-free.
package web

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
)

//go:embed index.html
var indexHTML []byte

// Hub is the simulator transport: it fans a snapshot out to every connected
// browser and collects their decisions.
type Hub struct {
	mu        sync.Mutex
	clients   map[chan []byte]struct{}
	last      []byte
	decisions chan protocol.Decision
}

// NewHub returns a ready simulator hub.
func NewHub() *Hub {
	return &Hub{
		clients:   make(map[chan []byte]struct{}),
		decisions: make(chan protocol.Decision, 16),
	}
}

// RegisterRoutes wires the simulator's HTTP endpoints onto mux.
func (h *Hub) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", h.serveIndex)
	mux.HandleFunc("/events", h.serveEvents)
	mux.HandleFunc("/decision", h.serveDecision)
}

// Update pushes a fresh snapshot to all connected browsers. Implements
// daemon.Device.
func (h *Hub) Update(snap protocol.Snapshot) {
	payload, err := json.Marshal(snap)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.last = payload
	for ch := range h.clients {
		select {
		case ch <- payload:
		default: // slow client: drop this frame, it'll get the next one
		}
	}
	h.mu.Unlock()
}

// Connected reports whether at least one browser is watching. Implements
// daemon.Device.
func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) > 0
}

// Decisions is the stream of answers coming back from the simulator. Implements
// daemon.Device.
func (h *Hub) Decisions() <-chan protocol.Decision {
	return h.decisions
}

// FirmwareVersion is always "" — the simulator is not a physical device with
// firmware to update. Implements daemon.Device.
func (h *Hub) FirmwareVersion() string { return "" }

func (h *Hub) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (h *Hub) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	last := h.last
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}()

	if last != nil {
		writeSSE(w, last)
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case payload := <-ch:
			writeSSE(w, payload)
			flusher.Flush()
		}
	}
}

func (h *Hub) serveDecision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var d protocol.Decision
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.SessionID == "" {
		http.Error(w, "bad decision", http.StatusBadRequest)
		return
	}
	select {
	case h.decisions <- d:
	default: // decision buffer full; the request will time out and fall back to ask
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeSSE(w http.ResponseWriter, data []byte) {
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
}
