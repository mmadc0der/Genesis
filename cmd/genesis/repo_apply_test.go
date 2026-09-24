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

func TestRepositoryApplyCreatesBindsAndShapes(t *testing.T) {
	frozen := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	fake := newApplyGitHub(t, frozen)
	fake.exists = false
	state, reposDir := applyState(t, fake, frozen)
	var logs bytes.Buffer
	registrar := state.registrar.(githubRegistrar)
	registrar.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	state.registrar = registrar
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))

	first, err := state.reconcileDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if first.RemoteMutation != remoteMutationApplied || first.Journal == nil || len(first.Journal.Bindings) != 1 {
		t.Fatalf("apply = %#v", first)
	}
	binding := first.Journal.Bindings[0]
	if binding.RepositoryID != 4242 || binding.NodeID != "R_testNode" || binding.Org != "octo-org" || binding.Name != "lab-widget" {
		t.Fatalf("binding = %#v", binding)
	}
	for _, kind := range []string{intentEnsureRepository, intentEnsureActions, intentEnsureProtection, intentEnsureBootstrap} {
		if !containsString(first.Applied, kind+":lab") {
			t.Fatalf("missing applied %s in %#v", kind, first.Applied)
		}
	}
	for _, item := range first.Unsupported {
		switch item.Kind {
		case intentEnsureSecrets, intentEnsureIdentities, intentRetainOnRemove:
			if item.Reason == "" || item.Reason == repositoryNotAppliedReason {
				t.Fatalf("unsupported = %#v", item)
			}
		default:
			t.Fatalf("applied intent left unsupported: %#v", item)
		}
	}
	if err := repositoryPlanRecordsApply(appliedPlan(first)); err != nil {
		t.Fatal(err)
	}
	create := fake.body("POST", "/orgs/octo-org/repos")
	for _, needle := range []string{`"auto_init":true`, `"visibility":"private"`, `"name":"lab-widget"`, "Lab widget service."} {
		if !strings.Contains(create, needle) {
			t.Fatalf("create body missing %s: %s", needle, create)
		}
	}
	for _, absent := range []string{"default_branch", "has_issues", "archived"} {
		if strings.Contains(create, absent) {
			t.Fatalf("create body included %s: %s", absent, create)
		}
	}
	patch := fake.body("PATCH", "/repos/octo-org/lab-widget")
	if strings.Contains(patch, "default_branch") || strings.Contains(patch, "archived") || !strings.Contains(patch, `"has_wiki":false`) {
		t.Fatalf("settings patch = %s", patch)
	}
	ruleset := fake.body("POST", "/repos/octo-org/lab-widget/rulesets")
	if !strings.Contains(ruleset, `"include":["refs/heads/main"]`) || !strings.Contains(ruleset, `"context":"ci"`) || strings.Contains(ruleset, "~DEFAULT_BRANCH") {
		t.Fatalf("ruleset = %s", ruleset)
	}
	file := fake.body("PUT", "/repos/octo-org/lab-widget/contents/.github/workflows/ci.yml")
	if strings.Contains(file, `"sha"`) || !strings.Contains(file, `"branch":"main"`) || !strings.Contains(file, "from template lab-widget") {
		t.Fatalf("bootstrap = %s", file)
	}
	if fake.count("DELETE", "") != 0 || fake.count("POST", "/repos/octo-org/lab-widget/merge") != 0 {
		t.Fatalf("destructive calls = %#v", fake.methods)
	}
	text := logs.String()
	for _, forbidden := range []string{"ghs_", "ghp_", "github_pat_", "BEGIN ", "eyJ", "PRIVATE KEY"} {
		if strings.Contains(text, forbidden) || strings.Contains(mustJSON(t, first), forbidden) {
			t.Fatalf("leaked %s", forbidden)
		}
	}
	if !strings.Contains(text, `"requested":"administration=write,metadata=read"`) || !strings.Contains(text, `"requested":"contents=write,metadata=read"`) {
		t.Fatalf("scopes missing: %s", text)
	}

	second, err := state.reconcileDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if second.RemoteMutation != remoteMutationNone || len(second.Applied) != 0 || second.Journal.Bindings[0] != binding {
		t.Fatalf("second apply = %#v binding %#v", second, second.Journal.Bindings)
	}
	if fake.count("POST", "/orgs/octo-org/repos") != 1 || fake.count("PUT", "/repos/octo-org/lab-widget/contents/.github/workflows/ci.yml") != 1 {
		t.Fatalf("second apply repeated writes: %#v", fake.methods)
	}
}

func TestRepositoryApplyRefusesWithoutHTTP(t *testing.T) {
	frozen := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	fake := newApplyGitHub(t, frozen)
	state, reposDir := applyState(t, fake, frozen)
	writeRepoFile(t, reposDir, "kept.yaml", strings.ReplaceAll(validRepositoryYAML("octo-org", "lab-widget"), "existing: adopt", "existing: refuse"))
	result, err := state.reconcileDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteMutation != remoteMutationNone || len(result.Applied) != 0 || len(fake.methods) != 0 {
		t.Fatalf("refuse status=%s applied=%v calls=%v", result.RemoteMutation, result.Applied, fake.methods)
	}
	if result.Unsupported[0].Reason != reasonRefuseUnsupported {
		t.Fatalf("refuse reason = %#v", result.Unsupported)
	}
}

func TestRepositoryApplyFailClosedBeforeWrite(t *testing.T) {
	frozen := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		prepare func(*testing.T, *applyGitHub, string, *privilegedState)
		want    string
		patch   bool
	}{
		{name: "sha pinning", prepare: func(_ *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			f.pinning = boolPtr(true)
			f.description = "old"
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
		}, want: errRepositoryActionsUnsafe.Error()},
		{name: "pinning omitted", prepare: func(_ *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			f.omitPinning = true
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
		}, want: errRepositoryActionsUnsafe.Error()},
		{name: "selected unknown", prepare: func(_ *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			f.selectedMissing = true
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
		}, want: errRepositoryActionsUnsafe.Error()},
		{name: "extra allowance", prepare: func(_ *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			f.extraActions = true
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
		}, want: errRepositoryActionsUnsafe.Error()},
		{name: "missing template", prepare: func(t *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			writeRepoFile(t, repos, "lab.yaml", strings.ReplaceAll(validRepositoryYAML("octo-org", "lab-widget"), "template: lab-widget", "template: missing-template"))
		}, want: errRepositoryTemplate.Error()},
		{name: "template checks", prepare: func(t *testing.T, _ *applyGitHub, repos string, _ *privilegedState) {
			body := strings.ReplaceAll(validRepositoryYAML("octo-org", "lab-widget"), "      - ci", "      - lint")
			writeRepoFile(t, repos, "lab.yaml", body)
		}, want: errRepositoryTemplateCheck.Error()},
		{name: "owner mismatch", prepare: func(t *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			f.owner = "other-org"
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
		}, want: errRepositoryIdentity.Error()},
		{name: "archived", prepare: func(t *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			f.archived = true
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
		}, want: errRepositoryArchived.Error()},
		{name: "write scope", prepare: func(t *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			f.adminLevel = "read"
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
		}, want: errRepositoryAuthScope.Error()},
		{name: "contents scope", prepare: func(t *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			f.contentsExtra = true
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
		}, want: errRepositoryAuthScope.Error()},
		{name: "created branch", prepare: func(t *testing.T, f *applyGitHub, repos string, _ *privilegedState) {
			f.exists = false
			f.createBranch = "trunk"
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
		}, want: errRepositoryDefaultBranch.Error()},
		{name: "bound missing", prepare: func(t *testing.T, f *applyGitHub, repos string, state *privilegedState) {
			f.exists = false
			writeRepoFile(t, repos, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
			if err := writeRepositoryJournal(state.dataDir, repositoryJournal{
				Version: observationVersion, Digest: mustRepositoryDigest(t, repos),
				Bindings: []repositoryBinding{{ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 4242, NodeID: "R_testNode"}},
				Observed: []repositoryObserved{},
			}); err != nil {
				t.Fatal(err)
			}
		}, want: errRepositoryNotFound.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newApplyGitHub(t, frozen)
			state, reposDir := applyState(t, fake, frozen)
			tc.prepare(t, fake, reposDir, state)
			_, err := state.reconcileDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "ghs_") {
				t.Fatalf("error = %v", err)
			}
			if fake.count("PATCH", "/repos/octo-org/lab-widget") != 0 || fake.count("PUT", "") != 0 || fake.count("DELETE", "") != 0 {
				t.Fatalf("fail closed still wrote %#v", fake.methods)
			}
			if tc.name != "created branch" && fake.count("POST", "/orgs/octo-org/repos") != 0 {
				t.Fatalf("unexpected create %#v", fake.methods)
			}
		})
	}
}

func TestRepositoryApplyKeepsBindingAndDoesNotRename(t *testing.T) {
	frozen := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	fake := newApplyGitHub(t, frozen)
	fake.files[".github/workflows/ci.yml"] = []byte("different\n")
	fake.branch = "trunk"
	fake.rulesetInclude = "refs/heads/main"
	fake.description = "old description"
	state, reposDir := applyState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	if err := writeRepositoryJournal(state.dataDir, repositoryJournal{
		Version: observationVersion,
		Digest:  mustRepositoryDigest(t, reposDir),
		Bindings: []repositoryBinding{{
			ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 4242, NodeID: "R_testNode",
		}},
		Observed: []repositoryObserved{},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := state.reconcileDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if result.Journal.Bindings[0].RepositoryID != 4242 || result.Journal.Bindings[0].NodeID != "R_testNode" {
		t.Fatalf("binding changed %#v", result.Journal.Bindings)
	}
	if fake.branch != "trunk" || fake.count("POST", "/repos/octo-org/lab-widget/branches") != 0 {
		t.Fatalf("default branch changed: %s %#v", fake.branch, fake.methods)
	}
	patch := fake.body("PATCH", "/repos/octo-org/lab-widget")
	if strings.Contains(patch, "default_branch") || !strings.Contains(patch, "Lab widget service.") {
		t.Fatalf("patch = %s", patch)
	}
	if !strings.Contains(fake.body("PUT", "/repos/octo-org/lab-widget/rulesets/9"), `"refs/heads/trunk"`) {
		t.Fatalf("ruleset target = %s", fake.body("PUT", "/repos/octo-org/lab-widget/rulesets/9"))
	}
	if fake.count("PUT", "/repos/octo-org/lab-widget/contents/.github/workflows/ci.yml") != 0 {
		t.Fatal("existing bootstrap file was overwritten")
	}
	fields := map[string]string{}
	for _, item := range result.Drift {
		fields[item.Field] = item.Status
	}
	if fields["settings.default_branch"] != driftDrift {
		t.Fatalf("default branch drift = %#v", result.Drift)
	}
}

func TestRepositoryApplyLeavesUnwritableRulesets(t *testing.T) {
	frozen := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	fake := newApplyGitHub(t, frozen)
	fake.files[".github/workflows/ci.yml"] = []byte("name: ci\n")
	fake.unmodeled = true
	fake.extraRuleset = true
	fake.description = "old description"
	state, reposDir := applyState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	result, err := state.reconcileDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if fake.count("PUT", "/repos/octo-org/lab-widget/rulesets/9") != 0 || fake.count("DELETE", "") != 0 {
		t.Fatalf("ruleset was overwritten %#v", fake.methods)
	}
	if !containsString(result.Applied, intentEnsureRepository+":lab") {
		t.Fatalf("settings were not applied: %#v", result.Applied)
	}
	found := false
	for _, item := range result.Unsupported {
		if item.Kind == intentEnsureProtection && strings.Contains(item.Reason, "cannot express") {
			found = true
		}
	}
	if !found {
		t.Fatalf("protection = %#v", result.Unsupported)
	}
}

func TestRepositoryApplySkipsUnobservableRulesets(t *testing.T) {
	frozen := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	fake := newApplyGitHub(t, frozen)
	fake.files[".github/workflows/ci.yml"] = []byte("name: ci\n")
	fake.rulesStatus = http.StatusNotFound
	fake.description = "old description"
	state, reposDir := applyState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	result, err := state.reconcileDesiredRepositories(context.Background(), mustRepositoryDigest(t, reposDir))
	if err != nil {
		t.Fatal(err)
	}
	if fake.count("POST", "/repos/octo-org/lab-widget/rulesets") != 0 {
		t.Fatal("unobservable rulesets were guessed")
	}
	found := false
	for _, item := range result.Unsupported {
		if item.Kind == intentEnsureProtection && item.Reason == reasonRulesetsUnobservable {
			found = true
		}
	}
	if !found || !containsString(result.Applied, intentEnsureRepository+":lab") {
		t.Fatalf("result = %#v", result)
	}
}

func TestRepositoryApplyDesiredChangeKeepsGeneration(t *testing.T) {
	frozen := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	fake := newApplyGitHub(t, frozen)
	fake.description = "old description"
	state, reposDir := applyState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	server := newSyncTestServer(t, nil)
	server.reposDir = reposDir
	if response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}}); response.Code != http.StatusOK {
		t.Fatalf("seed = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	seeded := server.generation.digest
	server.mu.RUnlock()
	state.dataDir = server.store.dataDir
	server.coordinator = stateCoordinator{state: state}
	fake.onPatch = func() {
		writeRepoFile(t, reposDir, "lab.yaml", strings.ReplaceAll(validRepositoryYAML("octo-org", "lab-widget"), "Lab widget service.", "Changed during apply."))
	}
	response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}})
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "privileged coordination failed") {
		t.Fatalf("race = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	kept := server.generation.digest == seeded
	server.mu.RUnlock()
	if !kept {
		t.Fatal("failed apply swapped the generation")
	}
}

func TestRepositoryApplyUnsafeActionsKeepGeneration(t *testing.T) {
	frozen := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	fake := newApplyGitHub(t, frozen)
	fake.pinning = boolPtr(true)
	fake.description = "old description"
	state, reposDir := applyState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	server := newSyncTestServer(t, nil)
	server.reposDir = reposDir
	if response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}}); response.Code != http.StatusOK {
		t.Fatalf("seed = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	seeded := server.generation.digest
	server.mu.RUnlock()
	state.dataDir = server.store.dataDir
	server.coordinator = stateCoordinator{state: state}
	response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unsafe = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	kept := server.generation.digest == seeded
	server.mu.RUnlock()
	if !kept || fake.count("PATCH", "/repos/octo-org/lab-widget") != 0 {
		t.Fatal("unsafe actions swapped the generation or patched settings")
	}
}

func TestLabWidgetTemplateCheckName(t *testing.T) {
	files, err := loadRepositoryTemplate("lab-widget")
	if err != nil {
		t.Fatal(err)
	}
	names, err := templateCheckNames(files)
	if err != nil || len(names) != 1 || names[0] != "ci" {
		t.Fatalf("checks = %#v %v", names, err)
	}
	if _, err := loadRepositoryTemplate("missing-template"); !errorsIs(err, errRepositoryTemplate) {
		t.Fatalf("missing template = %v", err)
	}
}

func errorsIs(err, target error) bool {
	return err != nil && target != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func appliedPlan(result repositoryObservationResult) repositoryPlan {
	repos, _, _ := loadRepositories("")
	_ = repos
	return repositoryPlan{
		Requested: true, Active: true, RemoteMutation: result.RemoteMutation, Observation: result.Status,
		Applied: result.Applied, Intents: repositoryIntents(mustDefinition()),
		Observed: result.Observed, Drift: result.Drift, Unsupported: result.Unsupported,
	}
}

func mustDefinition() repositoryDefinition {
	definition := repositoryDefinition{ID: "lab", Provider: repositoryProviderGitHub, Org: "octo-org", Name: "lab-widget"}
	if err := decodeYAMLBytes("lab.yaml", []byte(validRepositoryYAML("octo-org", "lab-widget")), &definition); err != nil {
		panic(err)
	}
	definition.ID = "lab"
	if err := definition.validate(); err != nil {
		panic(err)
	}
	return definition
}

func applyState(t *testing.T, fake *applyGitHub, now time.Time) (*privilegedState, string) {
	t.Helper()
	reposDir := t.TempDir()
	providersDir := t.TempDir()
	secrets := openTestSecretStore(t)
	key := testAppKey(t)
	secretName := "GITHUB_APP_RECONCILER_PEM"
	writeRepoFile(t, providersDir, "github.yaml", reconcilerProviderYAML(secretName))
	writeSecretPEM(t, secrets.root, secretName, key)
	dataDir := t.TempDir()
	if _, err := prepareDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	state := &privilegedState{
		logger: discardLogger(), host: unixHost{}, mutate: true,
		dataDir: dataDir, secrets: secrets, agentsDir: t.TempDir(), rulesDir: t.TempDir(),
		reposDir: reposDir, providersDir: providersDir,
	}
	state.registrar = githubRegistrar{
		baseURL: fake.server.URL, transport: fake.server.Client().Transport,
		now: func() time.Time { return now }, resolveRepo: state.resolveReconcilerRepository,
		logger: discardLogger(),
	}
	return state, reposDir
}

type applyGitHub struct {
	t               *testing.T
	server          *httptest.Server
	now             time.Time
	mu              sync.Mutex
	methods         []string
	bodies          []string
	exists          bool
	id              int64
	node            string
	owner           string
	name            string
	visibility      string
	description     string
	branch          string
	createBranch    string
	archived        bool
	issues          bool
	wiki            bool
	projects        bool
	squash          bool
	mergeCommit     bool
	rebase          bool
	deleteBranch    bool
	enabled         bool
	allowed         string
	patterns        []string
	pinning         *bool
	omitPinning     bool
	extraActions    bool
	selectedMissing bool
	rulesStatus     int
	rulesetID       int64
	rulesetInclude  string
	unmodeled       bool
	extraRuleset    bool
	files           map[string][]byte
	adminLevel      string
	contentsExtra   bool
	repoGets        int
	onPatch         func()
}

func newApplyGitHub(t *testing.T, now time.Time) *applyGitHub {
	t.Helper()
	fake := &applyGitHub{
		t: t, now: now, exists: true, id: 4242, node: "R_testNode",
		owner: "octo-org", name: "lab-widget", visibility: visibilityPrivate,
		description: "Lab widget service. This declaration is not applied.",
		branch:      "main", issues: true, squash: true, deleteBranch: true,
		enabled: true, allowed: actionsAllowedSelected, patterns: []string{"actions/checkout@v4"},
		pinning: boolPtr(false), rulesetID: 9, files: map[string][]byte{},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *applyGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.methods = append(f.methods, r.Method+" "+r.URL.Path)
	if len(body) > 0 {
		f.bodies = append(f.bodies, r.Method+" "+r.URL.Path+" "+string(body))
	}
	if strings.Contains(r.URL.Path, "/keys") || r.Method == http.MethodDelete {
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/100002/access_tokens":
		f.serveToken(w, body)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/octo-org/lab-widget":
		f.serveRepo(w)
	case r.Method == http.MethodPost && r.URL.Path == "/orgs/octo-org/repos":
		f.serveCreate(w, body)
	case r.Method == http.MethodPatch && r.URL.Path == "/repos/octo-org/lab-widget":
		f.servePatch(w, body)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/octo-org/lab-widget/actions/permissions":
		f.serveActions(w)
	case r.Method == http.MethodPut && r.URL.Path == "/repos/octo-org/lab-widget/actions/permissions":
		var payload struct {
			Enabled bool   `json:"enabled"`
			Allowed string `json:"allowed_actions"`
		}
		_ = json.Unmarshal(body, &payload)
		f.enabled = payload.Enabled
		f.allowed = payload.Allowed
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodGet && r.URL.Path == "/repos/octo-org/lab-widget/actions/permissions/selected-actions":
		if f.selectedMissing {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"github_owned_allowed": f.extraActions, "verified_allowed": false, "patterns_allowed": f.patterns,
		})
	case r.Method == http.MethodPut && r.URL.Path == "/repos/octo-org/lab-widget/actions/permissions/selected-actions":
		var payload struct {
			Owned    bool     `json:"github_owned_allowed"`
			Verified bool     `json:"verified_allowed"`
			Patterns []string `json:"patterns_allowed"`
		}
		_ = json.Unmarshal(body, &payload)
		f.extraActions = payload.Owned || payload.Verified
		f.patterns = append([]string(nil), payload.Patterns...)
		f.selectedMissing = false
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case r.URL.Path == "/repos/octo-org/lab-widget/rulesets" && r.Method == http.MethodGet:
		f.serveRules(w)
	case r.URL.Path == "/repos/octo-org/lab-widget/rulesets" && r.Method == http.MethodPost:
		f.rememberRuleset(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":9}`))
	case r.URL.Path == "/repos/octo-org/lab-widget/rulesets/9" && r.Method == http.MethodGet:
		f.serveRuleset(w, 9, "protect-main")
	case r.URL.Path == "/repos/octo-org/lab-widget/rulesets/8" && r.Method == http.MethodGet:
		f.serveRuleset(w, 8, "extra-rules")
	case r.URL.Path == "/repos/octo-org/lab-widget/rulesets/9" && r.Method == http.MethodPut:
		f.rememberRuleset(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":9}`))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/octo-org/lab-widget/rules/branches/"):
		f.serveBranch(w)
	case strings.HasPrefix(r.URL.Path, "/repos/octo-org/lab-widget/contents/"):
		f.serveContents(w, r, body)
	default:
		http.NotFound(w, r)
	}
}

func (f *applyGitHub) serveToken(w http.ResponseWriter, body []byte) {
	var request gitHubTokenRequest
	if err := json.Unmarshal(body, &request); err != nil {
		f.t.Errorf("token: %v", err)
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	permissions := map[string]string{}
	if request.Permissions.Administration != "" {
		level := request.Permissions.Administration
		if request.Permissions.Administration == "write" && f.adminLevel != "" {
			level = f.adminLevel
		}
		permissions["administration"] = level
	}
	if request.Permissions.Contents != "" {
		permissions["contents"] = request.Permissions.Contents
	}
	if request.Permissions.Metadata != "" {
		permissions["metadata"] = request.Permissions.Metadata
	}
	if f.contentsExtra && request.Permissions.Contents != "" {
		permissions["administration"] = "read"
	}
	id := f.id
	if id == 0 {
		id = 4242
	}
	_ = json.NewEncoder(w).Encode(gitHubTokenResponse{
		Token: "ghs_applytesttokenvalue000000000000", ExpiresAt: f.now.Add(time.Hour),
		Permissions: permissions, RepositorySelection: "selected",
		Repositories: []gitHubRepoRef{{ID: id, Name: "lab-widget", FullName: "octo-org/lab-widget"}},
	})
}

func (f *applyGitHub) serveRepo(w http.ResponseWriter) {
	f.repoGets++
	if !f.exists {
		http.NotFound(w, rNotFound())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": f.id, "node_id": f.node, "name": f.name, "full_name": f.owner + "/" + f.name,
		"owner": map[string]any{"login": f.owner}, "private": f.visibility != visibilityPublic, "visibility": f.visibility,
		"description": f.description, "default_branch": f.branch, "archived": f.archived,
		"has_issues": f.issues, "has_wiki": f.wiki, "has_projects": f.projects,
		"allow_squash_merge": f.squash, "allow_merge_commit": f.mergeCommit, "allow_rebase_merge": f.rebase,
		"delete_branch_on_merge": f.deleteBranch,
	})
}

func rNotFound() *http.Request {
	request, _ := http.NewRequest(http.MethodGet, "/", nil)
	return request
}

func (f *applyGitHub) serveCreate(w http.ResponseWriter, body []byte) {
	var payload struct {
		Name        string `json:"name"`
		Visibility  string `json:"visibility"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
		AutoInit    bool   `json:"auto_init"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || !payload.AutoInit || payload.Name == "" {
		f.t.Errorf("create payload %#v %v", payload, err)
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	f.exists = true
	f.name = payload.Name
	f.visibility = payload.Visibility
	f.description = payload.Description
	if f.createBranch != "" {
		f.branch = f.createBranch
	}
	f.wiki = true
	f.enabled = false
	f.allowed = actionsAllowedAll
	f.rulesetID = 0
	f.id = 4242
	f.node = "R_testNode"
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"id":4242}`))
}

func (f *applyGitHub) servePatch(w http.ResponseWriter, body []byte) {
	if f.onPatch != nil {
		f.onPatch()
		f.onPatch = nil
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	if _, ok := payload["default_branch"]; ok {
		f.t.Errorf("patch included default_branch")
	}
	if description, ok := payload["description"].(string); ok {
		f.description = description
	}
	if visibility, ok := payload["visibility"].(string); ok {
		f.visibility = visibility
	}
	setBool := func(key string, dest *bool) {
		if value, ok := payload[key].(bool); ok {
			*dest = value
		}
	}
	setBool("has_issues", &f.issues)
	setBool("has_wiki", &f.wiki)
	setBool("has_projects", &f.projects)
	setBool("allow_squash_merge", &f.squash)
	setBool("allow_merge_commit", &f.mergeCommit)
	setBool("allow_rebase_merge", &f.rebase)
	setBool("delete_branch_on_merge", &f.deleteBranch)
	f.serveRepo(w)
}

func (f *applyGitHub) serveActions(w http.ResponseWriter) {
	body := map[string]any{"enabled": f.enabled, "allowed_actions": f.allowed}
	if !f.omitPinning && f.pinning != nil {
		body["sha_pinning_required"] = *f.pinning
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *applyGitHub) serveRules(w http.ResponseWriter) {
	if f.rulesStatus != 0 {
		w.WriteHeader(f.rulesStatus)
		return
	}
	list := []map[string]any{}
	if f.rulesetID != 0 {
		list = append(list, map[string]any{
			"id": f.rulesetID, "name": "protect-main", "target": "branch", "source_type": "Repository", "enforcement": "active",
		})
	}
	if f.extraRuleset {
		list = append(list, map[string]any{
			"id": int64(8), "name": "extra-rules", "target": "branch", "source_type": "Repository", "enforcement": "active",
		})
	}
	_ = json.NewEncoder(w).Encode(list)
}

func (f *applyGitHub) rememberRuleset(body []byte) {
	var payload struct {
		Conditions struct {
			RefName struct {
				Include []string `json:"include"`
			} `json:"ref_name"`
		} `json:"conditions"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && len(payload.Conditions.RefName.Include) == 1 {
		f.rulesetInclude = payload.Conditions.RefName.Include[0]
	}
	f.unmodeled = false
	f.rulesetID = 9
}

func (f *applyGitHub) serveRuleset(w http.ResponseWriter, id int64, name string) {
	rules := []map[string]any{
		{"type": "pull_request", "parameters": map[string]any{"required_approving_review_count": 1, "dismiss_stale_reviews_on_push": true}},
		{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": true, "required_status_checks": []map[string]any{{"context": "ci"}}}},
	}
	if name == "protect-main" && f.unmodeled {
		rules = append(rules, map[string]any{"type": "deletion"})
	}
	if name != "protect-main" {
		rules = []map[string]any{{"type": "deletion"}}
	}
	include := f.rulesetInclude
	if include == "" {
		include = "refs/heads/" + f.branch
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id, "name": name, "target": "branch", "source_type": "Repository", "enforcement": "active",
		"conditions": map[string]any{"ref_name": map[string]any{"include": []string{include}, "exclude": []string{}}},
		"rules":      rules,
	})
}

func (f *applyGitHub) serveBranch(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode([]map[string]any{
		{"type": "pull_request", "parameters": map[string]any{"required_approving_review_count": 1, "dismiss_stale_reviews_on_push": true}},
		{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": true, "required_status_checks": []map[string]any{{"context": "ci"}}}},
	})
}

func (f *applyGitHub) serveContents(w http.ResponseWriter, r *http.Request, body []byte) {
	path := strings.TrimPrefix(r.URL.Path, "/repos/octo-org/lab-widget/contents/")
	switch r.Method {
	case http.MethodGet:
		content, ok := f.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "encoding": "base64", "name": filepath.Base(path), "path": path,
			"sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "content": content,
		})
	case http.MethodPut:
		if _, exists := f.files[path]; exists {
			f.t.Errorf("overwrote %s", path)
		}
		if bytes.Contains(body, []byte(`"sha"`)) {
			f.t.Errorf("create included sha")
		}
		f.files[path] = body
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"content":{"path":"` + path + `"}}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *applyGitHub) body(method, path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.bodies {
		if strings.HasPrefix(item, method+" "+path+" ") {
			return item
		}
	}
	return ""
}

func (f *applyGitHub) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, item := range f.methods {
		verb, rest, _ := strings.Cut(item, " ")
		if verb == method && (path == "" || rest == path) {
			total++
		}
	}
	return total
}

func TestRepositoryApplyPersistedIdentity(t *testing.T) {
	frozen := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	fake := newApplyGitHub(t, frozen)
	state, reposDir := applyState(t, fake, frozen)
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	digest := mustRepositoryDigest(t, reposDir)
	if err := writeRepositoryJournal(state.dataDir, repositoryJournal{
		Version: observationVersion, Digest: digest,
		Bindings: []repositoryBinding{{ID: "lab", Org: "octo-org", Name: "lab-widget", RepositoryID: 111, NodeID: "R_other"}},
		Observed: []repositoryObserved{},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := state.reconcileDesiredRepositories(context.Background(), digest)
	if err == nil || !strings.Contains(err.Error(), errRepositoryPersisted.Error()) {
		t.Fatalf("error = %v", err)
	}
	if fake.count("PATCH", "") != 0 {
		t.Fatal("identity mismatch patched settings")
	}
	if _, statErr := os.Lstat(filepath.Join(state.dataDir, observationDirName, observationFileName)); statErr != nil {
		t.Fatal(statErr)
	}
}
