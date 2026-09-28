package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueuedRunOmitsSecretsAndRestarts(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := newRunStore(dataDir, newEventBus(), discardLogger())
	document := sampleRunInvocation("gen_wait")
	document.Env = map[string]string{
		"ONLY":             "declared",
		deepSeekAPIKey:     "sk-queued-secret",
		homeEnvKey:         document.Home,
		systemPromptEnvKey: document.Instructions,
	}
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeQueuedRun(document.RunDir, document, []string{deepSeekAPIKey}); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(queuedRunPath(document.RunDir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "sk-queued-secret") {
		t.Fatalf("queue file kept the secret: %s", payload)
	}
	store.release(document.RunID)

	restarted := newRunStore(dataDir, newEventBus(), discardLogger())
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	events := readRunEvents(t, restarted, document.RunID)
	if hasType(events, lifecycleTypeEnd) || hasType(events, lifecycleTypeStart) {
		t.Fatalf("queued run was closed or started: %v", typesOf(events))
	}

	agentsDir := t.TempDir()
	writeAgent(t, filepath.Join(agentsDir, "workspace-janitor.yaml"), agentDefinition{
		Instructions: document.Instructions,
		Cwd:          document.Cwd,
		Home:         document.Home,
	})
	agents, err := loadAgents(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	fake := &fakeRunner{invocations: make(chan invocation, 1), release: release}
	server := &eventServer{
		runner:     fake,
		store:      restarted,
		secrets:    map[string]string{deepSeekAPIKey: "sk-queued-secret"},
		generation: &generation{agents: agents},
	}
	if err := server.restoreQueuedRuns(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fake.invocations:
		if got.RunID != "gen_wait" || got.Env[deepSeekAPIKey] != "sk-queued-secret" || got.Env["ONLY"] != "declared" {
			t.Fatalf("restored = %#v", got.Env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued run did not start")
	}
	if _, err := os.Lstat(queuedRunPath(document.RunDir)); !os.IsNotExist(err) {
		t.Fatalf("queue file after start: %v", err)
	}
}

func TestRestartStartsDifferentAgentsAndQueuesTheSameOne(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := newRunStore(dataDir, newEventBus(), discardLogger())
	clock := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	agentsDir := t.TempDir()
	for _, id := range []string{"alpha", "alpha-again", "beta"} {
		agent := id
		if id == "alpha-again" {
			agent = "alpha"
		}
		document := sampleRunInvocation("gen_" + strings.ReplaceAll(id, "-", "_"))
		document.Agent = agent
		document.RunID = "gen_" + strings.ReplaceAll(id, "-", "_")
		if err := store.Accept(&document, nil); err != nil {
			t.Fatal(err)
		}
		if err := writeQueuedRun(document.RunDir, document, nil); err != nil {
			t.Fatal(err)
		}
		store.release(document.RunID)
		writeAgent(t, filepath.Join(agentsDir, agent+".yaml"), agentDefinition{
			Instructions: document.Instructions,
			Cwd:          document.Cwd,
			Home:         document.Home,
		})
	}
	restarted := newRunStore(dataDir, newEventBus(), discardLogger())
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	agents, err := loadAgents(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	fake := &fakeRunner{invocations: make(chan invocation, 3), release: release}
	server := &eventServer{
		runner:     fake,
		store:      restarted,
		generation: &generation{agents: agents},
	}
	if err := server.restoreQueuedRuns(); err != nil {
		t.Fatal(err)
	}
	started := map[string]int{}
	for range 2 {
		select {
		case got := <-fake.invocations:
			started[got.Agent]++
		case <-time.After(2 * time.Second):
			t.Fatalf("started %#v", started)
		}
	}
	if started["alpha"] != 1 || started["beta"] != 1 {
		t.Fatalf("started %#v", started)
	}
	select {
	case got := <-fake.invocations:
		t.Fatalf("second alpha run started early: %s", got.RunID)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case got := <-fake.invocations:
		if got.Agent != "alpha" {
			t.Fatalf("released agent = %s", got.Agent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second alpha run did not start")
	}
}
