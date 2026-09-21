package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
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
	env := launchChildEnv("fresh-token", nil)
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

func TestLaunchChildEnvOmitsEmptyToken(t *testing.T) {
	t.Setenv(syncTokenEnv, "old")
	for _, item := range launchChildEnv("", nil) {
		key, _, _ := strings.Cut(item, "=")
		if key == syncTokenEnv {
			t.Fatalf("empty token leaked as %s", item)
		}
	}
}

func TestLaunchChildEnvAppliesDroppedUser(t *testing.T) {
	t.Setenv("HOME", "/root")
	t.Setenv("USER", "root")
	env := launchChildEnv("tok", &listenerIdentity{Username: "genesis", Home: "/home/genesis", Uid: 65532, Gid: 65532})
	foundHome, foundUser, foundToken := false, false, false
	for _, item := range env {
		switch item {
		case "HOME=/root", "USER=root":
			t.Fatalf("kept parent identity %s", item)
		case "HOME=/home/genesis":
			foundHome = true
		case "USER=genesis":
			foundUser = true
		case syncTokenEnv + "=tok":
			foundToken = true
		}
	}
	if !foundHome || !foundUser || !foundToken {
		t.Fatalf("dropped-user env home=%v user=%v token=%v", foundHome, foundUser, foundToken)
	}
}

func TestResolveListenerIdentityRequiresUserWhenRoot(t *testing.T) {
	_, err := resolveListenerIdentity(0, "")
	if err == nil || !strings.Contains(err.Error(), "requires -listener-user") {
		t.Fatalf("empty root identity error = %v", err)
	}
	_, err = resolveListenerIdentity(0, "genesis-no-such-listener")
	if err == nil || !strings.Contains(err.Error(), "listener user") {
		t.Fatalf("missing user error = %v", err)
	}
}

func TestResolveListenerIdentityLooksUpUserForRoot(t *testing.T) {
	name := firstExistingUsername(t, "nobody", "daemon", "ubuntu", "www-data")
	identity, err := resolveListenerIdentity(0, name)
	if err != nil {
		t.Fatal(err)
	}
	if identity == nil || identity.Username == "" || identity.Uid == 0 && name != "root" {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestResolveListenerIdentityLeavesNonRootUnchanged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("process is root")
	}
	identity, err := resolveListenerIdentity(os.Geteuid(), "")
	if err != nil || identity != nil {
		t.Fatalf("non-root default identity = %#v err = %v", identity, err)
	}
	_, err = resolveListenerIdentity(os.Geteuid(), "root")
	if err == nil || !strings.Contains(err.Error(), "cannot switch") {
		t.Fatalf("foreign user error = %v", err)
	}
	current, err := currentUsername()
	if err != nil {
		t.Fatal(err)
	}
	identity, err = resolveListenerIdentity(os.Geteuid(), current)
	if err != nil || identity != nil {
		t.Fatalf("same-user identity = %#v err = %v", identity, err)
	}
}

func TestApplyListenerIdentitySetsCredential(t *testing.T) {
	command := exec.Command("/bin/true")
	applyListenerIdentity(command, &listenerIdentity{Uid: 65532, Gid: 65532, Groups: []uint32{65532, 100}})
	if command.SysProcAttr == nil || command.SysProcAttr.Credential == nil {
		t.Fatal("missing credential")
	}
	cred := command.SysProcAttr.Credential
	if cred.Uid != 65532 || cred.Gid != 65532 || len(cred.Groups) != 2 {
		t.Fatalf("credential = %#v", cred)
	}
	plain := exec.Command("/bin/true")
	applyListenerIdentity(plain, nil)
	if plain.SysProcAttr != nil {
		t.Fatalf("unexpected attr = %#v", plain.SysProcAttr)
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
	skipLaunchBinaryIfRoot(t)
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
printf '%s\n' '{"v":1,"type":"session.created","run_id":"fake","session_id":"fake"}'
printf '%s\n' '{"v":1,"type":"result","deepseek_session_id":"fake","finish_reason":"completed","final_response":"ok","error":null,"diagnostics":null}'
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
	dataDir := filepath.Join(dir, "data")
	command := exec.Command(bin, "launch",
		"-listen", addr,
		"-agents", agentsDir,
		"-rules", rulesDir,
		"-data", dataDir,
		"-sync-token", "launch-token",
	)
	command.Env = append(launchTestEnv(pythonDir), syncTokenEnv+"=unused-parent")
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

func TestLaunchBinaryDisablesSyncWithoutToken(t *testing.T) {
	skipLaunchBinaryIfRoot(t)
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
	fakePython := writeExecutable(t, "#!/bin/sh\n/bin/cat >/dev/null\nprintf '%s\\n' '{\"v\":1,\"type\":\"result\",\"deepseek_session_id\":null,\"finish_reason\":null,\"final_response\":null,\"error\":null,\"diagnostics\":null}'\n")
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
	command := exec.Command(bin, "launch", "-listen", addr, "-agents", agentsDir, "-rules", rulesDir, "-data", filepath.Join(dir, "data"))
	command.Env = launchTestEnv(pythonDir)
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
	if !waitForListen(t, addr, logPath) {
		return
	}
	syncRequest, err := http.NewRequest(http.MethodPost, "http://"+addr+"/sync", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	syncRequest.Header.Set("Authorization", "Bearer guessed")
	response, err := http.DefaultClient.Do(syncRequest)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "sync is disabled") {
		t.Fatalf("disabled sync status = %d body = %s", response.StatusCode, body)
	}
}

func skipLaunchBinaryIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("binary launch tests preserve non-root ergonomics; root drop is covered by resolve/apply unit tests")
	}
}

func launchTestEnv(pythonDir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == syncTokenEnv || key == listenerUserEnv {
			continue
		}
		env = append(env, item)
	}
	return append(env, "PATH="+pythonDir+":"+os.Getenv("PATH"))
}

func waitForListen(t *testing.T, addr, logPath string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/events", strings.NewReader(`{
			"specversion":"1.0","id":"wait","source":"urn:test","type":"com.example.missing"
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
				return true
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	contents, _ := os.ReadFile(logPath)
	t.Fatalf("listener never became ready\n%s", contents)
	return false
}

func firstExistingUsername(t *testing.T, names ...string) string {
	t.Helper()
	for _, name := range names {
		if _, err := lookupListenerIdentity(name); err == nil {
			return name
		}
	}
	t.Skip("no suitable unprivileged account for lookup test")
	return ""
}

func currentUsername() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	return account.Username, nil
}
