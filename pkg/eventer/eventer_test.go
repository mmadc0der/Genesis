package eventer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEventerStoreOpenAppendQuery(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "eventer-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	schemaPath, err := filepath.Abs("../../schema/cloudevents-eventer.json")
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(tempDir, schemaPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	nowMs := now.UnixMilli()

	event1 := map[string]any{
		"time":        now.Format(time.RFC3339Nano),
		"specversion": "1.0",
		"id":          "evt_1",
		"type":        "dev.genesis.run.start",
		"source":      "urn:genesis:test",
		"runid":       "gen_12345",
		"agentid":     "cpu-bench",
		"data": map[string]any{
			"command": "echo test",
			"count":   42,
		},
	}

	raw1, err := json.Marshal(event1)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Append(raw1); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Query by time range
	res, err := store.Query(nowMs-1000, nowMs+1000)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	var results []map[string]any
	if err := json.Unmarshal(res, &results); err != nil {
		t.Fatalf("Unmarshal query result failed: %v, body=%s", err, string(res))
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %s", len(results), string(res))
	}

	if results[0]["id"] != "evt_1" {
		t.Fatalf("expected id evt_1, got %v", results[0]["id"])
	}

	dataObj, ok := results[0]["data"].(map[string]any)
	if !ok || dataObj["command"] != "echo test" {
		t.Fatalf("expected preserved json data with command='echo test', got: %#v", results[0]["data"])
	}
}

func TestEventerStoreMultiEvents(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "eventer-multi-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	schemaPath, err := filepath.Abs("../../schema/cloudevents-eventer.json")
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(tempDir, schemaPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	start := time.Now().UTC()
	startMs := start.UnixMilli()

	const count = 5000
	for i := 0; i < count; i++ {
		evTime := start.Add(time.Duration(i) * time.Millisecond)
		ev := map[string]any{
			"time":        evTime.Format(time.RFC3339Nano),
			"specversion": "1.0",
			"id":          "evt_multi",
			"sequence":    "1",
			"type":        "dev.genesis.run.chunk",
			"source":      "urn:genesis:test",
			"runid":       "gen_multi",
			"agentid":     "cpu-bench",
			"data": map[string]any{
				"idx": i,
				"msg": "streaming token test",
			},
		}
		raw, _ := json.Marshal(ev)
		if err := store.Append(raw); err != nil {
			t.Fatalf("Append failed at %d: %v", i, err)
		}
	}

	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	endMs := start.Add(time.Duration(count) * time.Millisecond).UnixMilli()
	res, err := store.Query(startMs, endMs)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	var results []map[string]any
	if err := json.Unmarshal(res, &results); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if len(results) != count {
		t.Fatalf("expected %d events, got %d", count, len(results))
	}
}

func TestEventerFollower(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "eventer-follower-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	schemaPath, err := filepath.Abs("../../schema/cloudevents-eventer.json")
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(tempDir, schemaPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	base := time.Now().UTC()
	startMs := base.UnixMilli()
	follower := store.NewFollower(startMs)

	// Append batch 1
	for i := 0; i < 5; i++ {
		ev := map[string]any{
			"time":        base.Add(time.Duration(i*10) * time.Millisecond).Format(time.RFC3339Nano),
			"specversion": "1.0",
			"id":          "evt_batch1",
			"type":        "dev.genesis.run.start",
			"runid":       "gen_follow",
		}
		if err := store.AppendEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	batch1End := startMs + 45
	events1, err := follower.Next(batch1End)
	if err != nil {
		t.Fatalf("follower Next failed: %v", err)
	}
	if len(events1) != 5 {
		t.Fatalf("expected 5 events in batch 1, got %d", len(events1))
	}

	// Follower cursor has advanced. Query again without new events.
	empty, err := follower.Next(batch1End)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected 0 events, got %d", len(empty))
	}

	// Append batch 2 with newer timestamps
	for i := 5; i < 8; i++ {
		ev := map[string]any{
			"time":        base.Add(time.Duration(i*10) * time.Millisecond).Format(time.RFC3339Nano),
			"specversion": "1.0",
			"id":          "evt_batch2",
			"type":        "dev.genesis.run.tool",
			"runid":       "gen_follow",
		}
		if err := store.AppendEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	batch2End := startMs + 80
	events2, err := follower.Next(batch2End)
	if err != nil {
		t.Fatal(err)
	}
	if len(events2) != 3 {
		t.Fatalf("expected 3 events in batch 2, got %d", len(events2))
	}
}

func TestEventerQueryFiltered(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "eventer-filter-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	schemaPath, err := filepath.Abs("../../schema/cloudevents-eventer.json")
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(tempDir, schemaPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	base := time.Now().UTC()
	startMs := base.UnixMilli()

	// Append mix of events: some 'dev.genesis.run.assistant', some 'dev.genesis.run.tool'
	types := []string{
		"dev.genesis.run.assistant",
		"dev.genesis.run.tool",
		"dev.genesis.run.assistant",
		"dev.genesis.run.tool",
		"dev.genesis.run.tool",
	}

	for i, typ := range types {
		ev := map[string]any{
			"time":        base.Add(time.Duration(i*10) * time.Millisecond).Format(time.RFC3339Nano),
			"specversion": "1.0",
			"id":          "evt_filt",
			"type":        typ,
			"runid":       "gen_filter",
			"agentid":     "oracle",
		}
		if err := store.AppendEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	endMs := startMs + 100

	// 1. Query filtered by type == dev.genesis.run.assistant
	assistantEvents, err := store.QueryEventsFiltered(startMs, endMs, "type", "dev.genesis.run.assistant")
	if err != nil {
		t.Fatalf("QueryEventsFiltered failed: %v", err)
	}
	if len(assistantEvents) != 2 {
		t.Fatalf("expected 2 assistant events, got %d", len(assistantEvents))
	}
	for _, raw := range assistantEvents {
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		if parsed["type"] != "dev.genesis.run.assistant" {
			t.Fatalf("unexpected type in filtered results: %v", parsed["type"])
		}
	}

	// 2. Query filtered by type == dev.genesis.run.tool
	toolEvents, err := store.QueryEventsFiltered(startMs, endMs, "type", "dev.genesis.run.tool")
	if err != nil {
		t.Fatalf("QueryEventsFiltered for tool failed: %v", err)
	}
	if len(toolEvents) != 3 {
		t.Fatalf("expected 3 tool events, got %d", len(toolEvents))
	}

	// 3. Query filtered by non-existent type
	none, err := store.QueryEventsFiltered(startMs, endMs, "type", "dev.genesis.run.nonexistent")
	if err != nil {
		t.Fatalf("QueryEventsFiltered none failed: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected 0 events, got %d", len(none))
	}

	// 4. Test NewFilteredFollower
	filteredFollower := store.NewFilteredFollower(startMs, "type", "dev.genesis.run.assistant")
	followed, err := filteredFollower.Next(endMs)
	if err != nil {
		t.Fatalf("FilteredFollower Next failed: %v", err)
	}
	if len(followed) != 2 {
		t.Fatalf("expected 2 followed assistant events, got %d", len(followed))
	}

	// 5. Test QueryRun and QueryAgent typed helpers
	runEvents, err := store.QueryRun("gen_filter", startMs, endMs)
	if err != nil || len(runEvents) != 5 {
		t.Fatalf("QueryRun failed: %v, count=%d", err, len(runEvents))
	}

	agentEvents, err := store.QueryAgent("oracle", startMs, endMs)
	if err != nil || len(agentEvents) != 5 {
		t.Fatalf("QueryAgent failed: %v, count=%d", err, len(agentEvents))
	}

	otherRun, err := store.QueryRun("gen_other", startMs, endMs)
	if err != nil || len(otherRun) != 0 {
		t.Fatalf("QueryRun other failed: %v, count=%d", err, len(otherRun))
	}

	// 6. Test NewRunFollower
	runFollower := store.NewRunFollower(startMs, "gen_filter")
	followedRun, err := runFollower.Next(endMs)
	if err != nil || len(followedRun) != 5 {
		t.Fatalf("NewRunFollower Next failed: %v, count=%d", err, len(followedRun))
	}
}

func TestEventerOpenDefault(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "eventer-default-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	store, err := OpenDefault(tempDir)
	if err != nil {
		t.Fatalf("OpenDefault failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	ev := map[string]any{
		"time":        now.Format(time.RFC3339Nano),
		"specversion": "1.0",
		"id":          "evt_default",
		"type":        "dev.genesis.run.start",
		"source":      "urn:genesis:default",
		"runid":       "gen_default",
		"agentid":     "cpu-bench",
		"data": map[string]any{
			"status": "ok",
		},
	}
	if err := store.AppendEvent(ev); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	events, err := store.QueryRun("gen_default", now.UnixMilli()-1000, now.UnixMilli()+1000)
	if err != nil {
		t.Fatalf("QueryRun failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}
