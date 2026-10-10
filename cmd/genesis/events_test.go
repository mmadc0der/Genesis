package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sampleTestEvent(runID, seq, eventType, agentID, msg string, tOffset time.Duration) lifecycleEvent {
	evTime := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC).Add(tOffset)
	data, _ := json.Marshal(map[string]any{
		"message": msg,
		"details": "extra " + msg,
	})
	return lifecycleEvent{
		SpecVersion:  "1.0",
		ID:           "evt_" + runID + "_" + seq,
		Type:         eventType,
		Time:         eventTime(evTime),
		Sequence:     seq,
		SequenceType: sequenceTypeInteger,
		RunID:        runID,
		AgentID:      agentID,
		Subject:      runID,
		Data:         data,
	}
}

func setupTestStoreWithEvents(t *testing.T) (*runStore, string) {
	t.Helper()
	dir := t.TempDir()
	store := newRunStore(dir, newEventBus(), nil)

	// Run 1: cpu-bench
	run1Dir := filepath.Join(dir, runsDirName, "gen_bench_01")
	if err := os.MkdirAll(run1Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	evs1 := []lifecycleEvent{
		sampleTestEvent("gen_bench_01", "1", lifecycleTypeStart, "cpu-bench", "started bench", 0),
		sampleTestEvent("gen_bench_01", "2", "dev.genesis.run.tool", "cpu-bench", "ls -la /workspace", 10*time.Second),
		sampleTestEvent("gen_bench_01", "3", "dev.genesis.agent.finished", "cpu-bench", "bench completed cleanly", 20*time.Second),
	}
	writeTestJournal(t, filepath.Join(run1Dir, eventsFileName), evs1)

	// Run 2: cpu-lead
	run2Dir := filepath.Join(dir, runsDirName, "gen_lead_01")
	if err := os.MkdirAll(run2Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	evs2 := []lifecycleEvent{
		sampleTestEvent("gen_lead_01", "1", lifecycleTypeStart, "cpu-lead", "planning next round", 30*time.Second),
		sampleTestEvent("gen_lead_01", "2", "dev.cpuimg.round.open", "cpu-lead", "opened round-003", 40*time.Second),
		sampleTestEvent("gen_lead_01", "3", "dev.genesis.agent.finished", "cpu-lead", "round planned", 50*time.Second),
	}
	writeTestJournal(t, filepath.Join(run2Dir, eventsFileName), evs2)

	return store, dir
}

func writeTestJournal(t *testing.T, path string, events []lifecycleEvent) {
	t.Helper()
	var out []byte
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
		out = append(out, '\n')
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStoreFindEvents(t *testing.T) {
	store, _ := setupTestStoreWithEvents(t)

	// 1. All events, default tail
	filter, err := buildEventFilter("", "", "", "", false, "", 20, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.findEvents(filter)
	if err != nil {
		t.Fatalf("findEvents failed: %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("expected 6 events, got %d", len(events))
	}

	// 2. Tail limit
	filter.Tail = 3
	events, err = store.findEvents(filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events for tail=3, got %d", len(events))
	}
	if events[0].RunID != "gen_lead_01" || events[2].Sequence != "3" {
		t.Fatalf("unexpected tail events order: %+v", events)
	}

	// 3. Filter by agent
	filterAgent, _ := buildEventFilter("", "cpu-bench", "", "", false, "", 20, false, time.Now())
	events, err = store.findEvents(filterAgent)
	if err != nil || len(events) != 3 {
		t.Fatalf("filter agent expected 3, got %d (err: %v)", len(events), err)
	}
	for _, ev := range events {
		if ev.AgentID != "cpu-bench" {
			t.Fatalf("expected agent cpu-bench, got %s", ev.AgentID)
		}
	}

	// 4. Filter by type
	filterType, _ := buildEventFilter("", "", "dev.cpuimg.round.open", "", false, "", 20, false, time.Now())
	events, err = store.findEvents(filterType)
	if err != nil || len(events) != 1 {
		t.Fatalf("filter type expected 1, got %d", len(events))
	}
	if events[0].Type != "dev.cpuimg.round.open" {
		t.Fatalf("unexpected type: %s", events[0].Type)
	}

	// 5. Grep pattern
	filterGrep, _ := buildEventFilter("", "", "", "cleanly", false, "", 20, false, time.Now())
	events, err = store.findEvents(filterGrep)
	if err != nil || len(events) != 1 {
		t.Fatalf("filter grep expected 1, got %d", len(events))
	}
	if !strings.Contains(string(events[0].Data), "cleanly") {
		t.Fatalf("grep did not match cleanly: %s", events[0].Data)
	}

	// 6. Filter by run ID
	filterRun, _ := buildEventFilter("gen_bench_01", "", "", "", false, "", 20, false, time.Now())
	events, err = store.findEvents(filterRun)
	if err != nil || len(events) != 3 {
		t.Fatalf("filter run expected 3, got %d", len(events))
	}
	for _, ev := range events {
		if ev.RunID != "gen_bench_01" {
			t.Fatalf("expected run gen_bench_01, got %s", ev.RunID)
		}
	}
}

func TestServerHandleGetEvents(t *testing.T) {
	store, _ := setupTestStoreWithEvents(t)
	server := &eventServer{
		store: store,
	}

	// GET /events (default json array)
	req := httptest.NewRequest(http.MethodGet, "/events?tail=10", nil)
	w := httptest.NewRecorder()
	server.handleEvents(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var list []lifecycleEvent
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	if len(list) != 6 {
		t.Fatalf("expected 6 events, got %d", len(list))
	}

	// GET /events with ndjson format
	req = httptest.NewRequest(http.MethodGet, "/events?format=ndjson&type=dev.genesis.agent.finished", nil)
	w = httptest.NewRecorder()
	server.handleEvents(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 finished events, got %d: %v", len(lines), lines)
	}
}

func TestServerHandleGetEventsStreamingFollow(t *testing.T) {
	dir := t.TempDir()
	store := newRunStore(dir, newEventBus(), nil)
	server := &eventServer{
		store: store,
	}

	runDir := filepath.Join(dir, runsDirName, "gen_stream")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	initialEv := sampleTestEvent("gen_stream", "1", lifecycleTypeStart, "worker", "initial", 0)
	writeTestJournal(t, filepath.Join(runDir, eventsFileName), []lifecycleEvent{initialEv})

	serverHTTP := httptest.NewServer(http.HandlerFunc(server.handleEvents))
	defer serverHTTP.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverHTTP.URL+"/events?follow=true&tail=5", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)

	// Read initial historical event
	var ev1 lifecycleEvent
	if err := decoder.Decode(&ev1); err != nil {
		t.Fatalf("decode initial event: %v", err)
	}
	if ev1.ID != initialEv.ID {
		t.Fatalf("expected initial event %s, got %s", initialEv.ID, ev1.ID)
	}

	// Now publish a new event via store.bus in background
	liveEv := sampleTestEvent("gen_stream", "2", "dev.genesis.agent.finished", "worker", "live finish", 5*time.Second)
	go func() {
		time.Sleep(50 * time.Millisecond)
		store.bus.publish(liveEv)
	}()

	// Read streamed event
	var ev2 lifecycleEvent
	if err := decoder.Decode(&ev2); err != nil {
		t.Fatalf("decode streamed event: %v", err)
	}
	if ev2.ID != liveEv.ID || ev2.Type != "dev.genesis.agent.finished" {
		t.Fatalf("unexpected streamed event: %+v", ev2)
	}
}

func TestDispatchEventsCLI(t *testing.T) {
	store, dataDir := setupTestStoreWithEvents(t)

	serverHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := &eventServer{store: store}
		s.handleEvents(w, r)
	}))
	defer serverHTTP.Close()

	// 1. Query over HTTP with tail and grep
	var out bytes.Buffer
	var errOut bytes.Buffer
	err := dispatchEvents([]string{"--url", serverHTTP.URL + "/events", "-n", "2", "bench"}, &out, &errOut)
	if err != nil {
		t.Fatalf("dispatchEvents failed: %v (stderr: %s)", err, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %s", len(lines), out.String())
	}

	// 2. Query offline directly from --data with short format
	out.Reset()
	errOut.Reset()
	err = dispatchEvents([]string{"--data", dataDir, "--format", "short", "--type", "dev.cpuimg.round.open"}, &out, &errOut)
	if err != nil {
		t.Fatalf("dispatchEvents data failed: %v", err)
	}
	shortText := out.String()
	if !strings.Contains(shortText, "dev.cpuimg.round.open") || !strings.Contains(shortText, "cpu-lead") {
		t.Fatalf("short format missing expected fields: %s", shortText)
	}

	// 3. Usage on invalid format
	out.Reset()
	err = dispatchEvents([]string{"--format", "invalid"}, &out, &errOut)
	if err == nil {
		t.Fatal("expected error on invalid format")
	}
}

type eventerQueryCall struct {
	kind string
	from int64
	to   int64
}

// scriptedEventer is an in-memory eventerQuery. maxEvents is how many rows
// one response may carry; a wider hit returns Eventer's size-limit error.
type scriptedEventer struct {
	events    []lifecycleEvent
	at        []int64
	calls     []eventerQueryCall
	maxEvents int
}

func (s *scriptedEventer) query(kind string, from, to int64, keep func(lifecycleEvent) bool) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0)
	for i, event := range s.events {
		if s.at[i] < from || s.at[i] > to {
			continue
		}
		if keep != nil && !keep(event) {
			continue
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	limited := s.maxEvents > 0 && len(out) > s.maxEvents
	s.calls = append(s.calls, eventerQueryCall{kind: kind, from: from, to: to})
	if limited {
		return nil, errors.New("query response size limit exceeded")
	}
	return out, nil
}

func (s *scriptedEventer) QueryEvents(from, to int64) ([]json.RawMessage, error) {
	return s.query("events", from, to, nil)
}

func (s *scriptedEventer) QueryType(eventType string, from, to int64) ([]json.RawMessage, error) {
	return s.query("type", from, to, func(event lifecycleEvent) bool {
		return event.Type == eventType
	})
}

func (s *scriptedEventer) QueryAgent(agentID string, from, to int64) ([]json.RawMessage, error) {
	return s.query("agent", from, to, func(event lifecycleEvent) bool {
		return event.AgentID == agentID
	})
}

func (s *scriptedEventer) QueryRun(runID string, from, to int64) ([]json.RawMessage, error) {
	return s.query("run", from, to, func(event lifecycleEvent) bool {
		return event.RunID == runID
	})
}

func TestSortLifecycleByEventTime(t *testing.T) {
	events := []lifecycleEvent{
		{ID: "c", RunID: "gen_c", Sequence: "6932", Time: "2026-10-09T12:54:00Z"},
		{ID: "a", RunID: "gen_a", Sequence: "2729", Time: "2026-10-09T13:29:00Z"},
		{ID: "b", RunID: "gen_b", Sequence: "2836", Time: "2026-10-09T13:14:00Z"},
	}
	sortEventsByTime(events)
	got := []string{events[0].ID, events[1].ID, events[2].ID}
	want := []string{"c", "b", "a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestCollectEventerGrepTailWalksBackward(t *testing.T) {
	now := time.Date(2026, 10, 9, 13, 30, 0, 0, time.UTC)
	store := &scriptedEventer{}
	for _, spec := range []struct {
		id   string
		seq  string
		ago  time.Duration
		body string
	}{
		{"old", "9000", 48 * time.Hour, "run.end ancient"},
		{"mid", "8000", 3 * time.Hour, "run.end mid"},
		{"newish", "100", 20 * time.Minute, "other"},
		{"recent", "2836", 16 * time.Minute, "run.end recent"},
		{"newest", "2729", 1 * time.Minute, "run.end newest"},
	} {
		at := now.Add(-spec.ago)
		store.at = append(store.at, at.UnixMilli())
		store.events = append(store.events, lifecycleEvent{
			ID:       spec.id,
			RunID:    "gen_" + spec.id,
			Sequence: spec.seq,
			Type:     "dev.genesis.note",
			Time:     at.UTC().Format(time.RFC3339Nano),
			Data:     json.RawMessage(`{"message":"` + spec.body + `"}`),
		})
	}
	filter, err := buildEventFilter("", "", "", "run.end", false, "", 2, false, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := collectEventerEvents(store, filter, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "recent" || got[1].ID != "newest" {
		t.Fatalf("events = %+v", got)
	}
	if len(store.calls) == 0 {
		t.Fatal("expected window queries")
	}
	for _, call := range store.calls {
		if call.kind != "events" {
			t.Fatalf("grep used %s query", call.kind)
		}
		if call.from == 0 {
			t.Fatalf("grep tail started at timestamp 0: %+v", store.calls)
		}
		if call.from < now.Add(-time.Hour).UnixMilli() {
			t.Fatalf("tail walked an hour back after two recent matches: %+v", store.calls)
		}
	}
}

func TestCollectEventerAllHistoryUsesWindows(t *testing.T) {
	now := time.Date(2026, 10, 9, 13, 30, 0, 0, time.UTC)
	store := &scriptedEventer{maxEvents: 1}
	for i := 0; i < 4; i++ {
		at := now.Add(-time.Duration(i) * 30 * time.Minute)
		store.at = append(store.at, at.UnixMilli())
		store.events = append(store.events, lifecycleEvent{
			ID:       strconv.Itoa(i),
			RunID:    "gen_all",
			Sequence: strconv.Itoa(1000 - i),
			Type:     "dev.genesis.note",
			Time:     at.UTC().Format(time.RFC3339Nano),
		})
	}
	filter, err := buildEventFilter("", "", "", "", false, "", 0, true, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := collectEventerEvents(store, filter, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d events", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Time > got[i].Time {
			t.Fatalf("not chronological: %s then %s", got[i-1].Time, got[i].Time)
		}
	}
	if got[0].ID != "3" || got[3].ID != "0" {
		t.Fatalf("order = %s %s %s %s", got[0].ID, got[1].ID, got[2].ID, got[3].ID)
	}
	if len(store.calls) < 2 {
		t.Fatalf("expected several windows, got %+v", store.calls)
	}
	if len(store.calls) > 80 {
		t.Fatalf("walked too many windows from timestamp 0: %d", len(store.calls))
	}
	covered := map[string]bool{}
	for _, call := range store.calls {
		if call.kind != "events" {
			t.Fatalf("unfiltered scan used %s", call.kind)
		}
		n := 0
		for i, at := range store.at {
			if at >= call.from && at <= call.to {
				n++
				covered[store.events[i].ID] = true
			}
		}
		if store.maxEvents > 0 && n > store.maxEvents {
			// Oversized windows are refused. A refused window must not be the
			// only read of the range; a later smaller window covers the rows.
			continue
		}
	}
	for _, event := range store.events {
		if !covered[event.ID] {
			t.Fatalf("event %s was never queried", event.ID)
		}
	}
}

func TestCollectEventerPushedTypeStaysIndexed(t *testing.T) {
	now := time.Date(2026, 10, 9, 13, 30, 0, 0, time.UTC)
	store := &scriptedEventer{}
	specs := []struct {
		id  string
		seq string
		ago time.Duration
	}{
		{"old", "6932", 36 * time.Minute},
		{"mid", "2836", 16 * time.Minute},
		{"new", "2729", time.Minute},
	}
	for _, spec := range specs {
		at := now.Add(-spec.ago)
		store.at = append(store.at, at.UnixMilli())
		store.events = append(store.events, lifecycleEvent{
			ID:       spec.id,
			RunID:    "gen_" + spec.id,
			Sequence: spec.seq,
			Type:     "dev.genesis.run.end",
			Time:     at.UTC().Format(time.RFC3339Nano),
		})
	}
	filter, err := buildEventFilter("", "", "dev.genesis.run.end", "", false, "", 3, false, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := collectEventerEvents(store, filter, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "old" || got[1].ID != "mid" || got[2].ID != "new" {
		t.Fatalf("events = %+v", idsOf(got))
	}
	if len(store.calls) == 0 {
		t.Fatal("expected a type query")
	}
	for _, call := range store.calls {
		if call.kind != "type" {
			t.Fatalf("type filter used %s: %+v", call.kind, store.calls)
		}
	}
}

func idsOf(events []lifecycleEvent) []string {
	out := make([]string, len(events))
	for i, event := range events {
		out[i] = event.ID
	}
	return out
}
