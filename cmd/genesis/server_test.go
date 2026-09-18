package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

type fakeRunner struct {
	invocations chan invocation
	release     <-chan struct{}
}

func (f *fakeRunner) Run(document invocation) {
	f.invocations <- document
	if f.release != nil {
		<-f.release
	}
}

func TestEventServerRoutesToAllMatchesAsynchronously(t *testing.T) {
	rulesDir := t.TempDir()
	firstCwd := t.TempDir()
	secondCwd := t.TempDir()
	writeRule(t, rulesDir+"/01-first.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Run: runSpec{
			Cwd: firstCwd,
			Env: map[string]string{"TOKEN": "first"},
		},
	})
	writeRule(t, rulesDir+"/02-second.yaml", rule{
		Match: map[string]string{"type": "com.example.run", "source": "urn:test"},
		Run: runSpec{
			Cwd: secondCwd,
			Env: map[string]string{"TOKEN": "second"},
		},
	})
	writeRule(t, rulesDir+"/03-other.yaml", rule{
		Match: map[string]string{"type": "com.example.other"},
		Run:   runSpec{Cwd: t.TempDir()},
	})

	release := make(chan struct{})
	defer close(release)
	fake := &fakeRunner{invocations: make(chan invocation, 2), release: release}
	runIDs := []string{"gen_first", "gen_second"}
	server := &eventServer{
		rulesDir: rulesDir,
		runner:   fake,
		logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		newRunID: func() (string, error) {
			runID := runIDs[0]
			runIDs = runIDs[1:]
			return runID, nil
		},
	}
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-1",
		"source":      "urn:test",
		"type":        "com.example.run",
		"data": map[string]any{
			"complete": true,
			"items":    []any{"one", "two"},
		},
	}

	response := sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var accepted struct {
		Runs []acceptedRun `json:"runs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	wantAccepted := []acceptedRun{
		{Rule: "01-first.yaml", RunID: "gen_first"},
		{Rule: "02-second.yaml", RunID: "gen_second"},
	}
	if !reflect.DeepEqual(accepted.Runs, wantAccepted) {
		t.Fatalf("accepted runs = %#v, want %#v", accepted.Runs, wantAccepted)
	}

	invocations := map[string]invocation{}
	for range 2 {
		select {
		case document := <-fake.invocations:
			invocations[document.Rule] = document
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for asynchronous runner")
		}
	}
	for _, acceptedRun := range wantAccepted {
		document := invocations[acceptedRun.Rule]
		if document.RunID != acceptedRun.RunID {
			t.Fatalf("%s run ID = %q, want %q", acceptedRun.Rule, document.RunID, acceptedRun.RunID)
		}
		var completeEvent map[string]any
		contents, err := json.Marshal(document.Event)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(contents, &completeEvent); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(completeEvent, event) {
			t.Fatalf("%s event = %#v, want %#v", acceptedRun.Rule, completeEvent, event)
		}
	}
	if got := invocations["01-first.yaml"]; got.Cwd != firstCwd ||
		!reflect.DeepEqual(got.Env, map[string]string{"TOKEN": "first"}) {
		t.Fatalf("first invocation = %#v", got)
	}
	if got := invocations["02-second.yaml"]; got.Cwd != secondCwd ||
		!reflect.DeepEqual(got.Env, map[string]string{"TOKEN": "second"}) {
		t.Fatalf("second invocation = %#v", got)
	}
}

func TestEventServerReloadsRulesEveryRequest(t *testing.T) {
	rulesDir := t.TempDir()
	rulePath := rulesDir + "/reload.yaml"
	loadedRule := rule{
		Match: map[string]string{"type": "com.example.run", "source": "urn:one"},
		Run:   runSpec{Cwd: t.TempDir()},
	}
	writeRule(t, rulePath, loadedRule)

	fake := &fakeRunner{invocations: make(chan invocation, 1)}
	server := &eventServer{
		rulesDir: rulesDir,
		runner:   fake,
		newRunID: func() (string, error) { return "gen_reload", nil },
		logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-2",
		"source":      "urn:two",
		"type":        "com.example.run",
	}

	response := sendEvent(server, event)
	if response.Code != http.StatusNoContent {
		t.Fatalf("non-match status = %d, body = %s", response.Code, response.Body.String())
	}

	loadedRule.Match["source"] = "urn:two"
	writeRule(t, rulePath, loadedRule)
	response = sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("reloaded match status = %d, body = %s", response.Code, response.Body.String())
	}
	select {
	case document := <-fake.invocations:
		if document.Rule != "reload.yaml" {
			t.Fatalf("rule = %q", document.Rule)
		}
	case <-time.After(time.Second):
		t.Fatal("reloaded rule was not invoked")
	}
}

func TestEventServerRequiresStructuredCloudEvent(t *testing.T) {
	server := &eventServer{rulesDir: t.TempDir()}

	request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"specversion":"1.0"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("plain JSON status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"specversion":"1.0"}`))
	request.Header.Set("Content-Type", cloudEventsJSON)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("incomplete CloudEvent status = %d", response.Code)
	}
}

func TestLoadRulesRejectsRawArguments(t *testing.T) {
	rulesDir := t.TempDir()
	contents := `
match:
  type: com.example.run
run:
  cwd: /tmp
  args: [unsafe]
  env: {}
`
	if err := os.WriteFile(rulesDir+"/invalid.yaml", []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRules(rulesDir); err == nil || !strings.Contains(err.Error(), "args") {
		t.Fatalf("loadRules error = %v, want unknown args field", err)
	}
}

func TestNewGenesisRunID(t *testing.T) {
	runID, err := newGenesisRunID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(runID, "gen_") {
		t.Fatalf("run ID = %q", runID)
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(runID, "gen_"))
	if err != nil || len(decoded) != 16 {
		t.Fatalf("run ID random part = %q, error = %v", runID, err)
	}
}

func TestProcessRunnerCapturesInvocationAndStructuredResult(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "invocation.json")
	t.Setenv("GENESIS_CAPTURE", capture)
	fakePython := writeExecutable(t, `
#!/bin/sh
/bin/cat >"$GENESIS_CAPTURE"
printf '%s\n' '{"deepseek_session_id":"dsh-session-1","finish_reason":"completed","final_response":"done","error":null,"diagnostics":null}'
`)

	var logs bytes.Buffer
	runner := processRunner{
		pythonPath: fakePython,
		source:     "embedded runner source",
		logger:     slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	document := invocation{
		Event: cloudEvent{
			"specversion": json.RawMessage(`"1.0"`),
			"id":          json.RawMessage(`"evt-3"`),
			"source":      json.RawMessage(`"urn:test"`),
			"type":        json.RawMessage(`"com.example.run"`),
		},
		Rule:  "test.yaml",
		RunID: "gen_test",
		Cwd:   "/workspace",
		Env:   map[string]string{"ONLY": "declared"},
	}
	runner.Run(document)

	var captured invocation
	contents, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contents, &captured); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(captured, document) {
		t.Fatalf("captured invocation = %#v, want %#v", captured, document)
	}

	records := decodeLogRecords(t, logs.Bytes())
	finished := records[len(records)-1]
	for key, want := range map[string]any{
		"level":               "INFO",
		"genesis_run_id":      "gen_test",
		"rule":                "test.yaml",
		"deepseek_session_id": "dsh-session-1",
		"finish_reason":       "completed",
		"final_response":      "done",
	} {
		if got := finished[key]; got != want {
			t.Fatalf("log %s = %#v, want %#v", key, got, want)
		}
	}
	if finished["error"] != nil {
		t.Fatalf("success error = %#v", finished["error"])
	}
}

func TestProcessRunnerStructuredLogsErrors(t *testing.T) {
	fakePython := writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"deepseek_session_id":"dsh-error","finish_reason":"error","final_response":"","error":null,"diagnostics":{"turn_end":{"type":"turn/end","data":{"reason":{"kind":"error","message":"upstream unavailable"}}},"events":[{"type":"agent/error","data":{"message":"provider request failed"}},{"type":"turn/end","data":{"reason":{"kind":"error","message":"upstream unavailable"}}}],"notifications":[]}}'
printf '%s\n' 'runner stderr' >&2
`)
	var logs bytes.Buffer
	runner := processRunner{
		pythonPath: fakePython,
		source:     "embedded runner source",
		logger:     slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	runner.Run(invocation{Rule: "bad.yaml", RunID: "gen_bad"})

	records := decodeLogRecords(t, logs.Bytes())
	finished := records[len(records)-1]
	errorMessage, _ := finished["error"].(string)
	if finished["level"] != "ERROR" ||
		finished["error_type"] != "DeepSeekRunError" ||
		!strings.Contains(errorMessage, "empty final response") ||
		finished["finish_reason"] != "error" ||
		finished["final_response"] != "" ||
		finished["stderr"] != "runner stderr" {
		t.Fatalf("error log = %#v", finished)
	}
	diagnostics, ok := finished["diagnostics"].(map[string]any)
	if !ok {
		t.Fatalf("diagnostics = %#v", finished["diagnostics"])
	}
	turnEnd, ok := diagnostics["turn_end"].(map[string]any)
	if !ok || turnEnd["type"] != "turn/end" {
		t.Fatalf("turn/end diagnostics = %#v", diagnostics["turn_end"])
	}
}

func writeRule(t *testing.T, path string, value rule) {
	t.Helper()
	contents, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sendEvent(server *eventServer, event map[string]any) *httptest.ResponseRecorder {
	contents, _ := json.Marshal(event)
	request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(string(contents)))
	request.Header.Set("Content-Type", cloudEventsJSON)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func writeExecutable(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-python")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeLogRecords(t *testing.T, contents []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var records []map[string]any
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				return records
			}
			t.Fatal(err)
		}
		records = append(records, record)
	}
}
