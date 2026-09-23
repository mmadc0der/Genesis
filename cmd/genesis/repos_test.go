package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRepositorySchemaAndClosedPaths(t *testing.T) {
	root := repoRoot(t)
	loaded, active, err := loadRepositories(filepath.Join(root, "repos.d"))
	if err != nil {
		t.Fatal(err)
	}
	if !active || len(loaded) != 1 {
		t.Fatalf("committed repos.d active=%v len=%d", active, len(loaded))
	}
	example := loaded["example"]
	if example.Provider != repositoryProviderGitHub || example.Org != "octo-org" || example.Name != "lab-widget" {
		t.Fatalf("example = %#v", example)
	}
	if example.Lifecycle.Remove != lifecycleRemoveRetain || example.Lifecycle.Existing != lifecycleExistingAdopt {
		t.Fatalf("lifecycle = %#v", example.Lifecycle)
	}
	if example.Bootstrap == nil || example.Bootstrap.Template != "lab-widget" {
		t.Fatalf("bootstrap = %#v", example.Bootstrap)
	}
	if example.Identities[0].Name != "programmer" || example.Identities[1].Role != roleReviewer {
		t.Fatalf("identities were not normalized: %#v", example.Identities)
	}
	plan, err := buildRepositoryPlan(loaded, true, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if plan.RemoteMutation != remoteMutationNone || len(plan.Applied) != 0 || len(plan.Unsupported) != len(plan.Intents) {
		t.Fatalf("plan = %s", text)
	}
	for _, forbidden := range []string{`"remote_mutation":"applied"`, "BEGIN ", "ghp_", "github_pat_", "https://", "github.com", "x-access-token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("plan contains %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, repositoryNotAppliedReason) || !strings.Contains(text, `"kind":"ensure_bootstrap"`) || !strings.Contains(text, `"kind":"retain_on_remove"`) {
		t.Fatalf("plan = %s", text)
	}
	again, err := buildRepositoryPlan(loaded, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Requested || again.Active || len(again.Intents) != 0 {
		t.Fatalf("unrequested plan = %#v", again)
	}

	directory := t.TempDir()
	writeRepoFile(t, directory, "b-widget.yaml", strings.Replace(validRepositoryYAML("b-org", "b-widget"), "existing: adopt", "existing: refuse", 1))
	writeRepoFile(t, directory, "a-widget.yaml", validRepositoryYAML("a-org", "a-widget"))
	repos, active, err := loadRepositories(directory)
	if err != nil || !active || len(repos) != 2 {
		t.Fatalf("load = %v active=%v len=%d", err, active, len(repos))
	}
	plan, err = buildRepositoryPlan(repos, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Intents[0].ID != "a-widget" || plan.Intents[len(plan.Intents)-1].ID != "b-widget" {
		t.Fatalf("intent order = %#v", plan.Intents)
	}
	second, err := buildRepositoryPlan(repos, true, true)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(plan)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("repository plan is not deterministic")
	}

	missing, active, err := loadRepositories(filepath.Join(t.TempDir(), "missing"))
	if err != nil || active || len(missing) != 0 {
		t.Fatalf("missing repos = %v active=%v len=%d", err, active, len(missing))
	}
	if _, active, err := loadRepositories(""); err != nil || active {
		t.Fatalf("empty path = %v active=%v", err, active)
	}

	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{name: "unknown field", mutate: func(s string) string { return s + "files:\n  - path: README.md\n" }, want: "field files not found"},
		{name: "secret value", mutate: func(s string) string {
			return strings.Replace(s, "    - DEEPSEEK_API_KEY\n", "    - name: DEEPSEEK_API_KEY\n      value: super-secret\n", 1)
		}, want: "cannot unmarshal"},
		{name: "delete policy", mutate: func(s string) string {
			return strings.Replace(s, "remove: retain", "remove: delete", 1)
		}, want: "only retains"},
		{name: "archive policy", mutate: func(s string) string {
			return strings.Replace(s, "remove: retain", "remove: archive", 1)
		}, want: "only retains"},
		{name: "template traversal", mutate: func(s string) string {
			return strings.Replace(s, "template: lab-widget", "template: ../secret", 1)
		}, want: "bootstrap.template"},
		{name: "template absolute", mutate: func(s string) string {
			return strings.Replace(s, "template: lab-widget", "template: /etc/passwd", 1)
		}, want: "bootstrap.template"},
		{name: "template url", mutate: func(s string) string {
			return strings.Replace(s, "template: lab-widget", "template: http://example.test/t", 1)
		}, want: "bootstrap.template"},
		{name: "provider url", mutate: func(s string) string {
			return strings.Replace(s, "provider: github", "provider: https://github.example", 1)
		}, want: "provider"},
		{name: "user owner", mutate: func(s string) string {
			return strings.Replace(s, "org: octo-org", "owner: octo-org", 1)
		}, want: "field owner not found"},
		{name: "selected with all", mutate: func(s string) string {
			return strings.Replace(s, "allowed: selected\n  selected:\n    - actions/checkout@v4\n", "allowed: all\n  selected: []\n", 1)
		}, want: "actions.selected"},
		{name: "unsafe action", mutate: func(s string) string {
			return strings.Replace(s, "actions/checkout@v4", "../checkout@v4", 1)
		}, want: "actions.selected"},
		{name: "reconciler role", mutate: func(s string) string {
			return strings.Replace(s, "role: programmer", "role: reconciler", 1)
		}, want: "identities.role"},
		{name: "duplicate role", mutate: func(s string) string {
			return strings.Replace(s, "role: reviewer", "role: programmer", 1)
		}, want: "duplicate identity role"},
		{name: "no merge method", mutate: func(s string) string {
			return strings.Replace(s, "allow_squash: true", "allow_squash: false", 1)
		}, want: "at least one"},
		{name: "omitted wiki", mutate: func(s string) string {
			return strings.Replace(s, "    wiki: false\n", "", 1)
		}, want: "settings.features.wiki"},
		{name: "secret material", mutate: func(s string) string {
			return strings.Replace(s, "Lab widget service. This declaration is not applied.", "token ghp_examplevalue", 1)
		}, want: "secret material"},
		{name: "strict without checks", mutate: func(s string) string {
			return strings.Replace(s, "    required_checks:\n      - ci\n", "", 1)
		}, want: "strict_checks"},
		{name: "too many reviews", mutate: func(s string) string {
			return strings.Replace(s, "required_approving_reviews: 1", "required_approving_reviews: 7", 1)
		}, want: "required_approving_reviews"},
		{name: "repeated org hyphen", mutate: func(s string) string {
			return strings.Replace(s, "org: octo-org", "org: octo--org", 1)
		}, want: "invalid org"},
		{name: "git suffix", mutate: func(s string) string {
			return strings.Replace(s, "name: lab-widget", "name: lab-widget.git", 1)
		}, want: "invalid name"},
		{name: "git suffix folded", mutate: func(s string) string {
			return strings.Replace(s, "name: lab-widget", "name: lab-widget.GIT", 1)
		}, want: "invalid name"},
		{name: "branch lock", mutate: func(s string) string {
			return strings.Replace(s, "default_branch: main", "default_branch: main.lock", 1)
		}, want: "default_branch"},
		{name: "branch head", mutate: func(s string) string {
			return strings.Replace(s, "default_branch: main", "default_branch: HEAD", 1)
		}, want: "default_branch"},
		{name: "branch slash", mutate: func(s string) string {
			return strings.Replace(s, "default_branch: main", "default_branch: release/1", 1)
		}, want: "default_branch"},
		{name: "action url", mutate: func(s string) string {
			return strings.Replace(s, "actions/checkout@v4", "https://github.com/actions/checkout@v4", 1)
		}, want: "actions.selected"},
		{name: "check traversal", mutate: func(s string) string {
			return strings.Replace(s, "      - ci\n", "      - ../ci\n", 1)
		}, want: "required_checks"},
		{name: "duplicate key", mutate: func(s string) string {
			return strings.Replace(s, "org: octo-org\n", "org: octo-org\norg: other-org\n", 1)
		}, want: "already defined"},
		{name: "explicit id", mutate: func(s string) string {
			return "id: other\n" + s
		}, want: "field id not found"},
		{name: "nested unknown", mutate: func(s string) string {
			return strings.Replace(s, "  default_branch: main\n", "  default_branch: main\n  homepage: https://example.test\n", 1)
		}, want: "field homepage not found"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			dir := t.TempDir()
			body := item.mutate(validRepositoryYAML("octo-org", "lab-widget"))
			writeRepoFile(t, dir, "lab.yaml", body)
			_, _, err := loadRepositories(dir)
			if err == nil || !strings.Contains(err.Error(), item.want) {
				t.Fatalf("error = %v, want %q", err, item.want)
			}
		})
	}

	dupDir := t.TempDir()
	writeRepoFile(t, dupDir, "one.yaml", validRepositoryYAML("octo-org", "Lab-Widget"))
	writeRepoFile(t, dupDir, "two.yaml", validRepositoryYAML("OCTO-ORG", "lab-widget"))
	if _, _, err := loadRepositories(dupDir); err == nil || !strings.Contains(err.Error(), "already declared") {
		t.Fatalf("duplicate org/name = %v", err)
	}

	idDir := t.TempDir()
	body := validRepositoryYAML("octo-org", "lab-widget")
	writeRepoFile(t, idDir, "lab.yaml", body)
	writeRepoFile(t, idDir, "lab.yml", body)
	if _, _, err := loadRepositories(idDir); err == nil || !strings.Contains(err.Error(), "duplicate repository id") {
		t.Fatalf("duplicate id = %v", err)
	}

	badName := t.TempDir()
	writeRepoFile(t, badName, "Bad Name.yaml", body)
	if _, _, err := loadRepositories(badName); err == nil || !strings.Contains(err.Error(), "invalid repository id") {
		t.Fatalf("bad filename = %v", err)
	}

	nested := t.TempDir()
	if err := os.Mkdir(filepath.Join(nested, "nested.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRepositories(nested); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory entry = %v", err)
	}

	linkParent := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(linkParent, "repos")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRepositories(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink dir = %v", err)
	}

	fileLinkDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "real.yaml")
	if err := os.WriteFile(outside, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(fileLinkDir, "lab.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRepositories(fileLinkDir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink file = %v", err)
	}

	dotDot := t.TempDir()
	writeRepoFile(t, dotDot, "a..b.yaml", body)
	if _, _, err := loadRepositories(dotDot); err == nil || !strings.Contains(err.Error(), "invalid repository id") {
		t.Fatalf("dotdot id = %v", err)
	}

	widened := strings.Replace(strings.Replace(body,
		"    - actions/checkout@v4\n",
		"    - actions/checkout@v4\n    - octo-org/*\n    - my-org/repo/.github/workflows/ci.yml@main\n",
		1,
	), "      - ci\n", "      - ci\n      - \"ci/circleci: test\"\n", 1)
	wideDir := t.TempDir()
	writeRepoFile(t, wideDir, "lab.yaml", widened)
	wide, active, err := loadRepositories(wideDir)
	if err != nil || !active {
		t.Fatalf("wider patterns = %v", err)
	}
	if got := wide["lab"].Actions.Selected; len(got) != 3 || got[0] != "actions/checkout@v4" {
		t.Fatalf("selected = %#v", got)
	}
	if got := wide["lab"].Protection.Ruleset.RequiredChecks; len(got) != 2 || got[1] != "ci/circleci: test" {
		t.Fatalf("checks = %#v", got)
	}

	if err := validateDescription("bad\xff"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("description utf-8 = %v", err)
	}
	if _, err := digestGeneration(map[string]agentDefinition{
		"ok": {Instructions: "bad\xff", Cwd: "/tmp/a", Home: "/tmp/b"},
	}, nil, nil, false); err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("digest utf-8 = %v", err)
	}
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "ok.yaml", nil)
	writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{Match: map[string]string{"type": "dev.genesis.run"}, Agent: "ok"})
	linkRoot := t.TempDir()
	linkRepos := filepath.Join(linkRoot, "repos.d")
	if err := os.Symlink(t.TempDir(), linkRepos); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGeneration(agentsDir, rulesDir, linkRepos); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink generation = %v", err)
	}
	usable, err := repositoryDirectoryUsable(linkRepos)
	if err == nil || usable || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("usable symlink = %v %v", usable, err)
	}
	inactive, err := loadGeneration(agentsDir, rulesDir, filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if inactive.digest != legacyAgentsRulesDigest(t, inactive.agents["ok"], cwd, home) {
		t.Fatal("symlink handling changed the missing-directory digest")
	}

	claimed := repositoryPlan{
		Requested:      true,
		Active:         true,
		RemoteMutation: "applied",
		Applied:        []string{"ensure_repository:lab"},
		Intents:        []repositoryIntent{{Kind: intentEnsureRepository, ID: "lab"}},
		Unsupported:    []repositoryUnsupported{{Kind: intentEnsureRepository, ID: "lab", Reason: "applied remotely"}},
	}
	if err := repositoryPlanIsNotApplied(claimed); err == nil {
		t.Fatal("repository plan accepted a claimed provider mutation")
	}
}

func TestAbsentRepositoryDirectoryPreservesDigest(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "ok.yaml", map[string]string{"LANG": "C"})
	writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{
		Match: map[string]string{"type": "dev.genesis.run"},
		Agent: "ok",
	})
	inactive, err := loadGeneration(agentsDir, rulesDir, filepath.Join(t.TempDir(), "missing-repos"))
	if err != nil {
		t.Fatal(err)
	}
	if inactive.reposActive || len(inactive.repos) != 0 {
		t.Fatalf("inactive generation = %#v", inactive)
	}
	want := legacyAgentsRulesDigest(t, inactive.agents["ok"], cwd, home)
	if inactive.digest != want {
		t.Fatalf("absent repos.d digest = %s, want %s", inactive.digest, want)
	}
	present, err := loadGeneration(agentsDir, rulesDir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !present.reposActive || present.digest == inactive.digest {
		t.Fatalf("empty repos.d digest = %s active=%v", present.digest, present.reposActive)
	}
}

func TestSyncRepositoryPlanIsNotApplied(t *testing.T) {
	server := newSyncTestServer(t, nil)
	reposDir := filepath.Join(t.TempDir(), "repos.d")
	server.reposDir = reposDir

	response := sendSync(server, "sync-secret", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("absent repos sync = %d %s", response.Code, response.Body.String())
	}
	var body syncResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.RepositoryPlan.Requested || body.RepositoryPlan.Active || body.RepositoryPlan.RemoteMutation != remoteMutationNone || len(body.RepositoryPlan.Applied) != 0 {
		t.Fatalf("absent plan = %#v", body.RepositoryPlan)
	}
	absentDigest := body.Digest

	if err := os.Mkdir(reposDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	response = sendSync(server, "sync-secret", map[string]any{"scope": []string{"agents"}})
	if response.Code != http.StatusOK {
		t.Fatalf("agents sync = %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Repositories != 0 || body.RepositoryPlan.Requested {
		t.Fatalf("agents-only plan = %#v count=%d", body.RepositoryPlan, body.Repositories)
	}
	server.mu.RLock()
	cached := server.generation.digest
	server.mu.RUnlock()
	if cached != absentDigest {
		t.Fatal("agents-only sync changed the inactive repository digest")
	}

	response = sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}})
	if response.Code != http.StatusOK {
		t.Fatalf("repos sync = %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Repositories != 1 || body.RepositoryPlan.RemoteMutation != remoteMutationNone || len(body.RepositoryPlan.Applied) != 0 || len(body.RepositoryPlan.Unsupported) == 0 {
		t.Fatalf("applied-looking plan = %#v", body.RepositoryPlan)
	}
	if strings.Contains(response.Body.String(), `"remote_mutation":"applied"`) {
		t.Fatalf("sync claimed remote mutation: %s", response.Body.String())
	}
	server.mu.RLock()
	appliedDigest := server.generation.digest
	eventStillMatches := server.generation.rules[0].Match["type"] == "com.example.run"
	server.mu.RUnlock()
	if !eventStillMatches || appliedDigest == absentDigest {
		t.Fatalf("repos sync digest=%s rules preserved=%v", appliedDigest, eventStillMatches)
	}

	writeAgent(t, filepath.Join(server.agentsDir, "ok.yaml"), agentDefinition{
		Instructions: "stay",
		Cwd:          "/home/workspace-janitor/workspace",
		Home:         "/home/workspace-janitor",
		User:         "workspace-janitor",
	})
	response = sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}})
	if response.Code != http.StatusOK {
		t.Fatalf("repos sync with dirty dedicated agent = %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Privileged.HostMutation != hostMutationNone || len(body.Privileged.Applied) != 0 || body.RepositoryPlan.RemoteMutation != remoteMutationNone {
		t.Fatalf("repos-only privileged = %#v plan = %#v", body.Privileged, body.RepositoryPlan)
	}
	server.mu.RLock()
	cachedUser := server.generation.agents["ok"].User
	sameDigest := server.generation.digest == appliedDigest
	server.mu.RUnlock()
	if cachedUser != "" || !sameDigest {
		t.Fatalf("repos-only sync reloaded agents user=%q digest unchanged=%v", cachedUser, sameDigest)
	}

	writeRepoFile(t, reposDir, "other.yaml", validRepositoryYAML("other-org", "other-widget"))
	response = sendSync(server, "sync-secret", map[string]any{"scope": []string{"rules"}})
	if response.Code != http.StatusOK {
		t.Fatalf("rules sync = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	stillOne := len(server.generation.repos) == 1
	server.mu.RUnlock()
	if !stillOne {
		t.Fatal("rules-only sync reloaded repositories")
	}

	if err := os.WriteFile(filepath.Join(reposDir, "lab.yaml"), []byte("nope: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	response = sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}})
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "repositories are invalid") {
		t.Fatalf("invalid repos = %d %s", response.Code, response.Body.String())
	}
	server.mu.RLock()
	unchanged := server.generation.digest == appliedDigest && len(server.generation.repos) == 1
	server.mu.RUnlock()
	if !unchanged {
		t.Fatal("invalid repository sync swapped the cache")
	}
}

func TestControlRepositoryCatalog(t *testing.T) {
	control, listener := newPanelFixture(t)
	reposDir := filepath.Join(t.TempDir(), "repos.d")
	control.reposDir = reposDir
	listener.reposDir = reposDir
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	inactive := getJSON[map[string]any](t, panel.URL+"/api/repositories")
	if inactive["repositories_active"] != false {
		t.Fatalf("inactive catalog = %#v", inactive)
	}
	if repos, _ := inactive["repositories"].([]any); len(repos) != 0 {
		t.Fatalf("inactive repositories = %#v", inactive["repositories"])
	}

	if err := os.Mkdir(reposDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	draft := getJSON[map[string]any](t, panel.URL+"/api/repositories")
	if draft["repositories_active"] != true {
		t.Fatalf("draft catalog = %#v", draft)
	}
	repositories, _ := draft["repositories"].([]any)
	if len(repositories) != 1 {
		t.Fatalf("draft repositories = %#v", draft["repositories"])
	}
	first, _ := repositories[0].(map[string]any)
	if first["presence"] != presenceDraft || first["id"] != "lab" || first["org"] != "octo-org" {
		t.Fatalf("draft repository = %#v", first)
	}
	secrets, _ := first["secrets"].(map[string]any)
	encoded, _ := json.Marshal(secrets)
	if strings.Contains(string(encoded), "super-secret") || strings.Contains(string(encoded), "ghp_") {
		t.Fatalf("catalog leaked a secret value: %s", encoded)
	}

	one := getJSON[listedRepository](t, panel.URL+"/api/repositories/lab")
	if one.Presence != presenceDraft || one.Name != "lab-widget" || one.Actions.Allowed != actionsAllowedSelected {
		t.Fatalf("one repository = %#v", one)
	}
	bad := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/repositories/../lab", nil)
	request.Host = "127.0.0.1"
	control.ServeHTTP(bad, request)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("traversal status = %d %s", bad.Code, bad.Body.String())
	}

	synced := postJSON(t, panel.URL+"/api/sync", `{"scope":["repos"]}`, http.StatusOK)
	if !strings.Contains(synced, `"remote_mutation":"none"`) || strings.Contains(synced, panelToken) {
		t.Fatalf("repos sync = %s", synced)
	}
	active := getJSON[map[string]any](t, panel.URL+"/api/repositories")
	activeRepos, _ := active["repositories"].([]any)
	activeOne, _ := activeRepos[0].(map[string]any)
	if activeOne["presence"] != presenceActive {
		t.Fatalf("active repository = %#v", activeOne)
	}
	state := getJSON[controlState](t, panel.URL+"/api/state")
	if state.Drift != driftInSync || !state.Desired.RepositoriesActive || len(state.Active.Repositories) != 1 {
		t.Fatalf("state = %#v", state)
	}

	if err := os.WriteFile(filepath.Join(reposDir, "lab.yaml"), []byte("files: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := getJSON[map[string]any](t, panel.URL+"/api/repositories")
	if !strings.Contains(broken["desired_error"].(string), "files") {
		t.Fatalf("broken catalog = %#v", broken)
	}
	kept, _ := broken["repositories"].([]any)
	keptOne, _ := kept[0].(map[string]any)
	if keptOne["presence"] != presenceActiveOnly || keptOne["id"] != "lab" {
		t.Fatalf("active cache hidden = %#v", broken)
	}
}

func TestRepositorySpecialFilesFailClosed(t *testing.T) {
	fifoDir := t.TempDir()
	fifo := filepath.Join(fifoDir, "lab.yaml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := loadRepositories(fifoDir)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("fifo = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("repository load blocked on a named pipe")
	}

	bigDir := t.TempDir()
	payload := make([]byte, maxRepositoryFileBytes+1)
	for i := range payload {
		payload[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(bigDir, "lab.yaml"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRepositories(bigDir); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize = %v", err)
	}
}

func TestRepositorySyncReadDoesNotRace(t *testing.T) {
	server := newSyncTestServer(t, nil)
	reposDir := filepath.Join(t.TempDir(), "repos.d")
	if err := os.Mkdir(reposDir, 0o700); err != nil {
		t.Fatal(err)
	}
	server.reposDir = reposDir
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	if response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}}); response.Code != http.StatusOK {
		t.Fatalf("seed sync = %d %s", response.Code, response.Body.String())
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	readErr := make(chan string, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			request := httptest.NewRequest(http.MethodGet, "/generation", nil)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"repositories_active":true`) {
				select {
				case readErr <- response.Body.String():
				default:
				}
				return
			}
		}
	}()
	for range 12 {
		response := sendSync(server, "sync-secret", map[string]any{"scope": []string{scopeRepos}})
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"remote_mutation":"none"`) {
			t.Fatalf("sync = %d %s", response.Code, response.Body.String())
		}
	}
	close(stop)
	wg.Wait()
	select {
	case body := <-readErr:
		t.Fatalf("generation read during sync = %s", body)
	default:
	}
}

func TestControlSymlinkReposDirectoryIsAnError(t *testing.T) {
	control, listener := newPanelFixture(t)
	parent := t.TempDir()
	reposDir := filepath.Join(parent, "repos.d")
	if err := os.Symlink(t.TempDir(), reposDir); err != nil {
		t.Fatal(err)
	}
	control.reposDir = reposDir
	listener.reposDir = reposDir
	panel := httptest.NewServer(control)
	t.Cleanup(panel.Close)

	catalog := getJSON[map[string]any](t, panel.URL+"/api/repositories")
	message, _ := catalog["desired_error"].(string)
	if !strings.Contains(message, "symlink") || catalog["repositories_active"] != false {
		t.Fatalf("symlink catalog = %#v", catalog)
	}
	synced := postJSON(t, panel.URL+"/api/sync", `{"scope":["repos"]}`, http.StatusInternalServerError)
	if !strings.Contains(synced, "repositories are invalid") {
		t.Fatalf("symlink sync = %s", synced)
	}
}

func validRepositoryYAML(org, name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(`provider: github
org: ORG
name: NAME
lifecycle:
  remove: retain
  existing: adopt
settings:
  visibility: private
  description: Lab widget service. This declaration is not applied.
  default_branch: main
  features:
    issues: true
    wiki: false
    projects: false
  merge:
    allow_squash: true
    allow_merge_commit: false
    allow_rebase: false
    delete_branch_on_merge: true
bootstrap:
  template: lab-widget
actions:
  enabled: true
  allowed: selected
  selected:
    - actions/checkout@v4
secrets:
  repository:
    - DEEPSEEK_API_KEY
  environments:
    - name: ci
      secrets:
        - CI_BOT_TOKEN
protection:
  ruleset:
    name: protect-main
    required_approving_reviews: 1
    dismiss_stale_reviews: true
    required_checks:
      - ci
    strict_checks: true
identities:
  - name: programmer
    role: programmer
  - name: reviewer
    role: reviewer
`, "ORG", org), "NAME", name)
}

func writeRepoFile(t *testing.T, directory, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func legacyAgentsRulesDigest(t *testing.T, definition agentDefinition, cwd, home string) string {
	t.Helper()
	payload, err := json.Marshal(struct {
		Agents []struct {
			ID           string            `json:"id"`
			Instructions string            `json:"instructions"`
			Cwd          string            `json:"cwd"`
			Home         string            `json:"home"`
			User         string            `json:"user"`
			Setup        *agentSetup       `json:"setup"`
			Env          map[string]string `json:"env"`
			Secrets      []string          `json:"secrets"`
		} `json:"agents"`
		Rules []struct {
			Name  string            `json:"name"`
			Match map[string]string `json:"match"`
			Agent string            `json:"agent"`
		} `json:"rules"`
	}{
		Agents: []struct {
			ID           string            `json:"id"`
			Instructions string            `json:"instructions"`
			Cwd          string            `json:"cwd"`
			Home         string            `json:"home"`
			User         string            `json:"user"`
			Setup        *agentSetup       `json:"setup"`
			Env          map[string]string `json:"env"`
			Secrets      []string          `json:"secrets"`
		}{{
			ID:           "ok",
			Instructions: definition.Instructions,
			Cwd:          cwd,
			Home:         home,
			Env:          definition.Env,
			Secrets:      definition.Secrets,
		}},
		Rules: []struct {
			Name  string            `json:"name"`
			Match map[string]string `json:"match"`
			Agent string            `json:"agent"`
		}{{
			Name:  "ok.yaml",
			Match: map[string]string{"type": "dev.genesis.run"},
			Agent: "ok",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
