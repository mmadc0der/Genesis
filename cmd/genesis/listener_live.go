package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// GET /live is the listener's event subscription. Every lifecycle event is
// published here after it has been redacted and appended to events.jsonl.
// Message chunks are ordinary events on this stream. The socket is not a
// second journal: a slow reader is closed and catches up from the file.

func (s *eventServer) handleLiveEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method must be GET", http.StatusMethodNotAllowed)
		return
	}
	if s.syncToken != "" && !s.authorizedSync(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.store == nil || s.store.bus == nil {
		http.Error(w, "event subscription is unavailable", http.StatusServiceUnavailable)
		return
	}
	events, unsubscribe := s.store.bus.Subscribe()
	defer unsubscribe()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log().Info("listener live rejected", "error", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(4096)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			writeCtx, cancelWrite := context.WithTimeout(ctx, 5*time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, payload)
			cancelWrite()
			if err != nil {
				return
			}
		}
	}
}
