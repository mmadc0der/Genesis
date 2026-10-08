package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentFormSchemaFollowsStruct(t *testing.T) {
	fields := agentFormSchema()
	byName := map[string]formField{}
	for _, field := range fields {
		byName[field.Name] = field
	}
	for _, name := range []string{"instructions", "cwd", "home", "user", "setup", "env", "secrets", "github", "max_parallel", "reasoning_effort"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("schema missing %s", name)
		}
	}
	if byName["instructions"].Format != "markdown" || !byName["instructions"].Required {
		t.Fatalf("instructions hint = %#v", byName["instructions"])
	}
	if byName["reasoning_effort"].Type != "string" || len(byName["reasoning_effort"].Enum) == 0 {
		t.Fatalf("reasoning_effort = %#v", byName["reasoning_effort"])
	}
	if byName["setup"].Type != "object" || len(byName["setup"].Fields) == 0 {
		t.Fatalf("setup = %#v", byName["setup"])
	}
	if byName["max_parallel"].Type != "integer" {
		t.Fatalf("max_parallel type = %s", byName["max_parallel"].Type)
	}
}

func TestSaveAgentDocumentValidates(t *testing.T) {
	dir := t.TempDir()
	id := "worker"
	path := filepath.Join(dir, id+".yaml")
	body := []byte("instructions: hello\ncwd: /tmp/cwd-a\nhome: /tmp/home-a\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	err := saveAgentDocument(dir, id, map[string]any{
		"instructions":     "hello",
		"cwd":              "/tmp/cwd-a",
		"home":             "/tmp/home-a",
		"reasoning_effort": "nope",
	})
	if err == nil {
		t.Fatal("expected reasoning_effort to be rejected")
	}
	if err := saveAgentDocument(dir, id, map[string]any{
		"instructions":     "hello there",
		"cwd":              "/tmp/cwd-a",
		"home":             "/tmp/home-a",
		"reasoning_effort": "low",
		"max_parallel":     float64(2),
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := readAgentDocument(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if doc["reasoning_effort"] != "low" {
		t.Fatalf("saved %#v", doc)
	}
}
