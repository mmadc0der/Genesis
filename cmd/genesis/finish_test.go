package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFinishEmitsOneEventAndADifferentAgentMatches(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeSharedAgents(t, agentsDir, "worker", "oracle", "scribe")
	writeRawRule(t, filepath.Join(rulesDir, "oracle.yaml"), `
match:
  type: dev.genesis.agent.finished
  subject: "*"
agent: oracle
`)
	writeRawRule(t, filepath.Join(rulesDir, "narrow.yaml"), `
match:
  type: dev.genesis.agent.finished
  subject: "worker/*"
agent: scribe
`)
	server, fake := newMatchServer(t, agentsDir, rulesDir, "gen_oracle")
	store := testStore(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	document := sampleRunInvocation("gen_finish")
	document.Agent = "worker"
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	runner := processRunner{
		pythonPath: writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_finish","session_id":"session-finish"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-finish","finish_reason":"completed","final_response":"TRANSCRIPT_MARKER_ok","error":null,"diagnostics":null}'
`),
		source:     "src",
		logger:     logger,
		store:      store,
		dispatch:   server.dispatchIngress,
	}
	runner.Run(document)

	got := takeInvocations(t, fake.invocations, 1)
	expectNoInvocation(t, fake.invocations)
	if got[0].Agent != "oracle" || got[0].Rule != "oracle.yaml" {
		t.Fatalf("started %#v", got[0])
	}
	event := got[0].Event
	if eventString(t, event, "specversion") != "1.0" {
		t.Fatalf("specversion = %s", eventString(t, event, "specversion"))
	}
	if eventString(t, event, "type") != agentFinishedType {
		t.Fatalf("type = %s", eventString(t, event, "type"))
	}
	if eventString(t, event, "source") != "urn:genesis:agent:worker" {
		t.Fatalf("source = %s", eventString(t, event, "source"))
	}
	if eventString(t, event, "subject") != "worker" || strings.Contains(eventString(t, event, "subject"), "/") {
		t.Fatalf("subject = %s", eventString(t, event, "subject"))
	}
	id := eventString(t, event, "id")
	if !strings.HasPrefix(id, "evt_") || len(id) != len("evt_")+32 {
		t.Fatalf("id = %s", id)
	}
	data := eventData(t, event)
	if data["runid"] != "gen_finish" || data["agent"] != "worker" || data["outcome"] != outcomeOK {
		t.Fatalf("data = %#v", data)
	}
	transcript, _ := data["transcript"].(string)
	if !filepath.IsAbs(transcript) {
		t.Fatalf("transcript = %q", transcript)
	}
	payload, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte("TRANSCRIPT_MARKER_ok")) || !bytes.Contains(payload, []byte(lifecycleTypeEnd)) {
		t.Fatalf("transcript missing journal text: %s", payload)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("TRANSCRIPT_MARKER_ok")) {
		t.Fatal("transcript body was copied into the finish event")
	}
	info, err := os.Stat(transcript)
	if err != nil || info.Mode().Perm() != transcriptFileMode {
		t.Fatalf("transcript mode = %v %v", info, err)
	}
	runsInfo, err := os.Stat(filepath.Join(store.dataDir, runsDirName))
	if err != nil || runsInfo.Mode().Perm() != dataDirMode || runsInfo.Mode().Perm()&0o004 != 0 {
		t.Fatalf("runs mode = %v %v", runsInfo, err)
	}
	events := readRunEvents(t, store, document.RunID)
	if events[len(events)-1].Type != lifecycleTypeEnd {
		t.Fatalf("journal tail = %s", events[len(events)-1].Type)
	}
}

func TestFinishOutcomeError(t *testing.T) {
	var got []cloudEvent
	store := testStore(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	document := sampleRunInvocation("gen_fail")
	document.Agent = "worker"
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	runner := processRunner{
		pythonPath: writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-fail","finish_reason":"error","final_response":"","error":null,"diagnostics":null}'
`),
		source:     "src",
		logger:     logger,
		store:      store,
		dispatch:   func(event cloudEvent) int {
			got = append(got, event)
			return http.StatusNoContent
		},
	}
	runner.Run(document)
	if len(got) != 1 {
		t.Fatalf("events = %d", len(got))
	}
	data := eventData(t, got[0])
	if got[0].stringAttributeOrEmpty("type") != agentFinishedType || data["outcome"] != outcomeError {
		t.Fatalf("event = %#v data %#v", got[0], data)
	}
}

func TestOracleFinishDoesNotStartOracle(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeSharedAgents(t, agentsDir, "oracle", "auditor")
	writeRawRule(t, filepath.Join(rulesDir, "oracle.yaml"), `
match:
  type: dev.genesis.agent.finished
  subject: "*"
agent: oracle
`)
	writeRawRule(t, filepath.Join(rulesDir, "auditor.yaml"), `
match:
  type: dev.genesis.agent.finished
  subject: oracle
agent: auditor
`)
	writeRawRule(t, filepath.Join(rulesDir, "other.yaml"), `
match:
  type: dev.genesis.agent.finished
  subject: other
agent: auditor
`)
	writeRawRule(t, filepath.Join(rulesDir, "plain.yaml"), `
match:
  type: com.example.run
  subject: oracle
agent: oracle
`)
	server, fake := newMatchServer(t, agentsDir, rulesDir, "gen_auditor", "gen_plain", "gen_same")
	store := testStore(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	document := sampleRunInvocation("gen_oracle_done")
	document.Agent = "oracle"
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	runner := processRunner{
		pythonPath: writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-oracle","finish_reason":"completed","final_response":"done","error":null,"diagnostics":null}'
`),
		source:     "src",
		logger:     logger,
		store:      store,
		dispatch:   server.dispatchIngress,
	}
	runner.Run(document)

	got := takeInvocations(t, fake.invocations, 1)
	expectNoInvocation(t, fake.invocations)
	if got[0].Agent != "auditor" || got[0].Rule != "auditor.yaml" {
		t.Fatalf("started %#v", got[0])
	}
	if eventString(t, got[0].Event, "subject") != "oracle" {
		t.Fatalf("subject = %s", eventString(t, got[0].Event, "subject"))
	}

	plain := sendEvent(server, map[string]any{
		"specversion": "1.0",
		"id":          "evt-plain",
		"source":      "urn:test",
		"type":        "dev.genesis.agent.finished",
		"subject":     "oracle",
	})
	if plain.Code != http.StatusAccepted {
		t.Fatalf("posted finish = %d %s", plain.Code, plain.Body.String())
	}
	posted := takeInvocations(t, fake.invocations, 1)
	if posted[0].Agent != "auditor" {
		t.Fatalf("posted start = %#v", posted[0])
	}

	sameAgent := sendEvent(server, map[string]any{
		"specversion": "1.0",
		"id":          "evt-other-type",
		"source":      "urn:test",
		"type":        "com.example.run",
		"subject":     "oracle",
	})
	if sameAgent.Code != http.StatusAccepted {
		t.Fatalf("other type = %d %s", sameAgent.Code, sameAgent.Body.String())
	}
	plainRun := takeInvocations(t, fake.invocations, 1)
	if plainRun[0].Agent != "oracle" || plainRun[0].Rule != "plain.yaml" {
		t.Fatalf("self subject of another type did not run: %#v", plainRun[0])
	}
}

func TestAgentEmitStampsSourceAndRejectsFinishedType(t *testing.T) {
	var got []cloudEvent
	store := testStore(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	document := sampleRunInvocation("gen_emit")
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	runner := processRunner{
		pythonPath: writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"emit","event":{"type":"com.example.note","subject":"lab","source":"urn:spoofed","id":"spoof","data":{"n":1}}}'
printf '%s\n' '{"v":1,"type":"emit","event":{"type":"dev.genesis.agent.finished","subject":"lab","source":"urn:spoofed","id":"spoof"}}'
printf '%s\n' '{"v":1,"type":"emit","event":{"type":"","subject":"lab"}}'
printf '%s\n' '{"v":1,"type":"emit","event":{"type":"com.example.note","subject":""}}'
printf '%s\n' '{"v":1,"type":"emit","event":{"type":"com.example.note","subject":"lab","data":["no"]}}'
printf '%s\n' '{"v":1,"type":"notification","method":"genesis.emit","payload":{"type":"com.example.ping","subject":"from-note","source":"urn:spoofed","id":"spoof-2","data":{"via":"notification"}}}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-emit","finish_reason":"completed","final_response":"done","error":null,"diagnostics":null}'
`),
		source:     "src",
		logger:     logger,
		store:      store,
		dispatch:   func(event cloudEvent) int {
			got = append(got, event)
			return http.StatusNoContent
		},
	}
	runner.Run(document)

	var notes, pings, finished []cloudEvent
	for _, event := range got {
		switch event.stringAttributeOrEmpty("type") {
		case "com.example.note":
			notes = append(notes, event)
		case "com.example.ping":
			pings = append(pings, event)
		case agentFinishedType:
			finished = append(finished, event)
		default:
			t.Fatalf("unexpected event type %s", event.stringAttributeOrEmpty("type"))
		}
	}
	if len(notes) != 1 || len(pings) != 1 || len(finished) != 1 {
		t.Fatalf("notes=%d pings=%d finished=%d", len(notes), len(pings), len(finished))
	}
	for _, event := range []cloudEvent{notes[0], pings[0], finished[0]} {
		source := eventString(t, event, "source")
		if source != "urn:genesis:agent:workspace-janitor" {
			t.Fatalf("source = %s", source)
		}
		id := eventString(t, event, "id")
		if id == "spoof" || id == "spoof-2" || !strings.HasPrefix(id, "evt_") {
			t.Fatalf("id = %s", id)
		}
	}
	if eventString(t, notes[0], "subject") != "lab" || eventString(t, pings[0], "subject") != "from-note" {
		t.Fatal("subjects were not preserved")
	}
	noteData := eventData(t, notes[0])
	if noteData["n"] != float64(1) {
		t.Fatalf("note data = %#v", noteData)
	}
	if eventString(t, finished[0], "subject") != "workspace-janitor" {
		t.Fatalf("finish subject = %s", eventString(t, finished[0], "subject"))
	}
	journal := mustReadEvents(t, store, document.RunID)
	if !bytes.Contains(journal, []byte("type dev.genesis.agent.finished is reserved")) {
		t.Fatalf("reserved type was not rejected: %s", journal)
	}
	if !bytes.Contains(journal, []byte("emit type is required")) || !bytes.Contains(journal, []byte("emit subject is required")) {
		t.Fatalf("empty fields were not rejected: %s", journal)
	}
	if !bytes.Contains(journal, []byte("emit data must be a JSON object")) {
		t.Fatalf("non-object data was not rejected: %s", journal)
	}
}

func TestAgentEmitRejectsOversizedData(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"type":    "com.example.note",
		"subject": "lab",
		"data":    map[string]string{"blob": strings.Repeat("x", maxEventBytes)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentEmitEvent("worker", raw); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized emit error = %v", err)
	}
	small, err := json.Marshal(map[string]any{
		"type":    "com.example.note",
		"subject": "lab",
		"source":  "urn:spoofed",
		"id":      "spoof",
		"data":    map[string]int{"n": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := agentEmitEvent("worker", small)
	if err != nil {
		t.Fatal(err)
	}
	if event.stringAttributeOrEmpty("source") != "urn:genesis:agent:worker" {
		t.Fatalf("source = %s", event.stringAttributeOrEmpty("source"))
	}
	if event.stringAttributeOrEmpty("id") == "spoof" {
		t.Fatal("agent id was kept")
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxEventBytes {
		t.Fatalf("event length = %d", len(encoded))
	}
}

func TestFinishDuringSyncRecordsJournalAndDoesNotStart(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeSharedAgents(t, agentsDir, "worker", "oracle")
	writeRawRule(t, filepath.Join(rulesDir, "oracle.yaml"), `
match:
  type: dev.genesis.agent.finished
  subject: "*"
agent: oracle
`)
	server, fake := newMatchServer(t, agentsDir, rulesDir, "gen_should_not_start")
	server.syncing = true
	store := testStore(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	document := sampleRunInvocation("gen_sync")
	document.Agent = "worker"
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	runner := processRunner{
		pythonPath: writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-sync","finish_reason":"completed","final_response":"done","error":null,"diagnostics":null}'
`),
		source:     "src",
		logger:     logger,
		store:      store,
		dispatch:   server.dispatchIngress,
	}
	runner.Run(document)
	expectNoInvocation(t, fake.invocations)

	events := readRunEvents(t, store, document.RunID)
	if !hasType(events, lifecycleTypeEnd) {
		t.Fatalf("events = %v", typesOf(events))
	}
	var sawSync bool
	for _, event := range events {
		if event.Type == lifecycleTypeError && bytes.Contains(event.Data, []byte(syncInProgressType)) && bytes.Contains(event.Data, []byte("sync in progress")) {
			sawSync = true
		}
	}
	if !sawSync {
		t.Fatalf("sync failure was not journaled: %v", typesOf(events))
	}
	if !journalHasEnd(events) {
		t.Fatal("journal lost its end event")
	}
	store.release(document.RunID)
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	recovered := mustReadEvents(t, store, document.RunID)
	if bytes.Contains(recovered, []byte(interruptedMsg)) {
		t.Fatalf("recovery rewrote a finished journal: %s", recovered)
	}
}

func TestTranscriptFileIsWorldReadableAndRunsAreNot(t *testing.T) {
	dataDir, err := os.MkdirTemp(os.TempDir(), "genesis-transcript-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	prepared, err := prepareDataDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	store := newRunStore(prepared, newEventBus(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	document := sampleRunInvocation("gen_transcript")
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.journal(document.RunID).Publish(lifecycleTypeEnd, originGenesis, endPayload(0, endStateCompleted)); err != nil {
		t.Fatal(err)
	}
	transcript, err := store.writeTranscript(document.RunID)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(document.RunDir, eventsFileName))
	if err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journal, copied) {
		t.Fatal("transcript is not the run journal")
	}
	if !filepath.IsAbs(transcript) || strings.Contains(transcript, string(os.PathSeparator)+"runs"+string(os.PathSeparator)) {
		t.Fatalf("transcript path = %s", transcript)
	}
	assertTranscriptModes(t, prepared, transcript)

	parent, err := os.Stat(filepath.Dir(prepared))
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 || parent.Mode().Perm()&0o001 == 0 {
		t.Logf("confirmed modes only (euid=%d parent=%o); data root is 0711, transcripts 0755, file 0644, runs 0700", os.Geteuid(), parent.Mode().Perm())
		return
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err := runAs(uint32(uid), uint32(gid), "/bin/cat", transcript); err != nil {
		t.Fatalf("other uid could not read %s: %v", transcript, err)
	}
	eventsPath := filepath.Join(document.RunDir, eventsFileName)
	if err := runAs(uint32(uid), uint32(gid), "/bin/cat", eventsPath); err == nil {
		t.Fatal("other uid read the private run journal")
	}
	if err := runAs(uint32(uid), uint32(gid), "/bin/ls", filepath.Join(prepared, runsDirName)); err == nil {
		t.Fatal("other uid listed runs")
	}
}

func assertTranscriptModes(t *testing.T, dataDir, transcript string) {
	t.Helper()
	root, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if root.Mode().Perm() != dataRootMode || root.Mode().Perm()&0o006 != 0 || root.Mode().Perm()&0o001 == 0 {
		t.Fatalf("data root mode = %o", root.Mode().Perm())
	}
	transcripts, err := os.Stat(filepath.Join(dataDir, transcriptsDirName))
	if err != nil || transcripts.Mode().Perm() != transcriptsDirMode {
		t.Fatalf("transcripts mode = %v %v", transcripts, err)
	}
	file, err := os.Stat(transcript)
	if err != nil || file.Mode().Perm() != transcriptFileMode || file.Mode().Perm()&0o004 == 0 {
		t.Fatalf("transcript mode = %v %v", file, err)
	}
	runs, err := os.Stat(filepath.Join(dataDir, runsDirName))
	if err != nil || runs.Mode().Perm() != dataDirMode || runs.Mode().Perm()&0o007 != 0 {
		t.Fatalf("runs mode = %v %v", runs, err)
	}
}

func runAs(uid, gid uint32, name string, args ...string) error {
	command := exec.Command(name, args...)
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid},
	}
	_, err := command.CombinedOutput()
	return err
}

func writeSharedAgents(t *testing.T, dir string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		writeNamedAgent(t, dir, id+".yaml", nil)
	}
}

func writeRawRule(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newMatchServer(t *testing.T, agentsDir, rulesDir string, runIDs ...string) (*eventServer, *fakeRunner) {
	t.Helper()
	fake := &fakeRunner{invocations: make(chan invocation, 4)}
	server := &eventServer{
		agentsDir: agentsDir,
		rulesDir:  rulesDir,
		runner:    fake,
		logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
		store:     testStore(t),
		newRunID:  sequentialRunIDs(runIDs...),
	}
	if err := server.loadInitialGeneration(); err != nil {
		t.Fatal(err)
	}
	return server, fake
}

func takeInvocations(t *testing.T, invocations <-chan invocation, count int) []invocation {
	t.Helper()
	got := make([]invocation, 0, count)
	timeout := time.After(2 * time.Second)
	for len(got) < count {
		select {
		case document := <-invocations:
			got = append(got, document)
		case <-timeout:
			t.Fatalf("got %d invocations, want %d", len(got), count)
		}
	}
	return got
}

func expectNoInvocation(t *testing.T, invocations <-chan invocation) {
	t.Helper()
	select {
	case document := <-invocations:
		t.Fatalf("unexpected invocation agent=%s rule=%s", document.Agent, document.Rule)
	case <-time.After(200 * time.Millisecond):
	}
}

func eventString(t *testing.T, event cloudEvent, key string) string {
	t.Helper()
	value, ok := event.stringAttribute(key)
	if !ok || value == "" {
		t.Fatalf("event %s = %q %#v", key, value, event)
	}
	return value
}

func (e cloudEvent) stringAttributeOrEmpty(key string) string {
	value, _ := e.stringAttribute(key)
	return value
}

func eventData(t *testing.T, event cloudEvent) map[string]any {
	t.Helper()
	raw, ok := event["data"]
	if !ok {
		t.Fatal("event has no data")
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return data
}
