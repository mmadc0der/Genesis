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
	t.Setenv(deepSeekAPIKey, "process-secret")
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	firstCwd, firstHome := writeNamedAgent(t, agentsDir, "first-agent.yaml", map[string]string{"TOKEN": "first"})
	secondCwd, secondHome := writeNamedAgent(t, agentsDir, "second-agent.yaml", map[string]string{"TOKEN": "second"})
	writeNamedAgent(t, agentsDir, "other-agent.yaml", nil)
	writeRule(t, rulesDir+"/01-first.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "first-agent",
	})
	writeRule(t, rulesDir+"/02-second.yaml", rule{
		Match: map[string]string{"type": "com.example.run", "source": "urn:test"},
		Agent: "second-agent",
	})
	writeRule(t, rulesDir+"/03-other.yaml", rule{
		Match: map[string]string{"type": "com.example.other"},
		Agent: "other-agent",
	})

	release := make(chan struct{})
	defer close(release)
	fake := &fakeRunner{invocations: make(chan invocation, 2), release: release}
	runIDs := []string{"gen_first", "gen_second"}
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    fake,
		logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
		newRunID: func() (string, error) {
			runID := runIDs[0]
			runIDs = runIDs[1:]
			return runID, nil
		},
		secrets: inheritedEnvironment(),
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
		{Rule: "01-first.yaml", Agent: "first-agent", RunID: "gen_first"},
		{Rule: "02-second.yaml", Agent: "second-agent", RunID: "gen_second"},
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
		if document.Agent != acceptedRun.Agent {
			t.Fatalf("%s agent = %q, want %q", acceptedRun.Rule, document.Agent, acceptedRun.Agent)
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
	if got := invocations["01-first.yaml"]; got.Cwd != firstCwd || got.Home != firstHome ||
		got.Instructions != "Test agent instructions." ||
		!reflect.DeepEqual(got.Env, map[string]string{
			"TOKEN":            "first",
			deepSeekAPIKey:     "process-secret",
			homeEnvKey:         firstHome,
			systemPromptEnvKey: "Test agent instructions.",
		}) {
		t.Fatalf("first invocation = %#v", got)
	}
	if got := invocations["02-second.yaml"]; got.Cwd != secondCwd || got.Home != secondHome ||
		!reflect.DeepEqual(got.Env, map[string]string{
			"TOKEN":            "second",
			deepSeekAPIKey:     "process-secret",
			homeEnvKey:         secondHome,
			systemPromptEnvKey: "Test agent instructions.",
		}) {
		t.Fatalf("second invocation = %#v", got)
	}
}

func TestEventServerReloadsAgentsAndRulesEveryRequest(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "reload-agent.yaml", nil)
	rulePath := rulesDir + "/reload.yaml"
	writeRule(t, rulePath, rule{
		Match: map[string]string{"type": "com.example.run", "source": "urn:one"},
		Agent: "reload-agent",
	})

	fake := &fakeRunner{invocations: make(chan invocation, 2)}
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    fake,
		newRunID:  func() (string, error) { return "gen_reload", nil },
		logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
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

	writeRule(t, rulePath, rule{
		Match: map[string]string{"type": "com.example.run", "source": "urn:two"},
		Agent: "reload-agent",
	})
	writeAgent(t, filepath.Join(agentsDir, "reload-agent.yaml"), agentDefinition{
		Instructions: "Reloaded instructions.",
		Cwd:          cwd,
		Home:         home,
	})
	response = sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("reloaded match status = %d, body = %s", response.Code, response.Body.String())
	}
	select {
	case document := <-fake.invocations:
		if document.Rule != "reload.yaml" || document.Agent != "reload-agent" {
			t.Fatalf("invocation identity = rule %q agent %q", document.Rule, document.Agent)
		}
		if document.Instructions != "Reloaded instructions." {
			t.Fatalf("instructions = %q", document.Instructions)
		}
		if document.Env[systemPromptEnvKey] != "Reloaded instructions." {
			t.Fatalf("system prompt env = %#v", document.Env)
		}
		if _, present := document.Env[deepSeekAPIKey]; present {
			t.Fatalf("credential-free invocation inherited a key: %#v", document.Env)
		}
	case <-time.After(time.Second):
		t.Fatal("reloaded rule was not invoked")
	}
}

func TestEventServerRejectsInvalidAgentsAndMissingReferences(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "ok.yaml", nil)
	writeRule(t, rulesDir+"/ok.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "missing-agent",
	})
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    &fakeRunner{invocations: make(chan invocation, 1)},
		logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-invalid",
		"source":      "urn:test",
		"type":        "com.example.run",
	}
	response := sendEvent(server, event)
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "rules are invalid") {
		t.Fatalf("missing agent status = %d, body = %s", response.Code, response.Body.String())
	}

	if err := os.WriteFile(filepath.Join(agentsDir, "ok.yaml"), []byte("user: alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRule(t, rulesDir+"/ok.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "ok",
	})
	response = sendEvent(server, event)
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "agents are invalid") {
		t.Fatalf("invalid agent status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestEventServerRequiresStructuredCloudEvent(t *testing.T) {
	server := &eventServer{agentsDir: t.TempDir(), rulesDir: t.TempDir()}

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

func TestLoadRulesRejectsRunBlockAndRawArguments(t *testing.T) {
	agentsDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "janitor.yaml", nil)
	agents, err := loadAgents(agentsDir)
	if err != nil {
		t.Fatal(err)
	}

	rulesDir := t.TempDir()
	runContents := `
match:
  type: com.example.run
run:
  cwd: /tmp
  env: {}
`
	if err := os.WriteFile(rulesDir+"/invalid.yaml", []byte(runContents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRules(rulesDir, agents); err == nil || !strings.Contains(err.Error(), "run") {
		t.Fatalf("loadRules error = %v, want unknown run field", err)
	}

	argsContents := `
match:
  type: com.example.run
agent: janitor
args: [unsafe]
`
	if err := os.WriteFile(rulesDir+"/invalid.yaml", []byte(argsContents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRules(rulesDir, agents); err == nil || !strings.Contains(err.Error(), "args") {
		t.Fatalf("loadRules error = %v, want unknown args field", err)
	}
}

func TestLoadRulesRejectsMissingAgent(t *testing.T) {
	rulesDir := t.TempDir()
	writeRule(t, rulesDir+"/missing.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "no-such-agent",
	})
	if _, err := loadRules(rulesDir, map[string]agentDefinition{}); err == nil ||
		!strings.Contains(err.Error(), `agent "no-such-agent" is not defined`) {
		t.Fatalf("loadRules error = %v, want missing agent", err)
	}
}

func TestLoadAgentsRejectsUnknownFields(t *testing.T) {
	agentsDir := t.TempDir()
	contents := `
instructions: stay
cwd: /tmp/work
home: /tmp/home
user: alice
`
	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "user") {
		t.Fatalf("loadAgents error = %v, want unknown user field", err)
	}
}

func TestLoadAgentsRejectsReservedEnvDuplicateSecretsAndDuplicateIDs(t *testing.T) {
	agentsDir := t.TempDir()
	reserved := `
instructions: stay
cwd: /tmp/work
home: /tmp/home
env:
  HOME: /tmp/home
`
	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(reserved), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "HOME") {
		t.Fatalf("loadAgents error = %v, want reserved HOME", err)
	}

	prompt := `
instructions: stay
cwd: /tmp/work
home: /tmp/home
env:
  DSH_SYSTEM_PROMPT: injected
`
	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "DSH_SYSTEM_PROMPT") {
		t.Fatalf("loadAgents error = %v, want reserved DSH_SYSTEM_PROMPT", err)
	}

	secretEnv := `
instructions: stay
cwd: /tmp/work
home: /tmp/home
env:
  DEEPSEEK_API_KEY: must-not-live-in-agents
`
	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(secretEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil ||
		!strings.Contains(err.Error(), "DEEPSEEK_API_KEY") {
		t.Fatalf("loadAgents error = %v, want secret-key rejection", err)
	}

	duplicates := `
instructions: stay
cwd: /tmp/work
home: /tmp/home
secrets: [DEEPSEEK_API_KEY, DEEPSEEK_API_KEY]
`
	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(duplicates), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "duplicate secret") {
		t.Fatalf("loadAgents error = %v, want duplicate secret", err)
	}

	valid := `
instructions: stay
cwd: /tmp/work
home: /tmp/home
`
	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentsDir+"/janitor.yml", []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "duplicate agent id") {
		t.Fatalf("loadAgents error = %v, want duplicate id", err)
	}
}

func TestLoadAgentsRejectsInvalidNamesAndIdenticalCwdHome(t *testing.T) {
	if _, err := agentIDFromFilename("workspace-janitor.yaml"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".yaml", "Bad Name.yaml", "-hidden.yaml"} {
		if _, err := agentIDFromFilename(name); err == nil {
			t.Fatalf("agentIDFromFilename(%q) succeeded", name)
		}
	}

	agentsDir := t.TempDir()
	if err := os.WriteFile(agentsDir+"/Bad Name.yaml", []byte(`
instructions: stay
cwd: /tmp/work
home: /tmp/home
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "invalid agent id") {
		t.Fatalf("loadAgents error = %v, want invalid agent id", err)
	}

	if err := os.Remove(filepath.Join(agentsDir, "Bad Name.yaml")); err != nil {
		t.Fatal(err)
	}
	same := `
instructions: stay
cwd: /tmp/work
home: /tmp/work/
`
	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(same), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil ||
		!strings.Contains(err.Error(), "cwd and home must be distinct") {
		t.Fatalf("loadAgents error = %v, want distinct cwd/home", err)
	}
}

func TestRuntimeEnvironmentUsesNamedSecretsAndStaysIsolated(t *testing.T) {
	declared := map[string]string{"PATH": "/agent/bin"}
	definition := agentDefinition{
		Instructions: "Standing instructions.",
		Cwd:          "/tmp/work",
		Home:         "/tmp/home",
		Env:          declared,
		Secrets:      []string{deepSeekAPIKey, "OTHER_SECRET"},
	}
	environment := runtimeEnvironment(definition, map[string]string{
		deepSeekAPIKey: "process-secret",
		"OTHER_SECRET": "other-value",
		"IGNORED":      "must-not-copy",
	})

	if environment[deepSeekAPIKey] != "process-secret" || environment["OTHER_SECRET"] != "other-value" {
		t.Fatalf("secrets = %#v", environment)
	}
	if environment[homeEnvKey] != "/tmp/home" || environment[systemPromptEnvKey] != "Standing instructions." {
		t.Fatalf("derived env = %#v", environment)
	}
	if environment["PATH"] != "/agent/bin" || environment["IGNORED"] != "" {
		t.Fatalf("runtime environment = %#v", environment)
	}
	environment["PATH"] = "/changed"
	if declared["PATH"] != "/agent/bin" {
		t.Fatal("runtime environment mutated the agent")
	}

	credentialFree := runtimeEnvironment(definition, map[string]string{})
	if _, present := credentialFree[deepSeekAPIKey]; present {
		t.Fatalf("empty secrets were injected: %#v", credentialFree)
	}
	if credentialFree[homeEnvKey] != "/tmp/home" {
		t.Fatalf("credential-free HOME = %#v", credentialFree)
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
		Rule:         "test.yaml",
		Agent:        "workspace-janitor",
		RunID:        "gen_test",
		Cwd:          "/workspace",
		Home:         "/home/janitor",
		Instructions: "You are the workspace janitor.",
		Env:          map[string]string{"ONLY": "declared"},
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
		"agent":               "workspace-janitor",
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
	runner.Run(invocation{Rule: "bad.yaml", Agent: "bad-agent", RunID: "gen_bad"})

	records := decodeLogRecords(t, logs.Bytes())
	finished := records[len(records)-1]
	errorMessage, _ := finished["error"].(string)
	if finished["level"] != "ERROR" ||
		finished["error_type"] != "DeepSeekRunError" ||
		finished["agent"] != "bad-agent" ||
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

func writeNamedAgent(t *testing.T, directory, filename string, env map[string]string) (cwd, home string) {
	t.Helper()
	cwd = t.TempDir()
	home = t.TempDir()
	writeAgent(t, filepath.Join(directory, filename), agentDefinition{
		Instructions: "Test agent instructions.",
		Cwd:          cwd,
		Home:         home,
		Env:          env,
	})
	return cwd, home
}

func writeAgent(t *testing.T, path string, value agentDefinition) {
	t.Helper()
	contents, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
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
