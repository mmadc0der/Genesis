package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fileConnForTest(file *os.File) (net.Conn, error) {
	return net.FileConn(file)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestSuperviseListenerPropagatesExitCode(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exit 7")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if code := superviseListener(command, discardLogger()); code != 7 {
		t.Fatalf("exit code = %d", code)
	}
}

func TestLaunchChildEnvOverridesInheritedFds(t *testing.T) {
	t.Setenv(privilegedFDEnv, "9")
	t.Setenv(syncTokenEnv, "old")
	env := launchChildEnv("fresh-token")
	foundToken := false
	foundFD := false
	for _, item := range env {
		switch item {
		case syncTokenEnv + "=old", privilegedFDEnv + "=9":
			t.Fatalf("leaked parent value %s", item)
		case syncTokenEnv + "=fresh-token":
			foundToken = true
		case privilegedFDEnv + "=3":
			foundFD = true
		}
	}
	if !foundToken || !foundFD {
		t.Fatalf("child env missing overrides: token=%v fd=%v", foundToken, foundFD)
	}
}

func TestParseCommandDefaultsToListen(t *testing.T) {
	command, args := parseCommand([]string{"-listen", "127.0.0.1:9"})
	if command != "listen" || len(args) != 2 {
		t.Fatalf("command = %q args = %#v", command, args)
	}
	command, args = parseCommand([]string{"launch", "-listen", "127.0.0.1:9"})
	if command != "launch" || args[0] != "-listen" {
		t.Fatalf("command = %q args = %#v", command, args)
	}
}

func TestLaunchBinarySupervisesListenerAndSync(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "genesis")
	build := exec.Command("go", "build", "-o", bin, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}

	agentsDir := filepath.Join(dir, "agents")
	rulesDir := filepath.Join(dir, "rules")
	if err := os.Mkdir(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rulesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeNamedAgent(t, agentsDir, "ok.yaml", nil)
	writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{
		Match: map[string]string{"type": "com.example.run"},
		Agent: "ok",
	})

	pythonDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(pythonDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakePython := writeExecutable(t, `
#!/bin/sh
/bin/cat >/dev/null
printf '%s\n' '{"deepseek_session_id":"fake","finish_reason":"completed","final_response":"ok","error":null,"diagnostics":null}'
`)
	if err := os.Rename(fakePython, filepath.Join(pythonDir, "python3")); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	logPath := filepath.Join(dir, "launch.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(bin, "launch",
		"-listen", addr,
		"-agents", agentsDir,
		"-rules", rulesDir,
		"-sync-token", "launch-token",
	)
	command.Env = append(os.Environ(), "PATH="+pythonDir+":"+os.Getenv("PATH"))
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		_, _ = command.Process.Wait()
		logFile.Close()
	}()

	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/events", strings.NewReader(`{
			"specversion":"1.0","id":"launch-1","source":"urn:test","type":"com.example.missing"
		}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", cloudEventsJSON)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusNoContent {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		contents, _ := os.ReadFile(logPath)
		t.Fatalf("listener never became ready\n%s", contents)
	}

	writeRule(t, filepath.Join(rulesDir, "ok.yaml"), rule{
		Match: map[string]string{"type": "com.example.launch"},
		Agent: "ok",
	})
	unsynced, err := http.NewRequest(http.MethodPost, "http://"+addr+"/events", strings.NewReader(`{
		"specversion":"1.0","id":"launch-2","source":"urn:test","type":"com.example.launch"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	unsynced.Header.Set("Content-Type", cloudEventsJSON)
	response, err := http.DefaultClient.Do(unsynced)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("unsynced status = %d", response.StatusCode)
	}

	syncRequest, err := http.NewRequest(http.MethodPost, "http://"+addr+"/sync", strings.NewReader(`{"scope":["agents","rules"]}`))
	if err != nil {
		t.Fatal(err)
	}
	syncRequest.Header.Set("Content-Type", "application/json")
	syncRequest.Header.Set("Authorization", "Bearer launch-token")
	syncHTTP, err := http.DefaultClient.Do(syncRequest)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(syncHTTP.Body)
	syncHTTP.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if syncHTTP.StatusCode != http.StatusOK {
		t.Fatalf("sync status = %d body = %s", syncHTTP.StatusCode, body)
	}
	var decoded syncResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Privileged.Attached {
		t.Fatalf("expected attached privileged ipc: %s", body)
	}
	if decoded.Privileged.HostMutation != hostMutationNone {
		t.Fatalf("host mutation = %s", body)
	}
	if len(decoded.Privileged.Unsupported) == 0 {
		t.Fatalf("expected unsupported privileged diffs: %s", body)
	}

	synced, err := http.NewRequest(http.MethodPost, "http://"+addr+"/events", strings.NewReader(`{
		"specversion":"1.0","id":"launch-3","source":"urn:test","type":"com.example.launch"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	synced.Header.Set("Content-Type", cloudEventsJSON)
	response, err = http.DefaultClient.Do(synced)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("synced status = %d body = %s", response.StatusCode, body)
	}
}
