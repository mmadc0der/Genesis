package main

import "testing"

func TestFinishEventerLoadMergesPendingAfterGrouped(t *testing.T) {
	runID := "gen_buffer"
	grouped := map[string][]lifecycleEvent{
		runID: {
			{ID: "scan", RunID: runID, Type: lifecycleTypeAccepted, Time: "2026-10-09T10:00:00Z", Sequence: "1"},
		},
	}
	transcript := map[string][]lifecycleEvent{}
	pending := []lifecycleEvent{
		{ID: "live_a", RunID: runID, Type: lifecycleTypeStart, Time: "2026-10-09T10:00:01Z", Sequence: "2"},
		{ID: "live_b", RunID: runID, Type: lifecycleTypeAssistant, Time: "2026-10-09T10:00:02Z", Sequence: "3"},
	}
	summaries := finishEventerLoadMaps(grouped, transcript, pending)
	if len(summaries[runID]) != 2 || summaries[runID][0].ID != "scan" || summaries[runID][1].ID != "live_a" {
		t.Fatalf("summaries = %+v", summaries[runID])
	}
	if len(transcript[runID]) != 1 || transcript[runID][0].ID != "live_b" {
		t.Fatalf("transcript = %+v", transcript[runID])
	}
}

func TestFinishEventerLoadKeepsPendingOnRetry(t *testing.T) {
	transcript := map[string][]lifecycleEvent{}
	pending := []lifecycleEvent{
		{ID: "only", RunID: "gen_retry", Type: lifecycleTypeAccepted, Time: "2026-10-09T10:00:00Z"},
	}
	summaries := finishEventerLoadMaps(map[string][]lifecycleEvent{}, transcript, pending)
	if len(summaries["gen_retry"]) != 1 || summaries["gen_retry"][0].ID != "only" {
		t.Fatalf("summaries = %+v", summaries["gen_retry"])
	}
}
