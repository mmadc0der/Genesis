package main

import (
	"bytes"
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
	// Until caps the indexed range. Zero means the collector's usual upper bound.
	Until      time.Time
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

func parseUntil(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid until value %q: expected RFC3339 timestamp", raw)
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

// lookupEventerEvents reads history from the listener's open Eventer store.
// handled is false when this process does not own the store, so callers fall
// back to per-run events.jsonl journals.
var lookupEventerEvents = func(*runStore, eventFilter) ([]lifecycleEvent, bool, error) {
	return nil, false, nil
}

// eventerQuery is the read API of an open Eventer store.
type eventerQuery interface {
	QueryRun(runID string, fromMs, toMs int64) ([]json.RawMessage, error)
	QueryType(eventType string, fromMs, toMs int64) ([]json.RawMessage, error)
	QueryAgent(agentID string, fromMs, toMs int64) ([]json.RawMessage, error)
	QueryEvents(fromMs, toMs int64) ([]json.RawMessage, error)
}

func (s *runStore) findEvents(filter eventFilter) ([]lifecycleEvent, error) {
	if s == nil {
		return nil, errors.New("run store is not configured")
	}
	if events, handled, err := lookupEventerEvents(s, filter); handled || err != nil {
		if err != nil {
			return nil, err
		}
		if events == nil {
			events = []lifecycleEvent{}
		}
		return events, nil
	}
	return s.findJournalEvents(filter)
}

func (s *runStore) findJournalEvents(filter eventFilter) ([]lifecycleEvent, error) {
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

// eventerScanWindowMs is the first span asked of an unfiltered Eventer query.
// Run, type, and agent predicates stay on one indexed query. A grep cannot be
// pushed, so history is walked in windows small enough that a response stays
// under Eventer's 64MiB cap instead of one [0, now] query that bisects.
const eventerScanWindowMs = int64((15 * time.Minute) / time.Millisecond)

func collectEventerEvents(q eventerQuery, filter eventFilter, now time.Time) ([]lifecycleEvent, error) {
	if q == nil {
		return nil, errors.New("eventer store is not open")
	}
	upper := now.Add(time.Minute).UnixMilli()
	if !filter.Until.IsZero() {
		upper = filter.Until.UnixMilli()
	}
	lower := int64(0)
	if !filter.Since.IsZero() {
		lower = filter.Since.UnixMilli()
		if lower > upper {
			return []lifecycleEvent{}, nil
		}
	}
	matched := []lifecycleEvent{}
	seen := map[string]struct{}{}
	absorbRaw := func(raw []json.RawMessage) {
		for _, item := range raw {
			event, ok := decodeLifecycle(item)
			if !ok {
				continue
			}
			if event.ID != "" {
				if _, dup := seen[event.ID]; dup {
					continue
				}
				seen[event.ID] = struct{}{}
			}
			if filter.matches(event) {
				matched = append(matched, event)
			}
		}
	}
	enough := func() bool {
		return filter.Tail > 0 && len(matched) >= filter.Tail
	}
	if err := walkEventerHistory(
		q, lower, upper,
		filter.RunID, filter.EventType, filter.AgentID,
		filter.Tail > 0,
		absorbRaw,
		func() int { return len(matched) },
		enough,
	); err != nil {
		return nil, err
	}
	sortEventsByTime(matched)
	if filter.Tail > 0 && len(matched) > filter.Tail {
		matched = matched[len(matched)-filter.Tail:]
	}
	return matched, nil
}

// eventerSizeLimit reports whether err is Eventer's response size cap. pkg/eventer
// exposes this only as a string today; match it here instead of at every caller.
func eventerSizeLimit(err error) bool {
	return err != nil && strings.Contains(err.Error(), "size limit")
}

// walkEventerHistory reads Eventer in bounded windows. Indexed predicates use
// QueryRun / QueryType / QueryAgent; unfiltered reads use QueryEvents.
// Tail walks backward from upper and stops once enough rows matched. A full read
// walks forward from lower. Empty stretches grow the step so the scan does not
// issue a query every 15 minutes back to 1970. A window that hits the size cap
// is retried at half the span.
func walkEventerHistory(
	q eventerQuery,
	lower, upper int64,
	runID, eventType, agentID string,
	backward bool,
	absorbRaw func([]json.RawMessage),
	count func() int,
	enough func() bool,
) error {
	step := eventerScanWindowMs
	if step < 1 {
		step = 1
	}
	shrink := func(span int64) bool {
		if span <= 1 {
			return false
		}
		step = span / 2
		if step < 1 {
			step = 1
		}
		return true
	}
	grow := func(span int64) {
		if span < step || step >= upper-lower+1 {
			return
		}
		next := step * 2
		if next < step {
			return
		}
		step = next
	}
	if !backward {
		cursor := lower
		for cursor <= upper {
			end := cursor + step - 1
			if end > upper {
				end = upper
			}
			raw, err := queryEventerOne(q, cursor, end, runID, eventType, agentID)
			if err != nil && eventerSizeLimit(err) && shrink(end-cursor+1) {
				continue
			}
			if err != nil {
				return err
			}
			before := count()
			absorbRaw(raw)
			if count() == before {
				grow(end - cursor + 1)
			}
			if end >= upper {
				break
			}
			cursor = end + 1
		}
		return nil
	}
	end := upper
	for end >= lower {
		start := end - step + 1
		if start < lower {
			start = lower
		}
		raw, err := queryEventerOne(q, start, end, runID, eventType, agentID)
		if err != nil && eventerSizeLimit(err) && shrink(end-start+1) {
			continue
		}
		if err != nil {
			return err
		}
		before := count()
		absorbRaw(raw)
		if start <= lower || enough() {
			break
		}
		if count() == before {
			grow(end - start + 1)
		}
		end = start - 1
	}
	return nil
}

// runQueryWindow is the [from, to] millisecond span for one run. Lifecycle
// rows mark the start and end. A finished run stays inside that span so a
// size-limit split does not walk the empty timeline. An open run extends to
// now so events that landed after the last lifecycle row are included.
func runQueryWindow(events []lifecycleEvent, now time.Time) (from, to int64) {
	upper := now.Add(time.Minute).UnixMilli()
	var minS, maxS string
	for _, event := range events {
		if event.Time == "" {
			continue
		}
		if minS == "" || event.Time < minS {
			minS = event.Time
		}
		if maxS == "" || event.Time > maxS {
			maxS = event.Time
		}
	}
	if minS == "" {
		return 0, upper
	}
	minT, errMin := time.Parse(time.RFC3339Nano, minS)
	maxT, errMax := time.Parse(time.RFC3339Nano, maxS)
	if errMin != nil || errMax != nil {
		return 0, upper
	}
	from = minT.Add(-2 * time.Second).UnixMilli()
	if from < 0 {
		from = 0
	}
	to = maxT.Add(2 * time.Second).UnixMilli()
	if !journalHasEnd(events) && to < upper {
		to = upper
	}
	if to < from {
		to = from
	}
	return from, to
}

func queryEventerSplit(q eventerQuery, from, to int64, runID, eventType, agentID string) ([]json.RawMessage, error) {
	if q == nil || from > to {
		return nil, nil
	}
	raw, err := queryEventerOne(q, from, to, runID, eventType, agentID)
	if err != nil && eventerSizeLimit(err) && to-from > 1 {
		mid := from + (to-from)/2
		left, leftErr := queryEventerSplit(q, from, mid-1, runID, eventType, agentID)
		if leftErr != nil {
			return nil, leftErr
		}
		right, rightErr := queryEventerSplit(q, mid, to, runID, eventType, agentID)
		if rightErr != nil {
			return nil, rightErr
		}
		return append(left, right...), nil
	}
	return raw, err
}

func queryEventerOne(q eventerQuery, from, to int64, runID, eventType, agentID string) ([]json.RawMessage, error) {
	switch {
	case runID != "":
		return q.QueryRun(runID, from, to)
	case eventType != "":
		return q.QueryType(eventType, from, to)
	case agentID != "":
		return q.QueryAgent(agentID, from, to)
	default:
		return q.QueryEvents(from, to)
	}
}

func decodeLifecycle(raw json.RawMessage) (lifecycleEvent, bool) {
	var event struct {
		lifecycleEvent
		Time flexTime `json:"time"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return lifecycleEvent{}, false
	}
	event.lifecycleEvent.Time = event.Time.Value
	if event.RunID == "" && event.Type == "" {
		return lifecycleEvent{}, false
	}
	return event.lifecycleEvent, true
}

type flexTime struct {
	Value string
}

func (t *flexTime) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '"' {
		return json.Unmarshal(raw, &t.Value)
	}
	var millis int64
	if err := json.Unmarshal(raw, &millis); err != nil {
		return err
	}
	t.Value = time.UnixMilli(millis).UTC().Format(time.RFC3339Nano)
	return nil
}

func sortLifecycle(events []lifecycleEvent) {
	slices.SortFunc(events, func(a, b lifecycleEvent) int {
		sa, aok := eventSequence(a)
		sb, bok := eventSequence(b)
		if aok && bok && sa != sb {
			if sa < sb {
				return -1
			}
			return 1
		}
		if a.Time != b.Time {
			if a.Time < b.Time {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
}

// sortEventsByTime orders a mixed-run events listing. Sequence is per run
// and restarts, so it must not decide order across runs. Equal timestamps
// inside one run still follow sequence.
func sortEventsByTime(events []lifecycleEvent) {
	slices.SortFunc(events, func(a, b lifecycleEvent) int {
		if a.Time != b.Time {
			if a.Time < b.Time {
				return -1
			}
			return 1
		}
		if a.RunID != "" && a.RunID == b.RunID {
			sa, aok := eventSequence(a)
			sb, bok := eventSequence(b)
			if aok && bok && sa != sb {
				if sa < sb {
					return -1
				}
				return 1
			}
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
}

func transcriptPayload(events []lifecycleEvent) ([]byte, error) {
	var buf bytes.Buffer
	for _, event := range events {
		if isEphemeralEvent(event.Type, event.Data) {
			continue
		}
		line, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
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
	until, err := parseUntil(r.URL.Query().Get("until"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter.Until = until

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
		if filter.Tail > 0 || !filter.Since.IsZero() {
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
