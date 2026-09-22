package main

import (
	"bytes"
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
	} {
		if !strings.Contains(designer.Instructions, phrase) {
			t.Fatalf("designer instructions missing %q", phrase)
		}
	}
	if strings.Contains(designer.Instructions, "POST /sync yourself") == false {
		t.Fatal("designer instructions must forbid self-sync")
	}

	janitor, ok := agents["workspace-janitor"]
	if !ok || janitor.User != "workspace-janitor" {
		t.Fatalf("workspace-janitor = %#v", janitor)
	}

	rules, err := loadRules(filepath.Join(root, "rules.d"), agents)
	if err != nil {
		t.Fatal(err)
	}
	var design rule
	for _, candidate := range rules {
		if candidate.name == "designer.yaml" {
			design = candidate
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

	generation, err := loadGeneration(filepath.Join(root, "agents.d"), filepath.Join(root, "rules.d"))
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
}

func TestDockerEntrypointSeedsMissingDefaultsWithoutOverwrite(t *testing.T) {
	root := repoRoot(t)
	entrypoint := filepath.Join(root, "docker-entrypoint.sh")
	defaults := t.TempDir()
	config := t.TempDir()
	data := t.TempDir()

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

	runSeed := func() {
		t.Helper()
		cmd := exec.Command("sh", entrypoint, "seed-config")
		cmd.Env = append(os.Environ(),
			"GENESIS_CONFIG_DIR="+config,
			"GENESIS_DATA_DIR="+data,
			"GENESIS_DEFAULTS_DIR="+defaults,
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("seed-config: %v\n%s", err, output)
		}
		if !bytes.Contains(output, []byte("seeded missing agents.d/designer.yaml")) {
			t.Fatalf("expected designer seed log, got:\n%s", output)
		}
		if bytes.Contains(output, []byte("workspace-janitor.yaml")) {
			t.Fatalf("seeded existing janitor:\n%s", output)
		}
	}
	runSeed()
	runSeed()

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
	if _, err := os.Stat(filepath.Join(config, "rules.d", "designer.yaml")); err != nil {
		t.Fatal(err)
	}

	empty := t.TempDir()
	cmd := exec.Command("sh", entrypoint, "seed-config")
	cmd.Env = append(os.Environ(),
		"GENESIS_CONFIG_DIR="+empty,
		"GENESIS_DATA_DIR="+t.TempDir(),
		"GENESIS_DEFAULTS_DIR="+defaults,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
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
