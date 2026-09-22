package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const panelToken = "panel-sync-token-not-for-browser"

func TestControlStateDriftSyncAndTokenBoundary(t *testing.T) {
	control, listener := newPanelFixture(t)
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	state := getJSON[controlState](t, panel.URL+"/api/state")
	if state.Drift != driftInSync || state.Active == nil || state.Desired == nil {
		t.Fatalf("initial state = %#v", state)
	}
	if state.Active.Digest != state.Desired.Digest || !state.Listener.SyncConfigured || !state.Listener.OK {
		t.Fatalf("expected active desired match, got %#v", state)
	}
	if strings.Contains(mustGET(t, panel.URL+"/api/state"), panelToken) {
		t.Fatal("state response exposed the sync token")
	}

	agents := getJSON[struct {
		Agents []listedAgent `json:"agents"`
	}](t, panel.URL+"/api/agents")
	if len(agents.Agents) != 1 || agents.Agents[0].Presence != presenceActive || agents.Agents[0].ID != "workspace-janitor" {
		t.Fatalf("agents = %#v", agents.Agents)
	}
	rules := getJSON[struct {
		Rules []listedRule `json:"rules"`
	}](t, panel.URL+"/api/rules")
	if len(rules.Rules) != 1 || rules.Rules[0].Presence != presenceActive {
		t.Fatalf("rules = %#v", rules.Rules)
	}

	writeRule(t, filepath.Join(listener.rulesDir, "extra.yaml"), rule{
		Match: map[string]string{"type": "dev.genesis.extra"},
		Agent: "workspace-janitor",
	})
	draft := getJSON[controlState](t, panel.URL+"/api/state")
	if draft.Drift != driftDraft || draft.Active.Digest == draft.Desired.Digest {
		t.Fatalf("draft state = active %s desired %s drift %s", draft.Active.Digest, draft.Desired.Digest, draft.Drift)
	}
	draftRules := getJSON[struct {
		Rules []listedRule `json:"rules"`
	}](t, panel.URL+"/api/rules")
	var extra listedRule
	for _, candidate := range draftRules.Rules {
		if candidate.Name == "extra.yaml" {
			extra = candidate
		}
	}
	if extra.Presence != presenceDraft {
		t.Fatalf("extra rule presence = %#v", draftRules.Rules)
	}

	syncResponse := postJSON(t, panel.URL+"/api/sync", `{"scope":["agents","rules"]}`, http.StatusOK)
	if strings.Contains(syncResponse, panelToken) {
		t.Fatalf("sync response exposed the sync token: %s", syncResponse)
	}
	if !strings.Contains(syncResponse, `"host_mutation":"none"`) {
		t.Fatalf("sync response = %s", syncResponse)
	}
	synced := getJSON[controlState](t, panel.URL+"/api/state")
	if synced.Drift != driftInSync || synced.Active.Digest != synced.Desired.Digest {
		t.Fatalf("after sync = %#v", synced)
	}
	if strings.Contains(mustGET(t, panel.URL+"/api/health"), panelToken) {
		t.Fatal("health exposed the sync token")
	}
}

func TestControlMessageProxyRunCursorAndReplay(t *testing.T) {
	control, _ := newPanelFixture(t)
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	miss := postJSON(t, panel.URL+"/api/messages", `{"message":"hello","type":"dev.genesis.miss"}`, http.StatusNoContent)
	if miss != "" && strings.Contains(miss, panelToken) {
		t.Fatalf("miss response exposed token: %s", miss)
	}

	accepted := postJSON(t, panel.URL+"/api/messages", `{"message":"inspect the bench","rule":"example.yaml"}`, http.StatusAccepted)
	if strings.Contains(accepted, panelToken) {
		t.Fatalf("accepted response exposed token: %s", accepted)
	}
	var body struct {
		Runs []acceptedRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(accepted), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Runs) != 1 || body.Runs[0].Agent != "workspace-janitor" || body.Runs[0].Rule != "example.yaml" {
		t.Fatalf("accepted = %s", accepted)
	}
	runID := body.Runs[0].RunID

	runs := getJSON[struct {
		Runs []runSummary `json:"runs"`
	}](t, panel.URL+"/api/runs")
	if len(runs.Runs) != 1 || runs.Runs[0].RunID != runID || runs.Runs[0].State != runStateOpen {
		t.Fatalf("runs = %#v", runs.Runs)
	}

	page := getJSON[eventsPage](t, panel.URL+"/api/runs/"+runID+"/events?after=0&limit=10")
	if len(page.Events) == 0 || page.Events[0].Type != lifecycleTypeAccepted || page.Cursor == "0" {
		t.Fatalf("events = %#v", page)
	}
	again := getJSON[eventsPage](t, panel.URL+"/api/runs/"+runID+"/events?after="+page.Cursor)
	if len(again.Events) != 0 || again.HasMore {
		t.Fatalf("cursor did not catch up: %#v", again)
	}

	detail := getJSON[runDetail](t, panel.URL+"/api/runs/"+runID)
	if detail.RunID != runID || detail.EventCount < 1 || detail.CauseType != "dev.genesis.run" {
		t.Fatalf("detail = %#v", detail)
	}

	journal := filepath.Join(control.dataDir, runsDirName, runID, eventsFileName)
	conn := dialLive(t, panel.URL)
	writeLiveOp(t, conn, liveClientOp{Op: "subscribe", Topic: "run", RunID: runID, After: page.Cursor})
	appendLifecycle(t, journal, sampleLifecycle(runID, "2", lifecycleTypeEnd, map[string]any{"exit_code": 0, "state": endStateCompleted}))
	frame := awaitEvent(t, conn, "2")
	if frame.Event == nil || frame.Event.Type != lifecycleTypeEnd || frame.Cursor != "2" {
		t.Fatalf("live frame = %#v", frame)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	reconnected := dialLive(t, panel.URL)
	writeLiveOp(t, reconnected, liveClientOp{Op: "subscribe", Topic: "run", RunID: runID, After: "2"})
	appendLifecycle(t, journal, sampleLifecycle(runID, "3", lifecycleTypeError, map[string]any{"error_type": "test", "message": "later"}))
	next := awaitEvent(t, reconnected, "3")
	if next.Cursor != "3" || next.Event.Type != lifecycleTypeError {
		t.Fatalf("reconnect frame = %#v", next)
	}
	if strings.Contains(string(mustFrame(next)), panelToken) {
		t.Fatal("live frame exposed the sync token")
	}

	clean, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	readable := getJSON[eventsPage](t, panel.URL+"/api/runs/"+runID+"/events?limit=10")
	torn := append(append([]byte{}, clean...), []byte(`{"torn":`)...)
	if err := os.WriteFile(journal, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	skipped := getJSON[eventsPage](t, panel.URL+"/api/runs/"+runID+"/events?limit=10")
	if len(skipped.Events) != len(readable.Events) {
		t.Fatalf("torn tail changed the readable prefix: got %d want %d", len(skipped.Events), len(readable.Events))
	}
	after, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(torn, after) {
		t.Fatal("control truncated the journal; data must stay read-only")
	}
}

func TestControlStaticAndEventProxy(t *testing.T) {
	control, _ := newPanelFixture(t)
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<title>Genesis</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	control.webDir = web
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	home := mustGET(t, panel.URL+"/")
	if !strings.Contains(home, "Genesis") {
		t.Fatalf("home = %s", home)
	}
	traversal := httptest.NewRecorder()
	traversalRequest := httptest.NewRequest(http.MethodGet, "/../../etc/passwd", nil)
	traversalRequest.Host = "127.0.0.1"
	control.ServeHTTP(traversal, traversalRequest)
	if traversal.Code != http.StatusNotFound {
		t.Fatalf("traversal status = %d body %s", traversal.Code, traversal.Body.String())
	}

	raw := postRaw(t, panel.URL+"/api/events", cloudEventsJSON, `{
		"specversion":"1.0","id":"direct-1","source":"urn:genesis:example","type":"dev.genesis.run"
	}`, http.StatusAccepted)
	if !strings.Contains(raw, `"run_id"`) || strings.Contains(raw, panelToken) {
		t.Fatalf("event proxy = %s", raw)
	}
}

func TestControlListenerDownDoesNotInventToken(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "workspace-janitor.yaml", nil)
	writeRule(t, filepath.Join(rulesDir, "example.yaml"), rule{
		Match: map[string]string{"type": "dev.genesis.run"},
		Agent: "workspace-janitor",
	})
	dataDir := t.TempDir()
	control := newControlServer(controlConfig{
		listenerURL: "http://127.0.0.1:1",
		agentsDir:   agentsDir,
		rulesDir:    rulesDir,
		dataDir:     dataDir,
		syncToken:   panelToken,
	}, discardLogger())
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	response, err := http.Get(panel.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("state status = %d %s", response.StatusCode, body)
	}
	if strings.Contains(string(body), panelToken) {
		t.Fatalf("down listener state exposed token: %s", body)
	}
	var state controlState
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	if state.Drift != driftListenerDown || state.Listener.SyncConfigured {
		t.Fatalf("state = %#v", state)
	}
	syncCode := postStatus(t, panel.URL+"/api/sync", `{"scope":["agents","rules"]}`)
	if syncCode != http.StatusBadGateway {
		t.Fatalf("sync while listener is down = %d", syncCode)
	}
}

func TestControlSyncRequiresJSONObject(t *testing.T) {
	control, _ := newPanelFixture(t)
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	missingType := postBare(t, panel.URL+"/api/sync", `{}`)
	if missingType.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type = %d %s", missingType.Code, missingType.Body.String())
	}
	plain := postRaw(t, panel.URL+"/api/sync", "text/plain", `{}`, http.StatusUnsupportedMediaType)
	if !strings.Contains(plain, "application/json") {
		t.Fatalf("plain content type body = %s", plain)
	}

	for _, payload := range []string{"", "   ", "[]", "null", `"x"`} {
		body := postJSON(t, panel.URL+"/api/sync", payload, http.StatusBadRequest)
		if !strings.Contains(body, "JSON object") {
			t.Fatalf("payload %q body = %s", payload, body)
		}
	}
	if body := postJSON(t, panel.URL+"/api/sync", `{"token":"secret"}`, http.StatusBadRequest); !strings.Contains(body, "invalid sync JSON") {
		t.Fatalf("unknown field = %s", body)
	}
	if body := postJSON(t, panel.URL+"/api/sync", `{"scope":["hosts"]}`, http.StatusBadRequest); !strings.Contains(body, "unknown sync scope") {
		t.Fatalf("bad scope = %s", body)
	}
	if body := postJSON(t, panel.URL+"/api/sync", `{}{}`, http.StatusBadRequest); !strings.Contains(body, "one JSON object") {
		t.Fatalf("trailing JSON = %s", body)
	}

	emptyObject := postRaw(t, panel.URL+"/api/sync", "application/json; charset=utf-8", `{}`, http.StatusOK)
	if !strings.Contains(emptyObject, `"host_mutation":"none"`) || strings.Contains(emptyObject, panelToken) {
		t.Fatalf("empty object sync = %s", emptyObject)
	}
}

func TestControlRejectsNonLoopbackHost(t *testing.T) {
	control, _ := newPanelFixture(t)
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<title>Genesis</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	control.webDir = web
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)
	if !strings.Contains(mustGET(t, panel.URL+"/"), "Genesis") {
		t.Fatal("loopback test server did not serve the panel")
	}

	rejected := []struct {
		path, host, origin string
	}{
		{"/api/state", "rebind.example", ""},
		{"/", "rebind.example:8790", ""},
		{"/api/live", "rebind.example", ""},
		{"/api/state", "127.0.0.1:8790", "http://rebind.example"},
		{"/api/health", "", ""},
	}
	for _, item := range rejected {
		response := serveControl(control, item.path, item.host, item.origin, item.path == "/api/live")
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "host is not loopback") {
			t.Fatalf("%s host %q origin %q = %d %s", item.path, item.host, item.origin, response.Code, response.Body.String())
		}
	}

	allowed := []struct {
		path, host, origin string
	}{
		{"/api/state", "127.0.0.1:8790", ""},
		{"/api/health", "localhost", ""},
		{"/", "[::1]:8790", ""},
		{"/api/health", "127.0.0.1", "http://127.0.0.1:8790"},
		{"/api/health", "localhost:8790", "http://[::1]:8790"},
	}
	for _, item := range allowed {
		response := serveControl(control, item.path, item.host, item.origin, false)
		if response.Code != http.StatusOK {
			t.Fatalf("%s host %q origin %q = %d %s", item.path, item.host, item.origin, response.Code, response.Body.String())
		}
	}
}

func TestControlInvalidDesiredKeepsActiveAndRuns(t *testing.T) {
	control, listener := newPanelFixture(t)
	runID := "gen_kept"
	runDir := filepath.Join(control.dataDir, runsDirName, runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	event := sampleLifecycle(runID, "1", lifecycleTypeAccepted, map[string]any{"event_type": "dev.genesis.run"})
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, eventsFileName), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(listener.agentsDir, "workspace-janitor.yaml"), []byte("nope: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	state := getJSON[controlState](t, panel.URL+"/api/state")
	if state.Drift != driftDesiredBad || state.Desired != nil || state.Active == nil || state.Active.Digest == "" || state.DesiredError == "" {
		t.Fatalf("state = %#v", state)
	}
	if !state.Listener.Reachable {
		t.Fatalf("listener should stay reachable when desired YAML is invalid: %#v", state.Listener)
	}

	agentsBody := mustGET(t, panel.URL+"/api/agents")
	var agents struct {
		Agents       []listedAgent `json:"agents"`
		DesiredError string        `json:"desired_error"`
	}
	if err := json.Unmarshal([]byte(agentsBody), &agents); err != nil {
		t.Fatal(err)
	}
	if agents.DesiredError == "" || len(agents.Agents) != 1 || agents.Agents[0].ID != "workspace-janitor" || agents.Agents[0].Presence != presenceActiveOnly {
		t.Fatalf("agents = %#v", agents)
	}
	rulesBody := mustGET(t, panel.URL+"/api/rules")
	var rules struct {
		Rules        []listedRule `json:"rules"`
		DesiredError string       `json:"desired_error"`
	}
	if err := json.Unmarshal([]byte(rulesBody), &rules); err != nil {
		t.Fatal(err)
	}
	if rules.DesiredError == "" || len(rules.Rules) != 1 || rules.Rules[0].Name != "example.yaml" || rules.Rules[0].Presence != presenceActiveOnly {
		t.Fatalf("rules = %#v", rules)
	}
	runs := getJSON[struct {
		Runs []runSummary `json:"runs"`
	}](t, panel.URL+"/api/runs")
	if len(runs.Runs) != 1 || runs.Runs[0].RunID != runID {
		t.Fatalf("runs = %#v", runs.Runs)
	}
}

func TestControlDesignerUserMessageRule(t *testing.T) {
	control, listener := newPanelFixture(t)
	root := repoRoot(t)
	copyFile(t, filepath.Join(root, "agents.d", "designer.yaml"), filepath.Join(listener.agentsDir, "designer.yaml"))
	copyFile(t, filepath.Join(root, "rules.d", "designer.yaml"), filepath.Join(listener.rulesDir, "designer.yaml"))

	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	agents := getJSON[struct {
		Agents []listedAgent `json:"agents"`
	}](t, panel.URL+"/api/agents")
	var designer listedAgent
	for _, candidate := range agents.Agents {
		if candidate.ID == "designer" {
			designer = candidate
		}
	}
	if designer.ID != "designer" || designer.Presence != presenceDraft || designer.User != "" {
		t.Fatalf("draft designer = %#v", designer)
	}
	if designer.Cwd != "/var/lib/genesis/config" {
		t.Fatalf("designer cwd = %q", designer.Cwd)
	}
	listed := getJSON[struct {
		Rules []listedRule `json:"rules"`
	}](t, panel.URL+"/api/rules")
	var designerRule listedRule
	for _, candidate := range listed.Rules {
		if candidate.Name == "designer.yaml" {
			designerRule = candidate
		}
	}
	if designerRule.Presence != presenceDraft || designerRule.Agent != "designer" ||
		designerRule.Match["type"] != "dev.genesis.user.message" ||
		designerRule.Match["source"] != "urn:genesis:control" ||
		designerRule.Match["subject"] != "designer" {
		t.Fatalf("draft designer rule = %#v", designerRule)
	}

	unsynced := postJSON(t, panel.URL+"/api/messages", `{"message":"add a lab agent","rule":"designer.yaml"}`, http.StatusNoContent)
	if unsynced != "" && strings.Contains(unsynced, panelToken) {
		t.Fatalf("unsynced response exposed token: %s", unsynced)
	}

	syncResponse := postJSON(t, panel.URL+"/api/sync", `{"scope":["agents","rules"]}`, http.StatusOK)
	if !strings.Contains(syncResponse, `"host_mutation":"none"`) {
		t.Fatalf("designer sync mutated host: %s", syncResponse)
	}

	accepted := postJSON(t, panel.URL+"/api/messages", `{"message":"add a lab agent","rule":"designer.yaml"}`, http.StatusAccepted)
	var body struct {
		Runs []acceptedRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(accepted), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Runs) != 1 || body.Runs[0].Agent != "designer" || body.Runs[0].Rule != "designer.yaml" {
		t.Fatalf("accepted = %s", accepted)
	}

	fake, ok := listener.runner.(*fakeRunner)
	if !ok {
		t.Fatal("expected fake runner")
	}
	select {
	case inv := <-fake.invocations:
		if inv.Agent != "designer" || inv.User != "" {
			t.Fatalf("invocation identity = agent %q user %q", inv.Agent, inv.User)
		}
		if inv.Cwd != "/var/lib/genesis/config" {
			t.Fatalf("invocation cwd = %q", inv.Cwd)
		}
		if string(inv.Event["type"]) != `"dev.genesis.user.message"` {
			t.Fatalf("event type = %s", inv.Event["type"])
		}
		if string(inv.Event["source"]) != `"urn:genesis:control"` {
			t.Fatalf("event source = %s", inv.Event["source"])
		}
		if string(inv.Event["subject"]) != `"designer"` {
			t.Fatalf("event subject = %s", inv.Event["subject"])
		}
		if !bytes.Contains(inv.Event["data"], []byte(`"message":"add a lab agent"`)) {
			t.Fatalf("event data = %s", inv.Event["data"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("designer invocation was not started")
	}

	active := getJSON[struct {
		Agents []listedAgent `json:"agents"`
	}](t, panel.URL+"/api/agents")
	for _, candidate := range active.Agents {
		if candidate.ID == "designer" && (candidate.User != "" || candidate.Presence != presenceActive) {
			t.Fatalf("active designer = %#v", candidate)
		}
	}
}

func TestControlMessageCopiesMatchAttributes(t *testing.T) {
	minted, err := buildMessageEvent(map[string]string{
		"specversion": "1.0",
		"id":          "fixed",
		"type":        "dev.genesis.run",
		"source":      "urn:genesis:example",
		"subject":     "bench",
		"dataset":     "lab",
	}, "inspect", func() (string, error) {
		t.Fatal("id was already present")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if minted["id"] != "fixed" || minted["dataset"] != "lab" || minted["subject"] != "bench" || minted["specversion"] != "1.0" {
		t.Fatalf("event = %#v", minted)
	}
	payload, ok := minted["data"].(map[string]any)
	if !ok || payload["message"] != "inspect" {
		t.Fatalf("data = %#v", minted["data"])
	}
	if _, err := buildMessageEvent(map[string]string{"specversion": "0.3", "type": "dev.genesis.run"}, "x", nil); err == nil || !strings.Contains(err.Error(), "specversion") {
		t.Fatalf("specversion conflict = %v", err)
	}
	if _, err := buildMessageEvent(map[string]string{"data": "payload", "type": "dev.genesis.run"}, "x", nil); err == nil || !strings.Contains(err.Error(), "data") {
		t.Fatalf("data conflict = %v", err)
	}
	if _, err := buildMessageEvent(map[string]string{"id": "  ", "type": "dev.genesis.run", "source": "urn:genesis:example"}, "x", nil); err == nil || !strings.Contains(err.Error(), "id") {
		t.Fatalf("empty id = %v", err)
	}

	control, listener := newPanelFixture(t)
	writeRule(t, filepath.Join(listener.rulesDir, "example.yaml"), rule{
		Match: map[string]string{"type": "dev.genesis.other"},
		Agent: "workspace-janitor",
	})
	writeRule(t, filepath.Join(listener.rulesDir, "dataset.yaml"), rule{
		Match: map[string]string{
			"type":    "dev.genesis.run",
			"source":  "urn:genesis:example",
			"dataset": "lab",
			"id":      "fixed-id",
		},
		Agent: "workspace-janitor",
	})
	if err := listener.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	writeRule(t, filepath.Join(listener.rulesDir, "spec-conflict.yaml"), rule{
		Match: map[string]string{"specversion": "0.3", "type": "dev.genesis.run", "source": "urn:genesis:example"},
		Agent: "workspace-janitor",
	})
	writeRule(t, filepath.Join(listener.rulesDir, "data-conflict.yaml"), rule{
		Match: map[string]string{"data": "payload", "type": "dev.genesis.run", "source": "urn:genesis:example"},
		Agent: "workspace-janitor",
	})
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	if body := postJSON(t, panel.URL+"/api/messages", `{"message":"no","rule":"spec-conflict.yaml"}`, http.StatusBadRequest); !strings.Contains(body, "specversion") {
		t.Fatalf("specversion HTTP = %s", body)
	}
	if body := postJSON(t, panel.URL+"/api/messages", `{"message":"no","rule":"data-conflict.yaml"}`, http.StatusBadRequest); !strings.Contains(body, "data") {
		t.Fatalf("data HTTP = %s", body)
	}
	accepted := postJSON(t, panel.URL+"/api/messages", `{"message":"inspect the bench","rule":"dataset.yaml"}`, http.StatusAccepted)
	var body struct {
		Runs []acceptedRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(accepted), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Runs) != 1 {
		t.Fatalf("custom match was not applied: %s", accepted)
	}
	detail := getJSON[runDetail](t, panel.URL+"/api/runs/"+body.Runs[0].RunID)
	if detail.CauseID != "fixed-id" || detail.CauseType != "dev.genesis.run" {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestControlGenerationOverflowDistinctFromDown(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "workspace-janitor.yaml", nil)
	writeRule(t, filepath.Join(rulesDir, "example.yaml"), rule{
		Match: map[string]string{"type": "dev.genesis.run"},
		Agent: "workspace-janitor",
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/generation":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 64))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	control := newControlServer(controlConfig{
		listenerURL: upstream.URL,
		agentsDir:   agentsDir,
		rulesDir:    rulesDir,
		dataDir:     t.TempDir(),
		syncToken:   panelToken,
	}, discardLogger())
	control.generationLimit = 32
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	state := getJSON[controlState](t, panel.URL+"/api/state")
	if state.Drift != driftGenerationHuge || !state.Listener.Reachable || state.Listener.OK || state.Drift == driftListenerDown {
		t.Fatalf("overflow state = %#v", state)
	}
	if !strings.Contains(state.Listener.Error, "larger than") || strings.Contains(state.Listener.Error, "unreachable") {
		t.Fatalf("overflow error = %q", state.Listener.Error)
	}

	down := newControlServer(controlConfig{
		listenerURL: "http://127.0.0.1:1",
		agentsDir:   agentsDir,
		rulesDir:    rulesDir,
		dataDir:     t.TempDir(),
		syncToken:   panelToken,
	}, discardLogger())
	downPanel := httptest.NewServer(down)
	t.Cleanup(downPanel.Close)
	unavailable := getJSON[controlState](t, downPanel.URL+"/api/state")
	if unavailable.Drift != driftListenerDown || unavailable.Listener.Reachable || !strings.Contains(unavailable.Listener.Error, "unreachable") {
		t.Fatalf("down state = %#v", unavailable)
	}
}

func serveControl(control *controlServer, target, host, origin string, upgrade bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Host = host
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if upgrade {
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "websocket")
		request.Header.Set("Sec-WebSocket-Version", "13")
		request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
	response := httptest.NewRecorder()
	control.ServeHTTP(response, request)
	return response
}

func postBare(t *testing.T, url, payload string) *httptest.ResponseRecorder {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	recorder.Code = response.StatusCode
	_, _ = recorder.Body.Write(body)
	return recorder
}

func newPanelFixture(t *testing.T) (*controlServer, *eventServer) {
	t.Helper()
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	dataDir := t.TempDir()
	if _, err := prepareDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	writeNamedAgent(t, agentsDir, "workspace-janitor.yaml", nil)
	writeRule(t, filepath.Join(rulesDir, "example.yaml"), rule{
		Match: map[string]string{"type": "dev.genesis.run", "source": "urn:genesis:example"},
		Agent: "workspace-janitor",
	})
	listener := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		logger:    discardLogger(),
		syncToken: panelToken,
		store:     newRunStore(dataDir, newEventBus(), discardLogger()),
		runner:    &fakeRunner{invocations: make(chan invocation, 8)},
		newRunID:  newGenesisRunID,
	}
	if err := listener.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(listener)
	t.Cleanup(upstream.Close)
	control := newControlServer(controlConfig{
		listenerURL: upstream.URL,
		agentsDir:   agentsDir,
		rulesDir:    rulesDir,
		dataDir:     dataDir,
		syncToken:   panelToken,
	}, discardLogger())
	control.pollEvery = 15 * time.Millisecond
	return control, listener
}

func mustGET(t *testing.T, url string) string {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", url, response.StatusCode, body)
	}
	return string(body)
}

func getJSON[T any](t *testing.T, url string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(mustGET(t, url)), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func postJSON(t *testing.T, url, payload string, want int) string {
	t.Helper()
	return postRaw(t, url, "application/json", payload, want)
}

func postRaw(t *testing.T, url, contentType, payload string, want int) string {
	t.Helper()
	response, err := http.Post(url, contentType, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("POST %s = %d (%s), want %d", url, response.StatusCode, body, want)
	}
	return string(body)
}

func postStatus(t *testing.T, url, payload string) int {
	t.Helper()
	response, err := http.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func sampleLifecycle(runID, seq, eventType string, data any) lifecycleEvent {
	payload, _ := json.Marshal(data)
	return lifecycleEvent{
		SpecVersion:  cloudEventSpecVersion,
		ID:           "evt_" + seq,
		Source:       genesisSource(runID),
		Type:         eventType,
		Time:         "2026-09-21T12:00:0" + seq + ".000000Z",
		Sequence:     seq,
		SequenceType: sequenceTypeInteger,
		RunID:        runID,
		AgentID:      "workspace-janitor",
		Rulefile:     "example.yaml",
		CauseID:      "cause-1",
		CauseSource:  "urn:genesis:example",
		CauseType:    "dev.genesis.run",
		Origin:       originGenesis,
		Data:         payload,
	}
}

func appendLifecycle(t *testing.T, path string, event lifecycleEvent) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(append(payload, '\n')); err != nil {
		t.Fatal(err)
	}
}

func dialLive(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	endpoint := "ws" + strings.TrimPrefix(serverURL, "http") + "/api/live"
	conn, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

func writeLiveOp(t *testing.T, conn *websocket.Conn, op liveClientOp) {
	t.Helper()
	payload, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func awaitEvent(t *testing.T, conn *websocket.Conn, seq string) liveFrame {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		_, payload, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var frame liveFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Op == "event" && frame.Cursor == seq {
			return frame
		}
	}
	t.Fatalf("timed out waiting for event %s", seq)
	return liveFrame{}
}

func mustFrame(frame liveFrame) []byte {
	payload, _ := json.Marshal(frame)
	return payload
}
