package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	maxStreams     = 32                     // concurrent live page connections
	streamThrottle = 300 * time.Millisecond // at most ~3 updates per second per client
	streamPing     = 20 * time.Second
)

func (s *Server) handleLivePage(w http.ResponseWriter, _ *http.Request) {
	page, err := staticFS.ReadFile("static/live.html")
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(page)
}

// handleLiveEvents streams the activity state as Server-Sent Events: a full "state" message
// on connect, then one after each change (throttled), with only the new feed events.
func (s *Server) handleLiveEvents(w http.ResponseWriter, r *http.Request) {
	if s.streams.Add(1) > maxStreams {
		s.streams.Add(-1)
		http.Error(w, "too many live viewers", http.StatusServiceUnavailable)
		return
	}
	defer s.streams.Add(-1)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	var since int64
	ping := time.NewTicker(streamPing)
	defer ping.Stop()
	if _, err := fmt.Fprint(w, "retry: 5000\n\n"); err != nil {
		return
	}
	for {
		changed := s.Live.Changed() // before the snapshot, so no change is missed
		snap := s.Live.Snapshot(since)
		since = snap.LastSeq
		data, err := json.Marshal(snap)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
			return
		}
		if rc.Flush() != nil {
			return
		}
		// Wait for a change, keeping the connection alive through proxies meanwhile.
		for waiting := true; waiting; {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					return
				}
			case <-changed:
				waiting = false
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamThrottle):
		}
	}
}
