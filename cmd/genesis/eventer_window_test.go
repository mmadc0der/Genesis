package main

import (
	"testing"
	"time"
)

func TestRunQueryWindowFinishedRunStaysInsideLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	events := []lifecycleEvent{
		{Type: lifecycleTypeAccepted, Time: "2026-10-01T10:00:00Z"},
		{Type: lifecycleTypeEnd, Time: "2026-10-01T10:05:00Z"},
	}
	from, to := runQueryWindow(events, now)
	start := time.Date(2026, 10, 1, 9, 59, 58, 0, time.UTC).UnixMilli()
	end := time.Date(2026, 10, 1, 10, 5, 2, 0, time.UTC).UnixMilli()
	if from != start || to != end {
		t.Fatalf("window = %d..%d, want %d..%d", from, to, start, end)
	}
	if to-from > int64((10 * time.Minute).Milliseconds()) {
		t.Fatalf("finished run window is %dms", to-from)
	}
}

func TestRunQueryWindowOpenRunExtendsToNow(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	events := []lifecycleEvent{
		{Type: lifecycleTypeAccepted, Time: "2026-10-09T11:00:00Z"},
	}
	from, to := runQueryWindow(events, now)
	if to != now.Add(time.Minute).UnixMilli() {
		t.Fatalf("open window end = %d, want now+1m", to)
	}
	if from >= to {
		t.Fatalf("empty window %d..%d", from, to)
	}
}

func TestDropEventsPresentInPrunesPending(t *testing.T) {
	full := []lifecycleEvent{
		{ID: "a", Type: lifecycleTypeAssistant},
		{ID: "b", Type: lifecycleTypeTool},
	}
	pending := []lifecycleEvent{
		{ID: "a", Type: lifecycleTypeAssistant},
		{ID: "c", Type: lifecycleTypeAssistant},
	}
	kept := dropEventsPresentIn(full, pending)
	if len(kept) != 1 || kept[0].ID != "c" {
		t.Fatalf("kept = %+v", kept)
	}
}

func TestMergeRunEventsDedupesByID(t *testing.T) {
	merged := mergeRunEvents(
		[]lifecycleEvent{{ID: "a", Time: "2026-10-01T00:00:01Z", Type: lifecycleTypeAccepted}},
		[]lifecycleEvent{{ID: "a", Time: "2026-10-01T00:00:01Z", Type: lifecycleTypeAccepted}, {ID: "b", Time: "2026-10-01T00:00:02Z", Type: lifecycleTypeAssistant}},
	)
	if len(merged) != 2 || merged[0].ID != "a" || merged[1].ID != "b" {
		t.Fatalf("merged = %+v", merged)
	}
}
