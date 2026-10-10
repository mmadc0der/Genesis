//go:build cgo

package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"genesis/pkg/eventer"
)

func init() {
	attachEventerStore = attachListenerEventer
	lookupEventerEvents = lookupOpenEventer
	transcriptFromEventer = transcriptFromOpenEventer
	citedEventsFromEventer = citedEventsFromOpenEventer
	recoverEventerRuns = recoverOpenEventer
	notePublishedEvent = func(event lifecycleEvent) {
		eventerViews.Range(func(_, value any) bool {
			view, ok := value.(*listenerEventer)
			if ok {
				view.noteLive(event)
			}
			return true
		})
	}
}

func attachListenerEventer(store *runStore) {
	if store == nil {
		return
	}
	dir := filepath.Join(store.dataDir, "eventer")
	if _, err := os.Stat(filepath.Join(dir, "schema.lock")); err != nil {
		return
	}
	opened, err := eventer.OpenDefault(dir)
	if err != nil {
		if store.logger != nil {
			store.logger.Error("eventer open failed", "dir", dir, "error", err)
		}
		return
	}
	store.events = opened
	if store.logger != nil {
		store.logger.Info("eventer source enabled", "dir", dir)
	}
}

func (s *eventServer) handleEventerRoutes(w http.ResponseWriter, r *http.Request) bool {
	if s == nil || s.store == nil || s.store.events == nil || r.Method != http.MethodGet {
		return false
	}
	switch {
	case r.URL.Path == "/runs":
		s.serveEventerRuns(w, r)
		return true
	case strings.HasPrefix(r.URL.Path, "/runs/"):
		s.serveEventerRun(w, r)
		return true
	default:
		return false
	}
}

func (s *eventServer) serveEventerRuns(w http.ResponseWriter, r *http.Request) {
	limit := queryLimit(r, "limit", defaultRunLimit, maxRunLimit)
	runs, err := s.store.eventerRunSummaries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if limit >= 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *eventServer) serveEventerRun(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/runs/")
	id, tail, _ := strings.Cut(rest, "/")
	if err := validateRunID(id); err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	if tail == "events" || tail == "transcript" {
		after, err := parseCursor(r.URL.Query().Get("after"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		limit := queryLimit(r, "limit", defaultEventLimit, maxEventLimit)
		var events []lifecycleEvent
		if tail == "transcript" {
			events, err = s.store.eventerTranscript(id)
		} else {
			events, err = s.store.eventerRunEvents(id)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(events) == 0 {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, pageEvents(id, events, after, limit))
		return
	}
	if tail != "" {
		http.NotFound(w, r)
		return
	}
	summary, ok, err := s.store.eventerRunSummary(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, runDetail{runSummary: summary, EventCount: summary.Events})
}

type listenerEventer struct {
	loadMu      sync.Mutex
	recoverOnce sync.Once
	mu          sync.Mutex
	summaries   map[string][]lifecycleEvent
	transcript  map[string][]lifecycleEvent
	pending     []lifecycleEvent
	ready       bool
	store       *eventer.Store
	logger      *slog.Logger
}

func (s *runStore) eventerHandle() *listenerEventer {
	appender, ok := s.events.(*eventer.Store)
	if !ok || appender == nil {
		return nil
	}
	if view, ok := eventerViews.Load(s); ok {
		return view.(*listenerEventer)
	}
	view := &listenerEventer{store: appender, logger: s.logger}
	actual, _ := eventerViews.LoadOrStore(s, view)
	return actual.(*listenerEventer)
}

var eventerViews sync.Map

// recoverOpenEventer ends runs that still look open because the listener
// stopped after a result and before dev.genesis.run.end. A live pid is left
// alone. The summary cache is updated in place so the rail does not keep a
// dead continuation in Running.
func recoverOpenEventer(s *runStore) error {
	if s == nil || s.events == nil {
		return nil
	}
	view := s.eventerHandle()
	if view == nil || view.store == nil {
		return nil
	}
	var recoverErr error
	view.recoverOnce.Do(func() {
		if err := view.load(); err != nil {
			recoverErr = err
			return
		}
		view.mu.Lock()
		open := make([]string, 0)
		for id, events := range view.summaries {
			if !journalHasEnd(events) {
				open = append(open, id)
			}
		}
		view.mu.Unlock()
		for _, id := range open {
			view.mu.Lock()
			cached := append([]lifecycleEvent(nil), view.summaries[id]...)
			view.mu.Unlock()
			if journalHasEnd(cached) {
				continue
			}
			if err := s.endInterrupted(cached); err != nil {
				if s.logger != nil {
					s.logger.Error("recover run journal", "run_id", id, "error", err)
				}
			}
		}
	})
	return recoverErr
}

func (s *runStore) eventerRunSummaries() ([]runSummary, error) {
	view := s.eventerHandle()
	if view == nil {
		return nil, os.ErrNotExist
	}
	if err := view.load(); err != nil {
		return nil, err
	}
	runs := make([]runSummary, 0, len(view.summaries))
	view.mu.Lock()
	copied := make([][]lifecycleEvent, 0, len(view.summaries))
	for _, events := range view.summaries {
		copied = append(copied, events)
	}
	view.mu.Unlock()
	for _, events := range copied {
		summary := summarizeRun(events)
		summary.Events = len(events)
		if summary.RunID != "" {
			runs = append(runs, summary)
		}
	}
	slices.SortFunc(runs, func(left, right runSummary) int {
		if left.AcceptedAt != right.AcceptedAt {
			if left.AcceptedAt < right.AcceptedAt {
				return 1
			}
			return -1
		}
		return strings.Compare(right.RunID, left.RunID)
	})
	return runs, nil
}

func (s *runStore) eventerRunSummary(runID string) (runSummary, bool, error) {
	view := s.eventerHandle()
	if view == nil {
		return runSummary{}, false, nil
	}
	if err := view.load(); err != nil {
		return runSummary{}, false, err
	}
	view.mu.Lock()
	events := append([]lifecycleEvent(nil), view.summaries[runID]...)
	view.mu.Unlock()
	if len(events) == 0 {
		return runSummary{}, false, nil
	}
	summary := summarizeRun(events)
	summary.Events = len(events)
	return summary, true, nil
}

func (s *runStore) eventerRunEvents(runID string) ([]lifecycleEvent, error) {
	view := s.eventerHandle()
	if view == nil {
		return nil, os.ErrNotExist
	}
	return view.readRun(runID)
}

func (v *listenerEventer) load() error {
	v.loadMu.Lock()
	defer v.loadMu.Unlock()
	if v.ready {
		return nil
	}
	grouped := map[string][]lifecycleEvent{}
	started := time.Now()
	for _, eventType := range []string{
		lifecycleTypeAccepted,
		lifecycleTypeStart,
		lifecycleTypeSessionCreated,
		lifecycleTypeTurn,
		lifecycleTypeResult,
		lifecycleTypeError,
		lifecycleTypeEnd,
	} {
		raw, err := queryEventerWindows(v.store, "", eventType)
		if err != nil {
			return err
		}
		for _, item := range raw {
			event, ok := decodeLifecycle(item)
			if !ok || event.RunID == "" {
				continue
			}
			grouped[event.RunID] = append(grouped[event.RunID], event)
		}
	}
	for id, events := range grouped {
		sortLifecycle(events)
		grouped[id] = events
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.transcript == nil {
		v.transcript = map[string][]lifecycleEvent{}
	}
	pending := append([]lifecycleEvent(nil), v.pending...)
	v.summaries = finishEventerLoadMaps(grouped, v.transcript, pending)
	v.pending = nil
	v.ready = true
	if v.logger != nil {
		v.logger.Info("eventer source loaded", "runs", len(grouped), "ms", time.Since(started).Milliseconds())
	}
	return nil
}

func (s *runStore) eventerTranscript(runID string) ([]lifecycleEvent, error) {
	view := s.eventerHandle()
	if view == nil {
		return nil, os.ErrNotExist
	}
	if err := view.load(); err != nil {
		return nil, err
	}
	view.mu.Lock()
	summary := append([]lifecycleEvent(nil), view.summaries[runID]...)
	live := append([]lifecycleEvent(nil), view.transcript[runID]...)
	view.mu.Unlock()
	if len(summary) == 0 {
		return view.readRun(runID)
	}
	from, to := runQueryWindow(summary, time.Now())
	// One run-id query. A type query over the same span would read every
	// run's assistant and tool rows and split on the 64MB response limit.
	full, err := readEventerRunBetween(view.store, runID, from, to)
	if err != nil {
		return nil, err
	}
	view.mu.Lock()
	if len(live) > 0 {
		view.transcript[runID] = dropEventsPresentIn(full, view.transcript[runID])
	}
	view.mu.Unlock()
	return mergeRunEvents(summary, live, full), nil
}

func (v *listenerEventer) noteLive(event lifecycleEvent) {
	if v == nil || event.RunID == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.ready {
		v.pending = append(v.pending, event)
		return
	}
	if v.summaries == nil {
		v.summaries = map[string][]lifecycleEvent{}
	}
	if v.transcript == nil {
		v.transcript = map[string][]lifecycleEvent{}
	}
	applyEventerLiveToMaps(v.summaries, v.transcript, event)
}

func mergeRunEvents(parts ...[]lifecycleEvent) []lifecycleEvent {
	seen := map[string]struct{}{}
	out := make([]lifecycleEvent, 0)
	for _, part := range parts {
		for _, event := range part {
			if event.ID != "" {
				if _, ok := seen[event.ID]; ok {
					continue
				}
				seen[event.ID] = struct{}{}
			}
			out = append(out, event)
		}
	}
	sortLifecycle(out)
	return out
}

func (v *listenerEventer) readRun(runID string) ([]lifecycleEvent, error) {
	if v == nil || v.store == nil {
		return nil, os.ErrNotExist
	}
	if err := v.load(); err != nil {
		return nil, err
	}
	// Lifecycle rows are already loaded. Query only that span so a size-limit
	// split does not bisect the empty range from 1970 to 2100.
	v.mu.Lock()
	summary := append([]lifecycleEvent(nil), v.summaries[runID]...)
	v.mu.Unlock()
	from, to := runQueryWindow(summary, time.Now())
	return readEventerRunBetween(v.store, runID, from, to)
}

func readEventerRun(store *eventer.Store, runID string) ([]lifecycleEvent, error) {
	to := time.Now().Add(time.Minute).UnixMilli()
	return readEventerRunBetween(store, runID, 0, to)
}

func readEventerRunBetween(store *eventer.Store, runID string, from, to int64) ([]lifecycleEvent, error) {
	raw, err := queryEventerSplit(store, from, to, runID, "", "")
	if err != nil {
		return nil, err
	}
	events := make([]lifecycleEvent, 0, len(raw))
	for _, item := range raw {
		event, ok := decodeLifecycle(item)
		if !ok {
			continue
		}
		events = append(events, event)
	}
	sortLifecycle(events)
	return events, nil
}

func queryEventerWindows(store *eventer.Store, runID, eventType string) ([]json.RawMessage, error) {
	to := time.Now().Add(time.Minute).UnixMilli()
	return queryEventerSplit(store, 0, to, runID, eventType, "")
}

func lookupOpenEventer(s *runStore, filter eventFilter) ([]lifecycleEvent, bool, error) {
	view := s.eventerHandle()
	if view == nil || view.store == nil {
		return nil, false, nil
	}
	events, err := collectEventerEvents(view.store, filter, time.Now())
	return events, true, err
}

func transcriptFromOpenEventer(s *runStore, runID string) ([]byte, bool, error) {
	view := s.eventerHandle()
	if view == nil || view.store == nil {
		return nil, false, nil
	}
	events, err := view.readRun(runID)
	if err != nil {
		return nil, true, err
	}
	payload, err := transcriptPayload(events)
	return payload, true, err
}

func citedEventsFromOpenEventer(s *runStore, runID string) ([]lifecycleEvent, bool, error) {
	view := s.eventerHandle()
	if view == nil || view.store == nil {
		return nil, false, nil
	}
	events, err := view.readRun(runID)
	return events, true, err
}

func dropEventsPresentIn(full, pending []lifecycleEvent) []lifecycleEvent {
	if len(pending) == 0 {
		return pending
	}
	seen := make(map[string]struct{}, len(full))
	for _, event := range full {
		if event.ID != "" {
			seen[event.ID] = struct{}{}
		}
	}
	kept := pending[:0]
	for _, event := range pending {
		if event.ID != "" {
			if _, ok := seen[event.ID]; ok {
				continue
			}
		}
		kept = append(kept, event)
	}
	return kept
}
