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
	"strconv"
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

func testStore(t *testing.T) *runStore {
	t.Helper()
	return newRunStore(t.TempDir(), newEventBus(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
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
		store:     testStore(t),
		newRunID: func() (string, error) {
			runID := runIDs[0]
			runIDs = runIDs[1:]
			return runID, nil
		},
		secrets: inheritedEnvironment(),
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
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

func TestEventServerIgnoresFilesystemEditsUntilSync(t *testing.T) {
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
		syncToken: "sync-secret",
		store:     testStore(t),
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
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
	if response.Code != http.StatusNoContent {
		t.Fatalf("unsynced edit status = %d, body = %s", response.Code, response.Body.String())
	}

	response = sendSync(server, "sync-secret", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("sync status = %d, body = %s", response.Code, response.Body.String())
	}
	response = sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("synced match status = %d, body = %s", response.Code, response.Body.String())
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
		t.Fatal("synced rule was not invoked")
	}
}

func TestEventServerKeepsCacheWhenDiskBecomesInvalid(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "ok.yaml", nil)
	writeRule(t, rulesDir+"/ok.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "ok",
	})
	fake := &fakeRunner{invocations: make(chan invocation, 2)}
	n := 0
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    fake,
		newRunID: func() (string, error) {
			n++
			return "gen_ok_" + strconv.Itoa(n), nil
		},
		logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
		syncToken: "sync-secret",
		store:     testStore(t),
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-invalid",
		"source":      "urn:test",
		"type":        "com.example.run",
	}
	response := sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("valid cache status = %d, body = %s", response.Code, response.Body.String())
	}
	<-fake.invocations

	writeRule(t, rulesDir+"/ok.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "missing-agent",
	})
	response = sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("stale valid cache status = %d, body = %s", response.Code, response.Body.String())
	}
	response = sendSync(server, "sync-secret", nil)
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "rules are invalid") {
		t.Fatalf("invalid rules sync status = %d, body = %s", response.Code, response.Body.String())
	}
	response = sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("cache after failed sync status = %d, body = %s", response.Code, response.Body.String())
	}

	if err := os.WriteFile(filepath.Join(agentsDir, "ok.yaml"), []byte("user: alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRule(t, rulesDir+"/ok.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "ok",
	})
	response = sendSync(server, "sync-secret", nil)
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "agents are invalid") {
		t.Fatalf("invalid agent sync status = %d, body = %s", response.Code, response.Body.String())
	}
	response = sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("cache after invalid agent sync status = %d, body = %s", response.Code, response.Body.String())
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

func TestRuleMatchesSubjectGlob(t *testing.T) {
	stringEvent := func(t *testing.T, attributes map[string]string) cloudEvent {
		t.Helper()
		event := make(cloudEvent, len(attributes))
		for key, value := range attributes {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			event[key] = raw
		}
		return event
	}

	tests := []struct {
		name    string
		match   map[string]string
		event   cloudEvent
		matches bool
	}{
		{
			name:    "worker star matches one child segment",
			match:   map[string]string{"subject": "worker/*"},
			event:   stringEvent(t, map[string]string{"subject": "worker/run-1"}),
			matches: true,
		},
		{
			name:    "worker star does not match an extra segment",
			match:   map[string]string{"subject": "worker/*"},
			event:   stringEvent(t, map[string]string{"subject": "worker/run-1/extra"}),
			matches: false,
		},
		{
			name:    "worker star does not match the parent subject",
			match:   map[string]string{"subject": "worker/*"},
			event:   stringEvent(t, map[string]string{"subject": "worker"}),
			matches: false,
		},
		{
			name:    "worker star does not match an empty segment",
			match:   map[string]string{"subject": "worker/*"},
			event:   stringEvent(t, map[string]string{"subject": "worker/"}),
			matches: false,
		},
		{
			name:    "single star matches a one-segment subject",
			match:   map[string]string{"subject": "*"},
			event:   stringEvent(t, map[string]string{"subject": "oracle"}),
			matches: true,
		},
		{
			name:    "single star does not match two segments",
			match:   map[string]string{"subject": "*"},
			event:   stringEvent(t, map[string]string{"subject": "oracle/1"}),
			matches: false,
		},
		{
			name:    "single star does not match an empty subject",
			match:   map[string]string{"subject": "*"},
			event:   stringEvent(t, map[string]string{"subject": ""}),
			matches: false,
		},
		{
			name:    "subject without a star is exact",
			match:   map[string]string{"subject": "dev.genesis.run"},
			event:   stringEvent(t, map[string]string{"subject": "dev.genesis.run"}),
			matches: true,
		},
		{
			name:    "subject without a star does not match a prefix",
			match:   map[string]string{"subject": "dev.genesis.run"},
			event:   stringEvent(t, map[string]string{"subject": "dev.genesis.run/extra"}),
			matches: false,
		},
		{
			name:    "slashed subject without a star is exact",
			match:   map[string]string{"subject": "worker/run-1"},
			event:   stringEvent(t, map[string]string{"subject": "worker/run-1"}),
			matches: true,
		},
		{
			name:    "slashed subject without a star does not match a sibling",
			match:   map[string]string{"subject": "worker/run-1"},
			event:   stringEvent(t, map[string]string{"subject": "worker/run-2"}),
			matches: false,
		},
		{
			name:  "exact type and glob subject both required",
			match: map[string]string{"type": "dev.genesis.run", "subject": "worker/*"},
			event: stringEvent(t, map[string]string{
				"type":    "dev.genesis.run",
				"subject": "worker/run-1",
			}),
			matches: true,
		},
		{
			name:  "type mismatch fails even when the subject glob matches",
			match: map[string]string{"type": "dev.genesis.run", "subject": "worker/*"},
			event: stringEvent(t, map[string]string{
				"type":    "dev.genesis.other",
				"subject": "worker/run-1",
			}),
			matches: false,
		},
		{
			name:    "star in type stays exact",
			match:   map[string]string{"type": "*"},
			event:   stringEvent(t, map[string]string{"type": "oracle"}),
			matches: false,
		},
		{
			name:    "literal star type matches only a star",
			match:   map[string]string{"type": "*"},
			event:   stringEvent(t, map[string]string{"type": "*"}),
			matches: true,
		},
		{
			name:    "star in source stays exact",
			match:   map[string]string{"source": "urn:genesis:*"},
			event:   stringEvent(t, map[string]string{"source": "urn:genesis:example"}),
			matches: false,
		},
		{
			name:    "double star is not a multi-segment glob",
			match:   map[string]string{"subject": "**"},
			event:   stringEvent(t, map[string]string{"subject": "oracle/1"}),
			matches: false,
		},
		{
			name:    "double star matches only the literal subject",
			match:   map[string]string{"subject": "**"},
			event:   stringEvent(t, map[string]string{"subject": "**"}),
			matches: true,
		},
		{
			name:    "question mark is not a wildcard",
			match:   map[string]string{"subject": "?"},
			event:   stringEvent(t, map[string]string{"subject": "a"}),
			matches: false,
		},
		{
			name:    "character class is not a wildcard",
			match:   map[string]string{"subject": "[ab]"},
			event:   stringEvent(t, map[string]string{"subject": "a"}),
			matches: false,
		},
		{
			name:    "literal segment is case-sensitive",
			match:   map[string]string{"subject": "Worker/*"},
			event:   stringEvent(t, map[string]string{"subject": "worker/run-1"}),
			matches: false,
		},
		{
			name:    "two single-segment stars match two segments",
			match:   map[string]string{"subject": "*/*"},
			event:   stringEvent(t, map[string]string{"subject": "oracle/1"}),
			matches: true,
		},
		{
			name:    "two single-segment stars do not match one segment",
			match:   map[string]string{"subject": "*/*"},
			event:   stringEvent(t, map[string]string{"subject": "oracle"}),
			matches: false,
		},
		{
			name:    "star does not match an empty middle segment",
			match:   map[string]string{"subject": "worker/*/*"},
			event:   stringEvent(t, map[string]string{"subject": "worker//extra"}),
			matches: false,
		},
		{
			name:    "empty subject pattern is exact and not match-all",
			match:   map[string]string{"subject": ""},
			event:   stringEvent(t, map[string]string{"subject": "oracle"}),
			matches: false,
		},
		{
			name:    "empty subject pattern matches an empty subject",
			match:   map[string]string{"subject": ""},
			event:   stringEvent(t, map[string]string{"subject": ""}),
			matches: true,
		},
		{
			name:    "missing subject does not match",
			match:   map[string]string{"subject": "worker/*"},
			event:   stringEvent(t, map[string]string{"type": "dev.genesis.run"}),
			matches: false,
		},
		{
			name:  "non-string subject does not match",
			match: map[string]string{"subject": "worker/*"},
			event: cloudEvent{
				"subject": json.RawMessage(`{"id":"worker/run-1"}`),
			},
			matches: false,
		},
		{
			name:  "nested data subject is not matched",
			match: map[string]string{"subject": "worker/*"},
			event: cloudEvent{
				"data": json.RawMessage(`{"subject":"worker/run-1"}`),
			},
			matches: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := (rule{Match: test.match}).matches(test.event)
			if got != test.matches {
				t.Fatalf("matches = %v, want %v", got, test.matches)
			}
		})
	}

	agents := map[string]agentDefinition{"worker": {}}
	globRule := rule{
		Match: map[string]string{"type": "dev.genesis.run", "subject": "worker/*"},
		Agent: "worker",
	}
	if err := globRule.validate(agents); err != nil {
		t.Fatalf("subject glob validate: %v", err)
	}
	emptySubject := rule{Match: map[string]string{"subject": ""}, Agent: "worker"}
	if err := emptySubject.validate(agents); err != nil {
		t.Fatalf("empty subject validate: %v, want acceptance because empty match values are allowed", err)
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
packages: [nmap]
`
	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "packages") {
		t.Fatalf("loadAgents error = %v, want unknown packages field", err)
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

func TestLoadAgentsDedicatedUserContract(t *testing.T) {
	agentsDir := t.TempDir()
	valid := `
instructions: stay
cwd: /home/workspace-janitor/workspace
home: /home/workspace-janitor
user: workspace-janitor
setup:
  workspace: private
`
	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	agents, err := loadAgents(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if agents["janitor"].User != "workspace-janitor" || agents["janitor"].setupContract().workspaceMode() != workspacePrivate {
		t.Fatalf("agent = %#v", agents["janitor"])
	}

	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(valid+"\n  command: id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("setup.command error = %v", err)
	}

	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(`
instructions: stay
cwd: /tmp/work
home: /tmp/home
setup:
  workspace: private
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "setup requires user") {
		t.Fatalf("setup without user error = %v", err)
	}

	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(`
instructions: stay
cwd: /tmp/work
home: /tmp/home
user: genesis
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved user error = %v", err)
	}

	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentsDir+"/other.yaml", []byte(`
instructions: stay
cwd: /home/workspace-janitor/other
home: /home/workspace-janitor
user: workspace-janitor
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "share OS user") {
		t.Fatalf("duplicate user error = %v", err)
	}

	if err := os.WriteFile(agentsDir+"/janitor.yaml", []byte(`
instructions: stay
cwd: /tmp/work
home: /tmp/home
user: workspace-janitor
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(agentsDir, "other.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(agentsDir); err == nil || !strings.Contains(err.Error(), "home must be") {
		t.Fatalf("dedicated home path error = %v", err)
	}
}

func TestLoadInitialGenerationRejectsDedicatedUserWithoutCoordinator(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeAgent(t, filepath.Join(agentsDir, "janitor.yaml"), agentDefinition{
		Instructions: "stay",
		Cwd:          "/home/workspace-janitor/workspace",
		Home:         "/home/workspace-janitor",
		User:         "workspace-janitor",
	})
	writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "janitor",
	})
	server := &eventServer{agentsDir: agentsDir, rulesDir: rulesDir, logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	if err := server.loadInitialGeneration(); err == nil || !strings.Contains(err.Error(), "requires root genesis launch") {
		t.Fatalf("error = %v", err)
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
	if _, present := environment["USER"]; present {
		t.Fatalf("shared-uid USER leaked: %#v", environment)
	}

	dedicated := definition
	dedicated.User = "workspace-janitor"
	withUser := runtimeEnvironment(dedicated, map[string]string{})
	if withUser["USER"] != "workspace-janitor" || withUser["LOGNAME"] != "workspace-janitor" || withUser["SHELL"] != agentShell {
		t.Fatalf("dedicated identity env = %#v", withUser)
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
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_test","session_id":"dsh-session-1"}'
printf '%s\n' '{"v":1,"type":"notification","method":"session.event","payload":{"sessionId":"dsh-session-1","event":{"type":"turn/start","seq":1,"data":{}}}}'
printf '%s\n' '{"v":1,"type":"notification","method":"session.event","payload":{"sessionId":"dsh-session-1","event":{"type":"tool/call","seq":2,"data":{"name":"bash","id":"call-1"}}}}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"dsh-session-1","finish_reason":"completed","final_response":"done","error":null,"diagnostics":null}'
`)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	runner := processRunner{
		pythonPath: fakePython,
		source:     "embedded runner source",
		logger:     logger,
		store:      store,
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
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
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

	events := readRunEvents(t, store, document.RunID)
	assertEventTypes(t, events, []string{
		lifecycleTypeAccepted,
		lifecycleTypeStart,
		lifecycleTypeSessionCreated,
		lifecycleTypeTurn,
		lifecycleTypeTool,
		lifecycleTypeResult,
		lifecycleTypeEnd,
	})
	if _, err := os.Stat(filepath.Join(document.DshHome)); err != nil {
		t.Fatalf("dsh_home was not retained: %v", err)
	}
}

func TestProcessRunnerStructuredLogsErrors(t *testing.T) {
	fakePython := writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_bad","session_id":"dsh-error"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"dsh-error","finish_reason":"error","final_response":"","error":null,"diagnostics":{"turn_end":{"type":"turn/end","data":{"reason":{"kind":"error","message":"upstream unavailable"}}},"events":[{"type":"agent/error","data":{"message":"provider request failed"}},{"type":"turn/end","data":{"reason":{"kind":"error","message":"upstream unavailable"}}}],"notifications":[]}}'
# Close stdout before writing stderr so Go observes stdout EOF while the
# child is still producing diagnostics. Wait() must not run until that
# pipe has been drained.
exec 1>&-
printf '%s\n' 'runner stderr' >&2
`)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	runner := processRunner{
		pythonPath: fakePython,
		source:     "embedded runner source",
		logger:     logger,
		store:      store,
	}
	document := invocation{Rule: "bad.yaml", Agent: "bad-agent", RunID: "gen_bad"}
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	runner.Run(document)

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
	loggedStderr, err := os.ReadFile(filepath.Join(document.RunDir, stderrFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(loggedStderr)) != "runner stderr" {
		t.Fatalf("stderr.log = %q", loggedStderr)
	}
}

func readRunEvents(t *testing.T, store *runStore, runID string) []lifecycleEvent {
	t.Helper()
	events, err := readJournalPrefix(filepath.Join(store.dataDir, runsDirName, runID, eventsFileName))
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func assertEventTypes(t *testing.T, events []lifecycleEvent, want []string) {
	t.Helper()
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, event.Type)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event types = %#v, want %#v", got, want)
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

func sendSync(server *eventServer, token string, body any) *httptest.ResponseRecorder {
	var reader io.Reader = http.NoBody
	request := httptest.NewRequest(http.MethodPost, "/sync", reader)
	if body != nil {
		contents, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		request = httptest.NewRequest(http.MethodPost, "/sync", bytes.NewReader(contents))
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
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
