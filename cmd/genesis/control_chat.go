package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// The chat view of a session. A session is the DSH conversation an agent
// keeps across runs: the first run starts it, and every continuation is
// another run on the same session id. Control condenses the run journals into
// what a reader wants to see (who said what, which tools ran) and leaves the
// token-by-token chunks out. It reads journals only, so it works for any run
// the panel can list.

const (
	chatTextLimit   = 48 * 1024
	chatToolLimit   = 8 * 1024
	chatCacheLimit  = 64
	chatMessageMax  = acceptedMessageLimit
	chatBodyMax     = chatMessageMax + 4*1024
	chatRoleUser    = "user"
	chatRoleAgent   = "assistant"
	chatRoleTool    = "tool"
	chatRoleSystem  = "system"
	chatKindText    = "text"
	chatKindThought = "reasoning"
	chatKindCall    = "call"
	chatKindResult  = "result"
	chatKindError   = "error"
)

type chatItem struct {
	Seq       string `json:"seq"`
	At        string `json:"at"`
	Role      string `json:"role"`
	Kind      string `json:"kind"`
	Name      string `json:"name,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Text      string `json:"text,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// chatTrigger is what started a run: the event type and source, and the
// message in it when it had one. Runs from before the accepted event carried
// the message have no text.
type chatTrigger struct {
	Type    string `json:"type,omitempty"`
	Source  string `json:"source,omitempty"`
	Message string `json:"message,omitempty"`
}

type chatRun struct {
	Run     runSummary  `json:"run"`
	Trigger chatTrigger `json:"trigger"`
	Items   []chatItem  `json:"items"`
	// Retries counts provider retries, which are folded away.
	Retries int `json:"retries,omitempty"`
}

type activeTurnData struct {
	Thought  string `json:"thought,omitempty"`
	Text     string `json:"text,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	ToolArgs string `json:"tool_args,omitempty"`
}

type chatSession struct {
	RunID           string          `json:"run_id"`
	SessionID       string          `json:"session_id,omitempty"`
	Agent           string          `json:"agent"`
	Runs            []chatRun       `json:"runs"`
	// ContinueRun is the run a follow-up cites. It is empty while a run is
	// open or when nothing can be continued, and ContinueBlocked then says why.
	ContinueRun     string          `json:"continue_run,omitempty"`
	ContinueBlocked string          `json:"continue_blocked,omitempty"`
	ActiveTurn      *activeTurnData `json:"active_turn,omitempty"`
}

// chatState is the condensed journal of one run, read forward only. It keeps
// the byte offset it has consumed so a poll reads just the new lines.
type chatState struct {
	mu      sync.Mutex
	offset  int64
	trigger chatTrigger
	items   []chatItem
	retries int
}

type activeStream struct {
	mu         sync.Mutex
	runID      string
	thought    strings.Builder
	text       strings.Builder
	toolName   string
	toolCallID string
	toolArgs   strings.Builder
	lastAt     string
}

var activeStreams = struct {
	sync.Mutex
	byRun map[string]*activeStream
}{byRun: map[string]*activeStream{}}

func getOrCreateStream(runID string) *activeStream {
	activeStreams.Lock()
	defer activeStreams.Unlock()
	s, ok := activeStreams.byRun[runID]
	if !ok {
		s = &activeStream{runID: runID}
		activeStreams.byRun[runID] = s
	}
	return s
}

func discardStream(runID string) {
	activeStreams.Lock()
	defer activeStreams.Unlock()
	delete(activeStreams.byRun, runID)
}

func (s *activeStream) snapshot() *activeTurnData {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	thought := s.thought.String()
	text := s.text.String()
	toolArgs := s.toolArgs.String()
	if thought == "" && text == "" && toolArgs == "" && s.toolName == "" {
		return nil
	}
	return &activeTurnData{
		Thought:  thought,
		Text:     text,
		ToolName: s.toolName,
		ToolArgs: toolArgs,
	}
}

func absorbChunkToStream(event lifecycleEvent) {
	if event.RunID == "" {
		return
	}
	var body struct {
		ChunkType string `json:"chunk_type"`
		Frame     string `json:"frame"`
		Raw       struct {
			Payload struct {
				Chunk struct {
					Type           string `json:"type"`
					Text           string `json:"text"`
					Name           string `json:"name"`
					ID             string `json:"id"`
					ArgumentsDelta string `json:"argumentsDelta"`
				} `json:"chunk"`
			} `json:"payload"`
		} `json:"raw"`
	}
	if err := json.Unmarshal(event.Data, &body); err != nil {
		return
	}
	chunk := body.Raw.Payload.Chunk
	chunkType := body.ChunkType
	if chunkType == "" {
		chunkType = chunk.Type
	}
	stream := getOrCreateStream(event.RunID)
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if event.Time != "" {
		stream.lastAt = event.Time
	}
	switch chunkType {
	case "reasoning-delta":
		stream.thought.WriteString(chunk.Text)
	case "text-delta":
		stream.text.WriteString(chunk.Text)
	case "tool-call-delta":
		if chunk.Name != "" {
			stream.toolName = chunk.Name
		}
		if chunk.ID != "" {
			stream.toolCallID = chunk.ID
		}
		stream.toolArgs.WriteString(chunk.ArgumentsDelta)
	}
}

var chatStates = struct {
	sync.Mutex
	byPath map[string]*chatState
}{byPath: map[string]*chatState{}}

func chatStateFor(path string) *chatState {
	chatStates.Lock()
	defer chatStates.Unlock()
	state, ok := chatStates.byPath[path]
	if ok {
		return state
	}
	if len(chatStates.byPath) >= chatCacheLimit {
		for key := range chatStates.byPath {
			delete(chatStates.byPath, key)
			break
		}
	}
	state = &chatState{}
	chatStates.byPath[path] = state
	return state
}

// chatRouteRun returns the run id and the tail for /api/runs/{id}/chat and
// /api/runs/{id}/continue, and false for every other path.
func chatRouteRun(urlPath string) (runID, tail string, ok bool) {
	rest, found := strings.CutPrefix(urlPath, "/api/runs/")
	if !found {
		return "", "", false
	}
	runID, tail, found = strings.Cut(rest, "/")
	if !found || (tail != "chat" && tail != "continue") {
		return "", "", false
	}
	return runID, tail, true
}

func chatRouted(urlPath string) bool {
	_, _, ok := chatRouteRun(urlPath)
	return ok
}

func (c *controlServer) handleChatRoute(w http.ResponseWriter, r *http.Request) {
	runID, tail, _ := chatRouteRun(r.URL.Path)
	if err := validateRunID(runID); err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	if tail == "chat" {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		c.handleChat(w, r, runID)
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	c.handleContinue(w, r, runID)
}

func (c *controlServer) handleChat(w http.ResponseWriter, r *http.Request, runID string) {
	session, err := c.readChatSession(runID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// sessionRuns returns the runs that share the session of runID, oldest
// first. A run with no session yet is its own session.
func (c *controlServer) sessionRuns(runID string) ([]runSummary, error) {
	own, err := c.summarizeCached(runID)
	if err != nil {
		return nil, err
	}
	if own.SessionID == "" {
		return []runSummary{own}, nil
	}
	all, err := c.listRuns(-1)
	if err != nil {
		return nil, err
	}
	runs := make([]runSummary, 0, 4)
	for _, candidate := range all {
		if candidate.SessionID == own.SessionID {
			runs = append(runs, candidate)
		}
	}
	slices.SortFunc(runs, func(left, right runSummary) int {
		if left.AcceptedAt != right.AcceptedAt {
			return strings.Compare(left.AcceptedAt, right.AcceptedAt)
		}
		return strings.Compare(left.RunID, right.RunID)
	})
	return runs, nil
}

func (c *controlServer) readChatSession(runID string) (chatSession, error) {
	runs, err := c.sessionRuns(runID)
	if err != nil {
		return chatSession{}, err
	}
	session := chatSession{RunID: runID, Runs: make([]chatRun, 0, len(runs))}
	for _, summary := range runs {
		path := filepath.Join(c.dataDir, runsDirName, summary.RunID, eventsFileName)
		trigger, items, retries, err := readChatRun(path)
		if err != nil {
			return chatSession{}, err
		}
		if items == nil {
			items = []chatItem{}
		}
		session.Runs = append(session.Runs, chatRun{Run: summary, Trigger: trigger, Items: items, Retries: retries})
		session.SessionID = summary.SessionID
		session.Agent = summary.Agent
	}
	session.ContinueRun, session.ContinueBlocked = continueTarget(runs)

	// If the newest run is still in-progress, attach any in-flight active turn
	if len(runs) > 0 && runs[len(runs)-1].State == runStateOpen {
		activeStreams.Lock()
		stream := activeStreams.byRun[runs[len(runs)-1].RunID]
		activeStreams.Unlock()
		if stream != nil {
			session.ActiveTurn = stream.snapshot()
		}
	}
	return session, nil
}

// continueTarget picks the run a follow-up should cite: the newest run of the
// session, once it has ended.
func continueTarget(runs []runSummary) (runID, blocked string) {
	if len(runs) == 0 {
		return "", "There is nothing to continue."
	}
	last := runs[len(runs)-1]
	if last.State == runStateOpen {
		return "", "The agent is working. A follow-up can go in when this run ends."
	}
	return last.RunID, ""
}

// readChatRun brings the cached state of one journal up to date and returns a
// copy of it.
func readChatRun(path string) (chatTrigger, []chatItem, int, error) {
	state := chatStateFor(path)
	state.mu.Lock()
	defer state.mu.Unlock()
	file, err := os.Open(path)
	if err != nil {
		return chatTrigger{}, nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return chatTrigger{}, nil, 0, err
	}
	if info.Size() < state.offset {
		// The journal was replaced; start over.
		state.offset, state.trigger, state.items, state.retries = 0, chatTrigger{}, nil, 0
	}
	if _, err := file.Seek(state.offset, io.SeekStart); err != nil {
		return chatTrigger{}, nil, 0, err
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		line, newline, tooLong, err := readJournalLine(reader)
		// A torn last line has no newline yet; the next poll picks it up.
		if tooLong || err != nil || !newline {
			break
		}
		state.offset += int64(len(line))
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || bytes.Contains(trimmed, []byte(`"type":"dev.genesis.run.chunk"`)) {
			continue
		}
		var event lifecycleEvent
		if json.Unmarshal(trimmed, &event) != nil {
			continue
		}
		state.absorb(event)
	}
	return state.trigger, slices.Clone(state.items), state.retries, nil
}

// absorb folds one journal event into the transcript.
func (s *chatState) absorb(event lifecycleEvent) {
	switch event.Type {
	case lifecycleTypeAccepted:
		var data struct {
			EventType   string `json:"event_type"`
			EventSource string `json:"event_source"`
			Message     string `json:"message"`
		}
		_ = json.Unmarshal(event.Data, &data)
		s.trigger = chatTrigger{Type: data.EventType, Source: data.EventSource, Message: data.Message}
		if s.trigger.Type == "" {
			s.trigger.Type = event.CauseType
		}
		if s.trigger.Source == "" {
			s.trigger.Source = event.CauseSource
		}
	case "dev.genesis.run.assistant":
		s.items = append(s.items, assistantItems(event)...)
	case "dev.genesis.run.tool":
		s.items = append(s.items, toolResultItems(event)...)
	case "dev.genesis.run.retry":
		s.retries++
	case lifecycleTypeError:
		var data struct {
			ErrorType string `json:"error_type"`
			Message   string `json:"message"`
		}
		_ = json.Unmarshal(event.Data, &data)
		text := strings.TrimSpace(data.Message)
		if text == "" {
			text = data.ErrorType
		}
		clipped := clipMessage(text, chatToolLimit)
		s.items = append(s.items, chatItem{
			Seq: event.Sequence, At: event.Time, Role: chatRoleSystem, Kind: chatKindError,
			Name: data.ErrorType, Text: clipped, Failed: true, Truncated: len(clipped) < len(text),
		})
	}
}

type chatMessage struct {
	Content []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Name      string `json:"name"`
		ID        string `json:"id"`
		Arguments string `json:"arguments"`
		CallID    string `json:"toolCallId"`
		IsError   bool   `json:"isError"`
		Inner     []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"content"`
}

// sdkMessage digs the committed message out of a session.event frame. It is
// nil for every other frame.
func sdkMessage(event lifecycleEvent, phase string) *chatMessage {
	var data struct {
		Phase string `json:"phase"`
		Raw   struct {
			Payload struct {
				Event struct {
					Data struct {
						Message chatMessage `json:"message"`
					} `json:"data"`
				} `json:"event"`
			} `json:"payload"`
		} `json:"raw"`
	}
	if json.Unmarshal(event.Data, &data) != nil || data.Phase != phase {
		return nil
	}
	return &data.Raw.Payload.Event.Data.Message
}

func assistantItems(event lifecycleEvent) []chatItem {
	message := sdkMessage(event, "message")
	if message == nil {
		return nil
	}
	items := make([]chatItem, 0, len(message.Content))
	for _, part := range message.Content {
		item := chatItem{Seq: event.Sequence, At: event.Time, Role: chatRoleAgent}
		switch part.Type {
		case "text", "reasoning":
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			item.Kind = chatKindText
			if part.Type == "reasoning" {
				item.Kind = chatKindThought
			}
			item.Text = clipMessage(part.Text, chatTextLimit)
			item.Truncated = len(item.Text) < len(part.Text)
		case "tool-call":
			item.Kind = chatKindCall
			item.Role = chatRoleTool
			item.Name = part.Name
			item.CallID = part.ID
			item.Text = clipMessage(part.Arguments, chatToolLimit)
			item.Truncated = len(item.Text) < len(part.Arguments)
		default:
			continue
		}
		items = append(items, item)
	}
	return items
}

func toolResultItems(event lifecycleEvent) []chatItem {
	message := sdkMessage(event, "result")
	if message == nil {
		return nil
	}
	items := make([]chatItem, 0, len(message.Content))
	for _, part := range message.Content {
		if part.Type != "tool-result" {
			continue
		}
		var text strings.Builder
		for _, inner := range part.Inner {
			if inner.Type == "text" {
				text.WriteString(inner.Text)
			}
		}
		full := text.String()
		items = append(items, chatItem{
			Seq: event.Sequence, At: event.Time, Role: chatRoleTool, Kind: chatKindResult,
			CallID: part.CallID, Text: full, Failed: part.IsError, Truncated: false,
		})
	}
	return items
}

type continueBody struct {
	Message string `json:"message"`
}

// handleContinue sends the operator's follow-up to the agent: a
// session.continue event citing the newest run of the session. The listener
// starts a new run on the same DSH session and answers 202 with its id, or
// 204 when it refuses.
func (c *controlServer) handleContinue(w http.ResponseWriter, r *http.Request, runID string) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, chatBodyMax))
	if err != nil {
		http.Error(w, "the message is too long or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var body continueBody
	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "invalid message JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	if len(body.Message) > chatMessageMax {
		http.Error(w, fmt.Sprintf("message is longer than %d bytes", chatMessageMax), http.StatusBadRequest)
		return
	}
	runs, err := c.sessionRuns(runID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target, blocked := continueTarget(runs)
	if target == "" {
		http.Error(w, blocked, http.StatusConflict)
		return
	}
	if target != runID {
		http.Error(w, "this session has newer runs; follow up on the latest one", http.StatusConflict)
		return
	}
	id, err := c.eventID()
	if err != nil {
		http.Error(w, "failed to create event id", http.StatusInternalServerError)
		return
	}
	event, err := json.Marshal(map[string]any{
		"specversion": cloudEventSpecVersion,
		"id":          id,
		"source":      controlSource,
		"type":        sessionContinueType,
		"subject":     runID,
		"data":        map[string]any{"message": body.Message},
	})
	if err != nil {
		http.Error(w, "failed to encode event", http.StatusInternalServerError)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, c.listenerURL+"/events", bytes.NewReader(event))
	if err != nil {
		http.Error(w, "failed to reach listener", http.StatusBadGateway)
		return
	}
	request.Header.Set("Content-Type", cloudEventsJSON)
	response, err := c.listenerClient.Do(request)
	if err != nil {
		http.Error(w, "listener is unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, maxControlBody))
	switch response.StatusCode {
	case http.StatusAccepted:
		var accepted struct {
			Runs []acceptedRun `json:"runs"`
		}
		if json.Unmarshal(answer, &accepted) != nil || len(accepted.Runs) == 0 {
			http.Error(w, "listener accepted the follow-up without a run", http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": accepted.Runs[0].RunID})
	case http.StatusNoContent:
		http.Error(w, "The listener refused the follow-up. The agent may be missing from the active config (sync first), or its session files are gone.", http.StatusConflict)
	case http.StatusServiceUnavailable:
		http.Error(w, "The listener is syncing. Try again in a moment.", http.StatusServiceUnavailable)
	default:
		http.Error(w, fmt.Sprintf("listener answered %d", response.StatusCode), http.StatusBadGateway)
	}
}

func (c *controlServer) eventID() (string, error) {
	if c.newEventID != nil {
		return c.newEventID()
	}
	return newControlEventID()
}
