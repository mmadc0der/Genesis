package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorMayContinueAnyEndedRunPastTheHopLimit(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "worker.yaml", nil)
	server, fake := newMatchServer(t, agentsDir, rulesDir, "gen_by_operator")
	document := sampleRunInvocation("gen_h8")
	document.Agent = "worker"
	document.Cwd = cwd
	document.Home = home
	acceptFinishedRun(t, server.store, document, "session-h8")
	for hop := 0; hop < maxContinuationHops; hop++ {
		id := hopID(hop)
		parent := ""
		if hop > 0 {
			parent = hopID(hop - 1)
		}
		dir := filepath.Join(server.store.dataDir, runsDirName, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeSessionRecord(filepath.Join(dir, sessionFileName), sessionRecord{
			SessionID:     "session-" + id,
			DshHome:       stableDshHome(server.store.dataDir, "gen_h0"),
			CorrelationID: "gen_h0",
			ContinuedFrom: parent,
			Cwd:           cwd,
			Agent:         "worker",
		}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(server.store.dataDir, runsDirName, "gen_h8", sessionFileName)
	record, err := readSessionRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	record.ContinuedFrom = "gen_h7"
	record.CorrelationID = "gen_h0"
	if err := writeSessionRecord(path, record); err != nil {
		t.Fatal(err)
	}

	// An agent is stopped at hop 9; the operator is not.
	if response := sendEvent(server, continuationEvent("evt_agent", "urn:genesis:agent:worker", "gen_h8", "more")); response.Code != http.StatusNoContent {
		t.Fatalf("agent status = %d", response.Code)
	}
	response := sendEvent(server, continuationEvent("evt_operator", controlSource, "gen_h8", "carry on"))
	if response.Code != http.StatusAccepted {
		t.Fatalf("operator status = %d body = %s", response.Code, response.Body.String())
	}
	got := takeInvocations(t, fake.invocations, 1)
	if got[0].Agent != "worker" || got[0].UserMessage != "carry on" {
		t.Fatalf("operator continuation = %#v", got[0])
	}
	events := readRunEvents(t, server.store, "gen_by_operator")
	var data struct {
		Message string `json:"message"`
		Source  string `json:"event_source"`
	}
	if err := json.Unmarshal(events[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if events[0].Type != lifecycleTypeAccepted || data.Message != "carry on" || data.Source != controlSource {
		t.Fatalf("accepted = %s %+v", events[0].Type, data)
	}
}

func TestAcceptedMessageIsClippedOnARuneBoundary(t *testing.T) {
	text := strings.Repeat("é", acceptedMessageLimit) // two bytes each
	clipped := clipMessage(text, acceptedMessageLimit)
	if len(clipped) != acceptedMessageLimit {
		t.Fatalf("len = %d", len(clipped))
	}
	if clipMessage("short", 100) != "short" {
		t.Fatal("short text changed")
	}
	if got := clipMessage("aé", 2); got != "a" {
		t.Fatalf("mid-rune cut = %q", got)
	}
}

// chatJournal writes a run journal with the events a real run leaves, plus
// chunk frames that the chat must skip.
func chatJournal(t *testing.T, dataDir, runID, sessionID, message string, ended bool) string {
	t.Helper()
	assistant := func(content ...map[string]any) map[string]any {
		return map[string]any{"phase": "message", "raw": map[string]any{"payload": map[string]any{"event": map[string]any{
			"data": map[string]any{"message": map[string]any{"role": "assistant", "content": content}},
		}}}}
	}
	toolResult := map[string]any{"phase": "result", "raw": map[string]any{"payload": map[string]any{"event": map[string]any{
		"data": map[string]any{"message": map[string]any{"role": "user", "content": []map[string]any{{
			"type": "tool-result", "toolCallId": "call_1", "isError": true,
			"content": []map[string]any{{"type": "text", "text": "no such file"}},
		}}}},
	}}}}
	events := []lifecycleEvent{
		sampleLifecycle(runID, "1", lifecycleTypeAccepted, func() map[string]any {
			data := map[string]any{
				"event_id": "evt_1", "event_source": controlSource, "event_type": "dev.genesis.user.message", "message": message,
			}
			if message == "follow up" {
				data["event_type"] = sessionContinueType
				data["event_subject"] = "gen_chat_a"
			}
			return data
		}()),
		sampleLifecycle(runID, "2", lifecycleTypeSessionCreated, map[string]any{"session_id": sessionID}),
		sampleLifecycle(runID, "3", "dev.genesis.run.chunk", map[string]any{"frame": "chunk"}),
		sampleLifecycle(runID, "4", "dev.genesis.run.retry", map[string]any{"code": "TRANSPORT"}),
		sampleLifecycle(runID, "5", "dev.genesis.run.assistant", assistant(
			map[string]any{"type": "reasoning", "text": "think first"},
			map[string]any{"type": "tool-call", "id": "call_1", "name": "bash", "arguments": `{"command":"ls /nope"}`},
		)),
		sampleLifecycle(runID, "6", "dev.genesis.run.tool", toolResult),
		sampleLifecycle(runID, "7", "dev.genesis.run.assistant", assistant(map[string]any{"type": "text", "text": "done: " + message})),
	}
	if ended {
		events = append(events, sampleLifecycle(runID, "8", lifecycleTypeEnd, endPayload(0, endStateCompleted)))
	}
	dir := filepath.Join(dataDir, runsDirName, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, eventsFileName)
	var out []byte
	for _, event := range events {
		event.SessionID = sessionID
		if event.Type == lifecycleTypeAccepted {
			event.SessionID = ""
		}
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func chatServer(t *testing.T, upstream string) (*controlServer, string) {
	t.Helper()
	dataDir := t.TempDir()
	control := newControlServer(controlConfig{listenerURL: upstream, agentsDir: t.TempDir(), rulesDir: t.TempDir(), dataDir: dataDir}, discardLogger())
	return control, dataDir
}

func chatRequest(control *controlServer, method, target, contentType, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Host = "127.0.0.1"
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	control.ServeHTTP(response, request)
	return response
}

func TestChatCondensesTheSessionAcrossRuns(t *testing.T) {
	control, dataDir := chatServer(t, "http://127.0.0.1:1")
	chatJournal(t, dataDir, "gen_chat_a", "session-chat", "first ask", true)
	chatJournal(t, dataDir, "gen_chat_b", "session-chat", "follow up", false)
	chatJournal(t, dataDir, "gen_other", "session-other", "elsewhere", true)

	response := chatRequest(control, http.MethodGet, "/api/runs/gen_chat_a/chat", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	var session chatSession
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.SessionID != "session-chat" || len(session.Runs) != 2 {
		t.Fatalf("session = %+v", session)
	}
	first := session.Runs[0]
	if first.Run.RunID != "gen_chat_a" || session.Runs[1].Run.RunID != "gen_chat_b" {
		t.Fatalf("order = %s, %s", first.Run.RunID, session.Runs[1].Run.RunID)
	}
	if first.Trigger.Message != "first ask" || first.Trigger.Type != "dev.genesis.user.message" || first.Trigger.Source != controlSource {
		t.Fatalf("trigger = %+v", first.Trigger)
	}
	if first.Retries != 1 {
		t.Fatalf("retries = %d", first.Retries)
	}
	kinds := []string{}
	for _, item := range first.Items {
		kinds = append(kinds, item.Role+"/"+item.Kind)
	}
	want := []string{"assistant/reasoning", "tool/call", "tool/result", "assistant/text"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("items = %v, want %v", kinds, want)
	}
	if first.Items[1].Name != "bash" || first.Items[1].CallID != "call_1" || !strings.Contains(first.Items[1].Text, "ls /nope") {
		t.Fatalf("call = %+v", first.Items[1])
	}
	if !first.Items[2].Failed || first.Items[2].Text != "no such file" || first.Items[2].CallID != "call_1" {
		t.Fatalf("result = %+v", first.Items[2])
	}
	if session.ContinueRun != "" || !strings.Contains(session.ContinueBlocked, "working") {
		t.Fatalf("an open run must block a follow-up: %q %q", session.ContinueRun, session.ContinueBlocked)
	}

	// The run ends and the same journal grows: only the new lines are read.
	chatJournal(t, dataDir, "gen_chat_b", "session-chat", "follow up", true)
	response = chatRequest(control, http.MethodGet, "/api/runs/gen_chat_b/chat", "", "")
	session = chatSession{}
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.ContinueRun != "gen_chat_b" || session.ContinueBlocked != "" {
		t.Fatalf("continue = %q %q", session.ContinueRun, session.ContinueBlocked)
	}
	if len(session.Runs[1].Items) != 4 {
		t.Fatalf("rebuilt journal items = %d", len(session.Runs[1].Items))
	}

	if chatRequest(control, http.MethodGet, "/api/runs/gen_missing/chat", "", "").Code != http.StatusNotFound {
		t.Fatal("unknown run was not 404")
	}
	if chatRequest(control, http.MethodPost, "/api/runs/gen_chat_a/chat", "", "").Code != http.StatusMethodNotAllowed {
		t.Fatal("chat accepted POST")
	}
}

func TestContinueSendsAnOperatorEventToTheListener(t *testing.T) {
	var got map[string]any
	status := http.StatusAccepted
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(body, &got)
		if status == http.StatusAccepted {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"runs":[{"rule":"session.continue","agent":"worker","run_id":"gen_new_run"}]}`))
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(upstream.Close)
	control, dataDir := chatServer(t, upstream.URL)
	chatJournal(t, dataDir, "gen_chat_a", "session-chat", "first ask", true)
	chatJournal(t, dataDir, "gen_chat_b", "session-chat", "second ask", true)
	post := func(runID, contentType, body string) *httptest.ResponseRecorder {
		return chatRequest(control, http.MethodPost, "/api/runs/"+runID+"/continue", contentType, body)
	}

	if code := post("gen_chat_b", "text/plain", `{"message":"x"}`).Code; code != http.StatusUnsupportedMediaType {
		t.Fatalf("media type = %d", code)
	}
	if code := post("gen_chat_b", "application/json", `{"message":"  "}`).Code; code != http.StatusBadRequest {
		t.Fatalf("blank = %d", code)
	}
	if code := post("gen_chat_b", "application/json", `{"message":"x","rule":"y"}`).Code; code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", code)
	}
	long := `{"message":"` + strings.Repeat("a", chatMessageMax+1) + `"}`
	if code := post("gen_chat_b", "application/json", long).Code; code != http.StatusBadRequest {
		t.Fatalf("long = %d", code)
	}
	if got != nil {
		t.Fatalf("a rejected request reached the listener: %v", got)
	}
	if code := post("gen_chat_a", "application/json", `{"message":"x"}`).Code; code != http.StatusConflict {
		t.Fatalf("older run = %d", code)
	}
	if code := post("gen_missing", "application/json", `{"message":"x"}`).Code; code != http.StatusNotFound {
		t.Fatalf("missing = %d", code)
	}

	response := post("gen_chat_b", "application/json", `{"message":"look again"}`)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"run_id":"gen_new_run"`) {
		t.Fatalf("accepted = %d %s", response.Code, response.Body.String())
	}
	data, _ := got["data"].(map[string]any)
	if got["type"] != sessionContinueType || got["source"] != controlSource || got["subject"] != "gen_chat_b" || data["message"] != "look again" {
		t.Fatalf("event = %v", got)
	}
	if id, _ := got["id"].(string); id == "" {
		t.Fatalf("event has no id: %v", got)
	}

	status = http.StatusNoContent
	response = post("gen_chat_b", "application/json", `{"message":"look again"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "refused") {
		t.Fatalf("refused = %d %s", response.Code, response.Body.String())
	}

	chatJournal(t, dataDir, "gen_chat_c", "session-chat", "third", false)
	if code := post("gen_chat_c", "application/json", `{"message":"x"}`).Code; code != http.StatusConflict {
		t.Fatalf("open run = %d", code)
	}
}
