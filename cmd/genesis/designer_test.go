package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func copyFile(t *testing.T, src, dest string) {
	t.Helper()
	payload, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedDesignerIsSharedUIDConfigEditor(t *testing.T) {
	root := repoRoot(t)
	agents, err := loadAgents(filepath.Join(root, "agents.d"))
	if err != nil {
		t.Fatal(err)
	}
	designer, ok := agents["designer"]
	if !ok {
		t.Fatal("committed agents.d is missing designer")
	}
	if designer.User != "" {
		t.Fatalf("designer user = %q, want omitted shared listener UID", designer.User)
	}
	if designer.Setup != nil {
		t.Fatalf("designer setup = %#v, want omitted", designer.Setup)
	}
	if designer.Cwd != "/var/lib/genesis/config" {
		t.Fatalf("designer cwd = %q", designer.Cwd)
	}
	if designer.Home != "/home/genesis" {
		t.Fatalf("designer home = %q", designer.Home)
	}
	if designer.GitHub != nil {
		t.Fatalf("designer github = %#v", designer.GitHub)
	}
	if len(designer.Secrets) != 1 || designer.Secrets[0] != deepSeekAPIKey {
		t.Fatalf("designer secrets = %#v, want default %s only", designer.Secrets, deepSeekAPIKey)
	}
	for _, secret := range designer.Secrets {
		if secret == syncTokenEnv {
			t.Fatal("designer must not name GENESIS_SYNC_TOKEN as a secret")
		}
	}
	for _, phrase := range []string{
		"agents.d",
		"rules.d",
		"exactly",
		"Sync",
		"inactive until",
		"desired state",
		"http://127.0.0.1:8787/events",
		"application/cloudevents+json",
		"No bearer",
		"8790",
		"urn:genesis:agent:",
		"dev.genesis.session.continue",
		"data.message",
		"listener failure",
		"dev.genesis.agent.finished",
		"rules-only",
		"omits repos",
		"max_parallel",
		"omitted when empty",
		"Other workers do not need",
		"data.transcript",
		"mode 0755",
		"0775",
		"umask 022",
		"/app/.venv",
		"/app/.venv/bin/python3",
		"/app/.venv/bin/dsh",
		"user: <username>",
		"home: /home/<username>",
		"cwd: /home/<username>/workspace",
		"workspace: private",
		"setup.groups",
		"/shared",
		"reasoning_effort",
		"GENESIS_SYNC_TOKEN is not in the environment",
		"genesis and root",
		"/bin/bash",
		"/etc/genesis/providers.d",
		repositorySchemaImagePath,
		"loader contract",
		"The only provider value the loader accepts is github.",
		"There is no local-project type.",
		"does not create a local project",
		"/var/lib/genesis/data/runs",
		"on PATH for every agent",
		"dedicated OS user",
		"genesis job",
		"genesis schedule",
		"Do not invent packages, PEMs, or sync tokens.",
		"Do not curl with GENESIS_SYNC_TOKEN",
		"listener accept path",
		"after this turn",
		"--each=15m",
		"Neural-net training",
		"genesis job stop",
		"genesis schedule cancel",
		"not a rules.d interval",
		"at least 1m",
	} {
		if !strings.Contains(designer.Instructions, phrase) {
			t.Fatalf("designer instructions missing %q", phrase)
		}
	}
	for _, phrase := range []string{
		"You should POST /sync",
		"run useradd",
		"call /sync yourself",
		"site.yaml",
		"stub",
		"stubs",
		"genesis.emit",
		"genesis_emit",
		"mode 0700",
	} {
		if strings.Contains(designer.Instructions, phrase) {
			t.Fatalf("designer instructions contain stale or privileged phrasing: %q", phrase)
		}
	}

	janitor, ok := agents["workspace-janitor"]
	if !ok || janitor.User != "workspace-janitor" {
		t.Fatalf("workspace-janitor = %#v", janitor)
	}

	rules, err := loadRules(filepath.Join(root, "rules.d"), agents)
	if err != nil {
		t.Fatal(err)
	}
	var design, example rule
	for _, candidate := range rules {
		switch candidate.name {
		case "designer.yaml":
			design = candidate
		case "example.yaml":
			example = candidate
		}
	}
	if design.Agent != "designer" {
		t.Fatalf("designer rule = %#v", design)
	}
	if design.Match["type"] != "dev.genesis.user.message" ||
		design.Match["source"] != "urn:genesis:control" ||
		design.Match["subject"] != "designer" {
		t.Fatalf("designer match = %#v", design.Match)
	}

	userMessage := cloudEvent{
		"specversion": json.RawMessage(`"1.0"`),
		"id":          json.RawMessage(`"msg-1"`),
		"source":      json.RawMessage(`"urn:genesis:control"`),
		"type":        json.RawMessage(`"dev.genesis.user.message"`),
		"subject":     json.RawMessage(`"designer"`),
		"data":        json.RawMessage(`{"message":"add a lab agent"}`),
	}
	if !design.matches(userMessage) {
		t.Fatal("designer rule did not match its dedicated user-message event")
	}
	wrongSubject := cloudEvent{
		"specversion": json.RawMessage(`"1.0"`),
		"id":          json.RawMessage(`"msg-2"`),
		"source":      json.RawMessage(`"urn:genesis:control"`),
		"type":        json.RawMessage(`"dev.genesis.user.message"`),
		"subject":     json.RawMessage(`"janitor"`),
	}
	if design.matches(wrongSubject) {
		t.Fatal("designer rule matched a different subject")
	}
	if example.Agent == "" {
		t.Fatal("committed example.yaml is missing")
	}
	if example.matches(userMessage) {
		t.Fatal("example.yaml must not match the designer user-message event")
	}

	generation, err := loadGeneration(filepath.Join(root, "agents.d"), filepath.Join(root, "rules.d"), filepath.Join(root, "repos.d"), filepath.Join(root, "providers.d"))
	if err != nil {
		t.Fatal(err)
	}
	plan := buildPlan(generation.agents, true)
	var sawDesignerPaths, sawDesignerUser, sawJanitorUser bool
	for _, intent := range plan.Intents {
		if intent.Agent != "designer" {
			if intent.Kind == intentEnsureAgentUser && intent.Agent == "workspace-janitor" {
				sawJanitorUser = true
			}
			continue
		}
		switch intent.Kind {
		case intentEnsureAgentPaths:
			sawDesignerPaths = true
		case intentEnsureAgentUser:
			sawDesignerUser = true
		}
	}
	if !sawDesignerPaths || sawDesignerUser {
		t.Fatalf("designer plan = %#v", plan.Intents)
	}
	if !sawJanitorUser {
		t.Fatal("workspace-janitor lost ensure_agent_user")
	}
	if _, err := evaluatePlan(plan); err == nil || !strings.Contains(err.Error(), "requires root genesis launch") {
		t.Fatalf("mixed designer+janitor plan must still require root for a dedicated user: %v", err)
	}
}

func TestDockerEntrypointSeedsMissingDefaultsWithoutOverwrite(t *testing.T) {
	root := repoRoot(t)
	entrypoint := filepath.Join(root, "docker-entrypoint.sh")
	defaults := t.TempDir()
	config := t.TempDir()
	data := t.TempDir()
	seedEnv := func(configDir, dataDir, defaultsDir string) []string {
		return append(os.Environ(),
			"GENESIS_CONFIG_DIR="+configDir,
			"GENESIS_DATA_DIR="+dataDir,
			"GENESIS_DEFAULTS_DIR="+defaultsDir,
		)
	}
	run := func(configDir, dataDir, defaultsDir string, args ...string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command("sh", append([]string{entrypoint}, args...)...)
		cmd.Env = seedEnv(configDir, dataDir, defaultsDir)
		return cmd.CombinedOutput()
	}

	copyFile(t, filepath.Join(root, "agents.d", "workspace-janitor.yaml"), filepath.Join(defaults, "agents.d", "workspace-janitor.yaml"))
	copyFile(t, filepath.Join(root, "agents.d", "designer.yaml"), filepath.Join(defaults, "agents.d", "designer.yaml"))
	copyFile(t, filepath.Join(root, "rules.d", "example.yaml"), filepath.Join(defaults, "rules.d", "example.yaml"))
	copyFile(t, filepath.Join(root, "rules.d", "designer.yaml"), filepath.Join(defaults, "rules.d", "designer.yaml"))

	oldJanitor := "instructions: persisted pre-upgrade janitor\ncwd: /tmp/work\nhome: /tmp/home\n"
	if err := os.MkdirAll(filepath.Join(config, "agents.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(config, "rules.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "agents.d", "workspace-janitor.yaml"), []byte(oldJanitor), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "rules.d", "example.yaml"), []byte("match:\n  type: dev.genesis.persisted\nagent: workspace-janitor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "rules.d", "operator-lab.yaml"), []byte("match:\n  type: dev.genesis.lab\nagent: workspace-janitor\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runSeed := func() []byte {
		t.Helper()
		output, err := run(config, data, defaults, "seed-config")
		if err != nil {
			t.Fatalf("seed-config: %v\n%s", err, output)
		}
		if bytes.Contains(output, []byte("workspace-janitor.yaml")) {
			t.Fatalf("seeded existing janitor:\n%s", output)
		}
		return output
	}
	first := runSeed()
	if !bytes.Contains(first, []byte("seeded missing agents.d/designer.yaml")) {
		t.Fatalf("expected designer seed log, got:\n%s", first)
	}
	if !bytes.Contains(first, []byte("seeded missing rules.d/designer.yaml")) {
		t.Fatalf("expected designer rule seed log, got:\n%s", first)
	}
	second := runSeed()
	if bytes.Contains(second, []byte("seeded missing")) {
		t.Fatalf("second seed copied files that already existed:\n%s", second)
	}

	gotJanitor, err := os.ReadFile(filepath.Join(config, "agents.d", "workspace-janitor.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJanitor) != oldJanitor {
		t.Fatalf("overwrote persisted janitor:\n%s", gotJanitor)
	}
	gotExample, err := os.ReadFile(filepath.Join(config, "rules.d", "example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(gotExample, []byte("dev.genesis.persisted")) {
		t.Fatalf("overwrote persisted example.yaml:\n%s", gotExample)
	}
	gotCanary, err := os.ReadFile(filepath.Join(config, "rules.d", "operator-lab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(gotCanary, []byte("dev.genesis.lab")) {
		t.Fatalf("lost operator file:\n%s", gotCanary)
	}

	designer, err := os.ReadFile(filepath.Join(config, "agents.d", "designer.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wantDesigner, err := os.ReadFile(filepath.Join(defaults, "agents.d", "designer.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(designer, wantDesigner) {
		t.Fatalf("seeded designer mismatch")
	}
	if info, err := os.Lstat(filepath.Join(config, "agents.d", "designer.yaml")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("seeded designer is not a regular file: %v %#v", err, info)
	}
	if _, err := os.Stat(filepath.Join(config, "rules.d", "designer.yaml")); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(config, "agents.d", "designer.yaml")); err != nil {
		t.Fatal(err)
	}
	reseed := runSeed()
	if !bytes.Contains(reseed, []byte("seeded missing agents.d/designer.yaml")) {
		t.Fatalf("deleted designer was not copied again:\n%s", reseed)
	}
	if bytes.Contains(reseed, []byte("workspace-janitor.yaml")) {
		t.Fatalf("reseed after delete overwrote janitor:\n%s", reseed)
	}

	empty := t.TempDir()
	output, err := run(empty, t.TempDir(), defaults, "seed-config")
	if err != nil {
		t.Fatalf("empty seed-config: %v\n%s", err, output)
	}
	for _, rel := range []string{
		"agents.d/designer.yaml",
		"agents.d/workspace-janitor.yaml",
		"rules.d/designer.yaml",
		"rules.d/example.yaml",
	} {
		if _, err := os.Stat(filepath.Join(empty, rel)); err != nil {
			t.Fatalf("empty volume missing %s: %v", rel, err)
		}
	}
}

func TestDockerEntrypointSeedRefusesUnsafePaths(t *testing.T) {
	root := repoRoot(t)
	entrypoint := filepath.Join(root, "docker-entrypoint.sh")
	run := func(configDir, dataDir, defaultsDir string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command("sh", entrypoint, "seed-config")
		cmd.Env = append(os.Environ(),
			"GENESIS_CONFIG_DIR="+configDir,
			"GENESIS_DATA_DIR="+dataDir,
			"GENESIS_DEFAULTS_DIR="+defaultsDir,
		)
		return cmd.CombinedOutput()
	}
	writeDefault := func(defaults, rel, body string) {
		t.Helper()
		copyFile(t, filepath.Join(root, "agents.d", "workspace-janitor.yaml"), filepath.Join(defaults, "agents.d", "workspace-janitor.yaml"))
		copyFile(t, filepath.Join(root, "agents.d", "designer.yaml"), filepath.Join(defaults, "agents.d", "designer.yaml"))
		copyFile(t, filepath.Join(root, "rules.d", "example.yaml"), filepath.Join(defaults, "rules.d", "example.yaml"))
		copyFile(t, filepath.Join(root, "rules.d", "designer.yaml"), filepath.Join(defaults, "rules.d", "designer.yaml"))
		if rel != "" {
			path := filepath.Join(defaults, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("dangling dest symlink is not written through", func(t *testing.T) {
		defaults := t.TempDir()
		config := t.TempDir()
		writeDefault(defaults, "", "")
		if err := os.MkdirAll(filepath.Join(config, "agents.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(config, "rules.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		canary := filepath.Join(t.TempDir(), "outside.yaml")
		if err := os.WriteFile(canary, []byte("untouched\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(canary, filepath.Join(config, "agents.d", "designer.yaml")); err != nil {
			t.Fatal(err)
		}
		output, err := run(config, t.TempDir(), defaults)
		if err != nil {
			t.Fatalf("seed-config: %v\n%s", err, output)
		}
		if !bytes.Contains(output, []byte("skipping existing symlink agents.d/designer.yaml")) {
			t.Fatalf("expected symlink skip log, got:\n%s", output)
		}
		if bytes.Contains(output, []byte("seeded missing agents.d/designer.yaml")) {
			t.Fatalf("seeded through dest symlink:\n%s", output)
		}
		got, err := os.ReadFile(canary)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "untouched\n" {
			t.Fatalf("wrote through dangling dest symlink:\n%s", got)
		}
		info, err := os.Lstat(filepath.Join(config, "agents.d", "designer.yaml"))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("replaced dest symlink: %v %#v", err, info)
		}
	})

	t.Run("dest directory symlink is refused", func(t *testing.T) {
		defaults := t.TempDir()
		config := t.TempDir()
		writeDefault(defaults, "", "")
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(config, "agents.d")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(config, "rules.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		output, err := run(config, t.TempDir(), defaults)
		if err == nil {
			t.Fatalf("seeded into symlink agents.d:\n%s", output)
		}
		if !bytes.Contains(output, []byte("must not be a symlink")) {
			t.Fatalf("error = %v\n%s", err, output)
		}
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("wrote into symlink agents.d target: %#v", entries)
		}
	})

	t.Run("source yaml symlink is refused", func(t *testing.T) {
		defaults := t.TempDir()
		writeDefault(defaults, "", "")
		designer := filepath.Join(defaults, "agents.d", "designer.yaml")
		if err := os.Remove(designer); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(defaults, "agents.d", "workspace-janitor.yaml"), designer); err != nil {
			t.Fatal(err)
		}
		output, err := run(t.TempDir(), t.TempDir(), defaults)
		if err == nil {
			t.Fatalf("seeded source symlink:\n%s", output)
		}
		if !bytes.Contains(output, []byte("non-regular agents.d/designer.yaml")) {
			t.Fatalf("error = %v\n%s", err, output)
		}
	})

	t.Run("unsafe default filename is refused", func(t *testing.T) {
		defaults := t.TempDir()
		writeDefault(defaults, "agents.d/-evil.yaml", "instructions: no\ncwd: /tmp/a\nhome: /tmp/b\n")
		output, err := run(t.TempDir(), t.TempDir(), defaults)
		if err == nil {
			t.Fatalf("seeded unsafe name:\n%s", output)
		}
		if !bytes.Contains(output, []byte("unsafe agents.d/-evil.yaml")) {
			t.Fatalf("error = %v\n%s", err, output)
		}
	})

	t.Run("default directory is refused", func(t *testing.T) {
		defaults := t.TempDir()
		writeDefault(defaults, "", "")
		if err := os.Mkdir(filepath.Join(defaults, "agents.d", "nested.yaml"), 0o755); err != nil {
			t.Fatal(err)
		}
		output, err := run(t.TempDir(), t.TempDir(), defaults)
		if err == nil {
			t.Fatalf("seeded directory:\n%s", output)
		}
		if !bytes.Contains(output, []byte("non-regular agents.d/nested.yaml")) {
			t.Fatalf("error = %v\n%s", err, output)
		}
	})
}

func TestDockerEntrypointReposDirectory(t *testing.T) {
	root := repoRoot(t)
	entrypoint := filepath.Join(root, "docker-entrypoint.sh")
	run := func(configDir, dataDir, defaultsDir string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command("sh", entrypoint, "seed-config")
		cmd.Env = append(os.Environ(),
			"GENESIS_CONFIG_DIR="+configDir,
			"GENESIS_DATA_DIR="+dataDir,
			"GENESIS_DEFAULTS_DIR="+defaultsDir,
		)
		return cmd.CombinedOutput()
	}

	t.Run("missing defaults create an empty directory and no declaration", func(t *testing.T) {
		config := t.TempDir()
		output, err := run(config, t.TempDir(), t.TempDir())
		if err != nil {
			t.Fatalf("seed-config: %v\n%s", err, output)
		}
		info, err := os.Lstat(filepath.Join(config, "repos.d"))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("repos.d = %v %#v", err, info)
		}
		entries, err := os.ReadDir(filepath.Join(config, "repos.d"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("invented repository declarations: %#v", entries)
		}
	})

	t.Run("copies a missing default and does not overwrite", func(t *testing.T) {
		defaults := t.TempDir()
		config := t.TempDir()
		copyFile(t, filepath.Join(root, "repos.d", "example.yaml"), filepath.Join(defaults, "repos.d", "example.yaml"))
		if err := os.MkdirAll(filepath.Join(config, "repos.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		kept := []byte("provider: github\n")
		if err := os.WriteFile(filepath.Join(config, "repos.d", "operator.yaml"), kept, 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := run(config, t.TempDir(), defaults)
		if err != nil {
			t.Fatalf("seed-config: %v\n%s", err, output)
		}
		if !bytes.Contains(output, []byte("seeded missing repos.d/example.yaml")) {
			t.Fatalf("expected example seed log, got:\n%s", output)
		}
		got, err := os.ReadFile(filepath.Join(config, "repos.d", "operator.yaml"))
		if err != nil || string(got) != string(kept) {
			t.Fatalf("overwrote operator repository file: %v %q", err, got)
		}
		again, err := run(config, t.TempDir(), defaults)
		if err != nil {
			t.Fatalf("second seed: %v\n%s", err, again)
		}
		if bytes.Contains(again, []byte("seeded missing repos.d/example.yaml")) {
			t.Fatalf("second seed recopied example:\n%s", again)
		}
	})

	t.Run("source symlink and unsafe name are refused", func(t *testing.T) {
		defaults := t.TempDir()
		example := filepath.Join(defaults, "repos.d", "example.yaml")
		copyFile(t, filepath.Join(root, "repos.d", "example.yaml"), example)
		if err := os.Remove(example); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "repos.d", "example.yaml"), example); err != nil {
			t.Fatal(err)
		}
		output, err := run(t.TempDir(), t.TempDir(), defaults)
		if err == nil || !bytes.Contains(output, []byte("non-regular repos.d/example.yaml")) {
			t.Fatalf("source symlink error = %v\n%s", err, output)
		}

		defaults = t.TempDir()
		copyFile(t, filepath.Join(root, "repos.d", "example.yaml"), filepath.Join(defaults, "repos.d", "-evil.yaml"))
		output, err = run(t.TempDir(), t.TempDir(), defaults)
		if err == nil || !bytes.Contains(output, []byte("unsafe repos.d/-evil.yaml")) {
			t.Fatalf("unsafe name error = %v\n%s", err, output)
		}
	})

	t.Run("destination directory symlink is refused", func(t *testing.T) {
		defaults := t.TempDir()
		config := t.TempDir()
		outside := t.TempDir()
		copyFile(t, filepath.Join(root, "repos.d", "example.yaml"), filepath.Join(defaults, "repos.d", "example.yaml"))
		if err := os.Symlink(outside, filepath.Join(config, "repos.d")); err != nil {
			t.Fatal(err)
		}
		output, err := run(config, t.TempDir(), defaults)
		if err == nil || !bytes.Contains(output, []byte("must not be a symlink")) {
			t.Fatalf("dest symlink error = %v\n%s", err, output)
		}
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("wrote through repos.d symlink: %#v", entries)
		}
	})
}

func TestDockerEntrypointOwnDoesNotFollowSymlinks(t *testing.T) {
	root := repoRoot(t)
	entrypoint := filepath.Join(root, "docker-entrypoint.sh")
	defaults := t.TempDir()
	config := t.TempDir()
	data := t.TempDir()
	copyFile(t, filepath.Join(root, "agents.d", "designer.yaml"), filepath.Join(defaults, "agents.d", "designer.yaml"))
	copyFile(t, filepath.Join(root, "rules.d", "designer.yaml"), filepath.Join(defaults, "rules.d", "designer.yaml"))
	cmd := exec.Command("sh", entrypoint, "seed-config")
	cmd.Env = append(os.Environ(),
		"GENESIS_CONFIG_DIR="+config,
		"GENESIS_DATA_DIR="+data,
		"GENESIS_DEFAULTS_DIR="+defaults,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed-config: %v\n%s", err, output)
	}

	canary := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(canary, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, filepath.Join(config, "agents.d", "alias.yaml")); err != nil {
		t.Fatal(err)
	}

	own := exec.Command("sh", entrypoint, "own-config")
	own.Env = append(os.Environ(),
		"GENESIS_CONFIG_DIR="+config,
		"GENESIS_DATA_DIR="+data,
		"GENESIS_DEFAULTS_DIR="+defaults,
	)
	if output, err := own.CombinedOutput(); err != nil {
		t.Fatalf("own-config: %v\n%s", err, output)
	}

	designerInfo, err := os.Stat(filepath.Join(config, "agents.d", "designer.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if designerInfo.Mode().Perm() != 0o644 {
		t.Fatalf("designer mode = %o", designerInfo.Mode().Perm())
	}
	canaryInfo, err := os.Stat(canary)
	if err != nil {
		t.Fatal(err)
	}
	if canaryInfo.Mode().Perm() != 0o600 {
		t.Fatalf("followed symlink and chmod'd canary: %o", canaryInfo.Mode().Perm())
	}
	linkInfo, err := os.Lstat(filepath.Join(config, "agents.d", "alias.yaml"))
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("alias.yaml is no longer a symlink: %v %#v", err, linkInfo)
	}
	dataInfo, err := os.Stat(filepath.Join(data, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if dataInfo.Mode().Perm() != 0o700 {
		t.Fatalf("data/runs mode = %o", dataInfo.Mode().Perm())
	}
	rootInfo, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o711 {
		t.Fatalf("data root mode = %o", rootInfo.Mode().Perm())
	}
	transcriptDir := filepath.Join(data, "transcripts")
	if info, err := os.Stat(transcriptDir); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("transcripts mode after first own-config = %v %v", info, err)
	}
	transcript := filepath.Join(transcriptDir, "gen_owned.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	again := exec.Command("sh", entrypoint, "own-config")
	again.Env = own.Env
	if output, err := again.CombinedOutput(); err != nil {
		t.Fatalf("own-config transcripts: %v\n%s", err, output)
	}
	transcriptInfo, err := os.Stat(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if transcriptInfo.Mode().Perm() != 0o644 {
		t.Fatalf("transcript mode = %o", transcriptInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(transcriptDir)
	if err != nil || dirInfo.Mode().Perm() != 0o755 {
		t.Fatalf("transcripts mode = %v %v", dirInfo, err)
	}
	runsAgain, err := os.Stat(filepath.Join(data, "runs"))
	if err != nil || runsAgain.Mode().Perm() != 0o700 {
		t.Fatalf("runs mode after transcripts = %v %v", runsAgain, err)
	}
}

func TestDockerEntrypointReusedVolumeOpensTranscripts(t *testing.T) {
	root := repoRoot(t)
	entrypoint := filepath.Join(root, "docker-entrypoint.sh")
	config := t.TempDir()
	data := t.TempDir()
	defaults := t.TempDir()
	secrets := t.TempDir()
	for _, name := range []string{"GITHUB_APP_RECONCILER_PEM", "GITHUB_APP_ID", "GITHUB_APP_WEBHOOK_SECRET"} {
		if err := os.WriteFile(filepath.Join(secrets, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runs := filepath.Join(data, "runs")
	transcripts := filepath.Join(data, "transcripts")
	sessionHome := filepath.Join(data, "sessions", "gen_stable", "dsh_home")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(transcripts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sessionHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(transcripts, "gen_reused.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionNote := filepath.Join(sessionHome, "note")
	if err := os.WriteFile(sessionNote, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionLog := filepath.Join(sessionHome, "session.v3.jsonl")
	if err := os.WriteFile(sessionLog, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := strippedEnv(t,
		"GENESIS_CONFIG_DIR="+config,
		"GENESIS_DATA_DIR="+data,
		"GENESIS_DEFAULTS_DIR="+defaults,
		"GENESIS_CREDENTIALS_DIR="+filepath.Join(t.TempDir(), "credentials"),
		"GENESIS_SECRETS_DIR="+secrets,
		"GENESIS_PROVIDERS_DIR="+filepath.Join(t.TempDir(), "no-providers"),
	)
	perm := func(path string) os.FileMode {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	runOwn := func() {
		t.Helper()
		cmd := exec.Command("sh", entrypoint, "own-config")
		cmd.Env = env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("own-config: %v\n%s", err, output)
		}
	}
	assertModes := func() {
		t.Helper()
		if got := perm(data); got != 0o711 {
			t.Fatalf("data root mode = %o", got)
		}
		if got := perm(runs); got != 0o700 {
			t.Fatalf("runs mode = %o", got)
		}
		if got := perm(transcripts); got != 0o755 {
			t.Fatalf("transcripts mode = %o", got)
		}
		if got := perm(transcript); got != 0o644 {
			t.Fatalf("transcript mode = %o", got)
		}
		if got := perm(filepath.Join(data, "sessions")); got != 0o711 {
			t.Fatalf("sessions mode = %o", got)
		}
		for _, path := range []string{filepath.Dir(sessionHome), sessionHome} {
			if got := perm(path); got != 0o710 {
				t.Fatalf("%s mode = %o", path, got)
			}
		}
		if got := perm(sessionLog); got != 0o640 {
			t.Fatalf("session log mode = %o", got)
		}
		if got := perm(sessionNote); got != 0o600 {
			t.Fatalf("session file mode = %o", got)
		}
	}
	runOwn()
	assertModes()
	runOwn()
	assertModes()
}

func TestDockerEntrypointChownsTranscriptsOnEveryStart(t *testing.T) {
	root := repoRoot(t)
	entrypoint := filepath.Join(root, "docker-entrypoint.sh")
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "chown.log")
	writeBin := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeBin("id", "#!/bin/sh\nif [ \"$1\" = \"-u\" ]; then\n\techo 0\n\texit 0\nfi\nexec /usr/bin/id \"$@\"\n")
	writeBin("chown", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CHOWN_LOG\"\nexit 0\n")

	config := t.TempDir()
	data := t.TempDir()
	defaults := t.TempDir()
	secrets := t.TempDir()
	for _, name := range []string{"GITHUB_APP_RECONCILER_PEM", "GITHUB_APP_ID", "GITHUB_APP_WEBHOOK_SECRET"} {
		if err := os.WriteFile(filepath.Join(secrets, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	transcripts := filepath.Join(data, "transcripts")
	env := strippedEnv(t,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CHOWN_LOG="+logPath,
		"GENESIS_CONFIG_DIR="+config,
		"GENESIS_DATA_DIR="+data,
		"GENESIS_DEFAULTS_DIR="+defaults,
		"GENESIS_CREDENTIALS_DIR="+filepath.Join(t.TempDir(), "credentials"),
		"GENESIS_SECRETS_DIR="+secrets,
		"GENESIS_PROVIDERS_DIR="+filepath.Join(t.TempDir(), "no-providers"),
	)
	runOwn := func() {
		t.Helper()
		if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", entrypoint, "own-config")
		cmd.Env = env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("own-config: %v\n%s", err, output)
		}
		payload, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(payload, []byte(transcripts)) {
			t.Fatalf("chown log missing transcripts:\n%s", payload)
		}
	}
	sessionDir := filepath.Join(data, "sessions", "gen_owned", "dsh_home", "sessions", "proj", "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionLog := filepath.Join(sessionDir, "session.v3.jsonl")
	if err := os.WriteFile(sessionLog, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(sessionDir, "notes.txt")
	if err := os.WriteFile(otherFile, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOwn()
	info, err := os.Stat(transcripts)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("created transcripts mode = %o", info.Mode().Perm())
	}
	runsInfo, err := os.Stat(filepath.Join(data, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if runsInfo.Mode().Perm() != 0o700 {
		t.Fatalf("runs mode = %o", runsInfo.Mode().Perm())
	}
	rootInfo, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o711 {
		t.Fatalf("data root mode = %o", rootInfo.Mode().Perm())
	}
	// A second start against the directory created above must chown it again
	// and leave runs private.
	runOwn()
	info, err = os.Stat(transcripts)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("reused transcripts mode = %o", info.Mode().Perm())
	}
	runsInfo, err = os.Stat(filepath.Join(data, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if runsInfo.Mode().Perm() != 0o700 {
		t.Fatalf("reused runs mode = %o", runsInfo.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Join(data, "sessions")); err != nil || info.Mode().Perm() != 0o711 {
		t.Fatalf("sessions mode = %v %v", info, err)
	}
	if info, err := os.Stat(sessionLog); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("session log mode = %v %v", info, err)
	}
	if info, err := os.Stat(sessionDir); err != nil || info.Mode().Perm() != 0o710 {
		t.Fatalf("session dir mode = %v %v", info, err)
	}
	if info, err := os.Stat(otherFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("non-session file mode = %v %v", info, err)
	}
}

func strippedEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") || strings.HasPrefix(entry, "CHOWN_LOG=") || strings.HasPrefix(entry, "GENESIS_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, extra...)
}

func TestCommittedOracleIsDedicatedReviewer(t *testing.T) {
	root := repoRoot(t)
	agents, err := loadAgents(filepath.Join(root, "agents.d"))
	if err != nil {
		t.Fatal(err)
	}
	oracle, ok := agents["oracle"]
	if !ok {
		t.Fatal("committed agents.d is missing oracle")
	}
	if oracle.User != "oracle" {
		t.Fatalf("oracle user = %q", oracle.User)
	}
	if oracle.Home != "/home/oracle" || oracle.Cwd != "/home/oracle/workspace" {
		t.Fatalf("oracle home/cwd = %s %s", oracle.Home, oracle.Cwd)
	}
	if oracle.Setup == nil || oracle.Setup.Workspace != workspacePrivate {
		t.Fatalf("oracle setup = %#v", oracle.Setup)
	}
	if len(oracle.Setup.Groups) != 2 || oracle.Setup.Groups[0] != sharedGroupName || oracle.Setup.Groups[1] != reporterGroupName {
		t.Fatalf("oracle groups = %#v", oracle.Setup.Groups)
	}
	if oracle.GitHub != nil {
		t.Fatalf("oracle github = %#v", oracle.GitHub)
	}
	for _, phrase := range []string{
		"data.runid",
		"data.agent",
		"data.session",
		"data.definition",
		"data.outcome",
		"reports/",
		"http://127.0.0.1:8787/events",
		"application/cloudevents+json",
		"dev.genesis.session.continue",
		"data.message",
		"urn:genesis:agent:oracle",
		"listener failure",
		"dev.genesis.agent.finished",
		"exit criterion",
		"You are the reporter",
		"genesis publish --kind report",
		"--body-file reports/<runid>.md",
		"--supersedes",
		"at most 120 characters",
		"at most 400 characters",
		"at most 16 KiB",
	} {
		if !strings.Contains(oracle.Instructions, phrase) {
			t.Fatalf("oracle instructions missing %q", phrase)
		}
	}
	for _, kind := range publicationKinds {
		if !strings.Contains(oracle.Instructions, kind) {
			t.Fatalf("oracle instructions do not name publication kind %q", kind)
		}
	}
	if maxPublicationHeadline != 120 || maxPublicationLede != 400 || maxPublicationBody != 16<<10 {
		t.Fatal("publication limits changed: update the oracle and designer instructions and docs/publications.md")
	}
	for _, phrase := range []string{
		"useradd",
		"GENESIS_SYNC_TOKEN",
		"POST /sync",
		"genesis.emit",
		"genesis_emit",
		"mode 0700",
	} {
		if strings.Contains(oracle.Instructions, phrase) {
			t.Fatalf("oracle instructions contain %q", phrase)
		}
	}

	designer := agents["designer"]
	for _, phrase := range []string{
		"agents.d/oracle.yaml",
		"rules.d/oracle.yaml",
		"listener curl",
		"Other workers do not need",
		"user: oracle",
		"home: /home/oracle",
		"cwd: /home/oracle/workspace",
		"workspace: private",
		"mostly redundant",
		"when a cycle ends",
		"genesis publish --kind <kind> --headline <text> --lede",
		"setup.groups contains reporter",
		"dev.genesis.publication.submitted",
		"append-only",
		"at most 120 characters",
		"at most 400",
		"16 KiB",
		"setup.groups shared and reporter",
	} {
		if !strings.Contains(designer.Instructions, phrase) {
			t.Fatalf("designer instructions missing %q", phrase)
		}
	}
	for _, kind := range publicationKinds {
		if !strings.Contains(designer.Instructions, kind) {
			t.Fatalf("designer instructions do not name publication kind %q", kind)
		}
	}

	rules, err := loadRules(filepath.Join(root, "rules.d"), agents)
	if err != nil {
		t.Fatal(err)
	}
	var oracleRule rule
	for _, candidate := range rules {
		if candidate.name == "oracle.yaml" {
			oracleRule = candidate
		}
	}
	if oracleRule.Agent != "oracle" {
		t.Fatalf("oracle rule = %#v", oracleRule)
	}
	if oracleRule.Match["type"] != agentFinishedType || oracleRule.Match["subject"] != "*" {
		t.Fatalf("oracle match = %#v", oracleRule.Match)
	}
	finished := cloudEvent{
		"specversion": json.RawMessage(`"1.0"`),
		"id":          json.RawMessage(`"evt-oracle"`),
		"source":      json.RawMessage(`"urn:genesis:agent:designer"`),
		"type":        json.RawMessage(`"dev.genesis.agent.finished"`),
		"subject":     json.RawMessage(`"designer"`),
	}
	if !oracleRule.matches(finished) {
		t.Fatal("oracle rule did not match a one-segment finish subject")
	}
	if skipFinishedSelf(oracleRule, finished) {
		t.Fatal("oracle rule skipped a different agent")
	}
	finished["subject"] = json.RawMessage(`"oracle"`)
	if !oracleRule.matches(finished) || !skipFinishedSelf(oracleRule, finished) {
		t.Fatal("oracle rule should match and skip its own finish")
	}
	twoSegment := cloudEvent{
		"specversion": json.RawMessage(`"1.0"`),
		"type":        json.RawMessage(`"dev.genesis.agent.finished"`),
		"subject":     json.RawMessage(`"designer/extra"`),
	}
	if oracleRule.matches(twoSegment) {
		t.Fatal("subject * matched more than one path segment")
	}
}
