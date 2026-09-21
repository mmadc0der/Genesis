package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// /api/live is ephemeral fan-out. Event frames are read from events.jsonl.
// A dropped socket does not lose history: reconnect with after=<sequence>
// and the file, not the socket, is authoritative.

type liveClientOp struct {
	Op    string `json:"op"`
	Topic string `json:"topic"`
	RunID string `json:"run_id,omitempty"`
	After string `json:"after,omitempty"`
}

type liveFrame struct {
	Op     string           `json:"op"`
	Topic  string           `json:"topic,omitempty"`
	RunID  string           `json:"run_id,omitempty"`
	Cursor string           `json:"cursor,omitempty"`
	Event  *lifecycleEvent  `json:"event,omitempty"`
	Run    *runSummary      `json:"run,omitempty"`
	State  *liveStateNotice `json:"state,omitempty"`
	Error  string           `json:"error,omitempty"`
}

type liveStateNotice struct {
	Drift          string `json:"drift"`
	ActiveDigest   string `json:"active_digest,omitempty"`
	DesiredDigest  string `json:"desired_digest,omitempty"`
	ListenerOK     bool   `json:"listener_ok"`
	Syncing        bool   `json:"syncing"`
	SyncConfigured bool   `json:"sync_configured"`
	DesiredError   string `json:"desired_error,omitempty"`
}

type runWatch struct {
	after uint64
	gen   uint64
}

type liveNote struct {
	op      string
	message string
}

type liveSubs struct {
	mu         sync.Mutex
	runs       map[string]runWatch
	runGen     uint64
	wantRuns   bool
	wantState  bool
	seenRuns   map[string]string
	stateStamp string
	notes      []liveNote
	wake       chan struct{}
}

func newLiveSubs() *liveSubs {
	return &liveSubs{
		runs:     map[string]runWatch{},
		seenRuns: map[string]string{},
		wake:     make(chan struct{}, 1),
	}
}

func (s *liveSubs) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *liveSubs) pushNote(op, message string) {
	s.mu.Lock()
	s.notes = append(s.notes, liveNote{op: op, message: message})
	s.mu.Unlock()
	s.kick()
}

func (c *controlServer) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method must be GET", http.StatusMethodNotAllowed)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"127.0.0.1:*", "localhost:*", "[::1]:*"},
	})
	if err != nil {
		c.logger.Info("control live rejected", "error", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(maxSyncBytes)

	subs := newLiveSubs()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		c.readLive(ctx, conn, subs)
	}()

	interval := c.pollEvery
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-subs.wake:
			if err := c.flushLive(ctx, conn, subs); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.flushLive(ctx, conn, subs); err != nil {
				return
			}
		}
	}
}

func (c *controlServer) readLive(ctx context.Context, conn *websocket.Conn, subs *liveSubs) {
	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var op liveClientOp
		if err := json.Unmarshal(payload, &op); err != nil {
			subs.pushNote("error", "invalid live JSON")
			continue
		}
		switch op.Op {
		case "subscribe":
			c.applySubscribe(subs, op)
		case "unsubscribe":
			c.applyUnsubscribe(subs, op)
		case "ping":
			subs.pushNote("pong", "")
		default:
			subs.pushNote("error", "unknown live op")
		}
	}
}

func (c *controlServer) applySubscribe(subs *liveSubs, op liveClientOp) {
	subs.mu.Lock()
	defer subs.mu.Unlock()
	switch op.Topic {
	case "run":
		if err := validateRunID(op.RunID); err != nil {
			subs.notes = append(subs.notes, liveNote{op: "error", message: "invalid run id"})
			break
		}
		after, err := parseCursor(op.After)
		if err != nil {
			subs.notes = append(subs.notes, liveNote{op: "error", message: err.Error()})
			break
		}
		subs.runGen++
		subs.runs[op.RunID] = runWatch{after: after, gen: subs.runGen}
	case "runs":
		subs.wantRuns = true
		subs.seenRuns = map[string]string{}
	case "state":
		subs.wantState = true
		subs.stateStamp = ""
	default:
		subs.notes = append(subs.notes, liveNote{op: "error", message: "unknown live topic"})
	}
	subs.kick()
}

func (c *controlServer) applyUnsubscribe(subs *liveSubs, op liveClientOp) {
	subs.mu.Lock()
	defer subs.mu.Unlock()
	switch op.Topic {
	case "run":
		delete(subs.runs, op.RunID)
	case "runs":
		subs.wantRuns = false
	case "state":
		subs.wantState = false
	default:
		subs.notes = append(subs.notes, liveNote{op: "error", message: "unknown live topic"})
	}
	subs.kick()
}

func (c *controlServer) flushLive(ctx context.Context, conn *websocket.Conn, subs *liveSubs) error {
	subs.mu.Lock()
	notes := append([]liveNote(nil), subs.notes...)
	subs.notes = nil
	watches := cloneWatches(subs.runs)
	wantRuns := subs.wantRuns
	wantState := subs.wantState
	seenRuns := cloneStrings(subs.seenRuns)
	stateStamp := subs.stateStamp
	subs.mu.Unlock()

	for _, note := range notes {
		frame := liveFrame{Op: note.op, Error: note.message}
		if err := writeLive(ctx, conn, frame); err != nil {
			return err
		}
	}

	for runID, watch := range watches {
		events, err := c.readRunEvents(runID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if err := writeLive(ctx, conn, liveFrame{Op: "error", RunID: runID, Error: "run not found"}); err != nil {
					return err
				}
				subs.mu.Lock()
				if current, ok := subs.runs[runID]; ok && current.gen == watch.gen {
					delete(subs.runs, runID)
				}
				subs.mu.Unlock()
			}
			continue
		}
		var last uint64
		delivered := false
		for _, event := range events {
			seq, ok := eventSequence(event)
			if !ok || seq <= watch.after {
				continue
			}
			copied := event
			if err := writeLive(ctx, conn, liveFrame{
				Op:     "event",
				Topic:  "run",
				RunID:  runID,
				Cursor: event.Sequence,
				Event:  &copied,
			}); err != nil {
				return err
			}
			last = seq
			delivered = true
		}
		if !delivered {
			continue
		}
		subs.mu.Lock()
		if current, ok := subs.runs[runID]; ok && current.gen == watch.gen && current.after == watch.after {
			current.after = last
			subs.runs[runID] = current
		}
		subs.mu.Unlock()
	}

	if wantRuns {
		runs, err := c.listRuns(defaultRunLimit)
		if err == nil {
			nextSeen := map[string]string{}
			for _, run := range runs {
				stamp := run.State + ":" + run.LastSeq + ":" + run.EndedAt
				nextSeen[run.RunID] = stamp
				if seenRuns[run.RunID] == stamp {
					continue
				}
				copied := run
				if err := writeLive(ctx, conn, liveFrame{Op: "run", Topic: "runs", Run: &copied}); err != nil {
					return err
				}
			}
			subs.mu.Lock()
			if subs.wantRuns {
				subs.seenRuns = nextSeen
			}
			subs.mu.Unlock()
		}
	}

	if wantState {
		notice := c.liveNotice(ctx)
		stamp := notice.Drift + "|" + notice.ActiveDigest + "|" + notice.DesiredDigest + "|" + notice.DesiredError
		if notice.Syncing {
			stamp += "|syncing"
		}
		if notice.ListenerOK {
			stamp += "|ok"
		}
		if notice.SyncConfigured {
			stamp += "|configured"
		}
		if stamp != stateStamp {
			if err := writeLive(ctx, conn, liveFrame{Op: "state", Topic: "state", State: &notice}); err != nil {
				return err
			}
			subs.mu.Lock()
			if subs.wantState && subs.stateStamp == stateStamp {
				subs.stateStamp = stamp
			}
			subs.mu.Unlock()
		}
	}
	return nil
}

func (c *controlServer) liveNotice(ctx context.Context) liveStateNotice {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.listenerURL+"/", nil)
	if err != nil {
		return liveStateNotice{Drift: driftListenerDown}
	}
	state := c.currentState(request)
	notice := liveStateNotice{
		Drift:          state.Drift,
		ListenerOK:     state.Listener.OK && state.Listener.Reachable,
		Syncing:        state.Listener.Syncing,
		SyncConfigured: state.Listener.SyncConfigured,
		DesiredError:   state.DesiredError,
	}
	if state.Active != nil {
		notice.ActiveDigest = state.Active.Digest
		if state.Active.SyncConfigured != nil {
			notice.SyncConfigured = *state.Active.SyncConfigured
		}
		if state.Active.Syncing != nil {
			notice.Syncing = *state.Active.Syncing
		}
	}
	if state.Desired != nil {
		notice.DesiredDigest = state.Desired.Digest
	}
	return notice
}

func writeLive(ctx context.Context, conn *websocket.Conn, frame liveFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, payload)
}

func cloneWatches(in map[string]runWatch) map[string]runWatch {
	out := make(map[string]runWatch, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
