package main

import (
	"encoding/json"
	"io"
	"log/slog"
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

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "job":
			runJob(slog.New(slog.NewJSONHandler(os.Stderr, nil)), os.Args[2:])
			os.Exit(0)
		case "schedule":
			runSchedule(slog.New(slog.NewJSONHandler(os.Stderr, nil)), os.Args[2:])
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func TestEventsURLFromListen(t *testing.T) {
	if got := eventsURLFromListen("0.0.0.0:8787"); got != "http://127.0.0.1:8787/events" {
		t.Fatalf("wildcard = %s", got)
	}
	if got := eventsURLFromListen("127.0.0.1:8791"); got != "http://127.0.0.1:8791/events" {
		t.Fatalf("explicit = %s", got)
	}
	if got := eventsURLFromListen(":8787"); got != "http://127.0.0.1:8787/events" {
		t.Fatalf("empty host = %s", got)
	}
}

func TestImageInstallsGenesisOnDefaultPATH(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join(repoRoot(t), "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(dockerfile)
	if !strings.Contains(text, "COPY --from=build /out/genesis /usr/local/bin/genesis") {
		t.Fatal("image does not install genesis at /usr/local/bin/genesis")
	}
	if !strings.Contains(text, "chmod 0755 /usr/local/bin/genesis") {
		t.Fatal("image does not make genesis executable for every account")
	}
	if !pathHasDir(defaultAgentPATH, "/usr/local/bin") {
		t.Fatalf("default PATH = %s", defaultAgentPATH)
	}
}

func TestAgentRejectsSyncTokenWebhookAndSecretStore(t *testing.T) {
	base := agentDefinition{Instructions: "stay", Cwd: "/tmp/work", Home: "/tmp/home"}
	for _, name := range []string{syncTokenEnv, webhookSecretName} {
		withSecret := base
		withSecret.Secrets = []string{name}
		if err := withSecret.validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s secret error = %v", name, err)
		}
		withEnv := base
		withEnv.Env = map[string]string{name: "x"}
		if err := withEnv.validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s env error = %v", name, err)
		}
	}
	withStore := base
	withStore.Env = map[string]string{"DIR": secretsDockerPath}
	if err := withStore.validate(); err == nil || !strings.Contains(err.Error(), "secret store") {
		t.Fatalf("secret store error = %v", err)
	}
	withEvents := base
	withEvents.Env = map[string]string{eventsURLEnv: "http://evil/events"}
	if err := withEvents.validate(); err == nil || !strings.Contains(err.Error(), eventsURLEnv) {
		t.Fatalf("events url error = %v", err)
	}
}

func TestParseEachBounds(t *testing.T) {
	if _, err := parseEach("15m"); err != nil {
		t.Fatal(err)
	}
	if _, err := parseEach("168h"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"1s", "0s", "30s", "59s", "15m1ms", "169h", "-1m", "soon"} {
		if _, err := parseEach(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestPostCloudEventRetriesSyncWithoutBearer(t *testing.T) {
	previous := eventRetryDelay
	eventRetryDelay = time.Millisecond
	t.Cleanup(func() { eventRetryDelay = previous })
	t.Setenv(syncTokenEnv, "super-secret-token")

	var calls int
	var auth string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/events" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != cloudEventsJSON {
			t.Errorf("content type = %s", r.Header.Get("Content-Type"))
		}
		payload, _ := io.ReadAll(r.Body)
		body = payload
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "sync in progress", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv(eventsURLEnv, server.URL+"/events")

	event, err := composeCloudEvent(nil, map[string]any{
		"specversion": "1.0",
		"id":          "evt-1",
		"source":      "urn:genesis:job",
		"type":        jobExitedType,
		"subject":     "job_1",
	}, nil, map[string]any{"exit_code": 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := postCloudEvent(event); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
	if auth != "" {
		t.Fatalf("authorization = %q", auth)
	}
	if strings.Contains(string(body), "super-secret-token") {
		t.Fatalf("body leaked the sync token: %s", body)
	}
}

func TestJobEmitsOnExitAndHidesSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(syncTokenEnv, "super-secret-token")
	t.Setenv(webhookSecretName, "hook-secret")
	t.Setenv("SECRET_DIR", "/var/lib/genesis/secrets")
	t.Setenv(genesisAgentEnv, "trainer")

	var mu sync.Mutex
	var bodies [][]byte
	var auth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, payload)
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv(eventsURLEnv, server.URL+"/events")

	id, err := jobStart([]string{
		"--emit", `{"specversion":"1.0","type":"dev.genesis.training.finished","source":"urn:genesis:agent:trainer","subject":"run-1","data":{"model":"net"}}`,
		"--", "/bin/sh", "-c", `echo trained; printf '%s' "$GENESIS_SYNC_TOKEN$GITHUB_APP_WEBHOOK_SECRET$SECRET_DIR"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := waitJobStatus(t, id, jobStatusExited)
	if state.ExitCode == nil || *state.ExitCode != 0 {
		t.Fatalf("exit = %#v", state.ExitCode)
	}
	if state.EmitError != "" {
		t.Fatalf("emit error = %s", state.EmitError)
	}
	log := waitJobLog(t, id)
	if !strings.Contains(log, "trained") {
		t.Fatalf("log = %q", log)
	}
	if strings.Contains(log, "super-secret-token") || strings.Contains(log, "hook-secret") || strings.Contains(log, "/var/lib/genesis/secrets") {
		t.Fatalf("log leaked secrets: %q", log)
	}
	var payload []byte
	var authorization string
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			return false
		}
		payload = append([]byte(nil), bodies[0]...)
		authorization = auth[0]
		return true
	})
	if authorization != "" {
		t.Fatalf("authorization = %q", authorization)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event["type"] != "dev.genesis.training.finished" || event["source"] != "urn:genesis:agent:trainer" || event["subject"] != "run-1" {
		t.Fatalf("event = %#v", event)
	}
	data, _ := event["data"].(map[string]any)
	if data["model"] != "net" || data["job"] != id || data["exit_code"] != float64(0) {
		t.Fatalf("data = %#v", data)
	}
	if strings.Contains(string(payload), "super-secret-token") {
		t.Fatalf("event leaked the sync token: %s", payload)
	}
	text, err := jobList()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, id+"\texited\t0\t") {
		t.Fatalf("list = %q", text)
	}
}

func TestJobStopEndsALiveCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(eventsURLEnv, server.URL+"/events")

	id, err := jobStart([]string{"--", "/bin/sh", "-c", "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jobStop(id) })
	waitJobStatus(t, id, jobStatusRunning)
	listed, err := jobList()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed, id+"\trunning\t") {
		t.Fatalf("list before stop = %q", listed)
	}
	if err := jobStop(id); err != nil {
		t.Fatal(err)
	}
	state := waitJobStatus(t, id, jobStatusExited)
	if state.ExitCode == nil || *state.ExitCode == 0 {
		t.Fatalf("stopped exit = %#v", state.ExitCode)
	}
}

func TestScheduleCreateListCancel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(stopScheduler)

	id, err := scheduleCreate([]string{
		"--each=15m",
		"--emit", `{"type":"dev.genesis.tick","subject":"scan","source":"urn:genesis:agent:designer","data":{"n":1}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "sch_") {
		t.Fatalf("id = %s", id)
	}
	text, err := scheduleList()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, id+"\t15m0s\t") || !strings.Contains(text, "dev.genesis.tick") || !strings.Contains(text, "scan") {
		t.Fatalf("list = %q", text)
	}
	specs, err := loadSchedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %#v", specs)
	}
	if specs[0].NextDue.Before(time.Now().Add(14*time.Minute)) || specs[0].NextDue.After(time.Now().Add(16*time.Minute)) {
		t.Fatalf("next due = %s", specs[0].NextDue)
	}
	if _, alive := liveScheduler(); !alive {
		t.Fatal("scheduler is not running")
	}
	if err := scheduleCancel(id); err != nil {
		t.Fatal(err)
	}
	specs, err = loadSchedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 0 {
		t.Fatalf("after cancel = %#v", specs)
	}
}

func TestFireSchedulePostsOnceAndCatchesUpOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(syncTokenEnv, "super-secret-token")
	var calls int
	var auth string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth = r.Header.Get("Authorization")
		body, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv(eventsURLEnv, server.URL+"/events")

	event, err := parseEmitObject(`{"type":"dev.genesis.training.tick","subject":"night","source":"urn:genesis:agent:trainer","data":{"model":"net"}}`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	spec := scheduleState{
		ID:      "sch_catchup",
		Each:    "15m",
		Event:   event,
		NextDue: now.Add(-2 * time.Hour),
		Created: now.Add(-3 * time.Hour),
	}
	if err := writeSchedule(spec); err != nil {
		t.Fatal(err)
	}
	if err := fireDueSchedules(now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
	if auth != "" {
		t.Fatalf("authorization = %q", auth)
	}
	var posted map[string]any
	if err := json.Unmarshal(body, &posted); err != nil {
		t.Fatal(err)
	}
	if posted["type"] != "dev.genesis.training.tick" || posted["subject"] != "night" {
		t.Fatalf("event = %#v", posted)
	}
	data, _ := posted["data"].(map[string]any)
	if data["model"] != "net" || data["schedule"] != "sch_catchup" || data["each"] != "15m" {
		t.Fatalf("data = %#v", data)
	}
	if strings.Contains(string(body), "super-secret-token") {
		t.Fatalf("event leaked the sync token: %s", body)
	}
	specs, err := loadSchedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %#v", specs)
	}
	if specs[0].NextDue.Before(now.Add(14*time.Minute)) || specs[0].NextDue.After(now.Add(16*time.Minute)) {
		t.Fatalf("next due = %s, now = %s", specs[0].NextDue, now)
	}
	if err := fireDueSchedules(now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("second fire calls = %d", calls)
	}
}

func waitJobStatus(t *testing.T, id, status string) jobState {
	t.Helper()
	var state jobState
	waitFor(t, func() bool {
		dir, err := existingJobDir(id)
		if err != nil {
			return false
		}
		state, err = readJobState(dir)
		return err == nil && state.Status == status
	})
	return state
}

func waitJobLog(t *testing.T, id string) string {
	t.Helper()
	var text string
	waitFor(t, func() bool {
		dir, err := existingJobDir(id)
		if err != nil {
			return false
		}
		payload, err := os.ReadFile(filepath.Join(dir, "log"))
		if err != nil || len(payload) == 0 {
			return false
		}
		text = string(payload)
		return true
	})
	return text
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func stopScheduler() {
	pid, alive := liveScheduler()
	if !alive {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := liveScheduler(); !alive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
