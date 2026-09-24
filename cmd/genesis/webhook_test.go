package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubWebhookIngress(t *testing.T) {
	const secret = "whsec-local-test-secret-value"
	const planted = "ghs_plantedtokenvalue"
	secrets := openTestSecretStore(t)
	if err := os.WriteFile(filepath.Join(secrets.root, webhookSecretName), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("bad signature", func(t *testing.T) {
		fixture := newWebhookFixture(t, secrets)
		body := githubIssueDelivery(4242, "opened", secret, planted)
		response := postWebhook(fixture.server, githubEventIssues, "33333333-3333-3333-3333-333333333333", body, "sha256="+strings.Repeat("ab", 32))
		if response.Code != http.StatusUnauthorized || response.Body.String() != "signature rejected\n" {
			t.Fatalf("status = %d body = %q", response.Code, response.Body.String())
		}
		malformed := postWebhook(fixture.server, githubEventIssues, "33333333-3333-3333-3333-333333333333", body, "sha256=nope")
		if malformed.Code != http.StatusUnauthorized || malformed.Body.String() != "signature rejected\n" {
			t.Fatalf("malformed status = %d body = %q", malformed.Code, malformed.Body.String())
		}
		if fixture.runs != 0 {
			t.Fatalf("runs = %d", fixture.runs)
		}
		assertNoDeliveries(t, fixture.store.dataDir)
		assertNoSecret(t, fixture.logs.String()+response.Body.String()+malformed.Body.String(), secret, planted)
	})

	t.Run("replayed delivery", func(t *testing.T) {
		fixture := newWebhookFixture(t, secrets)
		body := githubIssueDelivery(4242, "opened", secret, planted)
		delivery := "44444444-4444-4444-4444-444444444444"
		signature := githubTestSignature(secret, body)
		first := postWebhook(fixture.server, githubEventIssues, delivery, body, signature)
		if first.Code != http.StatusAccepted {
			t.Fatalf("first status = %d body = %s", first.Code, first.Body.String())
		}
		if fixture.runs != 1 {
			t.Fatalf("runs after first = %d", fixture.runs)
		}
		second := postWebhook(fixture.server, githubEventIssues, delivery, body, signature)
		if second.Code != http.StatusOK || second.Body.Len() != 0 {
			t.Fatalf("replay status = %d body = %q", second.Code, second.Body.String())
		}
		if fixture.runs != 1 {
			t.Fatalf("replay started another run: %d", fixture.runs)
		}
		entries, err := os.ReadDir(filepath.Join(fixture.store.dataDir, deliveryDirName))
		if err != nil || len(entries) != 1 || entries[0].Name() != delivery {
			t.Fatalf("deliveries = %v %v", err, entries)
		}
		assertNoSecret(t, first.Body.String()+second.Body.String()+fixture.logs.String(), secret, planted)
	})

	t.Run("unbound repository", func(t *testing.T) {
		fixture := newWebhookFixture(t, secrets)
		body := githubIssueDelivery(7, "opened", secret, planted)
		response := postWebhook(fixture.server, githubEventIssues, "55555555-5555-5555-5555-555555555555", body, githubTestSignature(secret, body))
		if response.Code != http.StatusOK || response.Body.Len() != 0 {
			t.Fatalf("status = %d body = %q", response.Code, response.Body.String())
		}
		if fixture.runs != 0 {
			t.Fatalf("unbound repository started %d runs", fixture.runs)
		}
		assertNoDeliveries(t, fixture.store.dataDir)
		assertNoSecret(t, response.Body.String()+fixture.logs.String(), secret, planted)
	})

	t.Run("accepted event", func(t *testing.T) {
		fixture := newWebhookFixture(t, secrets)
		body := githubIssueDelivery(4242, "opened", secret, planted)
		delivery := "11111111-1111-1111-1111-111111111111"
		response := postWebhook(fixture.server, githubEventIssues, delivery, body, githubTestSignature(secret, body))
		if response.Code != http.StatusAccepted {
			t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
		}
		if fixture.runs != 1 {
			t.Fatalf("runs = %d", fixture.runs)
		}
		var accepted struct {
			Runs []acceptedRun `json:"runs"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
			t.Fatal(err)
		}
		if len(accepted.Runs) != 1 || accepted.Runs[0].Rule != "github.yaml" || accepted.Runs[0].Agent != "webhook-agent" || accepted.Runs[0].RunID != "gen_webhook" {
			t.Fatalf("runs = %#v", accepted.Runs)
		}
		document := receiveInvocation(t, fixture.runner)
		assertGitHubEvent(t, document, delivery)
		if document.Instructions != "Test agent instructions." {
			t.Fatalf("instructions = %q", document.Instructions)
		}
		for key, value := range document.Env {
			if strings.Contains(key, secret) || strings.Contains(value, secret) || strings.Contains(value, planted) || strings.Contains(value, "rm -rf") {
				t.Fatalf("env leaked payload: %s", key)
			}
		}
		record, err := os.ReadFile(filepath.Join(fixture.store.dataDir, deliveryDirName, delivery))
		if err != nil || string(record) != "issues\n" {
			t.Fatalf("delivery record = %q %v", record, err)
		}
		info, err := os.Lstat(filepath.Join(fixture.store.dataDir, deliveryDirName, delivery))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("delivery mode = %v %v", info, err)
		}
		dirInfo, err := os.Lstat(filepath.Join(fixture.store.dataDir, deliveryDirName))
		if err != nil || dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("delivery dir mode = %v %v", dirInfo, err)
		}
		journal, err := os.ReadFile(filepath.Join(fixture.store.dataDir, runsDirName, "gen_webhook", eventsFileName))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(document.Event)
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecret(t, response.Body.String()+string(journal)+string(encoded)+string(record)+fixture.logs.String(), secret, planted)
		for _, forbidden := range []string{"do-not-copy", "rm -rf", "full_name", "command"} {
			if bytes.Contains(encoded, []byte(forbidden)) {
				t.Fatalf("event copied %q: %s", forbidden, encoded)
			}
		}
	})
}

func TestWebhookIgnoredAndFailClosed(t *testing.T) {
	const secret = "whsec-local-test-secret-value"
	secrets := openTestSecretStore(t)
	if err := os.WriteFile(filepath.Join(secrets.root, webhookSecretName), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newWebhookFixture(t, secrets)
	pingBody := []byte(`{"zen":"keep it logically awesome"}`)
	ping := postWebhook(fixture.server, "ping", "66666666-6666-6666-6666-666666666666", pingBody, githubTestSignature(secret, pingBody))
	if ping.Code != http.StatusOK || fixture.runs != 0 {
		t.Fatalf("ping = %d runs %d", ping.Code, fixture.runs)
	}
	assertNoDeliveries(t, fixture.store.dataDir)

	emptyBody := githubIssueDelivery(4242, "opened", secret, "ghs_plantedtokenvalue")
	empty := postWebhook(fixture.server, githubEventIssues, "", emptyBody, githubTestSignature(secret, emptyBody))
	if empty.Code != http.StatusBadRequest || empty.Body.String() != "delivery id is required\n" || fixture.runs != 0 {
		t.Fatalf("empty delivery = %d %q runs %d", empty.Code, empty.Body.String(), fixture.runs)
	}

	if err := os.WriteFile(filepath.Join(fixture.store.dataDir, deliveryDirName), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := githubIssueDelivery(4242, "opened", secret, "ghs_plantedtokenvalue")
	blocked := postWebhook(fixture.server, githubEventIssues, "77777777-7777-7777-7777-777777777777", body, githubTestSignature(secret, body))
	if blocked.Code != http.StatusInternalServerError || blocked.Body.String() != "delivery was not recorded\n" || fixture.runs != 0 {
		t.Fatalf("unrecorded = %d %q runs %d", blocked.Code, blocked.Body.String(), fixture.runs)
	}
	assertNoSecret(t, ping.Body.String()+empty.Body.String()+blocked.Body.String()+fixture.logs.String(), secret, "ghs_plantedtokenvalue")
}

func TestWebhookWithoutVerifierFailsClosed(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "webhook-agent.yaml", nil)
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    &fakeRunner{invocations: make(chan invocation, 1)},
		store:     testStore(t),
		newRunID:  func() (string, error) { return "gen_webhook", nil },
		logger:    discardLogger(),
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"opened","repository":{"id":4242}}`)
	response := postWebhook(server, githubEventIssues, "88888888-8888-8888-8888-888888888888", body, "sha256="+strings.Repeat("ab", 32))
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "webhook verification is unavailable\n" {
		t.Fatalf("status = %d body = %q", response.Code, response.Body.String())
	}
}

func TestWebhookSecretStaysInRootStore(t *testing.T) {
	const secret = "root-webhook-secret-value"
	const sentinel = "BINDING-SENTINEL"
	store := openTestSecretStore(t)
	_, err := store.readWebhookSecret()
	if !errors.Is(err, errWebhookSecretUnavailable) || strings.Contains(err.Error(), secret) {
		t.Fatalf("missing secret error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.root, webhookSecretName), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	dataDir := t.TempDir()
	state := newPrivilegedState(logger, "", "", "", dataDir)
	state.secrets = store
	body := []byte(`{"action":"opened","repository":{"id":4242}}`)
	signature := githubTestSignature(secret, body)
	if !state.webhookSignatureOK(body, signature) {
		t.Fatal("valid signature was rejected")
	}
	if state.webhookSignatureOK(body, "sha256="+strings.Repeat("ab", 32)) {
		t.Fatal("mismatched signature was accepted")
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("secret logged: %s", logs.String())
	}

	parent, childFile, err := privilegedSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		parent.Close()
		childFile.Close()
	})
	if err := writeRepositoryJournal(dataDir, repositoryJournal{
		Version: observationVersion,
		Bindings: []repositoryBinding{{
			ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 4242, NodeID: "R_testNode",
		}},
		Observed: []repositoryObserved{{
			ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 4242, NodeID: "R_testNode",
			Description: sentinel, Visibility: visibilityPrivate,
			ActionsStatus: observationObserved, RulesetStatus: observationObserved, BranchStatus: observationObserved,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	go servePrivilegedParent(parent, logger, state)
	conn, err := fileConnForTest(childFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	coordinator := &ipcCoordinator{conn: conn}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinator.VerifyWebhook(ctx, body, signature); err != nil {
		t.Fatal(err)
	}
	mismatch := coordinator.VerifyWebhook(ctx, body, "sha256="+strings.Repeat("cd", 32))
	if !errors.Is(mismatch, errWebhookSignature) || strings.Contains(mismatch.Error(), secret) {
		t.Fatalf("mismatch = %v", mismatch)
	}
	bound, err := coordinator.RepositoryBound(ctx, 4242)
	if err != nil || !bound {
		t.Fatalf("bound = %v %v", bound, err)
	}
	other, err := coordinator.RepositoryBound(ctx, 7)
	if err != nil || other {
		t.Fatalf("unbound = %v %v", other, err)
	}
	reply, err := handleWebhookRepository(state, []byte(`{"repository_id":4242}`))
	if err != nil || strings.Contains(string(reply), sentinel) || strings.Contains(string(reply), secret) || string(reply) != `{"bound":true}` {
		t.Fatalf("repository reply = %s %v", reply, err)
	}
	verified, err := handleVerifyWebhook(state, mustWebhookRequest(t, body, signature))
	if err != nil || strings.Contains(string(verified), secret) || string(verified) != `{"ok":true}` {
		t.Fatalf("verify reply = %s %v", verified, err)
	}

	if err := os.Chmod(filepath.Join(store.root, webhookSecretName), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.readWebhookSecret()
	if !errors.Is(err, errWebhookSecretUnavailable) || strings.Contains(err.Error(), secret) {
		t.Fatalf("loose secret error = %v", err)
	}
	if state.webhookSignatureOK(body, signature) {
		t.Fatal("loose webhook secret was accepted")
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("secret logged after rejection: %s", logs.String())
	}
}

func TestWebhookDocumentation(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "webhooks.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	for _, want := range []string{
		"POST /webhooks/github",
		"/var/lib/genesis/secrets/GITHUB_APP_WEBHOOK_SECRET",
		"0600",
		"0700",
		"GITHUB_APP_RECONCILER_PEM",
		"urn:genesis:github",
		"dev.genesis.github.issues",
		"dev.genesis.github.pull_request",
		"dev.genesis.github.push",
		"dev.genesis.github.check_run",
		"repositoryid",
		"X-GitHub-Delivery",
		"X-Hub-Signature-256",
		"webhook active",
		"issues",
		"pull_request",
		"push",
		"check_run",
		"deliveries",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("webhooks.md missing %q", want)
		}
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "docs/webhooks.md") || !strings.Contains(string(readme), "GITHUB_APP_WEBHOOK_SECRET") {
		t.Fatal("README does not point at the webhook secret")
	}
}

type webhookFixture struct {
	server *eventServer
	runner *fakeRunner
	store  *runStore
	logs   *bytes.Buffer
	runs   int
}

func newWebhookFixture(t *testing.T, secrets *secretStore) *webhookFixture {
	t.Helper()
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "webhook-agent.yaml", nil)
	writeRule(t, filepath.Join(rulesDir, "github.yaml"), rule{
		Match: map[string]string{
			"type":    githubTypeIssues,
			"source":  githubWebhookSource,
			"subject": githubEventIssues,
			"action":  "opened",
		},
		Agent: "webhook-agent",
	})
	store := testStore(t)
	if err := writeRepositoryJournal(store.dataDir, repositoryJournal{
		Version: observationVersion,
		Bindings: []repositoryBinding{{
			ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 4242, NodeID: "R_testNode",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	state := &privilegedState{logger: logger, secrets: secrets}
	fixture := &webhookFixture{
		runner: &fakeRunner{invocations: make(chan invocation, 2)},
		store:  store,
		logs:   logs,
	}
	fixture.server = &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    fixture.runner,
		store:     store,
		logger:    logger,
		newRunID: func() (string, error) {
			fixture.runs++
			return "gen_webhook", nil
		},
		verifyWebhook: func(_ context.Context, body []byte, signature string) error {
			if state.webhookSignatureOK(body, signature) {
				return nil
			}
			return errWebhookSignature
		},
	}
	if err := fixture.server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func githubIssueDelivery(repositoryID int64, action, secret, planted string) []byte {
	payload, err := json.Marshal(map[string]any{
		"action": action,
		"repository": map[string]any{
			"id":        repositoryID,
			"full_name": "octo-org/lab-widget",
		},
		"issue": map[string]any{
			"title": "do-not-copy",
			"body":  "command rm -rf / " + planted + " " + secret,
		},
		"command": "rm -rf /",
	})
	if err != nil {
		panic(err)
	}
	return payload
}

func githubTestSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(server *eventServer, eventName, delivery string, body []byte, signature string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(githubEventHeader, eventName)
	if delivery != "" {
		request.Header.Set(githubDeliveryHeader, delivery)
	}
	if signature != "" {
		request.Header.Set(githubSignatureHeader, signature)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func receiveInvocation(t *testing.T, runner *fakeRunner) invocation {
	t.Helper()
	select {
	case document := <-runner.invocations:
		return document
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the webhook run")
	}
	return invocation{}
}

func assertGitHubEvent(t *testing.T, document invocation, delivery string) {
	t.Helper()
	event := document.Event
	for attribute, want := range map[string]string{
		"specversion":     "1.0",
		"id":              delivery,
		"source":          githubWebhookSource,
		"type":            githubTypeIssues,
		"subject":         githubEventIssues,
		"event":           githubEventIssues,
		"action":          "opened",
		"repositoryid":    "4242",
		"datacontenttype": "application/json",
	} {
		got, ok := event.stringAttribute(attribute)
		if !ok || got != want {
			t.Fatalf("%s = %q ok=%v", attribute, got, ok)
		}
	}
	var data struct {
		Event        string `json:"event"`
		Action       string `json:"action"`
		RepositoryID int64  `json:"repository_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(event["data"]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data.Event != githubEventIssues || data.Action != "opened" || data.RepositoryID != 4242 {
		t.Fatalf("data = %#v", data)
	}
}

func assertNoDeliveries(t *testing.T, dataDir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, deliveryDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("deliveries recorded: %d", len(entries))
	}
}

func assertNoSecret(t *testing.T, text, secret, planted string) {
	t.Helper()
	for _, forbidden := range []string{secret, planted, "PRIVATE KEY"} {
		if forbidden != "" && strings.Contains(text, forbidden) {
			t.Fatalf("response leaked %q", forbidden)
		}
	}
}

func mustWebhookRequest(t *testing.T, body []byte, signature string) []byte {
	t.Helper()
	payload, err := json.Marshal(webhookVerifyRequest{Signature: signature, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
