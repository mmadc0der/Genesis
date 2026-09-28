package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestUsageAccountSumsJournaledFrames(t *testing.T) {
	store, control := newUsageStore(t)
	journal, doc := acceptRun(t, store, "gen_usage_sum")
	publishUsage(t, journal, "a1", map[string]any{
		"cacheReadTokens": 5,
		"inputTokens":     3,
		"outputTokens":    8,
		"reasoningTokens": 2,
		"totalTokens":     16,
	})
	publishUsage(t, journal, "a2", map[string]any{
		"cacheReadTokens": 1,
		"inputTokens":     4,
		"outputTokens":    10,
		"reasoningTokens": 10,
		"totalTokens":     999,
	})
	publishUsage(t, journal, "a3", map[string]any{})
	publishUsage(t, journal, "a1", map[string]any{
		"cacheReadTokens": 1000,
		"inputTokens":     1000,
		"outputTokens":    1000,
		"reasoningTokens": 1000,
		"totalTokens":     3000,
	})
	publishRawChunk(t, journal, `{"type":"chunk","attemptId":"text-1","sessionId":"session-1","chunk":{"type":"text-delta","text":"cacheReadTokens 999 inputTokens 999","usage":{"cacheReadTokens":999,"inputTokens":999,"outputTokens":999,"totalTokens":2997}}}`)
	if err := journal.Publish(lifecycleTypeAssistant, originSDKEvent, map[string]any{
		"phase": "message",
		"text":  "the model spent inputTokens 4000 cacheReadTokens 4000",
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Publish(lifecycleTypeResult, originPython, map[string]any{
		"finish_reason":  "completed",
		"final_response": "totalTokens 4000",
	}); err != nil {
		t.Fatal(err)
	}

	// a2's totalTokens (999) is ignored. reasoning is kept beside output and
	// is not added again. a3 contributes an attempt and zeros. The second a1
	// and the text/assistant/result frames contribute nothing.
	want := tokenAccount{CacheHit: 6, CacheMiss: 7, Output: 18, Reasoning: 12, Total: 31, Attempts: 3}
	assertUsage(t, mustUsage(t, doc.RunDir), want)
	events := readRunEvents(t, store, doc.RunID)
	if hasType(events, lifecycleTypeError) {
		t.Fatal("a disagreed usage total failed the run")
	}
	scanned, _ := foldUsage(tokenAccount{}, nil, events)
	assertUsage(t, scanned, want)
	if _, err := os.Lstat(filepath.Join(doc.RunDir, usageFileName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("usage temp file = %v", err)
	}
	record := readUsageRecord(t, doc.RunDir)
	if !slices.Equal(record.AttemptIDs, []string{"a1", "a2", "a3"}) {
		t.Fatalf("attempt ids = %#v", record.AttemptIDs)
	}
	if bytes.Contains(mustReadUsage(t, doc.RunDir), []byte("999")) || bytes.Contains(mustReadUsage(t, doc.RunDir), []byte("text-1")) {
		t.Fatal("usage file kept a non-usage or disagreed total")
	}

	detail, err := control.readRunDetail(doc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != runStateOpen || detail.EndedAt != "" {
		t.Fatalf("open detail = %+v", detail)
	}
	assertUsage(t, detail.Usage, want)
	if encoded, err := json.Marshal(detail.Usage); err != nil || string(encoded) != `{"cache_hit":6,"cache_miss":7,"output":18,"reasoning":12,"total":31,"attempts":3}` {
		t.Fatalf("usage JSON = %s %v", encoded, err)
	}
	runs, err := control.listRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	assertUsage(t, findRun(t, runs, doc.RunID).Usage, want)

	if err := journal.Publish(lifecycleTypeError, originGenesis, errorPayload("DeepSeekRunError", "stopped")); err != nil {
		t.Fatal(err)
	}
	if err := journal.Publish(lifecycleTypeEnd, originGenesis, endPayload(1, endStateFailed)); err != nil {
		t.Fatal(err)
	}
	assertUsage(t, mustUsage(t, doc.RunDir), want)
	failed, err := control.readRunDetail(doc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != runStateFailed {
		t.Fatalf("state = %s", failed.State)
	}
	assertUsage(t, failed.Usage, want)
	runs, err = control.listRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	listed := findRun(t, runs, doc.RunID)
	if listed.State != runStateFailed {
		t.Fatalf("listed state = %s", listed.State)
	}
	assertUsage(t, listed.Usage, want)
}

func TestUsageDuplicateAttemptIsCountedOnce(t *testing.T) {
	store, control := newUsageStore(t)
	journal, doc := acceptRun(t, store, "gen_usage_dup")
	publishUsage(t, journal, "a1", map[string]any{
		"cacheReadTokens": 5,
		"outputTokens":    1,
		"reasoningTokens": 1,
	})
	publishUsage(t, journal, "a1", map[string]any{
		"cacheReadTokens": 100,
		"outputTokens":    100,
		"reasoningTokens": 100,
	})
	store.release(doc.RunID)

	file, err := os.OpenFile(filepath.Join(doc.RunDir, eventsFileName), os.O_APPEND|os.O_WRONLY, dataFileMode)
	if err != nil {
		t.Fatal(err)
	}
	reopened := &runJournal{
		runID:    doc.RunID,
		runDir:   doc.RunDir,
		file:     file,
		bus:      newEventBus(),
		redactor: newRedactor(nil),
		now:      time.Now,
		newID:    newLifecycleEventID,
	}
	t.Cleanup(func() { _ = reopened.Close() })
	publishUsage(t, reopened, "a1", map[string]any{"cacheReadTokens": 50, "outputTokens": 50})
	publishUsage(t, reopened, "a2", map[string]any{
		"inputTokens":     2,
		"outputTokens":    3,
		"reasoningTokens": 3,
	})

	want := tokenAccount{CacheHit: 5, CacheMiss: 2, Output: 4, Reasoning: 4, Total: 11, Attempts: 2}
	assertUsage(t, mustUsage(t, doc.RunDir), want)
	seen := 0
	for _, event := range readRunEvents(t, store, doc.RunID) {
		frame, ok := parseUsageFrame(event.Data)
		if ok && frame.attemptID == "a1" {
			seen++
		}
	}
	if seen != 3 {
		t.Fatalf("journaled a1 frames = %d", seen)
	}
	detail, err := control.readRunDetail(doc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	assertUsage(t, detail.Usage, want)
	record := readUsageRecord(t, doc.RunDir)
	if !slices.Equal(record.AttemptIDs, []string{"a1", "a2"}) {
		t.Fatalf("attempt ids = %#v", record.AttemptIDs)
	}
}

func TestUsageFoldIgnoresNonIntegersAndBlankAttempts(t *testing.T) {
	events := []lifecycleEvent{
		usageLifecycle("blank-1", "", `{"cacheReadTokens":"4","inputTokens":null,"outputTokens":1.5,"reasoningTokens":1,"totalTokens":9}`),
		usageLifecycle("blank-2", "", `{"cacheReadTokens":2,"outputTokens":3,"reasoningTokens":1}`),
		usageLifecycle("a1", "a1", `{"cacheReadTokens":1,"totalTokens":100}`),
		usageLifecycle("a1-again", "a1", `{"cacheReadTokens":50,"outputTokens":50,"reasoningTokens":50}`),
		{Type: lifecycleTypeAssistant, Data: json.RawMessage(`{"text":"cacheReadTokens 80","chunk_type":"usage","raw":{"payload":{"chunk":{"usage":{"inputTokens":80}}}}}`)},
		{Type: lifecycleTypeChunk, Data: json.RawMessage(`{"chunk_type":"text-delta","attempt_id":"text-1","raw":{"payload":{"chunk":{"usage":{"inputTokens":70,"totalTokens":70}}}}}`)},
	}
	got, seen := foldUsage(tokenAccount{}, nil, events)
	assertUsage(t, got, tokenAccount{CacheHit: 3, CacheMiss: 0, Output: 3, Reasoning: 2, Total: 6, Attempts: 3})
	if _, ok := seen["a1"]; !ok || len(seen) != 1 {
		t.Fatalf("seen = %#v", seen)
	}
}

func TestRunWithoutUsageStaysZero(t *testing.T) {
	store, control := newUsageStore(t)
	journal, doc := acceptRun(t, store, "gen_usage_zero")
	detail, err := control.readRunDetail(doc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != runStateOpen {
		t.Fatalf("state = %s", detail.State)
	}
	assertUsage(t, detail.Usage, tokenAccount{})
	if _, err := os.Lstat(filepath.Join(doc.RunDir, usageFileName)); !os.IsNotExist(err) {
		t.Fatalf("usage file = %v", err)
	}

	if err := journal.Publish(lifecycleTypeError, originGenesis, errorPayload(processErrorType, "died before the model")); err != nil {
		t.Fatal(err)
	}
	if err := journal.Publish(lifecycleTypeEnd, originGenesis, endPayload(-1, endStateFailed)); err != nil {
		t.Fatal(err)
	}
	failed, err := control.readRunDetail(doc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != runStateFailed {
		t.Fatalf("state = %s", failed.State)
	}
	assertUsage(t, failed.Usage, tokenAccount{})
	runs, err := control.listRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	assertUsage(t, findRun(t, runs, doc.RunID).Usage, tokenAccount{})
	encoded, err := json.Marshal(failed.Usage)
	if err != nil || string(encoded) != `{"cache_hit":0,"cache_miss":0,"output":0,"reasoning":0,"total":0,"attempts":0}` {
		t.Fatalf("usage JSON = %s %v", encoded, err)
	}
}

func TestRecoverUsageFileKeepsOrRebuilds(t *testing.T) {
	t.Run("finished file is kept", func(t *testing.T) {
		store, _ := newUsageStore(t)
		journal, doc := acceptRun(t, store, "gen_usage_keep")
		publishUsage(t, journal, "a1", map[string]any{"cacheReadTokens": 80, "outputTokens": 5})
		if err := journal.Publish(lifecycleTypeEnd, originGenesis, endPayload(0, endStateCompleted)); err != nil {
			t.Fatal(err)
		}
		store.release(doc.RunID)
		kept := tokenAccount{CacheHit: 4, CacheMiss: 1, Output: 2, Reasoning: 2, Total: 7, Attempts: 1}
		if err := writeUsageRecord(doc.RunDir, kept, map[string]struct{}{"kept": {}}); err != nil {
			t.Fatal(err)
		}
		beforeEvents := len(readRunEvents(t, store, doc.RunID))
		before := mustReadUsage(t, doc.RunDir)
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, mustReadUsage(t, doc.RunDir)) {
			t.Fatalf("usage file changed:\n%s", mustReadUsage(t, doc.RunDir))
		}
		if len(readRunEvents(t, store, doc.RunID)) != beforeEvents {
			t.Fatal("finished journal was rewritten")
		}
		assertUsage(t, mustUsage(t, doc.RunDir), kept)
	})

	t.Run("missing file is rebuilt once", func(t *testing.T) {
		store, _ := newUsageStore(t)
		journal, doc := acceptRun(t, store, "gen_usage_rebuild")
		publishUsage(t, journal, "a1", map[string]any{
			"cacheReadTokens": 8,
			"inputTokens":     1,
			"outputTokens":    2,
			"reasoningTokens": 2,
			"totalTokens":     50,
		})
		publishUsage(t, journal, "a1", map[string]any{"cacheReadTokens": 40})
		if err := journal.Publish(lifecycleTypeEnd, originGenesis, endPayload(0, endStateCompleted)); err != nil {
			t.Fatal(err)
		}
		store.release(doc.RunID)
		if err := os.Remove(filepath.Join(doc.RunDir, usageFileName)); err != nil {
			t.Fatal(err)
		}
		beforeEvents := len(readRunEvents(t, store, doc.RunID))
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
		want := tokenAccount{CacheHit: 8, CacheMiss: 1, Output: 2, Reasoning: 2, Total: 11, Attempts: 1}
		assertUsage(t, mustUsage(t, doc.RunDir), want)
		if len(readRunEvents(t, store, doc.RunID)) != beforeEvents {
			t.Fatal("rebuild appended lifecycle events")
		}
		first := mustReadUsage(t, doc.RunDir)
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, mustReadUsage(t, doc.RunDir)) {
			t.Fatal("second recovery applied usage again")
		}
	})

	t.Run("no usage stays zero", func(t *testing.T) {
		store, _ := newUsageStore(t)
		journal, doc := acceptRun(t, store, "gen_usage_none")
		if err := journal.Publish(lifecycleTypeEnd, originGenesis, endPayload(0, endStateCompleted)); err != nil {
			t.Fatal(err)
		}
		store.release(doc.RunID)
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
		assertUsage(t, mustUsage(t, doc.RunDir), tokenAccount{})
	})

	t.Run("interrupted run keeps recorded usage", func(t *testing.T) {
		store, _ := newUsageStore(t)
		journal, doc := acceptRun(t, store, "gen_usage_stop")
		publishUsage(t, journal, "a1", map[string]any{"cacheReadTokens": 6, "outputTokens": 1, "reasoningTokens": 1})
		store.release(doc.RunID)
		before := mustReadUsage(t, doc.RunDir)
		if err := store.Recover(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, mustReadUsage(t, doc.RunDir)) {
			t.Fatalf("interrupted usage changed:\n%s", mustReadUsage(t, doc.RunDir))
		}
		events := readRunEvents(t, store, doc.RunID)
		if !hasType(events, lifecycleTypeEnd) {
			t.Fatal("interrupted run was not closed")
		}
		assertUsage(t, mustUsage(t, doc.RunDir), tokenAccount{CacheHit: 6, CacheMiss: 0, Output: 1, Reasoning: 1, Total: 7, Attempts: 1})
	})
}

func TestControlSummaryReadsUsageWithoutDoubleCounting(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control := &controlServer{dataDir: dataDir}
	fileRun := filepath.Join(dataDir, runsDirName, "gen_usage_file", eventsFileName)
	writeJournal(t, fileRun,
		sampleLifecycle("gen_usage_file", "1", lifecycleTypeAccepted, map[string]any{"event_id": "cause"}),
		sampleLifecycle("gen_usage_file", "2", lifecycleTypeChunk, journaledUsage(t, "event-1", 100)),
	)
	kept := tokenAccount{CacheHit: 4, CacheMiss: 1, Output: 2, Reasoning: 2, Total: 7, Attempts: 1}
	if err := writeUsageRecord(filepath.Dir(fileRun), kept, map[string]struct{}{"kept": {}}); err != nil {
		t.Fatal(err)
	}
	eventRun := filepath.Join(dataDir, runsDirName, "gen_usage_events", eventsFileName)
	writeJournal(t, eventRun,
		sampleLifecycle("gen_usage_events", "1", lifecycleTypeAccepted, map[string]any{"event_id": "cause"}),
		sampleLifecycle("gen_usage_events", "2", lifecycleTypeChunk, journaledUsage(t, "a1", 10)),
		sampleLifecycle("gen_usage_events", "3", lifecycleTypeChunk, journaledUsage(t, "a2", 3)),
	)

	runs, err := control.listRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	assertUsage(t, findRun(t, runs, "gen_usage_file").Usage, kept)
	assertUsage(t, findRun(t, runs, "gen_usage_events").Usage, tokenAccount{CacheHit: 13, Total: 13, Attempts: 2})
	if _, err := os.Lstat(filepath.Join(filepath.Dir(eventRun), usageFileName)); !os.IsNotExist(err) {
		t.Fatal("control wrote a usage file")
	}

	revised := tokenAccount{CacheHit: 9, CacheMiss: 1, Output: 2, Reasoning: 2, Total: 12, Attempts: 1}
	if err := writeUsageRecord(filepath.Dir(fileRun), revised, map[string]struct{}{"kept": {}}); err != nil {
		t.Fatal(err)
	}
	appendLifecycle(t, eventRun, sampleLifecycle("gen_usage_events", "4", lifecycleTypeChunk, journaledUsage(t, "a1", 100)))
	appendLifecycle(t, eventRun, sampleLifecycle("gen_usage_events", "5", lifecycleTypeChunk, journaledUsage(t, "a3", 1)))

	runs, err = control.listRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	assertUsage(t, findRun(t, runs, "gen_usage_file").Usage, revised)
	summed := tokenAccount{CacheHit: 14, Total: 14, Attempts: 3}
	assertUsage(t, findRun(t, runs, "gen_usage_events").Usage, summed)
	detail, err := control.readRunDetail("gen_usage_events")
	if err != nil {
		t.Fatal(err)
	}
	assertUsage(t, detail.Usage, summed)
	fileDetail, err := control.readRunDetail("gen_usage_file")
	if err != nil {
		t.Fatal(err)
	}
	assertUsage(t, fileDetail.Usage, revised)
}

func newUsageStore(t *testing.T) (*runStore, *controlServer) {
	t.Helper()
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newRunStore(dataDir, newEventBus(), discardLogger()), &controlServer{dataDir: dataDir}
}

func acceptRun(t *testing.T, store *runStore, runID string) (*runJournal, invocation) {
	t.Helper()
	document := sampleRunInvocation(runID)
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	return store.journal(document.RunID), document
}

func publishUsage(t *testing.T, journal *runJournal, attempt string, usage map[string]any) {
	t.Helper()
	chunk := map[string]any{"type": "usage"}
	if usage != nil {
		chunk["usage"] = usage
	}
	payload := map[string]any{
		"type":      "chunk",
		"sessionId": "session-1",
		"chunk":     chunk,
	}
	if attempt != "" {
		payload["attemptId"] = attempt
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	_, _, data, ok := mapOnChunk("session-1", raw)
	if !ok {
		t.Fatalf("usage frame not mapped: %s", raw)
	}
	if err := journal.Publish(lifecycleTypeChunk, originSDKChunk, data); err != nil {
		t.Fatal(err)
	}
}

func publishRawChunk(t *testing.T, journal *runJournal, payload string) {
	t.Helper()
	_, _, data, ok := mapOnChunk("session-1", json.RawMessage(payload))
	if !ok {
		t.Fatalf("chunk not mapped: %s", payload)
	}
	if err := journal.Publish(lifecycleTypeChunk, originSDKChunk, data); err != nil {
		t.Fatal(err)
	}
}

func journaledUsage(t *testing.T, attempt string, hit int) map[string]any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type":      "chunk",
		"attemptId": attempt,
		"sessionId": "session-1",
		"chunk": map[string]any{
			"type":  "usage",
			"usage": map[string]any{"cacheReadTokens": hit},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, data, ok := mapOnChunk("session-1", raw)
	if !ok {
		t.Fatal("usage frame not mapped")
	}
	return data
}

func usageLifecycle(id, attempt, usage string) lifecycleEvent {
	if usage == "" {
		usage = "{}"
	}
	attemptJSON, _ := json.Marshal(attempt)
	data := []byte(`{"chunk_type":"usage","attempt_id":` + string(attemptJSON) + `,"raw":{"payload":{"chunk":{"usage":` + usage + `}}}}`)
	return lifecycleEvent{ID: id, Type: lifecycleTypeChunk, Data: data}
}

func writeJournal(t *testing.T, path string, events ...lifecycleEvent) {
	t.Helper()
	var buf bytes.Buffer
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(payload, '\n'))
	}
	if err := os.MkdirAll(filepath.Dir(path), dataDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), dataFileMode); err != nil {
		t.Fatal(err)
	}
}

func mustUsage(t *testing.T, runDir string) tokenAccount {
	t.Helper()
	account, _, ok := readUsageAccount(filepath.Join(runDir, usageFileName))
	if !ok {
		t.Fatalf("usage file missing in %s", runDir)
	}
	return account
}

func mustReadUsage(t *testing.T, runDir string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(runDir, usageFileName))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func readUsageRecord(t *testing.T, runDir string) usageRecord {
	t.Helper()
	var record usageRecord
	if err := json.Unmarshal(mustReadUsage(t, runDir), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func assertUsage(t *testing.T, got, want tokenAccount) {
	t.Helper()
	if got != want {
		t.Fatalf("usage = %+v want %+v", got, want)
	}
	if got.Total != got.CacheHit+got.CacheMiss+got.Output {
		t.Fatalf("total %d is not cache_hit + cache_miss + output", got.Total)
	}
}

func findRun(t *testing.T, runs []runSummary, runID string) runSummary {
	t.Helper()
	for _, run := range runs {
		if run.RunID == runID {
			return run
		}
	}
	t.Fatalf("missing run %s", runID)
	return runSummary{}
}
