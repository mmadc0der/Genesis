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
