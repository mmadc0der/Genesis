package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionCreatedRecordsMintedID(t *testing.T) {
	loggerStore := testStore(t)
	document := sampleRunInvocation("gen_mint")
	if err := loggerStore.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	before, err := readSessionRecord(filepath.Join(document.RunDir, sessionFileName))
	if err != nil {
		t.Fatal(err)
	}
	if before.SessionID != "" || before.CorrelationID != "gen_mint" || before.ContinuedFrom != "" {
		t.Fatalf("root session = %#v", before)
	}
	if before.DshHome != stableDshHome(loggerStore.dataDir, "gen_mint") {
		t.Fatalf("root home = %s", before.DshHome)
	}
	runner := processRunner{
		pythonPath: writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"session.created","run_id":"gen_mint","session_id":"session-minted"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-minted","finish_reason":"completed","final_response":"ok","error":null,"diagnostics":null}'
`),
		source: "src",
		store:  loggerStore,
	}
	runner.Run(document)
	record, err := readSessionRecord(filepath.Join(document.RunDir, sessionFileName))
	if err != nil {
		t.Fatal(err)
	}
	if record.SessionID != "session-minted" {
		t.Fatalf("session id = %q", record.SessionID)
	}
	events := readRunEvents(t, loggerStore, document.RunID)
	var created bool
	for _, event := range events {
		if event.Type != lifecycleTypeSessionCreated {
			continue
		}
		created = true
		if event.SessionID != "session-minted" || event.CorrelationID != "gen_mint" {
			t.Fatalf("session.created = %#v", event)
		}
	}
	if !created {
		t.Fatal("missing session.created")
	}
	if events[0].CorrelationID != "gen_mint" || events[0].SessionID != "" {
		t.Fatalf("accepted correlation=%s session=%s", events[0].CorrelationID, events[0].SessionID)
	}
}

func TestContinuationCopiesSessionHomeAndJournalCause(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "worker.yaml", nil)
	writeRawRule(t, filepath.Join(rulesDir, "extra.yaml"), `
match:
  type: dev.genesis.session.continue
agent: worker
`)
	server, fake := newMatchServer(t, agentsDir, rulesDir, "gen_next", "gen_extra")
	root := sampleRunInvocation("gen_root")
	root.Agent = "worker"
	root.Cwd = cwd
	root.Home = home
	record := acceptFinishedRun(t, server.store, root, "session-root")

	response := sendEvent(server, continuationEvent("evt_continue_root", "urn:genesis:agent:worker", "gen_root", "follow up"))
	if response.Code != 202 {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	got := takeInvocations(t, fake.invocations, 1)
	expectNoInvocation(t, fake.invocations)
	if got[0].Agent != "worker" || got[0].Rule != continuationRuleName {
		t.Fatalf("started %#v", got[0])
	}
	if got[0].RunID != "gen_next" {
		t.Fatalf("run = %s", got[0].RunID)
	}
	if got[0].SessionID != "session-root" || got[0].DshHome != record.DshHome {
		t.Fatalf("session = %s home = %s", got[0].SessionID, got[0].DshHome)
	}
	if got[0].DshHome != stableDshHome(server.store.dataDir, "gen_root") {
		t.Fatalf("home = %s", got[0].DshHome)
	}
	if _, err := os.Stat(filepath.Join(server.store.dataDir, runsDirName, "gen_next", dshHomeDirName)); !os.IsNotExist(err) {
		t.Fatalf("continuation created a second dsh_home: %v", err)
	}
	if got[0].UserMessage != "follow up" || got[0].ContinuedFrom != "gen_root" || got[0].CorrelationID != "gen_root" {
		t.Fatalf("invocation = %#v", got[0])
	}
	if eventString(t, got[0].Event, "correlationid") != "gen_root" {
		t.Fatalf("event correlation = %s", eventString(t, got[0].Event, "correlationid"))
	}
	copied, err := readSessionRecord(filepath.Join(server.store.dataDir, runsDirName, "gen_next", sessionFileName))
	if err != nil {
		t.Fatal(err)
	}
	if copied.SessionID != "session-root" || copied.DshHome != record.DshHome || copied.CorrelationID != "gen_root" || copied.ContinuedFrom != "gen_root" {
		t.Fatalf("session.json = %#v", copied)
	}
	events := readRunEvents(t, server.store, "gen_next")
	if len(events) == 0 {
		t.Fatal("missing journal")
	}
	if events[0].CorrelationID != "gen_root" || events[0].CauseID != "evt_continue_root" {
		t.Fatalf("journal correlation=%s cause=%s", events[0].CorrelationID, events[0].CauseID)
	}
	if events[0].Type != lifecycleTypeAccepted {
		t.Fatalf("first event = %s", events[0].Type)
	}
}

func TestContinuationRefusesWhileSessionIsOpen(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "worker.yaml", nil)
	server, fake := newMatchServer(t, agentsDir, rulesDir, "gen_next", "gen_extra")
	root := sampleRunInvocation("gen_root")
	root.Agent = "worker"
	root.Cwd = cwd
	root.Home = home
	acceptFinishedRun(t, server.store, root, "session-root")

	first := sendEvent(server, continuationEvent("evt_continue_open", "urn:genesis:agent:worker", "gen_root", "follow up"))
	if first.Code != 202 {
		t.Fatalf("status = %d body = %s", first.Code, first.Body.String())
	}
	second := sendEvent(server, continuationEvent("evt_continue_again", "urn:genesis:agent:worker", "gen_root", "follow up"))
	if second.Code != 204 {
		t.Fatalf("second status = %d body = %s", second.Code, second.Body.String())
	}
	got := takeInvocations(t, fake.invocations, 1)
	expectNoInvocation(t, fake.invocations)
	if got[0].RunID != "gen_next" {
		t.Fatalf("run = %s", got[0].RunID)
	}
	if _, err := os.Stat(filepath.Join(server.store.dataDir, runsDirName, "gen_extra")); !os.IsNotExist(err) {
		t.Fatalf("second continuation created a run: %v", err)
	}
}

func TestContinuationDoesNotRuleMatch(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "worker.yaml", nil)
	writeNamedAgent(t, agentsDir, "scribe.yaml", nil)
	writeRawRule(t, filepath.Join(rulesDir, "scribe.yaml"), `
match:
  type: dev.genesis.session.continue
agent: scribe
`)
	server, fake := newMatchServer(t, agentsDir, rulesDir, "gen_only", "gen_scribe")
	root := sampleRunInvocation("gen_owner")
	root.Agent = "worker"
	root.Cwd = cwd
	root.Home = home
	acceptFinishedRun(t, server.store, root, "session-owner")

	response := sendEvent(server, continuationEvent("evt_continue_rule", "urn:genesis:agent:worker", "gen_owner", "again"))
	if response.Code != 202 {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	got := takeInvocations(t, fake.invocations, 1)
	expectNoInvocation(t, fake.invocations)
	if got[0].Agent != "worker" || got[0].Rule != continuationRuleName || got[0].RunID != "gen_only" {
		t.Fatalf("rule match started %#v", got[0])
	}
}

func TestContinuationRefusals(t *testing.T) {
	t.Run("unfinished", func(t *testing.T) {
		server, fake := continuationServer(t, "gen_should_not")
		cwd, home := agentPaths(t, server)
		document := sampleRunInvocation("gen_open")
		document.Agent = "worker"
		document.Cwd = cwd
		document.Home = home
		if err := server.store.Accept(&document, nil); err != nil {
			t.Fatal(err)
		}
		if err := server.store.noteSessionID("gen_open", "session-open"); err != nil {
			t.Fatal(err)
		}
		response := sendEvent(server, continuationEvent("evt_open", "urn:genesis:agent:worker", "gen_open", "more"))
		if response.Code != 204 {
			t.Fatalf("status = %d", response.Code)
		}
		expectNoInvocation(t, fake.invocations)
		events := readRunEvents(t, server.store, "gen_open")
		if !journalContains(events, continuationRefusedType) {
			t.Fatalf("refusal was not journaled: %v", typesOf(events))
		}
		if _, err := os.Stat(filepath.Join(server.store.dataDir, runsDirName, "gen_should_not")); !os.IsNotExist(err) {
			t.Fatalf("refused continuation created a run: %v", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		server, fake := continuationServer(t, "gen_should_not")
		response := sendEvent(server, continuationEvent("evt_missing", "urn:genesis:agent:worker", "gen_missing", "more"))
		if response.Code != 204 {
			t.Fatalf("status = %d", response.Code)
		}
		expectNoInvocation(t, fake.invocations)
		if _, err := os.Stat(filepath.Join(server.store.dataDir, runsDirName, "gen_missing")); !os.IsNotExist(err) {
			t.Fatalf("unknown id created a run: %v", err)
		}
	})

	t.Run("cwd", func(t *testing.T) {
		server, fake := continuationServer(t, "gen_should_not")
		cwd, home := agentPaths(t, server)
		document := sampleRunInvocation("gen_cwd")
		document.Agent = "worker"
		document.Cwd = cwd
		document.Home = home
		acceptFinishedRun(t, server.store, document, "session-cwd")
		record, err := readSessionRecord(filepath.Join(server.store.dataDir, runsDirName, "gen_cwd", sessionFileName))
		if err != nil {
			t.Fatal(err)
		}
		record.Cwd = "/tmp/genesis-other-cwd"
		if err := writeSessionRecord(filepath.Join(server.store.dataDir, runsDirName, "gen_cwd", sessionFileName), record); err != nil {
			t.Fatal(err)
		}
		response := sendEvent(server, continuationEvent("evt_cwd", "urn:genesis:agent:worker", "gen_cwd", "more"))
		if response.Code != 204 {
			t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
		}
		expectNoInvocation(t, fake.invocations)
	})

	t.Run("hop9", func(t *testing.T) {
		server, fake := continuationServer(t, "gen_should_not")
		cwd, home := agentPaths(t, server)
		document := sampleRunInvocation("gen_h8")
		document.Agent = "worker"
		document.Cwd = cwd
		document.Home = home
		acceptFinishedRun(t, server.store, document, "session-h8")
		for hop := 0; hop < maxContinuationHops; hop++ {
			id := hopID(hop)
			parent := ""
			if hop > 0 {
				parent = hopID(hop - 1)
			}
			dir := filepath.Join(server.store.dataDir, runsDirName, id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := writeSessionRecord(filepath.Join(dir, sessionFileName), sessionRecord{
				SessionID:     "session-" + id,
				DshHome:       stableDshHome(server.store.dataDir, "gen_h0"),
				CorrelationID: "gen_h0",
				ContinuedFrom: parent,
				Cwd:           cwd,
				Agent:         "worker",
			}); err != nil {
				t.Fatal(err)
			}
		}
		record, err := readSessionRecord(filepath.Join(server.store.dataDir, runsDirName, "gen_h8", sessionFileName))
		if err != nil {
			t.Fatal(err)
		}
		record.ContinuedFrom = "gen_h7"
		record.CorrelationID = "gen_h0"
		if err := writeSessionRecord(filepath.Join(server.store.dataDir, runsDirName, "gen_h8", sessionFileName), record); err != nil {
			t.Fatal(err)
		}
		response := sendEvent(server, continuationEvent("evt_hop", "urn:genesis:agent:worker", "gen_h8", "more"))
		if response.Code != 204 {
			t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
		}
		expectNoInvocation(t, fake.invocations)
		if _, err := os.Stat(filepath.Join(server.store.dataDir, runsDirName, "gen_should_not")); !os.IsNotExist(err) {
			t.Fatalf("hop 9 created a run: %v", err)
		}
	})

	t.Run("emitter", func(t *testing.T) {
		server, fake := continuationServer(t, "gen_should_not")
		cwd, home := agentPaths(t, server)
		document := sampleRunInvocation("gen_owned")
		document.Agent = "worker"
		document.Cwd = cwd
		document.Home = home
		acceptFinishedRun(t, server.store, document, "session-owned")
		response := sendEvent(server, continuationEvent("evt_stranger", "urn:genesis:agent:stranger", "gen_owned", "more"))
		if response.Code != 204 {
			t.Fatalf("status = %d", response.Code)
		}
		expectNoInvocation(t, fake.invocations)
	})
}

func TestOracleMayContinueAnotherAgent(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "worker.yaml", nil)
	writeNamedAgent(t, agentsDir, "oracle.yaml", nil)
	server, fake := newMatchServer(t, agentsDir, rulesDir, "gen_by_oracle")
	root := sampleRunInvocation("gen_worker")
	root.Agent = "worker"
	root.Cwd = cwd
	root.Home = home
	record := acceptFinishedRun(t, server.store, root, "session-worker")

	response := sendEvent(server, continuationEvent("evt_oracle", "urn:genesis:agent:oracle", "gen_worker", "review this"))
	if response.Code != 202 {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	got := takeInvocations(t, fake.invocations, 1)
	expectNoInvocation(t, fake.invocations)
	if got[0].Agent != "worker" || got[0].SessionID != "session-worker" || got[0].DshHome != record.DshHome {
		t.Fatalf("oracle continuation = %#v", got[0])
	}
	if _, err := os.Stat(filepath.Join(server.store.dataDir, runsDirName, "gen_by_oracle", dshHomeDirName)); !os.IsNotExist(err) {
		t.Fatalf("oracle continuation created a second home: %v", err)
	}
}

func TestContinuationEmitCarriesCause(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	cwd, home := writeNamedAgent(t, agentsDir, "workspace-janitor.yaml", nil)
	server, fake := newMatchServer(t, agentsDir, rulesDir, "gen_from_emit")
	root := sampleRunInvocation("gen_cited")
	root.Agent = "workspace-janitor"
	root.Cwd = cwd
	root.Home = home
	acceptFinishedRun(t, server.store, root, "session-cited")

	loggerStore := server.store
	document := sampleRunInvocation("gen_emitter")
	document.Agent = "workspace-janitor"
	document.Cwd = cwd
	document.Home = home
	if err := loggerStore.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	runner := processRunner{
		pythonPath: writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"v":1,"type":"emit","event":{"type":"dev.genesis.session.continue","subject":"gen_cited","data":{"message":"next turn"}}}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"session-emitter","finish_reason":"completed","final_response":"done","error":null,"diagnostics":null}'
`),
		source:   "src",
		logger:   server.logger,
		store:    loggerStore,
		dispatch: server.dispatchIngress,
	}
	runner.Run(document)
	got := takeInvocations(t, fake.invocations, 1)
	expectNoInvocation(t, fake.invocations)
	if eventString(t, got[0].Event, "causeid") != "evt-3" {
		t.Fatalf("causeid = %s", eventString(t, got[0].Event, "causeid"))
	}
	if eventString(t, got[0].Event, "causesource") != "urn:test" || eventString(t, got[0].Event, "causetype") != "com.example.run" {
		t.Fatalf("cause = %#v", got[0].Event)
	}
	events := readRunEvents(t, server.store, got[0].RunID)
	if events[0].CauseID != eventString(t, got[0].Event, "id") || events[0].CorrelationID != "gen_cited" {
		t.Fatalf("journal cause=%s correlation=%s", events[0].CauseID, events[0].CorrelationID)
	}
	if events[0].SessionID != "" {
		t.Fatalf("accepted line had a session id before session.created: %s", events[0].SessionID)
	}
}

func continuationServer(t *testing.T, runIDs ...string) (*eventServer, *fakeRunner) {
	t.Helper()
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	writeNamedAgent(t, agentsDir, "worker.yaml", nil)
	return newMatchServer(t, agentsDir, rulesDir, runIDs...)
}

func agentPaths(t *testing.T, server *eventServer) (string, string) {
	t.Helper()
	agent := server.generation.agents["worker"]
	return agent.Cwd, agent.Home
}

func acceptFinishedRun(t *testing.T, store *runStore, document invocation, sessionID string) sessionRecord {
	t.Helper()
	if err := store.Accept(&document, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.noteSessionID(document.RunID, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := store.journal(document.RunID).Publish(lifecycleTypeEnd, originGenesis, endPayload(0, endStateCompleted)); err != nil {
		t.Fatal(err)
	}
	store.release(document.RunID)
	record, err := readSessionRecord(filepath.Join(document.RunDir, sessionFileName))
	if err != nil {
		t.Fatal(err)
	}
	if record.SessionID != sessionID || record.DshHome == "" || record.CorrelationID != document.RunID {
		t.Fatalf("finished session = %#v", record)
	}
	return record
}

func continuationEvent(id, source, subject, message string) map[string]any {
	return map[string]any{
		"specversion": "1.0",
		"id":          id,
		"source":      source,
		"type":        sessionContinueType,
		"subject":     subject,
		"data":        map[string]any{"message": message},
	}
}

func TestPrepareSessionHomeModesOnOwnedTree(t *testing.T) {
	data := t.TempDir()
	home := stableDshHome(data, "gen_modes")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareSessionHomeModes(data, home); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{data, dataRootMode},
		{filepath.Join(data, sessionsDirName), dataRootMode},
		{filepath.Dir(home), dataRootMode},
		{home, dataDirMode},
	} {
		info, err := os.Stat(item.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != item.mode {
			t.Fatalf("%s mode = %o, want %o", item.path, info.Mode().Perm(), item.mode)
		}
	}
}

func hopID(hop int) string {
	return fmt.Sprintf("gen_h%d", hop)
}

func journalContains(events []lifecycleEvent, text string) bool {
	for _, event := range events {
		if event.Data != nil && strings.Contains(string(event.Data), text) {
			return true
		}
	}
	return false
}
