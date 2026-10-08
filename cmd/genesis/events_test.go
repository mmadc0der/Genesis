package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
