package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

type fakeDshReport struct {
	Cwd  string   `json:"cwd"`
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

func TestFakeDshProcess(t *testing.T) {
	if os.Getenv("GENESIS_FAKE_DSH") != "1" {
		return
	}

	if gate := os.Getenv("GENESIS_FAKE_DSH_GATE"); gate != "" {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(2)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		os.Exit(3)
	}
	arguments := []string{}
	for index, argument := range os.Args {
		if argument == "--" {
			arguments = append(arguments, os.Args[index+1:]...)
			break
		}
	}
	environment := os.Environ()
	sort.Strings(environment)
	report, err := json.Marshal(fakeDshReport{Cwd: cwd, Args: arguments, Env: environment})
	if err != nil {
		os.Exit(4)
	}

	output := os.Getenv("GENESIS_FAKE_DSH_OUTPUT")
	if err := os.WriteFile(output+".tmp", report, 0o600); err != nil {
		os.Exit(5)
	}
	if err := os.Rename(output+".tmp", output); err != nil {
		os.Exit(6)
	}
	os.Exit(0)
}

func TestEventServerLaunchesFakeDshAsynchronously(t *testing.T) {
	rulesDir := t.TempDir()
	cwd := t.TempDir()
	output := rulesDir + "/result.json"
	gate := rulesDir + "/release"
	writeRule(t, rulesDir+"/01-test.yaml", rule{
		Match: map[string]string{
			"type":    "com.example.run",
			"source":  "urn:test",
			"subject": "ready",
		},
		Run: fakeRun(cwd, output, gate, "alpha", "two words"),
	})

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := &eventServer{rulesDir: rulesDir, dshPath: executable}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- sendEvent(server, map[string]any{
			"specversion": "1.0",
			"id":          "evt-1",
			"source":      "urn:test",
			"type":        "com.example.run",
			"subject":     "ready",
			"data":        map[string]any{"ignored": true},
		})
	}()

	var response *httptest.ResponseRecorder
	select {
	case response = <-done:
	case <-time.After(time.Second):
		t.Fatal("handler waited for dsh to exit")
	}
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("fake dsh exited before its gate was released: %v", err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	report := waitForReport(t, output)
	if report.Cwd != cwd {
		t.Fatalf("cwd = %q, want %q", report.Cwd, cwd)
	}
	if want := []string{"alpha", "two words"}; !reflect.DeepEqual(report.Args, want) {
		t.Fatalf("args = %#v, want %#v", report.Args, want)
	}
	wantEnvironment := []string{
		"GENESIS_FAKE_DSH=1",
		"GENESIS_FAKE_DSH_GATE=" + gate,
		"GENESIS_FAKE_DSH_OUTPUT=" + output,
		"ONLY_DECLARED=yes",
	}
	sort.Strings(wantEnvironment)
	if !reflect.DeepEqual(report.Env, wantEnvironment) {
		t.Fatalf("environment = %#v, want only %#v", report.Env, wantEnvironment)
	}
}

func TestEventServerExactMatchesAndReloadsEveryRequest(t *testing.T) {
	rulesDir := t.TempDir()
	cwd := t.TempDir()
	output := rulesDir + "/result.json"
	rulePath := rulesDir + "/reload.yaml"
	loadedRule := rule{
		Match: map[string]string{"type": "com.example.run", "source": "urn:one"},
		Run:   fakeRun(cwd, output, ""),
	}
	writeRule(t, rulePath, loadedRule)

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := &eventServer{rulesDir: rulesDir, dshPath: executable}
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-2",
		"source":      "urn:two",
		"type":        "com.example.run",
	}

	response := sendEvent(server, event)
	if response.Code != http.StatusNoContent {
		t.Fatalf("non-match status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("non-matching event launched dsh: %v", err)
	}

	loadedRule.Match["source"] = "urn:two"
	writeRule(t, rulePath, loadedRule)
	response = sendEvent(server, event)
	if response.Code != http.StatusAccepted {
		t.Fatalf("reloaded match status = %d, body = %s", response.Code, response.Body.String())
	}
	_ = waitForReport(t, output)
}

func TestEventServerRequiresStructuredCloudEvent(t *testing.T) {
	server := &eventServer{rulesDir: t.TempDir(), dshPath: "/unused"}

	request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"specversion":"1.0"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("plain JSON status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"specversion":"1.0"}`))
	request.Header.Set("Content-Type", cloudEventsJSON)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("incomplete CloudEvent status = %d", response.Code)
	}
}

func TestLoadRulesRejectsAnExecutableOverride(t *testing.T) {
	rulesDir := t.TempDir()
	contents := `
match:
  type: com.example.run
run:
  cwd: /tmp
  args: []
  env: {}
  executable: /bin/sh
`
	if err := os.WriteFile(rulesDir+"/invalid.yaml", []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRules(rulesDir); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("loadRules error = %v, want unknown executable field", err)
	}
}

func fakeRun(cwd, output, gate string, arguments ...string) runSpec {
	return runSpec{
		Cwd:  cwd,
		Args: append([]string{"-test.run=^TestFakeDshProcess$", "--"}, arguments...),
		Env: map[string]string{
			"GENESIS_FAKE_DSH":        "1",
			"GENESIS_FAKE_DSH_GATE":   gate,
			"GENESIS_FAKE_DSH_OUTPUT": output,
			"ONLY_DECLARED":           "yes",
		},
	}
}

func writeRule(t *testing.T, path string, value rule) {
	t.Helper()
	contents, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sendEvent(server *eventServer, event map[string]any) *httptest.ResponseRecorder {
	contents, _ := json.Marshal(event)
	request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(string(contents)))
	request.Header.Set("Content-Type", cloudEventsJSON)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func waitForReport(t *testing.T, path string) fakeDshReport {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			var report fakeDshReport
			if err := json.Unmarshal(contents, &report); err != nil {
				t.Fatal(err)
			}
			return report
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for fake dsh report at %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
