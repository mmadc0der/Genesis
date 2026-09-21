package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcessRunnerDrainsStderrAfterStdoutCloses(t *testing.T) {
	const (
		prefix  = "runner stderr"
		trailer = "END_STDERR"
		fill    = 256 * 1024
	)
	fakePython := writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_stderr","session_id":"session-stderr"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-stderr","finish_reason":"completed","final_response":"ok","error":null,"diagnostics":null}'
exec 1>&-
/usr/bin/python3 -c 'import sys; sys.stderr.write("runner stderr\n" + "x"*262144 + "END_STDERR\n")'
`)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	document := sampleRunInvocation("gen_stderr")
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	processRunner{pythonPath: fakePython, source: "src", logger: logger, store: store}.Run(document)

	records := decodeLogRecords(t, logs.Bytes())
	finished := records[len(records)-1]
	stderrText, _ := finished["stderr"].(string)
	if !strings.HasPrefix(stderrText, prefix) || !strings.HasSuffix(stderrText, trailer) {
		t.Fatalf("log stderr prefix/suffix missing: len=%d value=%q", len(stderrText), truncateForTest(stderrText))
	}
	wantLen := len(prefix) + 1 + fill + len(trailer)
	if len(stderrText) != wantLen {
		t.Fatalf("log stderr length = %d, want %d", len(stderrText), wantLen)
	}
	logged, err := os.ReadFile(filepath.Join(document.RunDir, stderrFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(logged)) != stderrText {
		t.Fatalf("stderr.log does not match slog stderr")
	}
}

func TestProcessRunnerDrainsStderrBeforeStdoutWithoutDeadlock(t *testing.T) {
	fakePython := writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
/usr/bin/python3 -c 'import sys; sys.stderr.write("x"*262144 + "\n"); sys.stderr.flush()'
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_stderr_first","session_id":"session-stderr"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-stderr","finish_reason":"completed","final_response":"ok","error":null,"diagnostics":null}'
`)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	document := sampleRunInvocation("gen_stderr_first")
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		processRunner{pythonPath: fakePython, source: "src", logger: logger, store: store}.Run(document)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner deadlocked draining stderr before stdout")
	}
	logged, err := os.ReadFile(filepath.Join(document.RunDir, stderrFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(logged)) != 262144 {
		t.Fatalf("stderr.log length = %d", len(bytes.TrimSpace(logged)))
	}
}

func truncateForTest(value string) string {
	if len(value) <= 64 {
		return value
	}
	return value[:32] + "..." + value[len(value)-16:]
}

func TestProcessRunnerReadsFramesBeforeChildExits(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	proceed := filepath.Join(dir, "proceed")
	sessionMarker := filepath.Join(dir, "session.jsonl")
	fakePython := writeExecutable(t, `
#!/bin/sh
input=$(cat)
dsh_home=$(printf '%s' "$input" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin)["dsh_home"])')
mkdir -p "$dsh_home/sessions/dummy"
printf '%s\n' '{"retained":true}' >"$dsh_home/sessions/dummy/session.jsonl"
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_live","session_id":"session-live"}'
printf '%s\n' '{"v":1,"type":"notification","method":"session.event","payload":{"sessionId":"session-live","event":{"type":"turn/start","seq":1,"data":{}}}}'
printf '%s\n' '{"v":1,"type":"notification","method":"session.event","payload":{"sessionId":"session-live","event":{"type":"tool/call","seq":2,"data":{"name":"bash","id":"call-1"}}}}'
touch `+ready+`
while [ ! -f `+proceed+` ]; do sleep 0.01; done
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-live","finish_reason":"completed","final_response":"done","error":null,"diagnostics":null}'
`)
	_ = sessionMarker

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	runner := processRunner{pythonPath: fakePython, source: "src", logger: logger, store: store}
	document := sampleRunInvocation("gen_live")
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Run(document)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("child never published mid-stream frames")
	}

	events := readRunEvents(t, store, document.RunID)
	assertEventTypes(t, events, []string{
		lifecycleTypeAccepted,
		lifecycleTypeStart,
		lifecycleTypeSessionCreated,
		lifecycleTypeTurn,
		lifecycleTypeTool,
	})
	if err := os.WriteFile(proceed, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not finish after result")
	}
	events = readRunEvents(t, store, document.RunID)
	assertEventTypes(t, events, []string{
		lifecycleTypeAccepted,
		lifecycleTypeStart,
		lifecycleTypeSessionCreated,
		lifecycleTypeTurn,
		lifecycleTypeTool,
		lifecycleTypeResult,
		lifecycleTypeEnd,
	})
	if _, err := os.Stat(filepath.Join(document.DshHome, "sessions", "dummy", "session.jsonl")); err != nil {
		t.Fatalf("retained session jsonl missing: %v", err)
	}
}

func TestProcessRunnerDrainsStderrRemainderAfterCapWithoutDeadlock(t *testing.T) {
	const overflow = "OVERFLOW"
	fakePython := writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_stderr_cap","session_id":"session-stderr"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-stderr","finish_reason":"completed","final_response":"ok","error":null,"diagnostics":null}'
exec 1>&-
/usr/bin/python3 -c '
import sys
chunk = b"x" * (1024 * 1024)
for _ in range(16):
    sys.stderr.buffer.write(chunk)
sys.stderr.buffer.write(b"OVERFLOW")
sys.stderr.buffer.write(b"y" * (256 * 1024))
sys.stderr.buffer.flush()
'
`)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	document := sampleRunInvocation("gen_stderr_cap")
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		processRunner{pythonPath: fakePython, source: "src", logger: logger, store: store}.Run(document)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runner deadlocked after stderr exceeded the retained cap")
	}

	logged, err := os.ReadFile(filepath.Join(document.RunDir, stderrFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != maxFrameBytes {
		t.Fatalf("stderr.log length = %d, want %d", len(logged), maxFrameBytes)
	}
	if bytes.Contains(logged, []byte(overflow)) || bytes.Contains(logged, []byte("y")) {
		t.Fatal("stderr.log retained bytes past the 16MiB cap")
	}
	if !bytes.Equal(logged, bytes.Repeat([]byte("x"), maxFrameBytes)) {
		t.Fatal("stderr.log prefix is not the capped 16MiB of x")
	}

	records := decodeLogRecords(t, logs.Bytes())
	finished := records[len(records)-1]
	stderrText, _ := finished["stderr"].(string)
	if len(stderrText) != maxFrameBytes {
		t.Fatalf("log stderr length = %d, want %d", len(stderrText), maxFrameBytes)
	}
	if strings.Contains(stderrText, overflow) || strings.Contains(stderrText, "y") {
		t.Fatal("slog stderr retained bytes past the 16MiB cap")
	}
	if finished["finish_reason"] != "completed" {
		t.Fatalf("finish_reason = %v", finished["finish_reason"])
	}
}

func TestProcessRunnerRedactsSecretsFromJournalAndLogs(t *testing.T) {
	secret := "sk-live-secret-value"
	capture := filepath.Join(t.TempDir(), "invocation.json")
	t.Setenv("GENESIS_CAPTURE", capture)
	fakePython := writeExecutable(t, `
#!/bin/sh
input=$(cat)
printf '%s' "$input" >"$GENESIS_CAPTURE"
dsh_home=$(printf '%s' "$input" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin)["dsh_home"])')
mkdir -p "$dsh_home/sessions/dummy"
printf '%s\n' 'audit sk-live-secret-value' >"$dsh_home/sessions/dummy/session.jsonl"
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_secret","session_id":"session-secret"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-secret","finish_reason":"completed","final_response":"echo sk-live-secret-value","error":null,"diagnostics":{"hint":"used sk-live-secret-value"}}'
printf '%s\n' 'stderr mentions sk-live-secret-value' >&2
`)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	runner := processRunner{pythonPath: fakePython, source: "src", logger: logger, store: store}
	document := sampleRunInvocation("gen_secret")
	document.Env = map[string]string{deepSeekAPIKey: secret, "ONLY": "declared"}
	if err := store.Accept(&document, []string{secret}); err != nil {
		t.Fatal(err)
	}
	runner.Run(document)

	journal, err := os.ReadFile(filepath.Join(store.dataDir, runsDirName, document.RunID, eventsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journal, []byte(secret)) {
		t.Fatalf("secret leaked into events.jsonl: %s", journal)
	}
	if bytes.Contains(logs.Bytes(), []byte(secret)) {
		t.Fatalf("secret leaked into slog: %s", logs.Bytes())
	}
	stderr, err := os.ReadFile(filepath.Join(document.RunDir, stderrFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stderr, []byte(secret)) {
		t.Fatalf("secret leaked into stderr.log: %s", stderr)
	}
	captured, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(captured, []byte(secret)) {
		t.Fatalf("child stdin missing secret: %s", captured)
	}
	resultFile, err := os.ReadFile(filepath.Join(document.RunDir, resultFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(resultFile, []byte(secret)) {
		t.Fatalf("secret leaked into result.json: %s", resultFile)
	}
	sessionLog, err := os.ReadFile(filepath.Join(document.DshHome, "sessions", "dummy", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(sessionLog, []byte(secret)) {
		t.Fatalf("raw session audit lost the secret: %s", sessionLog)
	}
	records := decodeLogRecords(t, logs.Bytes())
	finished := records[len(records)-1]
	diagnostics, _ := json.Marshal(finished["diagnostics"])
	if bytes.Contains(diagnostics, []byte(secret)) {
		t.Fatalf("secret leaked into slog diagnostics: %s", diagnostics)
	}
	if !bytes.Contains(diagnostics, []byte(redactedSecret)) {
		t.Fatalf("slog diagnostics missing redaction: %s", diagnostics)
	}
}

func TestProcessRunnerIsolatesConcurrentRuns(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "first-agent.yaml", nil)
	writeNamedAgent(t, agentsDir, "second-agent.yaml", nil)
	writeRule(t, rulesDir+"/01-first.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "first-agent",
	})
	writeRule(t, rulesDir+"/02-second.yaml", rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "second-agent",
	})
	fakePython := writeExecutable(t, `
#!/bin/sh
input=$(cat)
run_id=$(printf '%s' "$input" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin)["run_id"])')
dsh_home=$(printf '%s' "$input" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin)["dsh_home"])')
mkdir -p "$dsh_home/sessions/dummy"
printf '%s\n' "$run_id" >"$dsh_home/sessions/dummy/session.jsonl"
printf '%s\n' "{\"v\":1,\"type\":\"session.created\",\"run_id\":\"$run_id\",\"session_id\":\"session-$run_id\"}"
printf '%s\n' "{\"v\":1,\"type\":\"result\",\"deepseek_session_id\":\"session-$run_id\",\"finish_reason\":\"completed\",\"final_response\":\"$run_id\",\"error\":null,\"diagnostics\":null}"
`)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    processRunner{pythonPath: fakePython, source: "src", logger: logger, store: store},
		newRunID:  sequentialRunIDs("gen_one", "gen_two"),
		logger:    logger,
		store:     store,
		secrets:   map[string]string{},
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	response := sendEvent(server, map[string]any{
		"specversion": "1.0",
		"id":          "evt-concurrent",
		"source":      "urn:test",
		"type":        "com.example.run",
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	var first, second []lifecycleEvent
	for time.Now().Before(deadline) {
		first = readRunEvents(t, store, "gen_one")
		second = readRunEvents(t, store, "gen_two")
		if hasType(first, lifecycleTypeEnd) && hasType(second, lifecycleTypeEnd) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !hasType(first, lifecycleTypeEnd) || !hasType(second, lifecycleTypeEnd) {
		t.Fatalf("missing end events first=%v second=%v", typesOf(first), typesOf(second))
	}
	if crossed(first, "gen_two") || crossed(second, "gen_one") {
		t.Fatalf("crossed run identity first=%v second=%v", first, second)
	}
	oneSession, err := os.ReadFile(filepath.Join(store.dataDir, runsDirName, "gen_one", dshHomeDirName, "sessions", "dummy", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	twoSession, err := os.ReadFile(filepath.Join(store.dataDir, runsDirName, "gen_two", dshHomeDirName, "sessions", "dummy", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(oneSession)) != "gen_one" || strings.TrimSpace(string(twoSession)) != "gen_two" {
		t.Fatalf("session files = %q %q", oneSession, twoSession)
	}
}

func TestProcessRunnerProtocolErrorsKeepFiles(t *testing.T) {
	t.Run("two results", func(t *testing.T) {
		fakePython := writeExecutable(t, `
#!/bin/sh
cat >/dev/null
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_two_results","session_id":"s1"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"s1","finish_reason":"completed","final_response":"one","error":null,"diagnostics":null}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"s1","finish_reason":"completed","final_response":"two","error":null,"diagnostics":null}'
`)
		assertFailedProtocol(t, fakePython, "gen_two_results", "multiple result")
	})
	t.Run("non-json", func(t *testing.T) {
		fakePython := writeExecutable(t, `
#!/bin/sh
cat >/dev/null
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_bad_json","session_id":"s1"}'
printf '%s\n' 'this is not json'
`)
		assertFailedProtocol(t, fakePython, "gen_bad_json", "invalid JSON")
	})
}

func TestMapperIgnoresAssistantMessageAsDomainTurn(t *testing.T) {
	eventType, origin, _, ok := mapSDKNotification("session-1", "session.event", json.RawMessage(`{
		"sessionId":"session-1",
		"event":{"type":"assistant/message","seq":9,"data":{"text":"hello"}}
	}`))
	if ok || eventType != "" {
		t.Fatalf("assistant/message became domain event type=%q origin=%q", eventType, origin)
	}
	if origin != originSDKEvent {
		t.Fatalf("origin = %q", origin)
	}

	eventType, origin, data, ok := mapSDKNotification("session-1", "session.event", json.RawMessage(`{
		"sessionId":"session-1",
		"event":{"type":"turn/start","seq":1,"data":{}}
	}`))
	if !ok || eventType != lifecycleTypeTurn || origin != originSDKEvent {
		t.Fatalf("turn/start mapping = type %q origin %q ok %v", eventType, origin, ok)
	}
	if data["phase"] != "start" {
		t.Fatalf("payload = %#v", data)
	}
	raw, _ := data["raw"].(map[string]any)
	if raw["method"] != "session.event" {
		t.Fatalf("raw = %#v", raw)
	}

	_, origin, _, ok = mapSDKNotification("session-1", "session.status", json.RawMessage(`{"sessionId":"session-1","status":"idle"}`))
	if ok || origin != originSDKStatus {
		t.Fatalf("session.status mapping ok=%v origin=%q", ok, origin)
	}
	_, origin, _, ok = mapSDKNotification("session-1", "subagent.started", json.RawMessage(`{}`))
	if ok || origin != originSDKOther {
		t.Fatalf("subagent mapping ok=%v origin=%q", ok, origin)
	}
	_, origin, _, ok = mapSDKNotification("session-1", "session.event", json.RawMessage(`{
		"sessionId":"child",
		"event":{"type":"turn/start","seq":1,"data":{}}
	}`))
	if ok || origin != originSDKOther {
		t.Fatalf("foreign session mapping ok=%v origin=%q", ok, origin)
	}
}

func TestPrepareDataDirRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareDataDir(path); err == nil {
		t.Fatal("expected error")
	}
	if _, err := prepareDataDir(""); err == nil {
		t.Fatal("expected empty path error")
	}
}

func TestRecoverInterruptedAndOrphanedRuns(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := newRunStore(dataDir, newEventBus(), logger)

	dead := sampleRunInvocation("gen_dead")
	if err := store.Accept(&dead, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.journal(dead.RunID).Publish(lifecycleTypeStart, originGenesis, map[string]any{"pid": 1 << 30}); err != nil {
		t.Fatal(err)
	}
	store.release(dead.RunID)

	live := sampleRunInvocation("gen_livepid")
	if err := store.Accept(&live, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.journal(live.RunID).Publish(lifecycleTypeStart, originGenesis, map[string]any{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	store.release(live.RunID)

	tornID := "gen_torn"
	tornDir := filepath.Join(dataDir, runsDirName, tornID)
	if err := os.MkdirAll(filepath.Join(tornDir, dshHomeDirName), dataDirMode); err != nil {
		t.Fatal(err)
	}
	partial := []byte(`{"specversion":"1.0","id":"evt_1","source":"urn:genesis:run:gen_torn","type":"` + lifecycleTypeAccepted + `","subject":"gen_torn","time":"2026-09-21T00:00:00.000000Z","datacontenttype":"application/json","sequence":"1","sequencetype":"Integer","runid":"gen_torn","agentid":"a","rulefile":"r.yaml","causeid":"c","causesource":"urn:test","causetype":"t","origin":"genesis","data":{"event_id":"c"}}` + "\n" + `{"specversion":"1.0","id":"evt_2"`)
	if err := os.WriteFile(filepath.Join(tornDir, eventsFileName), partial, dataFileMode); err != nil {
		t.Fatal(err)
	}

	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	deadEvents := readRunEvents(t, store, dead.RunID)
	if !hasType(deadEvents, lifecycleTypeError) || !hasType(deadEvents, lifecycleTypeEnd) {
		t.Fatalf("dead run events = %v", typesOf(deadEvents))
	}
	liveEvents := readRunEvents(t, store, live.RunID)
	if hasType(liveEvents, lifecycleTypeEnd) {
		t.Fatalf("orphaned run was ended: %v", typesOf(liveEvents))
	}
	tornEvents := readRunEvents(t, store, tornID)
	if len(tornEvents) < 1 || tornEvents[0].Type != lifecycleTypeAccepted {
		t.Fatalf("torn prefix = %#v", tornEvents)
	}
	if !hasType(tornEvents, lifecycleTypeEnd) {
		t.Fatalf("torn in-flight run was not ended: %v", typesOf(tornEvents))
	}
}

func TestLifecycleEventsAreCloudEvents(t *testing.T) {
	fakePython := writeExecutable(t, `
#!/bin/sh
cat >/dev/null
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_ce","session_id":"session-ce"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-ce","finish_reason":"completed","final_response":"ok","error":null,"diagnostics":null}'
`)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	document := sampleRunInvocation("gen_ce")
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	processRunner{pythonPath: fakePython, source: "src", logger: logger, store: store}.Run(document)
	events := readRunEvents(t, store, document.RunID)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	seen := map[string]struct{}{}
	for i, event := range events {
		if event.SpecVersion != cloudEventSpecVersion {
			t.Fatalf("specversion = %q", event.SpecVersion)
		}
		if event.ID == "" || !strings.HasPrefix(event.ID, "evt_") {
			t.Fatalf("id = %q", event.ID)
		}
		if _, dup := seen[event.ID]; dup {
			t.Fatalf("duplicate event id %q", event.ID)
		}
		seen[event.ID] = struct{}{}
		if event.Sequence != strconv.Itoa(i+1) {
			t.Fatalf("sequence = %q want %d", event.Sequence, i+1)
		}
		if event.RunID != document.RunID || event.AgentID != document.Agent || event.Rulefile != document.Rule {
			t.Fatalf("identity = %#v", event)
		}
		if event.CauseID != "evt-3" || event.CauseSource != "urn:test" || event.CauseType != "com.example.run" {
			t.Fatalf("causation = %#v", event)
		}
		if event.Source != genesisSource(document.RunID) {
			t.Fatalf("source = %q", event.Source)
		}
	}
}

func sampleRunInvocation(runID string) invocation {
	return invocation{
		Event: cloudEvent{
			"specversion": json.RawMessage(`"1.0"`),
			"id":          json.RawMessage(`"evt-3"`),
			"source":      json.RawMessage(`"urn:test"`),
			"type":        json.RawMessage(`"com.example.run"`),
		},
		Rule:         "test.yaml",
		Agent:        "workspace-janitor",
		RunID:        runID,
		Cwd:          "/workspace",
		Home:         "/home/janitor",
		Instructions: "You are the workspace janitor.",
		Env:          map[string]string{"ONLY": "declared"},
	}
}

func sequentialRunIDs(ids ...string) func() (string, error) {
	remaining := append([]string{}, ids...)
	return func() (string, error) {
		id := remaining[0]
		remaining = remaining[1:]
		return id, nil
	}
}

func assertFailedProtocol(t *testing.T, fakePython, runID, wantMessage string) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := newRunStore(t.TempDir(), newEventBus(), logger)
	document := sampleRunInvocation(runID)
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	processRunner{pythonPath: fakePython, source: "src", logger: logger, store: store}.Run(document)
	events := readRunEvents(t, store, runID)
	if !hasType(events, lifecycleTypeError) || !hasType(events, lifecycleTypeEnd) {
		t.Fatalf("events = %v", typesOf(events))
	}
	found := false
	for _, event := range events {
		if event.Type != lifecycleTypeError {
			continue
		}
		if bytes.Contains(event.Data, []byte(wantMessage)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing %q in %v", wantMessage, events)
	}
	last := events[len(events)-1]
	if last.Type != lifecycleTypeEnd {
		t.Fatalf("last event = %s", last.Type)
	}
	if _, err := os.Stat(document.RunDir); err != nil {
		t.Fatalf("run dir removed: %v", err)
	}
}

func hasType(events []lifecycleEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func typesOf(events []lifecycleEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}

func crossed(events []lifecycleEvent, otherRunID string) bool {
	for _, event := range events {
		if event.RunID == otherRunID {
			return true
		}
	}
	return false
}
