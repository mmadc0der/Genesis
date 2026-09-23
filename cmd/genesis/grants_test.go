package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	providerSecretRef  = "GENESIS_PROVIDER_SECRET_REF_XYZ"
	programmerSecret   = "GENESIS_PROVIDER_PROG_REF_XYZ"
	reviewerSecret     = "GENESIS_PROVIDER_REVIEW_REF_XYZ"
	programmerIdentity = "prog-bot"
	reviewerIdentity   = "review-bot"
	readerIdentity     = "public-bot"
	reconcilerIdentity = "root-bot"
)

func TestAbsentProvidersPreserveDigest(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "ok.yaml", map[string]string{"LANG": "C"})
	writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{
		Match: map[string]string{"type": "dev.genesis.run"},
		Agent: "ok",
	})
	missingProviders := filepath.Join(t.TempDir(), "missing-providers")
	inactive, err := loadGeneration(agentsDir, rulesDir, filepath.Join(t.TempDir(), "missing-repos"), missingProviders)
	if err != nil {
		t.Fatal(err)
	}
	if inactive.providersActive || inactive.reposActive {
		t.Fatalf("inactive flags providers=%v repos=%v", inactive.providersActive, inactive.reposActive)
	}
	want := legacyAgentsRulesDigest(t, inactive.agents["ok"], cwd, home)
	if inactive.digest != want {
		t.Fatalf("absent providers digest = %s, want %s", inactive.digest, want)
	}
	reposOnly, err := loadGeneration(agentsDir, rulesDir, t.TempDir(), missingProviders)
	if err != nil {
		t.Fatal(err)
	}
	if reposOnly.providersActive || reposOnly.digest == inactive.digest {
		t.Fatalf("empty repos digest changed providers state: %s active=%v", reposOnly.digest, reposOnly.providersActive)
	}
}

func TestProviderSchemaRejectsSecretMaterialAndUnsafePaths(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "ok.yaml", nil)
	writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{
		Match: map[string]string{"type": "dev.genesis.run"},
		Agent: "ok",
	})
	reposDir := t.TempDir()
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))

	insideVolume := filepath.Join(designerWritableConfigRoot, "providers.d")
	if _, _, err := loadProviders(insideVolume, agentsDir, rulesDir, reposDir); err == nil || !strings.Contains(err.Error(), "designer-writable") {
		t.Fatalf("config volume providers error = %v", err)
	}
	nested := filepath.Join(agentsDir, "providers.d")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProviders(nested, agentsDir, rulesDir, reposDir); err == nil || !strings.Contains(err.Error(), "inside agents") {
		t.Fatalf("nested providers error = %v", err)
	}

	providersDir := t.TempDir()
	writeRepoFile(t, providersDir, "github.yaml", strings.ReplaceAll(validProviderYAML(), programmerSecret, "-----BEGIN PRIVATE KEY-----\nabc"))
	_, _, err := loadProviders(providersDir, agentsDir, rulesDir, reposDir)
	if err == nil || strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), programmerSecret) {
		t.Fatalf("pem error = %v", err)
	}
	writeRepoFile(t, providersDir, "github.yaml", strings.Replace(validProviderYAML(), "secret: "+programmerSecret, "value: hunter2\n    secret: "+programmerSecret, 1))
	_, _, err = loadProviders(providersDir, agentsDir, rulesDir, reposDir)
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), programmerSecret) {
		t.Fatalf("value field error = %v", err)
	}
	shared := strings.ReplaceAll(validProviderYAML(), reviewerSecret, programmerSecret)
	writeRepoFile(t, providersDir, "github.yaml", shared)
	_, _, err = loadProviders(providersDir, agentsDir, rulesDir, reposDir)
	if err == nil || !strings.Contains(err.Error(), "share a secret reference") || strings.Contains(err.Error(), programmerSecret) {
		t.Fatalf("shared secret error = %v", err)
	}

	link := filepath.Join(t.TempDir(), "linked-providers")
	if err := os.Symlink(providersDir, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProviders(link, agentsDir, rulesDir, reposDir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink providers error = %v", err)
	}
	if _, _, err := loadProviders("providers.d", agentsDir, rulesDir, reposDir); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative providers error = %v", err)
	}

	hidden := filepath.Join(agentsDir, "nested-providers")
	if err := os.Mkdir(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(agentsDir, filepath.Join(outside, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProviders(filepath.Join(outside, "alias", "nested-providers"), agentsDir, rulesDir, reposDir); err == nil || !strings.Contains(err.Error(), "inside agents") {
		t.Fatalf("parent symlink providers error = %v", err)
	}

	encoded, err := json.Marshal(providerDefinition{
		ID:       "github",
		Provider: repositoryProviderGitHub,
		Org:      "octo-org",
		Identities: []providerIdentity{{
			Name: programmerIdentity, Role: roleProgrammer, Credential: credentialApp, Secret: programmerSecret,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), programmerSecret) || strings.Contains(string(encoded), programmerIdentity) {
		t.Fatalf("provider JSON = %s", encoded)
	}
}

func TestGrantPolicyFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(dir, repos string)
		want   string
		secret bool
	}{
		{name: "reconciler", mutate: func(dir, repos string) {
			writeGrantAgent(t, dir, "worker", programmerIdentity, "lab", gitWrite, map[string]string{"contents": "write", "metadata": "read"})
			replaceAgentIdentity(t, dir, "worker", reconcilerIdentity)
		}, want: "reconciler identity cannot be granted"},
		{name: "designer", mutate: func(dir, repos string) {
			writeAgent(t, filepath.Join(dir, "designer.yaml"), agentDefinition{
				Instructions: "Test agent instructions.",
				Cwd:          designerWritableConfigRoot,
				Home:         "/tmp/designer-home",
				GitHub: &agentGitHub{
					Repository: "lab", Identity: programmerIdentity, Git: gitRead,
					Permissions: map[string]string{"contents": "read", "metadata": "read"},
				},
			})
		}, want: "designer cannot declare a GitHub capability"},
		{name: "administration", mutate: func(dir, repos string) {
			writeGrantAgent(t, dir, "worker", programmerIdentity, "lab", gitWrite, map[string]string{
				"contents": "write", "administration": "write",
			})
		}, want: "administration, secrets, or environments"},
		{name: "reviewer write", mutate: func(dir, repos string) {
			writeGrantAgent(t, dir, "worker", reviewerIdentity, "lab", gitWrite, map[string]string{
				"contents": "write", "pull_requests": "write",
			})
		}, want: "not allowlisted"},
		{name: "private without credential", mutate: func(dir, repos string) {
			writeGrantAgent(t, dir, "worker", readerIdentity, "lab", gitRead, map[string]string{
				"contents": "read", "metadata": "read",
			})
		}, want: "cannot omit a credential"},
		{name: "missing providers", mutate: func(dir, repos string) {
			writeGrantAgent(t, dir, "worker", programmerIdentity, "lab", gitWrite, map[string]string{
				"contents": "write", "metadata": "read",
			})
		}, want: "requires providers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentsDir := t.TempDir()
			rulesDir := t.TempDir()
			writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{
				Match: map[string]string{"type": "dev.genesis.run"},
				Agent: "worker",
			})
			if tc.name == "designer" {
				writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{
					Match: map[string]string{"type": "dev.genesis.run"},
					Agent: "designer",
				})
			}
			reposDir := t.TempDir()
			writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
			providersDir := t.TempDir()
			if tc.name != "missing providers" {
				writeRepoFile(t, providersDir, "github.yaml", validProviderYAML())
			} else {
				providersDir = filepath.Join(t.TempDir(), "missing")
			}
			tc.mutate(agentsDir, reposDir)
			_, err := loadGeneration(agentsDir, rulesDir, reposDir, providersDir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
			for _, forbidden := range []string{providerSecretRef, programmerSecret, reviewerSecret, programmerIdentity, reviewerIdentity, readerIdentity, reconcilerIdentity, "BEGIN", "ghp_"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error exposed %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestGrantPlanIsLocalAndDoesNotExposeProviderMaterial(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	reposDir := t.TempDir()
	providersDir := t.TempDir()
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	writeRepoFile(t, reposDir, "docs.yaml", strings.ReplaceAll(validRepositoryYAML("octo-org", "public-docs"), "visibility: private", "visibility: public"))
	writeRepoFile(t, providersDir, "github.yaml", validProviderYAML())
	writeGrantAgent(t, agentsDir, "worker", programmerIdentity, "lab", gitWrite, map[string]string{
		"contents": "write", "pull_requests": "write", "metadata": "read",
	})
	writeGrantAgent(t, agentsDir, "reader", readerIdentity, "docs", gitRead, map[string]string{
		"contents": "read", "metadata": "read",
	})
	writeRule(t, filepath.Join(rulesDir, "worker.yaml"), rule{Match: map[string]string{"type": "dev.genesis.run"}, Agent: "worker"})
	writeRule(t, filepath.Join(rulesDir, "reader.yaml"), rule{Match: map[string]string{"type": "dev.genesis.read"}, Agent: "reader"})

	loaded, err := loadGeneration(agentsDir, rulesDir, reposDir, providersDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.agents["worker"].credentialMode != credentialPending || loaded.agents["reader"].credentialMode != credentialNone {
		t.Fatalf("modes worker=%s reader=%s", loaded.agents["worker"].credentialMode, loaded.agents["reader"].credentialMode)
	}
	plan := buildPlan(loaded.agents, true)
	first, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(buildPlan(loaded.agents, true))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("grant intents are not deterministic")
	}
	if !strings.Contains(string(first), programmerIdentity) {
		t.Fatal("privileged plan dropped the identity binding")
	}
	grants, err := buildGrantPlan(plan, true)
	if err != nil {
		t.Fatal(err)
	}
	if grants.CredentialActive || grants.KeyMaterial != keyMaterialPending || grants.RemoteRegistration != remoteRegistrationUnsupported {
		t.Fatalf("grant plan = %#v", grants)
	}
	if err := grantPlanExposes(grants, providerSecretRef, programmerSecret, reviewerSecret, programmerIdentity, reviewerIdentity, readerIdentity, reconcilerIdentity); err != nil {
		t.Fatal(err)
	}
	publicOnly := buildPlan(map[string]agentDefinition{"reader": loaded.agents["reader"]}, true)
	publicPlan, err := buildGrantPlan(publicOnly, true)
	if err != nil {
		t.Fatal(err)
	}
	if publicPlan.KeyMaterial != keyMaterialNone || publicPlan.CredentialActive || publicPlan.RemoteRegistration != remoteRegistrationUnsupported {
		t.Fatalf("public plan = %#v", publicPlan)
	}

	var applyLogs bytes.Buffer
	host := newMemoryHost()
	store := openTestCredentialStore(t)
	state := &privilegedState{
		logger:      slog.New(slog.NewJSONHandler(&applyLogs, nil)),
		host:        host,
		mutate:      true,
		dataDir:     t.TempDir(),
		credentials: store,
	}
	applied, err := state.apply(plan)
	if err != nil {
		t.Fatal(err)
	}
	if applied.HostMutation != hostMutationNone || len(host.users) != 0 {
		t.Fatalf("apply activated host state: %#v users=%d", applied, len(host.users))
	}
	if !slicesContainsAll(applied.Applied, intentEnsureRepositoryGrant+":reader", intentEnsureCredential+":reader", intentEnsureRepositoryGrant+":worker", intentEnsureCredential+":worker") {
		t.Fatalf("applied = %#v", applied.Applied)
	}
	sawRemote := false
	for _, change := range applied.Unsupported {
		if change.Kind == intentEnsureCredential {
			t.Fatalf("credential intent stayed unsupported: %#v", change)
		}
		if strings.Contains(change.Reason, "remote registration") || strings.Contains(change.Reason, "no SSH identity") {
			sawRemote = true
		}
		for _, forbidden := range []string{programmerSecret, programmerIdentity, "PRIVATE KEY"} {
			if strings.Contains(change.Reason, forbidden) {
				t.Fatalf("unsupported reason exposed %q", forbidden)
			}
		}
	}
	if !sawRemote {
		t.Fatalf("unsupported = %#v", applied.Unsupported)
	}
	workerKey, readerKey := grantKeyPaths(t, store)
	if workerKey == "" || readerKey != "" {
		t.Fatalf("worker key %q reader key %q", workerKey, readerKey)
	}
	keyBytes, err := os.ReadFile(workerKey)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(workerKey)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("key mode = %o", info.Mode())
	}
	again, err := state.apply(plan)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := os.ReadFile(workerKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyBytes, retry) || len(again.RetainedGrants) != 0 {
		t.Fatalf("retry rotated material or retained grants: retained=%v", again.RetainedGrants)
	}
	encodedApply, _ := json.Marshal(applied)
	if bytes.Contains(encodedApply, keyBytes) || bytes.Contains(applyLogs.Bytes(), keyBytes) || bytes.Contains(encodedApply, []byte(programmerIdentity)) {
		t.Fatal("apply result exposed grant material")
	}

	var logs bytes.Buffer
	runner := &fakeRunner{invocations: make(chan invocation, 2)}
	server := &eventServer{
		agentsDir:    agentsDir,
		rulesDir:     rulesDir,
		reposDir:     reposDir,
		providersDir: providersDir,
		runner:       runner,
		newRunID:     func() (string, error) { return "gen_grant", nil },
		logger:       slog.New(slog.NewJSONHandler(&logs, nil)),
		syncToken:    "sync-secret",
		store:        testStore(t),
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	rulesOnly := sendSync(server, "sync-secret", map[string]any{"scope": []string{"rules"}})
	if rulesOnly.Code != http.StatusOK {
		t.Fatalf("rules-only sync = %d %s", rulesOnly.Code, rulesOnly.Body.String())
	}
	var rulesPlan syncResponse
	if err := json.Unmarshal(rulesOnly.Body.Bytes(), &rulesPlan); err != nil {
		t.Fatal(err)
	}
	if rulesPlan.GrantPlan.Requested || len(rulesPlan.GrantPlan.Intents) != 0 {
		t.Fatalf("rules-only grant plan = %#v", rulesPlan.GrantPlan)
	}
	response := sendSync(server, "sync-secret", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, forbidden := range []string{providerSecretRef, programmerSecret, reviewerSecret, programmerIdentity, reviewerIdentity, readerIdentity, reconcilerIdentity, "BEGIN", "ghp_", "GH_TOKEN"} {
		if strings.Contains(body, forbidden) || strings.Contains(logs.String(), forbidden) {
			t.Fatalf("sync exposed %q\nbody=%s\nlogs=%s", forbidden, body, logs.String())
		}
	}
	var decoded syncResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GrantPlan.CredentialActive || decoded.GrantPlan.KeyMaterial != keyMaterialPending || decoded.GrantPlan.RemoteRegistration != remoteRegistrationUnsupported {
		t.Fatalf("http grant plan = %#v", decoded.GrantPlan)
	}
	if decoded.Privileged.HostMutation != hostMutationNone || len(decoded.Privileged.Applied) != 0 {
		t.Fatalf("privileged = %#v", decoded.Privileged)
	}

	generation := sendEvent(server, map[string]any{
		"specversion": "1.0",
		"id":          "grant-1",
		"source":      "urn:genesis:test",
		"type":        "dev.genesis.run",
	})
	if generation.Code != http.StatusAccepted {
		t.Fatalf("event = %d %s", generation.Code, generation.Body.String())
	}
	document := <-runner.invocations
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GIT_ASKPASS"} {
		if _, ok := document.Env[key]; ok {
			t.Fatalf("invocation env activated %s: %#v", key, document.Env)
		}
	}
	encodedInvocation, _ := json.Marshal(document)
	if strings.Contains(string(encodedInvocation), programmerSecret) || strings.Contains(string(encodedInvocation), programmerIdentity) {
		t.Fatalf("invocation exposed provider material: %s", encodedInvocation)
	}

	view := generationSnapshot(loaded, false, false)
	encodedView, _ := json.Marshal(view)
	if strings.Contains(string(encodedView), programmerSecret) || strings.Contains(string(encodedView), programmerIdentity) {
		t.Fatalf("generation view = %s", encodedView)
	}

	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)
	panel := httptest.NewServer(newControlServer(controlConfig{
		listenerURL:  listener.URL,
		agentsDir:    agentsDir,
		rulesDir:     rulesDir,
		reposDir:     reposDir,
		providersDir: providersDir,
		dataDir:      server.store.dataDir,
	}, discardLogger()))
	t.Cleanup(panel.Close)
	agentsBody := mustGET(t, panel.URL+"/api/agents")
	stateBody := mustGET(t, panel.URL+"/api/state")
	providersRoute, err := http.Get(panel.URL + "/api/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer providersRoute.Body.Close()
	if providersRoute.StatusCode != http.StatusNotFound {
		t.Fatalf("providers route = %d", providersRoute.StatusCode)
	}
	for _, forbidden := range []string{providerSecretRef, programmerSecret, reviewerSecret, programmerIdentity, reviewerIdentity, readerIdentity, reconcilerIdentity} {
		if strings.Contains(agentsBody, forbidden) || strings.Contains(stateBody, forbidden) {
			t.Fatalf("control exposed %q\nagents=%s\nstate=%s", forbidden, agentsBody, stateBody)
		}
	}
	if !strings.Contains(agentsBody, `"credential":"pending"`) || !strings.Contains(agentsBody, `"credential":"none"`) {
		t.Fatalf("agents hid grant posture: %s", agentsBody)
	}
}

func TestReviewerIdentityStaysSeparate(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	reposDir := t.TempDir()
	providersDir := t.TempDir()
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	writeRepoFile(t, providersDir, "github.yaml", validProviderYAML())
	writeGrantAgent(t, agentsDir, "author", programmerIdentity, "lab", gitWrite, map[string]string{
		"contents": "write", "metadata": "read",
	})
	writeGrantAgent(t, agentsDir, "author", programmerIdentity, "lab", gitRead, map[string]string{
		"pull_requests": "write", "contents": "read", "metadata": "read",
	})
	writeRule(t, filepath.Join(rulesDir, "author.yaml"), rule{Match: map[string]string{"type": "dev.genesis.run"}, Agent: "author"})
	if _, err := loadGeneration(agentsDir, rulesDir, reposDir, providersDir); err == nil || !strings.Contains(err.Error(), "reviewer identity") {
		t.Fatalf("same-file separation error = %v", err)
	}

	writeGrantAgent(t, agentsDir, "author", programmerIdentity, "lab", gitWrite, map[string]string{
		"contents": "write", "metadata": "read",
	})
	writeGrantAgent(t, agentsDir, "critic", programmerIdentity, "lab", gitRead, map[string]string{
		"pull_requests": "write", "contents": "read", "metadata": "read",
	})
	writeRule(t, filepath.Join(rulesDir, "critic.yaml"), rule{Match: map[string]string{"type": "dev.genesis.review"}, Agent: "critic"})
	if _, err := loadGeneration(agentsDir, rulesDir, reposDir, providersDir); err == nil || !strings.Contains(err.Error(), "review access requires the reviewer identity") {
		t.Fatalf("programmer-as-reviewer error = %v", err)
	}

	writeGrantAgent(t, agentsDir, "critic", reviewerIdentity, "lab", gitRead, map[string]string{
		"pull_requests": "write", "contents": "read", "metadata": "read",
	})
	if _, err := loadGeneration(agentsDir, rulesDir, reposDir, providersDir); err != nil {
		t.Fatal(err)
	}
}

func TestScopedSyncDoesNotPlanUsersOrGrants(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	reposDir := t.TempDir()
	providersDir := t.TempDir()
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	writeRepoFile(t, providersDir, "github.yaml", validProviderYAML())
	writeAgent(t, filepath.Join(agentsDir, "worker.yaml"), agentDefinition{
		Instructions: "Test agent instructions.",
		Cwd:          "/home/worker/work",
		Home:         "/home/worker",
		User:         "worker",
		GitHub: &agentGitHub{
			Repository:  "lab",
			Identity:    programmerIdentity,
			Git:         gitWrite,
			Permissions: map[string]string{"contents": "write", "metadata": "read"},
		},
	})
	writeRule(t, filepath.Join(rulesDir, "worker.yaml"), rule{
		Match: map[string]string{"type": "dev.genesis.run"},
		Agent: "worker",
	})
	recorder := &recordingCoordinator{}
	server := &eventServer{
		agentsDir:    agentsDir,
		rulesDir:     rulesDir,
		reposDir:     reposDir,
		providersDir: providersDir,
		runner:       &fakeRunner{invocations: make(chan invocation, 1)},
		newRunID:     func() (string, error) { return "gen_scope", nil },
		logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
		syncToken:    "sync-secret",
		coordinator:  recorder,
		store:        testStore(t),
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"rules", "repos"} {
		response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scope}})
		if response.Code != http.StatusOK {
			t.Fatalf("%s sync = %d %s", scope, response.Code, response.Body.String())
		}
		var body syncResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.GrantPlan.Requested || len(body.GrantPlan.Intents) != 0 || len(body.Privileged.Unsupported) != 0 || len(body.Privileged.Applied) != 0 || body.Privileged.HostMutation != hostMutationNone {
			t.Fatalf("%s sync planned work: grant=%#v privileged=%#v", scope, body.GrantPlan, body.Privileged)
		}
	}
	if len(recorder.plans) != 2 {
		t.Fatalf("plans = %d", len(recorder.plans))
	}
	for _, plan := range recorder.plans {
		if len(plan.Intents) != 0 || plan.Agents {
			t.Fatalf("scoped plan = %#v", plan)
		}
	}

	loaded, err := loadGeneration(agentsDir, rulesDir, reposDir, providersDir)
	if err != nil {
		t.Fatal(err)
	}
	agentPlan := buildPlan(loaded.agents, true)
	host := newMemoryHost()
	store := openTestCredentialStore(t)
	state := &privilegedState{logger: discardLogger(), host: host, mutate: true, dataDir: t.TempDir(), credentials: store}
	applied, err := state.apply(agentPlan)
	if err != nil {
		t.Fatal(err)
	}
	if applied.HostMutation != hostMutationApplied || len(host.users) != 1 || host.users["worker"] == nil {
		t.Fatalf("apply users=%#v result=%#v", host.users, applied)
	}
	if !slicesContainsAll(applied.Applied, intentEnsureAgentUser+":worker", intentEnsureRepositoryGrant+":worker", intentEnsureCredential+":worker") {
		t.Fatalf("applied = %#v", applied.Applied)
	}
	sawRemote := false
	for _, change := range applied.Unsupported {
		if change.Kind == intentEnsureRemoteRegistration {
			sawRemote = true
		}
		if change.Kind == intentEnsureCredential || change.Kind == intentEnsureRepositoryGrant {
			t.Fatalf("grant intent stayed unsupported: %#v", change)
		}
		if strings.Contains(change.Reason, programmerSecret) || strings.Contains(change.Reason, programmerIdentity) || strings.Contains(change.Reason, "PRIVATE KEY") {
			t.Fatalf("unsupported exposed provider material: %s", change.Reason)
		}
	}
	if !sawRemote {
		t.Fatalf("missing unsupported remote registration in %#v", applied.Unsupported)
	}
	workerKey, readerKey := grantKeyPaths(t, store)
	if workerKey == "" || readerKey != "" {
		t.Fatalf("worker key %q reader key %q", workerKey, readerKey)
	}
}

type recordingCoordinator struct {
	plans []privilegedPlan
}

func (r *recordingCoordinator) Coordinate(_ context.Context, plan privilegedPlan) (coordinateResult, error) {
	r.plans = append(r.plans, plan)
	return evaluatePlan(plan)
}

func TestEntrypointDoesNotSeedProvidersIntoConfigVolume(t *testing.T) {
	root := repoRoot(t)
	config := t.TempDir()
	data := t.TempDir()
	defaults := t.TempDir()
	if err := os.MkdirAll(filepath.Join(defaults, "providers.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaults, "providers.d", "github.yaml"), []byte("provider: github\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(root, "agents.d", "designer.yaml"), filepath.Join(defaults, "agents.d", "designer.yaml"))
	copyFile(t, filepath.Join(root, "rules.d", "designer.yaml"), filepath.Join(defaults, "rules.d", "designer.yaml"))
	cmd := exec.Command("sh", filepath.Join(root, "docker-entrypoint.sh"), "seed-config")
	cmd.Env = append(os.Environ(),
		"GENESIS_CONFIG_DIR="+config,
		"GENESIS_DATA_DIR="+data,
		"GENESIS_DEFAULTS_DIR="+defaults,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("seed-config: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(config, "providers.d")); !os.IsNotExist(err) {
		t.Fatalf("config volume gained providers.d: %v", err)
	}
}

func TestEntrypointLocksProviderFilesWithoutFollowingSymlinks(t *testing.T) {
	root := repoRoot(t)
	config := t.TempDir()
	data := t.TempDir()
	defaults := t.TempDir()
	providers := t.TempDir()
	secret := filepath.Join(providers, "github.yaml")
	if err := os.WriteFile(secret, []byte("provider: github\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(providers, "alias.yaml")); err != nil {
		t.Fatal(err)
	}
	run := func(providersPath, command string) ([]byte, error) {
		cmd := exec.Command("sh", filepath.Join(root, "docker-entrypoint.sh"), command)
		cmd.Env = append(os.Environ(),
			"GENESIS_CONFIG_DIR="+config,
			"GENESIS_DATA_DIR="+data,
			"GENESIS_DEFAULTS_DIR="+defaults,
			"GENESIS_PROVIDERS_DIR="+providersPath,
		)
		return cmd.CombinedOutput()
	}
	output, err := run(providers, "seed-config")
	if err != nil {
		t.Fatalf("seed-config: %v\n%s", err, output)
	}
	seeded, err := os.Stat(secret)
	if err != nil {
		t.Fatal(err)
	}
	if seeded.Mode().Perm() != 0o644 {
		t.Fatalf("seed-config changed provider mode to %o", seeded.Mode().Perm())
	}
	output, err = run(providers, "own-config")
	if err != nil {
		t.Fatalf("own-config: %v\n%s", err, output)
	}
	locked, err := os.Stat(secret)
	if err != nil {
		t.Fatal(err)
	}
	if locked.Mode().Perm() != 0o640 {
		t.Fatalf("provider file mode = %o", locked.Mode().Perm())
	}
	dirInfo, err := os.Stat(providers)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o750 {
		t.Fatalf("providers directory mode = %o", dirInfo.Mode().Perm())
	}
	outsideInfo, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if outsideInfo.Mode().Perm() != 0o644 {
		t.Fatalf("followed provider symlink and chmod'd target: %o", outsideInfo.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(config, "providers.d")); !os.IsNotExist(err) {
		t.Fatalf("config volume gained providers.d: %v", err)
	}
	link := filepath.Join(t.TempDir(), "linked-providers")
	if err := os.Symlink(providers, link); err != nil {
		t.Fatal(err)
	}
	output, err = run(link, "own-config")
	if err == nil || !bytes.Contains(output, []byte("must not be a symlink")) {
		t.Fatalf("symlink providers directory error = %v\n%s", err, output)
	}
}

func TestDockerImageDoesNotInstallExampleProviders(t *testing.T) {
	root := repoRoot(t)
	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(dockerfile)
	if strings.Contains(text, "COPY providers.d") {
		t.Fatal("image copies providers.d into the live directory")
	}
	if strings.Contains(strings.ToLower(text), "openssh") {
		t.Fatal("image installs OpenSSH; the agent protocol is in-process")
	}
	compose, err := os.ReadFile(filepath.Join(root, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(compose), "target: /var/lib/genesis/credentials") != 1 {
		t.Fatal("credential store must be mounted on the root service only")
	}
	entrypoint, err := os.ReadFile(filepath.Join(root, "docker-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entrypoint), credentialsDockerPath) || !strings.Contains(text, credentialsDockerPath) || !strings.Contains(string(compose), credentialsDockerPath) {
		t.Fatal("image credential path is not wired to /var/lib/genesis/credentials")
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "/etc/genesis/providers.d") && (strings.Contains(line, "a+rX") || strings.Contains(line, "0644")) {
			t.Fatalf("provider path is world-readable: %s", line)
		}
	}
	for _, want := range []string{
		"groupadd --gid 65532 genesis",
		"useradd --create-home --uid 65532 --gid 65532",
		"chown root:genesis /etc/genesis/providers.d",
		"chmod 0750 /etc/genesis/providers.d",
		"chown root:root /var/lib/genesis /var/lib/genesis/credentials",
		"chmod 0755 /var/lib/genesis",
		"chmod 0700 /var/lib/genesis/credentials",
		"-credentials", "/var/lib/genesis/credentials",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Dockerfile missing %q", want)
		}
	}
}

func TestCommittedProviderExampleLoads(t *testing.T) {
	root := repoRoot(t)
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "ok.yaml", nil)
	writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{Match: map[string]string{"type": "dev.genesis.run"}, Agent: "ok"})
	loaded, err := loadGeneration(agentsDir, rulesDir, filepath.Join(root, "repos.d"), filepath.Join(root, "providers.d"))
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.providersActive || len(loaded.providers) != 1 {
		t.Fatalf("providers = %#v active=%v", loaded.providers, loaded.providersActive)
	}
	plan := buildPlan(loaded.agents, true)
	for _, intent := range plan.Intents {
		switch intent.Kind {
		case intentEnsureRepositoryGrant, intentEnsureCredential, intentEnsureRemoteRegistration:
			t.Fatalf("example providers created a grant without an agent capability: %#v", intent)
		}
	}
}

func validProviderYAML() string {
	return `provider: github
org: octo-org
identities:
  - name: ` + reconcilerIdentity + `
    role: reconciler
    credential: app
    secret: ` + providerSecretRef + `
  - name: ` + programmerIdentity + `
    role: programmer
    credential: app
    secret: ` + programmerSecret + `
  - name: ` + reviewerIdentity + `
    role: reviewer
    credential: app
    secret: ` + reviewerSecret + `
  - name: ` + readerIdentity + `
    role: reader
    credential: none
`
}

func writeGrantAgent(t *testing.T, directory, id, identity, repository, gitAccess string, permissions map[string]string) {
	t.Helper()
	writeAgent(t, filepath.Join(directory, id+".yaml"), agentDefinition{
		Instructions: "Test agent instructions.",
		Cwd:          "/tmp/" + id + "-work",
		Home:         "/tmp/" + id + "-home",
		GitHub: &agentGitHub{
			Repository:  repository,
			Identity:    identity,
			Git:         gitAccess,
			Permissions: permissions,
		},
	})
}

func replaceAgentIdentity(t *testing.T, directory, id, identity string) {
	t.Helper()
	path := filepath.Join(directory, id+".yaml")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(contents), "identity: "+programmerIdentity, "identity: "+identity, 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}
