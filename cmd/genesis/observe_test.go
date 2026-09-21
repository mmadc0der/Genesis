package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestObserveHealthAndGeneration(t *testing.T) {
	const token = "token-should-not-leak"
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "workspace-janitor.yaml", map[string]string{"LANG": "C"})
	writeRule(t, rulesDir+"/example.yaml", rule{
		Match: map[string]string{"type": "dev.genesis.run", "source": "urn:genesis:example"},
		Agent: "workspace-janitor",
	})
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
		syncToken: token,
		store:     testStore(t),
		runner:    &fakeRunner{invocations: make(chan invocation, 1)},
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}

	health := httptest.NewRecorder()
	server.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"ok":true`) {
		t.Fatalf("health = %d %s", health.Code, health.Body.String())
	}
	if strings.Contains(health.Body.String(), token) {
		t.Fatalf("health exposed sync token: %s", health.Body.String())
	}

	postHealth := httptest.NewRecorder()
	server.ServeHTTP(postHealth, httptest.NewRequest(http.MethodPost, "/health", nil))
	if postHealth.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /health = %d", postHealth.Code)
	}

	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/generation", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("generation = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), token) {
		t.Fatalf("generation exposed sync token: %s", response.Body.String())
	}
	var view generationView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.SyncConfigured == nil || !*view.SyncConfigured || view.Syncing == nil || *view.Syncing || view.Digest == "" || len(view.Agents) != 1 || len(view.Rules) != 1 {
		t.Fatalf("generation view = %#v", view)
	}
	if view.Agents[0].ID != "workspace-janitor" || view.Agents[0].Env["LANG"] != "C" {
		t.Fatalf("agent view = %#v", view.Agents[0])
	}
	if view.Rules[0].Name != "example.yaml" || view.Rules[0].Agent != "workspace-janitor" {
		t.Fatalf("rule view = %#v", view.Rules[0])
	}
	if _, ok := view.Agents[0].Env["DEEPSEEK_API_KEY"]; ok {
		t.Fatal("generation included a secret value slot in env")
	}

	server.mu.Lock()
	server.syncing = true
	server.mu.Unlock()
	syncing := httptest.NewRecorder()
	server.ServeHTTP(syncing, httptest.NewRequest(http.MethodGet, "/generation", nil))
	var during generationView
	if err := json.Unmarshal(syncing.Body.Bytes(), &during); err != nil {
		t.Fatal(err)
	}
	if during.Syncing == nil || !*during.Syncing || during.Digest != view.Digest {
		t.Fatalf("syncing generation = %#v", during)
	}
	server.mu.Lock()
	server.syncing = false
	server.mu.Unlock()

	miss := sendEvent(server, map[string]any{
		"specversion": "1.0",
		"id":          "observe-1",
		"source":      "urn:genesis:other",
		"type":        "dev.genesis.other",
	})
	if miss.Code != http.StatusNoContent {
		t.Fatalf("unmatched event = %d", miss.Code)
	}
}

func TestObserveGenerationRequiresCache(t *testing.T) {
	server := &eventServer{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/generation", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("empty generation = %d", response.Code)
	}
}
