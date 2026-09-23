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
		"exact",
		"Sync",
		"active",
		"desired",
		"cwd",
		"home",
		"user",
		"GENESIS_SYNC_TOKEN",
		"useradd",
		"You must not POST /sync yourself",
		"Do not run privileged host setup",
	} {
		if !strings.Contains(designer.Instructions, phrase) {
			t.Fatalf("designer instructions missing %q", phrase)
		}
	}
	for _, phrase := range []string{
		"You should POST /sync",
		"run useradd",
		"call /sync yourself",
	} {
		if strings.Contains(designer.Instructions, phrase) {
			t.Fatalf("designer instructions imply a privileged or self-sync action: %q", phrase)
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

	generation, err := loadGeneration(filepath.Join(root, "agents.d"), filepath.Join(root, "rules.d"), filepath.Join(root, "repos.d"))
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
	if _, err := evaluatePlan(plan); err == nil || !strings.Contains(err.Error(), "workspace-janitor") {
		t.Fatalf("mixed designer+janitor plan must still require root for the dedicated janitor: %v", err)
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
}
