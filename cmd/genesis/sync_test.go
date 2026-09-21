package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type blockingCoordinator struct {
	started chan struct{}
	release chan struct{}
	result  coordinateResult
	err     error
}

func (b *blockingCoordinator) Coordinate(ctx context.Context, plan privilegedPlan) (coordinateResult, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return coordinateResult{}, ctx.Err()
	}
	return b.result, b.err
}

func TestSyncRequiresBearerToken(t *testing.T) {
	server := newSyncTestServer(t, nil)
	response := sendSync(server, "", nil)
	if response.Code != http.StatusUnauthorized ||
		!strings.Contains(response.Body.String(), "unauthorized") {
		t.Fatalf("missing token status = %d body = %s", response.Code, response.Body.String())
	}
	response = sendSync(server, "wrong", nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", response.Code)
	}
	response = sendSync(server, "sync-secret", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestSyncDisabledWhenTokenEmpty(t *testing.T) {
	server := newSyncTestServer(t, nil)
	server.syncToken = ""
	response := sendSync(server, "anything", nil)
	if response.Code != http.StatusUnauthorized ||
		!strings.Contains(response.Body.String(), "sync is disabled") {
		t.Fatalf("disabled sync status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestSyncOptionalScopeLeavesUnspecifiedLayerCached(t *testing.T) {
	server := newSyncTestServer(t, nil)
	writeRule(t, filepath.Join(server.rulesDir, "ok.yaml"), rule{
		Match: map[string]string{"type": "com.example.updated"},
		Agent: "ok",
	})
	writeNamedAgent(t, server.agentsDir, "extra.yaml", nil)

	response := sendSync(server, "sync-secret", map[string]any{"scope": []string{"rules"}})
	if response.Code != http.StatusOK {
		t.Fatalf("rules-only sync status = %d, body = %s", response.Code, response.Body.String())
	}
	var body syncResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Agents != 1 || body.Rules != 1 {
		t.Fatalf("rules-only counts = agents %d rules %d", body.Agents, body.Rules)
	}
	if body.Privileged.Attached {
		t.Fatal("standalone listener unexpectedly attached privileged ipc")
	}
	if len(body.Privileged.Unsupported) != 0 {
		t.Fatalf("rules-only unsupported = %#v", body.Privileged.Unsupported)
	}

	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-scope",
		"source":      "urn:test",
		"type":        "com.example.updated",
	}
	response = sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("rules-only event status = %d, body = %s", response.Code, response.Body.String())
	}

	writeRule(t, filepath.Join(server.rulesDir, "extra.yaml"), rule{
		Match: map[string]string{"type": "com.example.extra"},
		Agent: "extra",
	})
	response = sendSync(server, "sync-secret", map[string]any{"scope": []string{"rules"}})
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "rules are invalid") {
		t.Fatalf("rules-only unknown agent status = %d, body = %s", response.Code, response.Body.String())
	}

	response = sendSync(server, "sync-secret", map[string]any{"scope": []string{"agents"}})
	if response.Code != http.StatusOK {
		t.Fatalf("agents-only sync status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Agents != 2 || body.Rules != 1 {
		t.Fatalf("agents-only counts = agents %d rules %d", body.Agents, body.Rules)
	}
	if body.Privileged.HostMutation != hostMutationNone {
		t.Fatalf("host mutation = %q", body.Privileged.HostMutation)
	}
	if len(body.Privileged.Unsupported) == 0 {
		t.Fatal("expected unsupported privileged diffs for agents")
	}
	response = sendSync(server, "sync-secret", map[string]any{"scope": []string{"rules"}})
	if response.Code != http.StatusOK {
		t.Fatalf("rules sync after agents status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestSyncRejectsUnknownScope(t *testing.T) {
	server := newSyncTestServer(t, nil)
	response := sendSync(server, "sync-secret", map[string]any{"scope": []string{"packages"}})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown scope status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestEventsReturn503DuringSync(t *testing.T) {
	coordinator := &blockingCoordinator{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		result: coordinateResult{
			HostMutation: hostMutationNone,
			Applied:      []string{},
			Unsupported:  []unsupportedChange{},
		},
	}
	defer close(coordinator.release)
	server := newSyncTestServer(t, coordinator)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- sendSync(server, "sync-secret", nil)
	}()
	select {
	case <-coordinator.started:
	case <-time.After(time.Second):
		t.Fatal("coordinator was not called")
	}

	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-busy",
		"source":      "urn:test",
		"type":        "com.example.run",
	}
	response := sendEvent(server, event)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy event status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") != syncRetryAfter {
		t.Fatalf("Retry-After = %q", response.Header().Get("Retry-After"))
	}

	conflict := sendSync(server, "sync-secret", nil)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("concurrent sync status = %d, body = %s", conflict.Code, conflict.Body.String())
	}
	coordinator.release <- struct{}{}
	select {
	case response := <-done:
		if response.Code != http.StatusOK {
			t.Fatalf("blocking sync status = %d, body = %s", response.Code, response.Body.String())
		}
		if !json.Valid(response.Body.Bytes()) {
			t.Fatalf("sync body = %s", response.Body.String())
		}
		var body syncResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.Privileged.Attached {
			t.Fatal("expected attached privileged coordinator")
		}
	case <-time.After(time.Second):
		t.Fatal("blocking sync did not finish")
	}
}

func TestSyncDoesNotSwapWhenCoordinatorFails(t *testing.T) {
	coordinator := &blockingCoordinator{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		err:     io.ErrUnexpectedEOF,
	}
	server := newSyncTestServer(t, coordinator)
	writeRule(t, filepath.Join(server.rulesDir, "ok.yaml"), rule{
		Match: map[string]string{"type": "com.example.updated"},
		Agent: "ok",
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- sendSync(server, "sync-secret", nil) }()
	<-coordinator.started
	close(coordinator.release)
	response := <-done
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "privileged coordination failed") {
		t.Fatalf("failed coordinate status = %d, body = %s", response.Code, response.Body.String())
	}
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-stale",
		"source":      "urn:test",
		"type":        "com.example.updated",
	}
	response = sendEvent(server, event)
	if response.Code != http.StatusNoContent {
		t.Fatalf("cache after ipc failure status = %d, body = %s", response.Code, response.Body.String())
	}
}

func newSyncTestServer(t *testing.T, coordinator privilegedCoordinator) *eventServer {
	t.Helper()
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "ok.yaml", map[string]string{"PATH": "/usr/bin"})
	writeRule(t, rulesDir+"/ok.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "ok",
	})
	server := &eventServer{
		agentsDir:   agentsDir,
		rulesDir:    rulesDir,
		runner:      &fakeRunner{invocations: make(chan invocation, 4)},
		newRunID:    func() (string, error) { return "gen_sync", nil },
		logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		syncToken:   "sync-secret",
		coordinator: coordinator,
		store:       testStore(t),
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	return server
}

func TestSanitizedChildEnvOmitsSyncSecrets(t *testing.T) {
	t.Setenv(syncTokenEnv, "must-not-leak")
	t.Setenv(privilegedFDEnv, "3")
	t.Setenv("GENESIS_CAPTURE", "/tmp/keep")
	foundCapture := false
	for _, item := range sanitizedChildEnv() {
		key, _, _ := strings.Cut(item, "=")
		if key == syncTokenEnv || key == privilegedFDEnv {
			t.Fatalf("leaked %s", key)
		}
		if key == "GENESIS_CAPTURE" {
			foundCapture = true
		}
	}
	if !foundCapture {
		t.Fatal("GENESIS_CAPTURE was stripped")
	}
}
