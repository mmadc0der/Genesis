package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// /api/live is ephemeral fan-out of listener lifecycle events. Control keeps
// one subscription to the listener's /live socket, which publishes every
// journaled event after it is durable. Generation chunks are those events.
// A dropped socket does not lose history: reconnect with after=<sequence>.
// The journal remains the catch-up source and is tailed from the last byte
// offset, not reread from the start.

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
	after  uint64
	gen    uint64
	offset int64
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
	out        chan liveFrame
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
	subs.out = make(chan liveFrame, 256)
	c.addLive(subs)
	c.ensureListenerFeed()
	ctx, cancel := context.WithCancel(r.Context())
	defer func() {
		c.removeLive(subs)
		cancel()
	}()
	go func() {
		defer cancel()
		c.readLive(ctx, conn, subs)
	}()
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-subs.out:
				if err := writeLive(ctx, conn, frame); err != nil {
					return
				}
			}
		}
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
			c.flushLive(ctx, subs)
		case <-ticker.C:
			c.flushLive(ctx, subs)
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

func (c *controlServer) flushLive(ctx context.Context, subs *liveSubs) {
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
		subs.emit(liveFrame{Op: note.op, Error: note.message})
	}

	for runID, watch := range watches {
		path := filepath.Join(c.dataDir, runsDirName, runID, eventsFileName)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				subs.emit(liveFrame{Op: "error", RunID: runID, Error: "run not found"})
				subs.mu.Lock()
				if current, ok := subs.runs[runID]; ok && current.gen == watch.gen {
					delete(subs.runs, runID)
				}
				subs.mu.Unlock()
			}
			continue
		}
		advanced, next, err := c.tailRun(ctx, subs, runID, watch)
		if err != nil {
			continue
		}
		subs.mu.Lock()
		if current, ok := subs.runs[runID]; ok && current.gen == watch.gen && current.offset == watch.offset {
			if current.after == watch.after {
				current.after = advanced
			}
			current.offset = next
			subs.runs[runID] = current
		}
		subs.mu.Unlock()
	}

	if wantRuns {
		runs, err := c.listRuns(defaultRunLimit)
		if err == nil {
			nextSeen := map[string]string{}
			for _, run := range runs {
				stamp := run.State + ":" + run.LastSeq + ":" + run.EndedAt + ":" + run.Usage.stamp()
				if seenRuns[run.RunID] == stamp {
					nextSeen[run.RunID] = stamp
					continue
				}
				copied := run
				if !subs.emit(liveFrame{Op: "run", Topic: "runs", Run: &copied}) {
					continue
				}
				nextSeen[run.RunID] = stamp
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
		if stamp != stateStamp && subs.emit(liveFrame{Op: "state", Topic: "state", State: &notice}) {
			subs.mu.Lock()
			if subs.wantState && subs.stateStamp == stateStamp {
				subs.stateStamp = stamp
			}
			subs.mu.Unlock()
		}
	}
}

func (c *controlServer) tailRun(ctx context.Context, subs *liveSubs, runID string, watch runWatch) (uint64, int64, error) {
	gate := c.runGate(runID)
	gate.Lock()
	defer gate.Unlock()
	idx, err := c.ensureIndexLocked(ctx, runID)
	if err != nil {
		return watch.after, watch.offset, err
	}
	start := watch.offset
	if start <= 0 || start > idx.size {
		start = idx.byteAfter(watch.after)
	}
	advanced := watch.after
	path := filepath.Join(c.dataDir, runsDirName, runID, eventsFileName)
	next, err := walkJournal(path, start, func(event lifecycleEvent, _, _ int64) bool {
		if ctx.Err() != nil {
			return false
		}
		seq, ok := eventSequence(event)
		if !ok || seq <= advanced {
			return true
		}
		subs.mu.Lock()
		current, watching := subs.runs[runID]
		already := watching && current.gen == watch.gen && seq <= current.after
		subs.mu.Unlock()
		if already {
			advanced = seq
			return true
		}
		event.Data = redactPrivateKeys(event.Data)
		copied := event
		if !subs.emit(liveFrame{
			Op:     "event",
			Topic:  "run",
			RunID:  runID,
			Cursor: event.Sequence,
			Event:  &copied,
		}) {
			return false
		}
		advanced = seq
		return true
	})
	return advanced, next, err
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

func (s *liveSubs) emit(frame liveFrame) bool {
	if s == nil || s.out == nil {
		return false
	}
	select {
	case s.out <- frame:
		return true
	default:
		return false
	}
}

func (s *liveSubs) offer(event lifecycleEvent) {
	seq, ok := eventSequence(event)
	if !ok || event.RunID == "" {
		return
	}
	s.mu.Lock()
	watch, watching := s.runs[event.RunID]
	if !watching || seq <= watch.after {
		s.mu.Unlock()
		return
	}
	copied := event
	frame := liveFrame{
		Op:     "event",
		Topic:  "run",
		RunID:  event.RunID,
		Cursor: event.Sequence,
		Event:  &copied,
	}
	if !s.emit(frame) {
		s.mu.Unlock()
		return
	}
	watch.after = seq
	s.runs[event.RunID] = watch
	s.mu.Unlock()
}

func (c *controlServer) addLive(subs *liveSubs) {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if c.liveSet == nil {
		c.liveSet = map[*liveSubs]struct{}{}
	}
	c.liveSet[subs] = struct{}{}
}

func (c *controlServer) removeLive(subs *liveSubs) {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	delete(c.liveSet, subs)
}

func (c *controlServer) deliverListenerEvent(event lifecycleEvent) {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	for subs := range c.liveSet {
		subs.offer(event)
	}
}

func (c *controlServer) ensureListenerFeed() {
	c.feedOnce.Do(func() {
		go c.loopListenerFeed()
	})
}

func (c *controlServer) loopListenerFeed() {
	backoff := 200 * time.Millisecond
	for {
		err := c.readListenerFeed()
		if err != nil {
			c.logger.Info("listener event subscription dropped", "error", err)
		}
		time.Sleep(backoff)
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (c *controlServer) readListenerFeed() error {
	header := http.Header{}
	if c.syncToken != "" {
		header.Set("Authorization", "Bearer "+c.syncToken)
	}
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, listenerLiveURL(c.listenerURL), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return err
	}
	c.feedReadyOnce.Do(func() {
		if c.feedReady != nil {
			close(c.feedReady)
		}
	})
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(maxFrameBytes)
	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var event lifecycleEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			continue
		}
		c.deliverListenerEvent(event)
	}
}

func listenerLiveURL(raw string) string {
	switch {
	case strings.HasPrefix(raw, "https://"):
		return "wss://" + strings.TrimPrefix(raw, "https://") + "/live"
	case strings.HasPrefix(raw, "http://"):
		return "ws://" + strings.TrimPrefix(raw, "http://") + "/live"
	default:
		return strings.TrimRight(raw, "/") + "/live"
	}
}
