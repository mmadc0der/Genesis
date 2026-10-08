package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type eventFilter struct {
	RunID      string
	AgentID    string
	EventType  string
	Grep       string
	IgnoreCase bool
	Regex      *regexp.Regexp
	Since      time.Time
	Tail       int
	ShowChunks bool
}

func parseSince(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(raw, "d") {
		daysStr := strings.TrimSuffix(raw, "d")
		days, err := strconv.Atoi(daysStr)
		if err == nil && days > 0 {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if dur, err := time.ParseDuration(raw); err == nil {
		if dur > 0 {
			return now.Add(-dur), nil
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05", raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since value %q: expected duration (e.g. 10m, 2h, 1d) or RFC3339 timestamp", raw)
}

func buildEventFilter(runID, agentID, eventType, grep string, ignoreCase bool, sinceStr string, tail int, showChunks bool, now time.Time) (eventFilter, error) {
	if runID != "" {
		if err := validateRunID(runID); err != nil {
			return eventFilter{}, fmt.Errorf("invalid run id %q: %w", runID, err)
		}
	}
	sinceTime, err := parseSince(sinceStr, now)
	if err != nil {
		return eventFilter{}, err
	}
	filter := eventFilter{
		RunID:      runID,
		AgentID:    agentID,
		EventType:  eventType,
		Grep:       grep,
		IgnoreCase: ignoreCase,
		Since:      sinceTime,
		Tail:       tail,
		ShowChunks: showChunks,
	}
	if grep != "" {
		pattern := grep
		if ignoreCase {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return eventFilter{}, fmt.Errorf("invalid grep pattern: %w", err)
		}
		filter.Regex = re
	}
	return filter, nil
}

func (f eventFilter) matches(event lifecycleEvent) bool {
	if !f.ShowChunks && isEphemeralEvent(event.Type, event.Data) {
		return false
	}
	if f.RunID != "" && event.RunID != f.RunID && event.Subject != f.RunID {
		return false
	}
	if f.AgentID != "" && event.AgentID != f.AgentID {
		return false
	}
	if f.EventType != "" && event.Type != f.EventType {
		return false
	}
	if !f.Since.IsZero() {
		t, err := time.Parse(time.RFC3339Nano, event.Time)
		if err != nil {
			t, err = time.Parse(time.RFC3339, event.Time)
		}
		if err == nil && t.Before(f.Since) {
			return false
		}
	}
	if f.Regex != nil {
		if f.Regex.MatchString(event.Type) ||
			f.Regex.MatchString(event.RunID) ||
			f.Regex.MatchString(event.AgentID) ||
			f.Regex.MatchString(event.Subject) ||
			f.Regex.MatchString(event.ID) ||
			f.Regex.MatchString(event.CauseType) ||
			f.Regex.MatchString(event.CauseSource) ||
			f.Regex.Match(event.Data) {
			return true
		}
		return false
	} else if f.Grep != "" {
		needle := f.Grep
		if f.IgnoreCase {
			needle = strings.ToLower(needle)
			return strings.Contains(strings.ToLower(event.Type), needle) ||
				strings.Contains(strings.ToLower(event.RunID), needle) ||
				strings.Contains(strings.ToLower(event.AgentID), needle) ||
				strings.Contains(strings.ToLower(event.Subject), needle) ||
				strings.Contains(strings.ToLower(event.ID), needle) ||
				strings.Contains(strings.ToLower(event.CauseType), needle) ||
				strings.Contains(strings.ToLower(event.CauseSource), needle) ||
				strings.Contains(strings.ToLower(string(event.Data)), needle)
		}
		return strings.Contains(event.Type, needle) ||
			strings.Contains(event.RunID, needle) ||
			strings.Contains(event.AgentID, needle) ||
			strings.Contains(event.Subject, needle) ||
			strings.Contains(event.ID, needle) ||
			strings.Contains(event.CauseType, needle) ||
			strings.Contains(event.CauseSource, needle) ||
			strings.Contains(string(event.Data), needle)
	}
	return true
}

func (s *runStore) findEvents(filter eventFilter) ([]lifecycleEvent, error) {
	if s == nil {
		return nil, errors.New("run store is not configured")
	}

	if filter.RunID != "" {
		if err := validateRunID(filter.RunID); err != nil {
			return nil, err
		}
		path := filepath.Join(s.dataDir, runsDirName, filter.RunID, eventsFileName)
		events, _, err := readJournalFrom(path, 0)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		var matched []lifecycleEvent
		for _, ev := range events {
			if filter.matches(ev) {
				matched = append(matched, ev)
			}
		}
		if filter.Tail > 0 && len(matched) > filter.Tail {
			matched = matched[len(matched)-filter.Tail:]
		}
		return matched, nil
	}

	runsDir := filepath.Join(s.dataDir, runsDirName)
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	type runEntry struct {
		name    string
		modTime time.Time
	}
	var runs []runEntry
	for _, entry := range entries {
		if !entry.IsDir() || validateRunID(entry.Name()) != nil {
			continue
		}
		info, statErr := entry.Info()
		mtime := time.Time{}
		if statErr == nil {
			mtime = info.ModTime()
		}
		runs = append(runs, runEntry{name: entry.Name(), modTime: mtime})
	}

	// Sort runs by modification time descending (newest first)
	slices.SortFunc(runs, func(a, b runEntry) int {
		if a.modTime.Equal(b.modTime) {
			return strings.Compare(b.name, a.name)
		}
		if a.modTime.After(b.modTime) {
			return -1
		}
		return 1
	})

	var allMatched []lifecycleEvent
	for _, r := range runs {
		path := filepath.Join(runsDir, r.name, eventsFileName)
		events, _, readErr := readJournalFrom(path, 0)
		if readErr != nil {
			continue
		}
		for _, ev := range events {
			if filter.matches(ev) {
				allMatched = append(allMatched, ev)
			}
		}
		if filter.Tail > 0 && filter.Since.IsZero() && len(allMatched) >= filter.Tail*4 {
			break
		}
	}

	// Sort matching events chronologically
	slices.SortFunc(allMatched, func(a, b lifecycleEvent) int {
		if a.Time != b.Time {
			return strings.Compare(a.Time, b.Time)
		}
		seqA, _ := strconv.ParseUint(a.Sequence, 10, 64)
		seqB, _ := strconv.ParseUint(b.Sequence, 10, 64)
		if seqA < seqB {
			return -1
		} else if seqA > seqB {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})

	if filter.Tail > 0 && len(allMatched) > filter.Tail {
		allMatched = allMatched[len(allMatched)-filter.Tail:]
	}
	return allMatched, nil
}

func (s *eventServer) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	tail := 20
	if val := r.URL.Query().Get("tail"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n >= 0 {
			tail = n
		}
	} else if val := r.URL.Query().Get("n"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n >= 0 {
			tail = n
		}
	} else if val := r.URL.Query().Get("limit"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n >= 0 {
			tail = n
		}
	}

	runID := strings.TrimSpace(r.URL.Query().Get("run"))
	agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
	eventType := strings.TrimSpace(r.URL.Query().Get("type"))
	grep := r.URL.Query().Get("grep")
	if grep == "" {
		grep = r.URL.Query().Get("e")
	}
	if grep == "" {
		grep = r.URL.Query().Get("pattern")
	}
	ignoreCase := r.URL.Query().Get("ignore_case") == "true" || r.URL.Query().Get("i") == "true"
	sinceStr := r.URL.Query().Get("since")
	showChunks := r.URL.Query().Get("chunks") == "true"
	follow := r.URL.Query().Get("follow") == "true" || r.URL.Query().Get("f") == "true"
	format := r.URL.Query().Get("format")

	filter, err := buildEventFilter(runID, agentID, eventType, grep, ignoreCase, sinceStr, tail, showChunks, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if follow {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		eventsChan, unsubscribe := s.store.bus.Subscribe()
		defer unsubscribe()

		seenIDs := make(map[string]struct{})
		if filter.Tail > 0 {
			historical, err := s.store.findEvents(filter)
			if err == nil {
				for _, ev := range historical {
					seenIDs[ev.ID] = struct{}{}
					line, err := json.Marshal(ev)
					if err == nil {
						_, _ = w.Write(append(line, '\n'))
					}
				}
				flusher.Flush()
			}
		}

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-eventsChan:
				if !ok {
					return
				}
				if _, seen := seenIDs[ev.ID]; seen {
					continue
				}
				if !filter.matches(ev) {
					continue
				}
				seenIDs[ev.ID] = struct{}{}
				line, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				_, _ = w.Write(append(line, '\n'))
				flusher.Flush()
			}
		}
	}

	events, err := s.store.findEvents(filter)
	if err != nil {
		s.log().Error("find events", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []lifecycleEvent{}
	}

	if format == "ndjson" || format == "jsonl" || r.Header.Get("Accept") == "application/x-ndjson" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			line, err := json.Marshal(ev)
			if err == nil {
				_, _ = w.Write(append(line, '\n'))
			}
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, events)
}
