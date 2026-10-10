package main

// applyEventerLiveToMaps folds one published lifecycle event into the
// listener eventer summary and transcript maps, deduping by event ID.
func applyEventerLiveToMaps(summaries map[string][]lifecycleEvent, transcript map[string][]lifecycleEvent, event lifecycleEvent) {
	if event.RunID == "" {
		return
	}
	switch event.Type {
	case lifecycleTypeAssistant, lifecycleTypeTool, lifecycleTypeRetry:
		if transcript == nil {
			return
		}
		transcript[event.RunID] = appendUniqueLifecycleEvent(transcript[event.RunID], event)
	case lifecycleTypeAccepted, lifecycleTypeStart, lifecycleTypeSessionCreated, lifecycleTypeTurn, lifecycleTypeResult, lifecycleTypeError, lifecycleTypeEnd:
		if summaries == nil {
			return
		}
		summaries[event.RunID] = appendUniqueLifecycleEvent(summaries[event.RunID], event)
	}
}

// finishEventerLoadMaps applies a successful type-window scan, then merges
// live events that arrived before ready in FIFO order.
func finishEventerLoadMaps(grouped map[string][]lifecycleEvent, transcript map[string][]lifecycleEvent, pending []lifecycleEvent) map[string][]lifecycleEvent {
	summaries := grouped
	if summaries == nil {
		summaries = map[string][]lifecycleEvent{}
	}
	if transcript == nil {
		transcript = map[string][]lifecycleEvent{}
	}
	for _, event := range pending {
		applyEventerLiveToMaps(summaries, transcript, event)
	}
	for id, events := range summaries {
		sortLifecycle(events)
		summaries[id] = events
	}
	return summaries
}

func appendUniqueLifecycleEvent(events []lifecycleEvent, event lifecycleEvent) []lifecycleEvent {
	if event.ID != "" {
		for _, existing := range events {
			if existing.ID == event.ID {
				return events
			}
		}
	}
	return append(events, event)
}
