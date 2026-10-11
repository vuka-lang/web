package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// streams serves development event streams (server-sent events): each
// sends a value when a page connects, then each new value as it changes,
// read every 100ms on the server, so the page asks nothing. They end with
// the request, on the app's Stop, and on Shutdown of any server that
// served one, so a graceful shutdown doesn't wait on them.
type streams struct {
	stop    chan struct{}
	once    sync.Once
	servers sync.Map // *http.Server → struct{}: those told to end the streams on Shutdown
}

func newStreams() *streams { return &streams{stop: make(chan struct{})} }

// serve streams what current returns while ok, each time it differs from
// the last sent.
func (s *streams) serve(w http.ResponseWriter, r *http.Request, current func() (string, bool)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server); ok {
		if _, seen := s.servers.LoadOrStore(srv, struct{}{}); !seen {
			srv.RegisterOnShutdown(s.close)
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sent, first := "", true
	check := time.NewTicker(100 * time.Millisecond)
	defer check.Stop()
	idle := time.Now()
	for {
		if v, ok := current(); ok && (first || v != sent) {
			sent, first, idle = v, false, time.Now()
			fmt.Fprintf(w, "data: %s\n\n", v)
			flusher.Flush()
		} else if time.Since(idle) > 15*time.Second {
			idle = time.Now()
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
		select {
		case <-check.C:
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		}
	}
}

func (s *streams) close() {
	if s != nil {
		s.once.Do(func() { close(s.stop) })
	}
}
