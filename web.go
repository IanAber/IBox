package main

// Web dashboard + JSON API. Replaces the FlowFuse Dashboard pages
// (/dashboard/configuration and /dashboard/eg4-test) and the "/" redirect.
// Live updates are pushed to every open browser with Server-Sent Events.

import (
	"embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
)

//go:embed web/index.html
var webFS embed.FS

type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func NewHub() *Hub { return &Hub{clients: map[chan []byte]struct{}{}} }

func sseFrame(event string, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("web: marshal %s: %v", event, err)
		return nil
	}
	return []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
}

func (h *Hub) Broadcast(event string, v any) {
	frame := sseFrame(event, v)
	if frame == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c <- frame:
		default: // slow client; drop rather than block the controller
		}
	}
}

func (h *Hub) add() chan []byte {
	c := make(chan []byte, 32)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) remove(c chan []byte) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("web: writing JSON response: %v", err)
	}
}

type enableReq struct {
	Enabled *bool `json:"enabled"`
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	page, _ := webFS.ReadFile("web/index.html")

	servePage := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(page) // client may have gone away; nothing useful to do
	}

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/configuration", http.StatusFound)
	})
	mux.HandleFunc("GET /dashboard/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/configuration", http.StatusFound)
	})
	mux.HandleFunc("GET /dashboard/configuration", servePage)
	mux.HandleFunc("GET /dashboard/eg4-test", servePage)

	mux.HandleFunc("GET /dashboard/events", a.handleEvents)
	mux.HandleFunc("GET /api/eg4/raw", a.handleRawDump)

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		resp := map[string]any{
			"config":   a.cfg,
			"status":   a.lastStatus,
			"sim":      a.simStateLocked(),
			"haveData": a.haveData,
			"health":   a.healthLocked(a.now()),
		}
		a.mu.Unlock()
		writeJSON(w, 200, resp)
	})

	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		cfg, err := a.SaveConfig(body)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, cfg)
	})

	mux.HandleFunc("POST /api/config/reload", func(w http.ResponseWriter, r *http.Request) {
		a.LoadConfigFromDisk()
		a.mu.Lock()
		cfg := a.cfg
		a.mu.Unlock()
		writeJSON(w, 200, cfg)
	})

	mux.HandleFunc("POST /api/sim/data", func(w http.ResponseWriter, r *http.Request) {
		var d EG4Data
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&d); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if !a.StoreSimulatedData(d) {
			writeJSON(w, 409, map[string]string{"error": "live EG4 read is enabled; test data ignored"})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	toggle := func(set func(bool)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req enableReq
			if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil || req.Enabled == nil {
				writeJSON(w, 400, map[string]string{"error": `body must be {"enabled": true|false}`})
				return
			}
			set(*req.Enabled)
			writeJSON(w, 200, a.SimState())
		}
	}
	mux.HandleFunc("POST /api/sim/read-enable", toggle(a.SetReadEnabled))
	mux.HandleFunc("POST /api/sim/relay-actions", toggle(a.SetRelayActionsEnabled))
	mux.HandleFunc("POST /api/sim/relay-hw-simulated", toggle(a.SetRelayHWSimulated))

	return mux
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	c := a.hub.add()
	defer a.hub.remove(c)

	// Initial snapshot (like resendOnRefresh on the dashboard templates).
	a.mu.Lock()
	initial := [][]byte{
		sseFrame("config", a.cfg),
		sseFrame("simstate", a.simStateLocked()),
		sseFrame("health", a.healthLocked(a.now())),
	}
	if a.lastStatus != nil {
		initial = append(initial, sseFrame("status", a.lastStatus))
	}
	a.mu.Unlock()
	for _, f := range initial {
		if _, err := w.Write(f); err != nil {
			return // client went away
		}
	}
	fl.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case f := <-c:
			if _, err := w.Write(f); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
