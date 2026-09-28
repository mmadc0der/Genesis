package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestEventBusClosesASlowSubscriber(t *testing.T) {
	bus := newEventBus()
	events, unsubscribe := bus.Subscribe()
	defer unsubscribe()
	for i := 0; i < subscriberQueueSize; i++ {
		bus.publish(lifecycleEvent{Sequence: "1"})
	}
	bus.publish(lifecycleEvent{Sequence: "overflow"})
	count := 0
	for range events {
		count++
	}
	if count != subscriberQueueSize {
		t.Fatalf("delivered %d events, want %d", count, subscriberQueueSize)
	}
}

func TestListenerLiveDeliversChunks(t *testing.T) {
	logger := discardLogger()
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	listener := &eventServer{store: store, syncToken: "live-token", logger: logger}
	server := httptest.NewServer(listener)
	t.Cleanup(server.Close)

	denied, err := http.Get(server.URL + "/live")
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized live = %d", denied.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	header := http.Header{}
	header.Set("Authorization", "Bearer live-token")
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/live", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	document := sampleRunInvocation("gen_live")
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	accepted := readLiveEvent(t, ctx, conn)
	if accepted.Type != lifecycleTypeAccepted || accepted.Sequence != "1" {
		t.Fatalf("accepted = %#v", accepted)
	}
	journal := store.journal("gen_live")
	if err := journal.Publish(lifecycleTypeChunk, originSDKChunk, map[string]any{"text": "token"}); err != nil {
		t.Fatal(err)
	}
	chunk := readLiveEvent(t, ctx, conn)
	if chunk.Type != lifecycleTypeChunk || chunk.Sequence != "2" || chunk.RunID != "gen_live" {
		t.Fatalf("chunk = %#v", chunk)
	}
}

func TestReadJournalFromResumesAtOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), eventsFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	appendLifecycle(t, path, sampleLifecycle("gen_tail", "1", lifecycleTypeAccepted, map[string]any{"event_type": "dev.genesis.run"}))
	events, next, err := readJournalFrom(path, 0)
	if err != nil || len(events) != 1 || events[0].Sequence != "1" {
		t.Fatalf("prefix = %v %d %v", err, len(events), events)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if next != info.Size() {
		t.Fatalf("offset = %d, size = %d", next, info.Size())
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(`{"torn":`)); err != nil {
		t.Fatal(err)
	}
	file.Close()
	skipped, stayed, err := readJournalFrom(path, next)
	if err != nil || len(skipped) != 0 || stayed != next {
		t.Fatalf("torn tail = %v %d offset %d", err, len(skipped), stayed)
	}
	appendLifecycle(t, path, sampleLifecycle("gen_tail", "2", lifecycleTypeEnd, map[string]any{"state": endStateCompleted}))
	// The torn prefix is still the first unread byte, so the finished line behind it stays unread.
	blocked, blockedAt, err := readJournalFrom(path, next)
	if err != nil || len(blocked) != 0 || blockedAt != next {
		t.Fatalf("torn prefix blocked the suffix = %v %d %d", err, len(blocked), blockedAt)
	}
	clean := filepath.Join(t.TempDir(), "clean.jsonl")
	if err := os.WriteFile(clean, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	appendLifecycle(t, clean, sampleLifecycle("gen_tail", "1", lifecycleTypeAccepted, map[string]any{"event_type": "dev.genesis.run"}))
	_, cursor, err := readJournalFrom(clean, 0)
	if err != nil {
		t.Fatal(err)
	}
	appendLifecycle(t, clean, sampleLifecycle("gen_tail", "2", lifecycleTypeChunk, map[string]any{"text": "x"}))
	fresh, cursor2, err := readJournalFrom(clean, cursor)
	if err != nil || len(fresh) != 1 || fresh[0].Type != lifecycleTypeChunk || fresh[0].Sequence != "2" {
		t.Fatalf("suffix = %v %#v", err, fresh)
	}
	if cursor2 <= cursor {
		t.Fatalf("cursor did not advance: %d -> %d", cursor, cursor2)
	}
	reset, _, err := readJournalFrom(clean, cursor2+10)
	if err != nil || len(reset) != 2 {
		t.Fatalf("offset past end = %v %d", err, len(reset))
	}
}

func TestSummarizeCachedFoldsNewJournalLines(t *testing.T) {
	control, _ := newPanelFixture(t)
	runID := "gen_cache"
	dir := filepath.Join(control.dataDir, runsDirName, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, eventsFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	appendLifecycle(t, path, sampleLifecycle(runID, "1", lifecycleTypeAccepted, map[string]any{"event_type": "dev.genesis.run"}))
	first, err := control.summarizeCached(runID)
	if err != nil || first.RunID != runID || first.LastSeq != "1" {
		t.Fatalf("first summary = %#v %v", first, err)
	}
	appendLifecycle(t, path, sampleLifecycle(runID, "2", lifecycleTypeEnd, map[string]any{"state": endStateCompleted}))
	second, err := control.summarizeCached(runID)
	if err != nil || second.LastSeq != "2" || second.State != runStateCompleted {
		t.Fatalf("second summary = %#v %v", second, err)
	}
}

func TestControlForwardsListenerEventsWithoutWaitingForPoll(t *testing.T) {
	control, listener := newPanelFixture(t)
	control.pollEvery = time.Hour
	release := make(chan struct{})
	listener.runner.(*fakeRunner).release = release
	t.Cleanup(func() { close(release) })
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	control.ensureListenerFeed()
	select {
	case <-control.feedReady:
	case <-time.After(2 * time.Second):
		t.Fatal("listener subscription was not ready")
	}

	accepted := postJSON(t, panel.URL+"/api/messages", `{"message":"inspect the bench","rule":"example.yaml"}`, http.StatusAccepted)
	var body struct {
		Runs []acceptedRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(accepted), &body); err != nil {
		t.Fatal(err)
	}
	runID := body.Runs[0].RunID
	page := getJSON[eventsPage](t, panel.URL+"/api/runs/"+runID+"/events?limit=10")

	conn := dialLive(t, panel.URL)
	writeLiveOp(t, conn, liveClientOp{Op: "subscribe", Topic: "run", RunID: runID, After: page.Cursor})
	writeLiveOp(t, conn, liveClientOp{Op: "ping"})
	awaitLiveOp(t, conn, "pong")

	journal := listener.store.journal(runID)
	if journal == nil {
		t.Fatal("run journal is closed")
	}
	if err := journal.Publish(lifecycleTypeChunk, originSDKChunk, map[string]any{"text": "token"}); err != nil {
		t.Fatal(err)
	}
	frame := awaitEvent(t, conn, "2")
	if frame.Event == nil || frame.Event.Type != lifecycleTypeChunk {
		t.Fatalf("live frame = %#v", frame)
	}
}

func readLiveEvent(t *testing.T, ctx context.Context, conn *websocket.Conn) lifecycleEvent {
	t.Helper()
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var event lifecycleEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func awaitLiveOp(t *testing.T, conn *websocket.Conn, op string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		_, payload, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var frame liveFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Op == op {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", op)
}
