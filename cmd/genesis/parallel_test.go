package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaxParallelMustBeAtLeastOne(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{
		"instructions: hello\ncwd: /tmp/cwd-a\nhome: /tmp/home-a\nmax_parallel: 0\n",
		"instructions: hello\ncwd: /tmp/cwd-b\nhome: /tmp/home-b\nmax_parallel: -3\n",
	} {
		path := filepath.Join(dir, "worker.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadAgents(dir)
		if err == nil || !strings.Contains(err.Error(), "max_parallel must be at least 1") {
			t.Fatalf("loadAgents(%q) error = %v", body, err)
		}
	}

	limit := 2
	writeAgent(t, filepath.Join(dir, "worker.yaml"), agentDefinition{
		Instructions: "hello",
		Cwd:          "/tmp/cwd-c",
		Home:         "/tmp/home-c",
		MaxParallel:  &limit,
	})
	agents, err := loadAgents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if agents["worker"].parallelLimit() != 2 {
		t.Fatalf("limit = %d", agents["worker"].parallelLimit())
	}
	writeAgent(t, filepath.Join(dir, "plain.yaml"), agentDefinition{
		Instructions: "hello",
		Cwd:          "/tmp/cwd-d",
		Home:         "/tmp/home-d",
	})
	plain, err := loadAgents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if plain["plain"].parallelLimit() != 1 || plain["plain"].MaxParallel != nil {
		t.Fatalf("default = limit %d stored %#v", plain["plain"].parallelLimit(), plain["plain"].MaxParallel)
	}
}

func TestReasoningEffortAllowlist(t *testing.T) {
	dir := t.TempDir()
	writeAgent(t, filepath.Join(dir, "worker.yaml"), agentDefinition{
		Instructions:    "hello",
		Cwd:             "/tmp/cwd",
		Home:            "/tmp/home",
		ReasoningEffort: "low",
	})
	agents, err := loadAgents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if agents["worker"].ReasoningEffort != "low" {
		t.Fatalf("effort = %q", agents["worker"].ReasoningEffort)
	}
	writeAgent(t, filepath.Join(dir, "worker.yaml"), agentDefinition{
		Instructions:    "hello",
		Cwd:             "/tmp/cwd",
		Home:            "/tmp/home",
		ReasoningEffort: "turbo",
	})
	if _, err := loadAgents(dir); err == nil || !strings.Contains(err.Error(), "reasoning_effort must be off, low, high, or max") {
		t.Fatalf("loadAgents error = %v", err)
	}
}

func TestMaxParallelHoldsTheNextRunOfThatAgent(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	limit := 1
	writeAgent(t, filepath.Join(agentsDir, "worker.yaml"), agentDefinition{
		Instructions: "Test agent instructions.",
		Cwd:          t.TempDir(),
		Home:         t.TempDir(),
		MaxParallel:  &limit,
	})
	writeRule(t, filepath.Join(rulesDir, "worker.yaml"), rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "worker",
	})
	release := make(chan struct{})
	fake := &fakeRunner{invocations: make(chan invocation, 2), release: release}
	runIDs := []string{"gen_first", "gen_second"}
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    fake,
		store:     testStore(t),
		newRunID: func() (string, error) {
			id := runIDs[0]
			runIDs = runIDs[1:]
			return id, nil
		},
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	first := map[string]any{
		"specversion": "1.0", "id": "evt-1", "source": "urn:test", "type": "com.example.run",
	}
	if response := sendEvent(server, first); response.Code != 202 {
		t.Fatalf("first status = %d body = %s", response.Code, response.Body.String())
	}
	select {
	case got := <-fake.invocations:
		if got.RunID != "gen_first" {
			t.Fatalf("first run = %s", got.RunID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not start")
	}
	second := map[string]any{
		"specversion": "1.0", "id": "evt-2", "source": "urn:test", "type": "com.example.run",
	}
	if response := sendEvent(server, second); response.Code != 202 || !strings.Contains(response.Body.String(), "gen_second") {
		t.Fatalf("second status = %d body = %s", response.Code, response.Body.String())
	}
	select {
	case got := <-fake.invocations:
		t.Fatalf("second run started while the slot was full: %s", got.RunID)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case got := <-fake.invocations:
		if got.RunID != "gen_second" {
			t.Fatalf("released run = %s", got.RunID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued run did not start")
	}
}

func TestParallelGateLetsOtherAgentsStart(t *testing.T) {
	var gate parallelGate
	started := make(chan string, 4)
	release := make(chan struct{})
	run := func(name string) func() {
		return func() {
			started <- name
			<-release
		}
	}
	gate.start("worker", 1, run("worker-1"))
	gate.start("other", 1, run("other-1"))
	got := map[string]bool{}
	for range 2 {
		select {
		case name := <-started:
			got[name] = true
		case <-time.After(2 * time.Second):
			t.Fatal("cross-agent runs did not both start")
		}
	}
	if !got["worker-1"] || !got["other-1"] {
		t.Fatalf("started = %#v", got)
	}
	gate.start("worker", 1, run("worker-2"))
	select {
	case name := <-started:
		t.Fatalf("%s started while worker was already running", name)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case name := <-started:
		if name != "worker-2" {
			t.Fatalf("released run = %s", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued run of the same agent did not start")
	}
}

func TestDefaultParallelStillStartsDifferentAgents(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "alpha.yaml", nil)
	writeNamedAgent(t, agentsDir, "beta.yaml", nil)
	writeRule(t, filepath.Join(rulesDir, "alpha.yaml"), rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "alpha",
	})
	writeRule(t, filepath.Join(rulesDir, "beta.yaml"), rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "beta",
	})
	release := make(chan struct{})
	fake := &fakeRunner{invocations: make(chan invocation, 2), release: release}
	defer close(release)
	runIDs := []string{"gen_alpha", "gen_beta"}
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    fake,
		store:     testStore(t),
		newRunID: func() (string, error) {
			id := runIDs[0]
			runIDs = runIDs[1:]
			return id, nil
		},
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"specversion": "1.0", "id": "evt-both", "source": "urn:test", "type": "com.example.run",
	}
	if response := sendEvent(server, event); response.Code != 202 {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case got := <-fake.invocations:
			seen[got.Agent] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("started %#v, want both agents", seen)
		}
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Fatalf("started %#v", seen)
	}
}
