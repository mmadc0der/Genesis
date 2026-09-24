package main

import (
	"bytes"
	"context"
	"crypto/rsa"
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
	"time"
)

func TestRunTokenIsDeliveredThenRemoved(t *testing.T) {
	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	store := openTestCredentialStore(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	state := newPrivilegedState(logger, "/usr/bin/python3", runTokenPython(t), account.Username, dataDir)
	state.host = newMemoryHost()
	state.mutate = true
	state.credentials = store
	state.remember(identity)
	state.registrar = &fakeRegistrar{Status: remoteStatusReady, KeyID: "ready-key"}
	fixture := gitFixture(t)
	report := filepath.Join(t.TempDir(), "report")
	leak := filepath.Join(dshHome, "leak.txt")
	var gitCalls []gitCall
	state.observeGit = func(env, args []string) {
		gitCalls = append(gitCalls, gitCall{env: append([]string{}, env...), args: append([]string{}, args...)})
	}
	state.gitRemote = func(org, repo string) (string, error) {
		if org != "octo-org" || repo != "lab-widget" {
			t.Fatalf("remote %s/%s", org, repo)
		}
		return fixture, nil
	}
	fake := newRunTokenFake(t, map[string]string{"contents": "write", "metadata": "read"})
	reg := fake.registrar(logger)
	state.tokens = reg
	if _, err := state.apply(privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, gitWrite, credentialPending)}); err != nil {
		t.Fatal(err)
	}
	plan, err := buildGrantPlan(privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, gitWrite, credentialPending)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CredentialActive {
		t.Fatal("sync plan reported a credential active")
	}
	t.Cleanup(func() { state.killAll() })
	pid, err := state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome, NeedsToken: true,
		Env: map[string]string{"REPORT": report, "LEAK": leak, "GITHUB_TOKEN": "ghs_attacker_token_value_000", "GH_TOKEN": "ghp_attacker_token_value_000"},
	}, devNull(t), devNull(t), devNull(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(waitFile(t, report)); got != fake.token {
		t.Fatalf("token delivery = %q", got)
	}
	if !state.credentialActive(pid) {
		t.Fatal("credential_active was false while the token was delivered")
	}
	readme, err := os.ReadFile(filepath.Join(identity.Cwd, "README.md"))
	if err != nil || string(readme) != "fixture\n" {
		t.Fatalf("clone = %q %v", readme, err)
	}
	config, err := os.ReadFile(filepath.Join(identity.Cwd, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(config, []byte(fake.token)) || bytes.Contains(config, []byte("ghs_")) || bytes.Contains(config, []byte("https://")) {
		t.Fatalf("remote config = %s", config)
	}
	if !strings.Contains(string(config), fixture) {
		t.Fatalf("origin = %s", config)
	}
	assertGitUsedSocket(t, gitCalls, fake.token)
	if fake.posts.Load() != 1 {
		t.Fatalf("token posts = %d", fake.posts.Load())
	}
	if !permissionMapsEqual(fake.requested, map[string]string{"contents": "write", "metadata": "read"}) || len(fake.repos) != 1 || fake.repos[0] != "lab-widget" {
		t.Fatalf("token request = %#v repos=%v", fake.requested, fake.repos)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Kill()
	if _, err := state.wait(pid); err != nil && !strings.Contains(err.Error(), "signal") && !strings.Contains(err.Error(), "killed") {
		// A killed child is a non-zero wait, which wait() returns as an error only for unknown pids.
	}
	if state.credentialActive(pid) {
		t.Fatal("credential_active stayed true after the run")
	}
	leaked, err := os.ReadFile(leak)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(leaked, []byte(fake.token)) || bytes.Contains(leaked, []byte("ghs_")) {
		t.Fatalf("dsh_home kept the token: %s", leaked)
	}
	if !bytes.Contains(leaked, []byte(redactedSecret)) {
		t.Fatalf("dsh_home was not redacted: %s", leaked)
	}
	text := logs.String()
	if strings.Contains(text, fake.token) || strings.Contains(text, "eyJ") || strings.Contains(text, "ghs_attacker") {
		t.Fatalf("logs kept a credential: %s", text)
	}
	if !strings.Contains(text, `"credential_active":true`) || !strings.Contains(text, `"credential_active":false`) {
		t.Fatalf("credential_active log = %s", text)
	}
	if strings.Contains(newRedactor(nil).text("token "+fake.token), fake.token) {
		t.Fatal("journal redactor kept the installation token")
	}
}

func TestMismatchedTokenDoesNotStartTheSession(t *testing.T) {
	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	store := openTestCredentialStore(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	marker := filepath.Join(t.TempDir(), "started")
	state := newPrivilegedState(logger, "/usr/bin/python3", "import pathlib,os\npathlib.Path(os.environ['MARKER']).write_text('started')\n", account.Username, dataDir)
	state.host = newMemoryHost()
	state.mutate = true
	state.credentials = store
	state.remember(identity)
	state.registrar = &fakeRegistrar{Status: remoteStatusReady, KeyID: "ready-key"}
	fake := newRunTokenFake(t, map[string]string{"contents": "write", "metadata": "read", "actions": "write"})
	state.tokens = fake.registrar(logger)
	state.gitRemote = func(string, string) (string, error) { return gitFixture(t), nil }
	if _, err := state.apply(privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, gitWrite, credentialPending)}); err != nil {
		t.Fatal(err)
	}
	_, err := state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome, NeedsToken: true, Env: map[string]string{"MARKER": marker},
	}, devNull(t), devNull(t), devNull(t))
	if err == nil || err.Error() != errTokenPermissions.Error() || strings.Contains(err.Error(), fake.token) {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("session started after a mismatched token")
	}
	entries, err := os.ReadDir(identity.Cwd)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cwd = %v %v", entries, err)
	}
	if strings.Contains(logs.String(), fake.token) {
		t.Fatalf("logs = %s", logs.String())
	}
	if state.credentialActive(0) {
		t.Fatal("credential_active without a delivered token")
	}
}

func TestForeignWorkspaceDoesNotStartOrMint(t *testing.T) {
	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	store := openTestCredentialStore(t)
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "import pathlib,os\npathlib.Path(os.environ['MARKER']).write_text('started')\n", account.Username, dataDir)
	state.host = newMemoryHost()
	state.mutate = true
	state.credentials = store
	state.remember(identity)
	state.registrar = &fakeRegistrar{Status: remoteStatusReady, KeyID: "ready-key"}
	fake := newRunTokenFake(t, map[string]string{"contents": "write", "metadata": "read"})
	state.tokens = fake.registrar(discardLogger())
	state.gitRemote = func(string, string) (string, error) { return t.TempDir(), nil }
	if err := os.WriteFile(filepath.Join(identity.Cwd, "notes.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := state.apply(privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, gitWrite, credentialPending)}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	_, err := state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome, NeedsToken: true, Env: map[string]string{"MARKER": marker},
	}, devNull(t), devNull(t), devNull(t))
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("error = %v", err)
	}
	if fake.posts.Load() != 0 {
		t.Fatalf("minted before a closed checkout: %d", fake.posts.Load())
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("session started")
	}
	kept, err := os.ReadFile(filepath.Join(identity.Cwd, "notes.txt"))
	if err != nil || string(kept) != "keep" {
		t.Fatalf("workspace = %q %v", kept, err)
	}
}

func TestExistingRepositoryIsFetched(t *testing.T) {
	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	store := openTestCredentialStore(t)
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "print('ok')\n", account.Username, dataDir)
	state.host = newMemoryHost()
	state.mutate = true
	state.credentials = store
	state.remember(identity)
	state.registrar = &fakeRegistrar{Status: remoteStatusReady, KeyID: "ready-key"}
	fixture := gitFixture(t)
	clone := exec.Command("git", "clone", fixture, identity.Cwd)
	clone.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("preclone: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(fixture, "next.txt"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := exec.Command("git", "add", "next.txt")
	commit.Dir = fixture
	commit.Env = gitFixtureEnv()
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	commit = exec.Command("git", "commit", "-m", "next")
	commit.Dir = fixture
	commit.Env = gitFixtureEnv()
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	head := exec.Command("git", "rev-parse", "HEAD")
	head.Dir = fixture
	head.Env = gitFixtureEnv()
	want, err := head.Output()
	if err != nil {
		t.Fatal(err)
	}
	state.gitRemote = func(string, string) (string, error) { return fixture, nil }
	fake := newRunTokenFake(t, map[string]string{"contents": "read", "metadata": "read"})
	state.tokens = fake.registrar(discardLogger())
	if _, err := state.apply(privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, gitRead, credentialPending)}); err != nil {
		t.Fatal(err)
	}
	var sawFetch bool
	state.observeGit = func(env, args []string) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "fetch") {
			sawFetch = true
		}
		if strings.Contains(joined, " clone ") || strings.HasSuffix(joined, " clone") {
			t.Fatalf("fetched repository was cloned again: %s", joined)
		}
		for _, item := range append(env, args...) {
			if strings.Contains(item, fake.token) {
				t.Fatalf("git saw the token: %s", item)
			}
		}
	}
	pid, err := state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome, NeedsToken: true,
	}, devNull(t), devNull(t), devNull(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.wait(pid); err != nil {
		t.Fatal(err)
	}
	if !sawFetch {
		t.Fatal("existing repository was not fetched")
	}
	got := exec.Command("git", "rev-parse", "origin/main")
	got.Dir = identity.Cwd
	got.Env = gitFixtureEnv()
	fetched, err := got.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(fetched)) != strings.TrimSpace(string(want)) {
		t.Fatalf("fetched %s want %s", fetched, want)
	}
}

func TestNoCredentialGrantsDoNotMintOrClone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		git        string
		credential string
	}{
		{name: "git none", git: gitNone, credential: credentialPending},
		{name: "public read", git: gitRead, credential: credentialNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
			store := openTestCredentialStore(t)
			report := filepath.Join(t.TempDir(), "report")
			state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "import os\nopen(os.environ['REPORT'],'w').write(os.environ.get('GITHUB_TOKEN') or 'NONE')\n", account.Username, dataDir)
			state.host = newMemoryHost()
			state.mutate = true
			state.credentials = store
			state.remember(identity)
			state.registrar = &fakeRegistrar{Status: remoteStatusReady, KeyID: "ready-key"}
			fake := newRunTokenFake(t, nil)
			state.tokens = fake.registrar(discardLogger())
			state.gitRemote = func(string, string) (string, error) {
				t.Fatal("checkout remote was requested")
				return "", nil
			}
			if _, err := state.apply(privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, tc.git, tc.credential)}); err != nil {
				t.Fatal(err)
			}
			pid, err := state.spawn(spawnRequest{
				Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
				RunDir: runDir, DshHome: dshHome, Env: map[string]string{"REPORT": report},
			}, devNull(t), devNull(t), devNull(t))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := state.wait(pid); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(waitFile(t, report)); got != "NONE" {
				t.Fatalf("token = %q", got)
			}
			entries, err := os.ReadDir(identity.Cwd)
			if err != nil || len(entries) != 0 {
				t.Fatalf("cwd = %v %v", entries, err)
			}
			if fake.posts.Load() != 0 {
				t.Fatalf("token posts = %d", fake.posts.Load())
			}
			if state.credentialActive(pid) {
				t.Fatal("credential_active without a token")
			}
		})
	}
}

func TestSharedUIDGrantDoesNotStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	python := writeExecutable(t, "#!/bin/sh\nprintf started > \"$MARKER\"\n")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	document := sampleRunInvocation("gen_notoken")
	document.Git = gitWrite
	document.Credential = credentialPending
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MARKER", marker)
	processRunner{pythonPath: python, source: "src", logger: logger, store: store}.Run(document)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("shared-uid session started")
	}
	events := readRunEvents(t, store, document.RunID)
	if hasType(events, lifecycleTypeStart) || hasType(events, lifecycleTypeSessionCreated) {
		t.Fatalf("events = %v", typesOf(events))
	}
	if !hasType(events, lifecycleTypeError) || !hasType(events, lifecycleTypeEnd) {
		t.Fatalf("events = %v", typesOf(events))
	}
	last := events[len(events)-1]
	if last.Type != lifecycleTypeEnd || !bytes.Contains(last.Data, []byte(endStateFailed)) {
		t.Fatalf("end = %#v", last)
	}
}

func TestAgentYAMLCannotDeclareTheRunToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "worker.yaml"), []byte("instructions: Test agent instructions.\ncwd: /tmp/token-work\nhome: /tmp/token-home\nenv:\n  GITHUB_TOKEN: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgents(dir); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("error = %v", err)
	}
}

func TestCanonicalRemoteRejectsTokenURLs(t *testing.T) {
	if !sameCheckoutRemote("git@github.com:octo-org/lab-widget.git", "ssh://git@github.com/octo-org/lab-widget") {
		t.Fatal("ssh forms did not match")
	}
	if !sameCheckoutRemote("https://github.com/octo-org/lab-widget", "git@github.com:octo-org/lab-widget.git") {
		t.Fatal("https form did not match the ssh remote")
	}
	if sameCheckoutRemote("https://ghs_run_token_value_0123456789@github.com/octo-org/lab-widget", "git@github.com:octo-org/lab-widget.git") {
		t.Fatal("token URL was accepted")
	}
	if allowCheckoutRemote("https://github.com/octo-org/lab-widget.git") || allowCheckoutRemote("git@github.com:octo-org/lab-widget.git") == false {
		t.Fatal("remote allow list changed")
	}
}

type gitCall struct {
	env  []string
	args []string
}

func assertGitUsedSocket(t *testing.T, calls []gitCall, token string) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("git was not invoked")
	}
	sawClone := false
	for _, call := range calls {
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, "clone") {
			sawClone = true
		}
		sock := false
		for _, item := range call.env {
			if strings.Contains(item, token) || strings.HasPrefix(item, githubTokenEnv+"=") || strings.HasPrefix(item, ghTokenEnv+"=") {
				t.Fatalf("git environment kept a token: %s", item)
			}
			if strings.HasPrefix(item, sshAuthSockEnv+"=") && strings.TrimPrefix(item, sshAuthSockEnv+"=") != "" {
				sock = true
			}
		}
		if !sock {
			t.Fatalf("git environment = %v", call.env)
		}
		for _, arg := range call.args {
			if strings.Contains(arg, token) || strings.Contains(arg, "ghs_") {
				t.Fatalf("git arg kept a token: %s", arg)
			}
		}
	}
	if !sawClone {
		t.Fatal("empty workspace was not cloned")
	}
}

func runTokenPython(t *testing.T) string {
	t.Helper()
	return "import os,time\n" +
		"token=os.environ.get('GITHUB_TOKEN') or ''\n" +
		"open(os.environ['REPORT'],'w').write(token)\n" +
		"open(os.environ['LEAK'],'w').write(token)\n" +
		"time.sleep(30)\n"
}

func gitFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitFixtureEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
}

func gitFixtureEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=Genesis Test",
		"GIT_AUTHOR_EMAIL=genesis@example.com",
		"GIT_COMMITTER_NAME=Genesis Test",
		"GIT_COMMITTER_EMAIL=genesis@example.com",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
}

type runTokenFake struct {
	t         *testing.T
	server    *httptest.Server
	key       *rsa.PrivateKey
	now       time.Time
	token     string
	returned  map[string]string
	posts     atomicInt
	requested map[string]string
	repos     []string
}

func newRunTokenFake(t *testing.T, returned map[string]string) *runTokenFake {
	t.Helper()
	fake := &runTokenFake{
		t: t, key: testAppKey(t), now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC),
		token: "ghs_run_token_value_0123456789", returned: clonePermissions(returned),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *runTokenFake) registrar(logger *slog.Logger) githubRegistrar {
	return githubRegistrar{
		baseURL:   f.server.URL,
		transport: f.server.Client().Transport,
		now:       func() time.Time { return f.now },
		logger:    logger,
		resolve: func(context.Context, grantRegistration) (githubAppBinding, error) {
			return githubAppBinding{
				Org: "octo-org", Repo: "lab-widget",
				AppID: "100001", InstallationID: "100002", Key: f.key,
			}, nil
		},
	}
}

func (f *runTokenFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/app/installations/100002/access_tokens" {
		http.NotFound(w, r)
		return
	}
	f.posts.Add(1)
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || strings.Count(strings.TrimPrefix(auth, "Bearer "), ".") != 2 {
		f.t.Errorf("authorization = %q", auth)
	}
	var request grantTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		f.t.Errorf("body: %v", err)
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	f.requested = request.Permissions
	f.repos = append([]string(nil), request.Repositories...)
	_ = json.NewEncoder(w).Encode(gitHubTokenResponse{
		Token:               f.token,
		ExpiresAt:           f.now.Add(time.Hour),
		Permissions:         f.returned,
		RepositorySelection: "selected",
		Repositories:        []gitHubRepoRef{{ID: 4242, Name: "lab-widget", FullName: "octo-org/lab-widget"}},
	})
}
