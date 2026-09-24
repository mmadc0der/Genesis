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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRepositoryObservationReadsWithoutMutation(t *testing.T) {
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	fake := newObservationFake(t, frozen)
	state, reposDir := observationState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))

	first, err := state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != observationObserved || first.Journal == nil || len(first.Journal.Bindings) != 1 {
		t.Fatalf("observation = %#v", first)
	}
	binding := first.Journal.Bindings[0]
	if binding.ID != "lab" || binding.Org != "octo-org" || binding.Name != "lab-widget" || binding.RepositoryID != 4242 || binding.NodeID != "R_testNode" {
		t.Fatalf("binding = %#v", binding)
	}
	if len(first.Observed) != 1 || first.Observed[0].RepositoryID != 4242 || first.Observed[0].Visibility != visibilityPrivate {
		t.Fatalf("observed = %#v", first.Observed)
	}
	fields := map[string]string{}
	for _, item := range first.Drift {
		fields[item.Field] = item.Status
		if item.Status == driftDrift {
			t.Fatalf("unexpected settings drift %#v", item)
		}
	}
	for _, field := range []string{"bootstrap.template", "secrets", "secrets.environments", "identities", "lifecycle.remove"} {
		if fields[field] == "" {
			t.Fatalf("missing classification %s in %#v", field, first.Drift)
		}
	}
	for _, item := range first.Unsupported {
		if item.Kind == intentEnsureRepository || item.Kind == intentEnsureActions || item.Kind == intentEnsureProtection {
			t.Fatalf("observed intent marked unsupported: %#v", item)
		}
	}
	if err := repositoryPlanIsReadOnly(observationPlan(first)); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(first)
	for _, forbidden := range []string{"ghs_", "ghp_", "github_pat_", "BEGIN ", "eyJ", programmerSecret, "PRIVATE KEY"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("observation leaked %q", forbidden)
		}
	}
	if fake.mutations.Load() != 0 || fake.posts.Load() == 0 {
		t.Fatalf("mutations=%d tokenPosts=%d methods=%v", fake.mutations.Load(), fake.posts.Load(), fake.methods)
	}
	for _, method := range fake.methods {
		verb, _, _ := strings.Cut(method, " ")
		if verb != http.MethodGet && verb != http.MethodPost {
			t.Fatalf("method %s", method)
		}
		if verb == http.MethodPost && !strings.Contains(method, "/access_tokens") {
			t.Fatalf("unexpected post %s", method)
		}
	}

	second, err := state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if second.Journal.Bindings[0] != binding {
		t.Fatalf("binding changed %#v", second.Journal.Bindings[0])
	}
	if fake.repoCalls.Load() < 4 {
		t.Fatalf("expected a second read, calls=%d", fake.repoCalls.Load())
	}
}

func TestRepositoryObservationLogsScopeWithoutSecrets(t *testing.T) {
	var logs bytes.Buffer
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	fake := newObservationFake(t, frozen)
	state, reposDir := observationState(t, fake, frozen)
	registrar := state.registrar.(githubRegistrar)
	registrar.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	state.registrar = registrar
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	if _, err := state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir)); err != nil {
		t.Fatal(err)
	}
	text := logs.String()
	for _, forbidden := range []string{"ghs_", "ghp_", "github_pat_", "BEGIN ", "eyJ", "PRIVATE KEY"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, text)
		}
	}
	var posts, gets, scopes int
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		switch entry["msg"] {
		case "github call":
			method, _ := entry["method"].(string)
			path, _ := entry["path"].(string)
			switch method {
			case http.MethodGet:
				gets++
			case http.MethodPost:
				posts++
				if !strings.HasSuffix(path, "/access_tokens") {
					t.Fatalf("post %s", path)
				}
			default:
				t.Fatalf("method %s", method)
			}
		case "github token scope":
			scopes++
			if entry["requested"] != "administration=read,metadata=read" || entry["returned"] != "administration=read,metadata=read" {
				t.Fatalf("scope = %#v", entry)
			}
		default:
			t.Fatalf("unexpected log %s", line)
		}
	}
	if posts < 2 || gets < 2 || scopes < 2 {
		t.Fatalf("posts=%d gets=%d scopes=%d logs=%s", posts, gets, scopes, text)
	}
}

func TestPermissionLogRedactsUnexpectedValues(t *testing.T) {
	got := formatPermissionMap(map[string]string{
		"administration": "read",
		"metadata":       "write",
		"contents":       "ghs_abcdefghijklmnopqrstuvwxyz",
	})
	if got != "administration=read,contents=redacted,metadata=write" {
		t.Fatalf("permissions = %s", got)
	}
	if redactGitHubPath("/repos/octo-org/lab-widget?per_page=100") != "/repos/octo-org/lab-widget" {
		t.Fatal("query was kept")
	}
	if redactGitHubPath("/repos/octo-org/ghs_abcdefghijklmnopqrstuvwxyz") != "redacted" {
		t.Fatal("token path was kept")
	}
}

func TestRepositoryObservationFailClosed(t *testing.T) {
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	cases := []struct {
		name    string
		prepare func(*observationFake)
		want    string
	}{
		{name: "missing", prepare: func(f *observationFake) { f.repoStatus = http.StatusNotFound }, want: errRepositoryNotFound.Error()},
		{name: "owner", prepare: func(f *observationFake) { f.owner = "other-org" }, want: errRepositoryIdentity.Error()},
		{name: "name", prepare: func(f *observationFake) { f.repoName = "other-widget" }, want: errRepositoryIdentity.Error()},
		{name: "malformed", prepare: func(f *observationFake) { f.omitNode = true }, want: errRepositoryMalformed.Error()},
		{name: "auth scope", prepare: func(f *observationFake) { f.administration = "write" }, want: errRepositoryAuthScope.Error()},
		{name: "extra scope", prepare: func(f *observationFake) { f.extraPermission = "contents" }, want: errRepositoryAuthScope.Error()},
		{name: "rate", prepare: func(f *observationFake) { f.rate = true }, want: errRepositoryRate.Error()},
		{name: "truncated", prepare: func(f *observationFake) { f.truncateRules = true }, want: errRepositoryTruncated.Error()},
		{name: "secret description", prepare: func(f *observationFake) { f.description = "token ghp_abcdefghijklmnopqrstuvwxyz" }, want: errRepositoryMalformed.Error()},
		{name: "forbidden", prepare: func(f *observationFake) { f.repoStatus = http.StatusForbidden }, want: errRepositoryAuthScope.Error()},
		{name: "not modified", prepare: func(f *observationFake) { f.repoStatus = http.StatusNotModified }, want: errRepositoryMalformed.Error()},
		{name: "truncated body", prepare: func(f *observationFake) { f.repoBody = `{"id":` }, want: errRepositoryMalformed.Error()},
		{name: "pinned identity", prepare: func(f *observationFake) { f.secondMismatch = true }, want: errRepositoryMalformed.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newObservationFake(t, frozen)
			tc.prepare(fake)
			state, reposDir := observationState(t, fake, frozen)
			writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
			_, err := state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "ghs_") {
				t.Fatalf("error = %v", err)
			}
			if _, statErr := os.Lstat(filepath.Join(state.dataDir, observationDirName, observationFileName)); !os.IsNotExist(statErr) {
				t.Fatal("failure wrote a journal")
			}
			if fake.mutations.Load() != 0 {
				t.Fatalf("mutations=%d", fake.mutations.Load())
			}
		})
	}
}

func TestRepositoryObservationPersistedIdentity(t *testing.T) {
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	fake := newObservationFake(t, frozen)
	state, reposDir := observationState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	first, err := state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRepositoryJournal(state.dataDir, *first.Journal); err != nil {
		t.Fatal(err)
	}
	fake.repositoryID = 9999
	_, err = state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err == nil || !strings.Contains(err.Error(), errRepositoryPersisted.Error()) {
		t.Fatalf("replaced repository = %v", err)
	}
	journal, err := readRepositoryJournal(state.dataDir)
	if err != nil || journal.Bindings[0].RepositoryID != 4242 {
		t.Fatalf("journal changed: %v %#v", err, journal.Bindings)
	}

	fake.repositoryID = 4242
	body := validRepositoryYAML("other-org", "lab-widget")
	writeRepoFile(t, reposDir, "lab.yaml", body)
	_, err = state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err == nil || !strings.Contains(err.Error(), errRepositoryRetarget.Error()) {
		t.Fatalf("retarget = %v", err)
	}
	journal, err = readRepositoryJournal(state.dataDir)
	if err != nil || journal.Bindings[0].Org != "octo-org" || journal.Bindings[0].RepositoryID != 4242 {
		t.Fatalf("retarget wrote a new identity: %v %#v", err, journal)
	}
}

func TestRepositoryObservationDesiredChangeAndRace(t *testing.T) {
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	fake := newObservationFake(t, frozen)
	state, reposDir := observationState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	entered := make(chan struct{})
	release := make(chan struct{})
	fake.blockRepo = func() {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
		errCh <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("observation did not reach GitHub")
	}
	writeRepoFile(t, reposDir, "lab.yaml", strings.ReplaceAll(validRepositoryYAML("octo-org", "lab-widget"), "Lab widget service.", "Changed during observation."))
	close(release)
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), errDesiredChanged.Error()) {
		t.Fatalf("concurrent change = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(state.dataDir, observationDirName, observationFileName)); !os.IsNotExist(statErr) {
		t.Fatal("racy observation wrote a journal")
	}
}

func TestRepositoryObservationSyncPreservesGeneration(t *testing.T) {
	server := newSyncTestServer(t, nil)
	reposDir := filepath.Join(t.TempDir(), "repos.d")
	if err := os.Mkdir(reposDir, 0o700); err != nil {
		t.Fatal(err)
	}
	server.reposDir = reposDir
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	if response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}}); response.Code != http.StatusOK {
		t.Fatalf("seed = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	seeded := server.generation.digest
	server.mu.RUnlock()

	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	fake := newObservationFake(t, frozen)
	fake.repoStatus = http.StatusNotFound
	state, _ := observationState(t, fake, frozen)
	state.reposDir = reposDir
	state.dataDir = server.store.dataDir
	server.coordinator = stateCoordinator{state: state}
	writeRepoFile(t, reposDir, "lab.yaml", strings.ReplaceAll(validRepositoryYAML("octo-org", "lab-widget"), "Lab widget service.", "Should stay inactive."))
	response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeAgents, scopeRules, scopeRepos}})
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "privileged coordination failed") {
		t.Fatalf("failed observation = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	kept := server.generation.digest == seeded && server.generation.repos["lab"].Settings.Description != "Should stay inactive."
	server.mu.RUnlock()
	if !kept {
		t.Fatal("failed observation swapped the generation")
	}
	if _, statErr := os.Lstat(filepath.Join(server.store.dataDir, observationDirName, observationFileName)); !os.IsNotExist(statErr) {
		t.Fatal("failed sync wrote a journal")
	}

	agents := &captureCoordinator{}
	server.coordinator = agents
	if response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeAgents}}); response.Code != http.StatusOK {
		t.Fatalf("agents sync = %d %s", response.Code, response.Body.String())
	}
	if len(agents.plans) != 1 || agents.plans[0].ObserveRepositories {
		t.Fatalf("agents sync observed repositories: %#v", agents.plans)
	}
}

func TestRepositoryObservationControlView(t *testing.T) {
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	fake := newObservationFake(t, frozen)
	server := newSyncTestServer(t, nil)
	reposDir := filepath.Join(t.TempDir(), "repos.d")
	if err := os.Mkdir(reposDir, 0o700); err != nil {
		t.Fatal(err)
	}
	server.reposDir = reposDir
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	state, _ := observationState(t, fake, frozen)
	state.reposDir = reposDir
	state.dataDir = server.store.dataDir
	server.coordinator = stateCoordinator{state: state}

	response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"observation":"observed"`) || !strings.Contains(response.Body.String(), `"remote_mutation":"none"`) {
		t.Fatalf("sync = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "ghs_") || strings.Contains(response.Body.String(), programmerSecret) {
		t.Fatalf("sync leaked material: %s", response.Body.String())
	}
	var body syncResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.RepositoryPlan.Applied) != 0 || body.RepositoryPlan.Observed[0].RepositoryID != 4242 {
		t.Fatalf("plan = %#v", body.RepositoryPlan)
	}

	upstream := httptest.NewServer(server)
	t.Cleanup(upstream.Close)
	control := newControlServer(controlConfig{
		listenerURL: upstream.URL,
		agentsDir:   server.agentsDir,
		rulesDir:    server.rulesDir,
		reposDir:    reposDir,
		dataDir:     server.store.dataDir,
		syncToken:   "sync-secret",
	}, discardLogger())
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)
	catalog := getJSON[map[string]any](t, panel.URL+"/api/repositories")
	encoded, _ := json.Marshal(catalog)
	if strings.Contains(string(encoded), "ghs_") || strings.Contains(string(encoded), "BEGIN ") {
		t.Fatalf("catalog leaked material: %s", encoded)
	}
	repos, _ := catalog["repositories"].([]any)
	if len(repos) != 1 {
		t.Fatalf("catalog = %#v", catalog)
	}
	one, _ := repos[0].(map[string]any)
	observed, _ := one["observed"].(map[string]any)
	if one["observation"] != observationObserved || observed["repository_id"] != float64(4242) || observed["node_id"] != "R_testNode" {
		t.Fatalf("repository view = %#v", one)
	}
	drift, _ := one["drift"].([]any)
	if len(drift) == 0 {
		t.Fatal("drift was not exposed")
	}
	for _, method := range fake.methods {
		if strings.Contains(method, "/keys") || strings.Contains(method, "PATCH") || strings.Contains(method, "PUT") || strings.Contains(method, "DELETE") {
			t.Fatalf("repos sync reached a mutation route: %s", method)
		}
	}

	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("other-org", "lab-widget"))
	retarget := getJSON[listedRepository](t, panel.URL+"/api/repositories/lab")
	if retarget.Presence != presenceDraft || retarget.Org != "other-org" || retarget.Observed != nil || retarget.Observation != "" {
		t.Fatalf("retargeted draft kept the previous identity: %#v", retarget)
	}
}

func TestRepositoryObservationRefuseDoesNotCallGitHub(t *testing.T) {
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	fake := newObservationFake(t, frozen)
	state, reposDir := observationState(t, fake, frozen)
	writeRepoFile(t, reposDir, "kept.yaml", strings.ReplaceAll(validRepositoryYAML("octo-org", "lab-widget"), "existing: adopt", "existing: refuse"))
	result, err := state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != observationObserved || fake.posts.Load() != 0 || fake.repoCalls.Load() != 0 {
		t.Fatalf("refuse called GitHub status=%s posts=%d gets=%d", result.Status, fake.posts.Load(), fake.repoCalls.Load())
	}
	if len(result.Drift) != 1 || result.Drift[0].Field != "lifecycle.existing" || result.Drift[0].Status != driftUnsupported {
		t.Fatalf("refuse drift = %#v", result.Drift)
	}
}

func TestActionsAndRulesetsUnobservable(t *testing.T) {
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	fake := newObservationFake(t, frozen)
	fake.actionsStatus = http.StatusNotFound
	fake.rulesStatus = http.StatusNotFound
	fake.branchStatus = http.StatusNotFound
	state, reposDir := observationState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	result, err := state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for _, item := range result.Drift {
		fields[item.Field] = item.Status
	}
	if fields["actions.enabled"] != driftUnobservable || fields["protection.ruleset"] != driftUnobservable || fields["protection.branch"] != driftUnobservable {
		t.Fatalf("fields = %#v", result.Drift)
	}
	if result.Observed[0].Visibility != visibilityPrivate || result.Observed[0].RepositoryID != 4242 {
		t.Fatalf("settings were dropped: %#v", result.Observed[0])
	}
}

type stateCoordinator struct {
	state *privilegedState
}

func (c stateCoordinator) Coordinate(ctx context.Context, plan privilegedPlan) (coordinateResult, error) {
	return executePlan(plan, c.state)
}

type captureCoordinator struct {
	mu    sync.Mutex
	plans []privilegedPlan
}

func (c *captureCoordinator) Coordinate(ctx context.Context, plan privilegedPlan) (coordinateResult, error) {
	c.mu.Lock()
	c.plans = append(c.plans, plan)
	c.mu.Unlock()
	return evaluatePlan(plan)
}

func mustRepositoryDigest(t *testing.T, reposDir string) string {
	t.Helper()
	repos, active, err := loadRepositories(reposDir)
	if err != nil || !active {
		t.Fatalf("load repos: %v active=%v", err, active)
	}
	digest, err := repositoryDigest(repos)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func observationPlan(result repositoryObservationResult) repositoryPlan {
	return repositoryPlan{
		Requested: true, Active: true, RemoteMutation: remoteMutationNone, Observation: result.Status,
		Applied: []string{}, Intents: []repositoryIntent{{Kind: intentEnsureRepository, ID: "lab"}},
		Observed: result.Observed, Drift: result.Drift, Unsupported: result.Unsupported,
	}
}

func observationState(t *testing.T, fake *observationFake, now time.Time) (*privilegedState, string) {
	t.Helper()
	reposDir := t.TempDir()
	providersDir := t.TempDir()
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	dataDir := t.TempDir()
	if _, err := prepareDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	secrets := openTestSecretStore(t)
	key := testAppKey(t)
	fake.key = key
	secretName := "GITHUB_APP_RECONCILER_PEM"
	writeRepoFile(t, providersDir, "github.yaml", reconcilerProviderYAML(secretName))
	writeSecretPEM(t, secrets.root, secretName, key)
	state := &privilegedState{
		logger: discardLogger(), host: unixHost{}, mutate: true,
		dataDir: dataDir, secrets: secrets, agentsDir: agentsDir, rulesDir: rulesDir,
		reposDir: reposDir, providersDir: providersDir,
	}
	state.registrar = githubRegistrar{
		baseURL: fake.server.URL, transport: fake.server.Client().Transport,
		now: func() time.Time { return now }, resolveRepo: state.resolveReconcilerRepository,
	}
	return state, reposDir
}

type observationFake struct {
	t               *testing.T
	server          *httptest.Server
	key             any
	now             time.Time
	mu              sync.Mutex
	methods         []string
	posts           atomicInt
	mutations       atomicInt
	repoCalls       atomicInt
	repoStatus      int
	actionsStatus   int
	rulesStatus     int
	branchStatus    int
	owner           string
	repoName        string
	description     string
	repositoryID    int64
	omitNode        bool
	administration  string
	extraPermission string
	rate            bool
	truncateRules   bool
	blockRepo       func()
	secondMismatch  bool
	defaultBranch   string
	omitPinning     bool
	extraRule       bool
	omitDismiss     bool
	repoBody        string
}

func newObservationFake(t *testing.T, now time.Time) *observationFake {
	t.Helper()
	fake := &observationFake{t: t, now: now, administration: "read", repositoryID: 4242}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *observationFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.methods = append(f.methods, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
		f.t.Errorf("conditional github request %s", r.URL.Path)
	}
	if r.Method == http.MethodGet && r.ContentLength > 0 {
		f.t.Errorf("github GET included a body %s", r.URL.Path)
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		if r.URL.Path != "/app/installations/100002/access_tokens" {
			f.mutations.Add(1)
			http.Error(w, "mutation refused", http.StatusMethodNotAllowed)
			return
		}
		f.posts.Add(1)
		f.serveToken(w, r)
		return
	case http.MethodPatch:
		if r.URL.Path != "/repos/octo-org/lab-widget" {
			f.mutations.Add(1)
			http.Error(w, "mutation refused", http.StatusMethodNotAllowed)
			return
		}
		var patch map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			f.t.Errorf("settings patch: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if _, ok := patch["default_branch"]; ok {
			f.t.Errorf("settings patch changed the default branch")
		}
		if _, ok := patch["archived"]; ok {
			f.t.Errorf("settings patch changed archived")
		}
		if raw, ok := patch["description"]; ok {
			var description string
			if err := json.Unmarshal(raw, &description); err != nil {
				f.t.Errorf("description patch: %v", err)
			} else {
				f.description = description
			}
		}
		f.serveRepo(w)
		return
	default:
		f.mutations.Add(1)
		http.Error(w, "mutation refused", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case r.URL.Path == "/repos/octo-org/lab-widget":
		f.repoCalls.Add(1)
		if f.blockRepo != nil {
			f.blockRepo()
		}
		if f.rate {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if f.repoStatus != 0 {
			w.WriteHeader(f.repoStatus)
			return
		}
		f.serveRepo(w)
	case r.URL.Path == "/repos/octo-org/lab-widget/actions/permissions":
		if f.actionsStatus != 0 {
			w.WriteHeader(f.actionsStatus)
			return
		}
		body := map[string]any{"enabled": true, "allowed_actions": "selected"}
		if !f.omitPinning {
			body["sha_pinning_required"] = false
		}
		_ = json.NewEncoder(w).Encode(body)
	case r.URL.Path == "/repos/octo-org/lab-widget/actions/permissions/selected-actions":
		_ = json.NewEncoder(w).Encode(map[string]any{"github_owned_allowed": false, "verified_allowed": false, "patterns_allowed": []string{"actions/checkout@v4"}})
	case r.URL.Path == "/repos/octo-org/lab-widget/rulesets":
		if f.rulesStatus != 0 {
			w.WriteHeader(f.rulesStatus)
			return
		}
		if f.truncateRules {
			w.Header().Set("Link", "<"+f.server.URL+"/repos/octo-org/lab-widget/rulesets?per_page=100&page=2>; rel=\"next\"")
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": 9, "name": "protect-main", "target": "branch", "source_type": "Repository", "source": "octo-org/lab-widget", "enforcement": "active",
		}})
	case strings.HasPrefix(r.URL.Path, "/repos/octo-org/lab-widget/rulesets/") && f.truncateRules:
		w.Header().Set("Link", "<"+f.server.URL+r.URL.Path+"?per_page=100&page=2>; rel=\"next\"")
		_, _ = w.Write([]byte("[]"))
	case r.URL.Path == "/repos/octo-org/lab-widget/rulesets/9":
		pull := map[string]any{"required_approving_review_count": 1, "require_code_owner_review": false, "require_last_push_approval": false, "required_review_thread_resolution": false}
		if !f.omitDismiss {
			pull["dismiss_stale_reviews_on_push"] = true
		}
		rules := []map[string]any{
			{"type": "pull_request", "parameters": pull},
			{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": true, "required_status_checks": []map[string]any{{"context": "ci"}}}},
		}
		if f.extraRule {
			rules = append(rules, map[string]any{"type": "deletion"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 9, "name": "protect-main", "target": "branch", "source_type": "Repository", "enforcement": "active",
			"conditions": map[string]any{"ref_name": map[string]any{"include": []string{"refs/heads/main"}, "exclude": []string{}}},
			"rules":      rules,
		})
	case r.URL.Path == "/repos/octo-org/lab-widget/contents/.github/workflows/ci.yml":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "encoding": "base64", "name": "ci.yml",
			"path": ".github/workflows/ci.yml", "sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"content": "Y2kK",
		})
	case r.URL.Path == "/repos/octo-org/lab-widget/rules/branches/main":
		if f.branchStatus != 0 {
			w.WriteHeader(f.branchStatus)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"type": "pull_request", "ruleset_source_type": "Repository", "ruleset_id": 9, "parameters": map[string]any{"required_approving_review_count": 1, "dismiss_stale_reviews_on_push": true}},
			{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": true, "required_status_checks": []map[string]any{{"context": "ci"}}}},
		})
	default:
		http.NotFound(w, r)
	}
}

func (f *observationFake) serveToken(w http.ResponseWriter, r *http.Request) {
	var request gitHubTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		f.t.Errorf("token body: %v", err)
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	var permissions map[string]string
	switch {
	case request.Permissions.Administration == "write" && request.Permissions.Metadata == "read" && request.Permissions.Contents == "":
		permissions = map[string]string{"administration": "write", "metadata": "read"}
	case request.Permissions.Contents == "write" && request.Permissions.Metadata == "read" && request.Permissions.Administration == "":
		permissions = map[string]string{"contents": "write", "metadata": "read"}
	case request.Permissions.Administration == "read" && request.Permissions.Metadata == "read" && request.Permissions.Contents == "":
		permissions = map[string]string{"administration": f.administration, "metadata": "read"}
		if f.extraPermission != "" {
			permissions[f.extraPermission] = "read"
		}
	default:
		f.t.Errorf("unexpected token permissions %#v", request.Permissions)
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	id := f.repositoryID
	if id == 0 {
		id = 4242
	}
	_ = json.NewEncoder(w).Encode(gitHubTokenResponse{
		Token: "ghs_test_observation_token_value", ExpiresAt: f.now.Add(time.Hour),
		Permissions: permissions, RepositorySelection: "selected",
		Repositories: []gitHubRepoRef{{ID: id, Name: "lab-widget", FullName: "octo-org/lab-widget"}},
	})
}

func (f *observationFake) serveRepo(w http.ResponseWriter) {
	owner := f.owner
	if owner == "" {
		owner = "octo-org"
	}
	name := f.repoName
	if name == "" {
		name = "lab-widget"
	}
	description := f.description
	if description == "" {
		description = "Lab widget service. This declaration is not applied."
	}
	id := f.repositoryID
	if id == 0 {
		id = 4242
	}
	branch := f.defaultBranch
	if branch == "" {
		branch = "main"
	}
	payload := map[string]any{
		"id": id, "node_id": "R_testNode", "name": name, "full_name": owner + "/" + name,
		"owner": map[string]any{"login": owner}, "private": true, "visibility": "private",
		"description": description, "default_branch": branch, "archived": false,
		"has_issues": true, "has_wiki": false, "has_projects": false,
		"allow_squash_merge": true, "allow_merge_commit": false, "allow_rebase_merge": false,
		"delete_branch_on_merge": true,
	}
	if f.omitNode {
		delete(payload, "node_id")
	}
	if f.secondMismatch && f.repoCalls.Load() >= 2 {
		payload["owner"] = map[string]any{"login": "evil-org"}
		payload["name"] = "other-widget"
		payload["full_name"] = "octo-org/lab-widget"
	}
	if f.repoBody != "" {
		_, _ = w.Write([]byte(f.repoBody))
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

type atomicInt struct{ value int64 }

func (a *atomicInt) Add(n int64) { a.value += n }
func (a *atomicInt) Load() int64 { return a.value }

func TestRepositoryObservationVisibilityGaps(t *testing.T) {
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	cases := []struct {
		name    string
		prepare func(*observationFake)
		field   string
		status  string
		absent  string
	}{
		{name: "sha pinning omitted", prepare: func(f *observationFake) { f.omitPinning = true }, field: "actions.sha_pinning", status: driftUnobservable},
		{name: "unmodeled rule", prepare: func(f *observationFake) { f.extraRule = true }, field: "protection.ruleset.additional_rules", status: driftUnobservable},
		{name: "dismiss omitted", prepare: func(f *observationFake) { f.omitDismiss = true }, field: "protection.ruleset.dismiss_stale_reviews", status: driftUnobservable},
		{name: "slash branch", prepare: func(f *observationFake) { f.defaultBranch = "release/1" }, field: "protection.branch", status: driftUnobservable, absent: "/rules/branches/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newObservationFake(t, frozen)
			tc.prepare(fake)
			state, reposDir := observationState(t, fake, frozen)
			writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
			result, err := state.observeDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range result.Drift {
				if item.Status == driftDrift && tc.name != "slash branch" && item.Field != "settings.default_branch" && item.Field != "protection.ruleset.target" {
					t.Fatalf("unexpected drift %#v", item)
				}
				if item.Field == tc.field && item.Status == tc.status {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s %s in %#v", tc.field, tc.status, result.Drift)
			}
			for _, method := range fake.methods {
				if tc.absent != "" && strings.Contains(method, tc.absent) {
					t.Fatalf("requested %s via %s", tc.absent, method)
				}
				verb, _, _ := strings.Cut(method, " ")
				if verb != http.MethodGet && verb != http.MethodPost {
					t.Fatalf("method %s", method)
				}
			}
			if fake.mutations.Load() != 0 {
				t.Fatalf("mutations=%d", fake.mutations.Load())
			}
		})
	}
}

func TestRepositoryJournalRejectsUnsafeReplacement(t *testing.T) {
	dir := t.TempDir()
	if _, err := prepareDataDir(dir); err != nil {
		t.Fatal(err)
	}
	journal := repositoryJournal{
		Version: observationVersion, Digest: "abc",
		Bindings: []repositoryBinding{{ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 4242, NodeID: "R_testNode"}},
		Observed: []repositoryObserved{{ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 4242, NodeID: "R_testNode", Visibility: "private", ActionsStatus: observationObserved, RulesetStatus: observationObserved, BranchStatus: observationObserved}},
	}
	if err := writeRepositoryJournal(dir, journal); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, observationDirName, observationFileName)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	journal.Observed[0].Description = strings.Repeat("a", 1<<20)
	if err := writeRepositoryJournal(dir, journal); err == nil {
		t.Fatal("oversized journal was accepted")
	}
	second, err := os.ReadFile(path)
	if err != nil || string(first) != string(second) {
		t.Fatal("oversized write replaced the journal")
	}
	journal.Observed[0].Description = ""
	journal.Bindings = append(journal.Bindings, repositoryBinding{ID: "other", Org: "octo-org", Name: "other-widget", RepositoryID: 7, NodeID: "R_testNode"})
	if err := journal.normalize(); err == nil {
		t.Fatal("duplicate node id was accepted")
	}
}

func TestRepositoryJournalWriteFailurePreservesGeneration(t *testing.T) {
	frozen := time.Date(2026, 9, 23, 14, 49, 0, 0, time.UTC)
	fake := newObservationFake(t, frozen)
	server := newSyncTestServer(t, nil)
	reposDir := filepath.Join(t.TempDir(), "repos.d")
	if err := os.Mkdir(reposDir, 0o700); err != nil {
		t.Fatal(err)
	}
	server.reposDir = reposDir
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	state, _ := observationState(t, fake, frozen)
	state.reposDir = reposDir
	state.dataDir = server.store.dataDir
	server.coordinator = stateCoordinator{state: state}
	if response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}}); response.Code != http.StatusOK {
		t.Fatalf("seed = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	seeded := server.generation.digest
	server.mu.RUnlock()
	journalPath := filepath.Join(server.store.dataDir, observationDirName, observationFileName)
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(server.store.dataDir, observationDirName, ".repositories.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, reposDir, "lab.yaml", strings.ReplaceAll(validRepositoryYAML("octo-org", "lab-widget"), "Lab widget service.", "Should stay inactive."))
	response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}})
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "repository observation journal failed") {
		t.Fatalf("blocked journal = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	kept := server.generation.digest == seeded
	server.mu.RUnlock()
	after, err := os.ReadFile(journalPath)
	if err != nil || !kept || string(before) != string(after) {
		t.Fatal("failed journal write replaced the generation or the previous journal")
	}
}

func TestRepositoryObservationJournalRoundTrip(t *testing.T) {
	journal := repositoryJournal{
		Version: observationVersion, Digest: "abc",
		Bindings: []repositoryBinding{{ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 4242, NodeID: "R_testNode"}},
		Observed: []repositoryObserved{{ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 4242, NodeID: "R_testNode", Visibility: "private", ActionsStatus: observationObserved, RulesetStatus: observationObserved, BranchStatus: observationObserved}},
	}
	dir := t.TempDir()
	if _, err := prepareDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := writeRepositoryJournal(dir, journal); err != nil {
		t.Fatal(err)
	}
	loaded, err := readRepositoryJournal(dir)
	if err != nil || loaded.Bindings[0].NodeID != "R_testNode" {
		t.Fatalf("reload = %v %#v", err, loaded)
	}
	payload, err := os.ReadFile(filepath.Join(dir, observationDirName, observationFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "ghs_") || strings.Contains(string(payload), "BEGIN ") {
		t.Fatalf("journal leaked material: %s", payload)
	}
	info, err := os.Stat(filepath.Join(dir, observationDirName, observationFileName))
	if err != nil || info.Mode().Perm() != observationFileMode {
		t.Fatalf("mode = %v %v", info, err)
	}
	dirInfo, err := os.Lstat(filepath.Join(dir, observationDirName))
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || dirInfo.Mode().Perm() != observationDirMode {
		t.Fatalf("directory mode = %v %v", dirInfo, err)
	}
	_ = io.EOF
}
