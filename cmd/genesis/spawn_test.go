package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPrivilegedSpawnAndWaitRoundTrip(t *testing.T) {
	parent, childFile, err := privilegedSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		parent.Close()
		childFile.Close()
	})

	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	state := newPrivilegedState(slog.New(slog.NewJSONHandler(io.Discard, nil)), "/usr/bin/python3", "import sys; sys.stdout.write(sys.stdin.read())", account.Username, dataDir)
	state.mutate = false
	state.host = newMemoryHost()
	state.remember(identity)
	go servePrivilegedParent(parent, discardLogger(), state)

	conn, err := fileConnForTest(childFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	coordinator := &ipcCoordinator{conn: conn}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stderrR.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pid, err := coordinator.Spawn(ctx, spawnRequest{
		Agent:   identity.Agent,
		User:    identity.Username,
		Cwd:     identity.Cwd,
		Home:    identity.Home,
		RunDir:  runDir,
		DshHome: dshHome,
		Env:     map[string]string{"ONLY": "yes"},
	}, stdinR, stdoutW, stderrW)
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		t.Fatal(err)
	}
	if pid <= 0 {
		t.Fatalf("pid = %d", pid)
	}
	if _, err := stdinW.Write([]byte("hello-os-user")); err != nil {
		t.Fatal(err)
	}
	stdinW.Close()
	output, err := io.ReadAll(stdoutR)
	stdoutR.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "hello-os-user" {
		t.Fatalf("output = %q", output)
	}
	code, err := coordinator.Wait(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
}

func TestWaitDoesNotBlockCoordinate(t *testing.T) {
	parent, childFile, err := privilegedSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		parent.Close()
		childFile.Close()
	})

	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "import time,sys; time.sleep(2); sys.stdout.write('done')", account.Username, dataDir)
	state.mutate = false
	state.host = newMemoryHost()
	state.remember(identity)
	go servePrivilegedParent(parent, discardLogger(), state)

	conn, err := fileConnForTest(childFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	coordinator := &ipcCoordinator{conn: conn}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()
	defer stderrR.Close()
	stdinW.Close()

	spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer spawnCancel()
	pid, err := coordinator.Spawn(spawnCtx, spawnRequest{
		Agent:   identity.Agent,
		User:    identity.Username,
		Cwd:     identity.Cwd,
		Home:    identity.Home,
		RunDir:  runDir,
		DshHome: dshHome,
	}, stdinR, stdoutW, stderrW)
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		t.Fatal(err)
	}

	waitErr := make(chan error, 1)
	go func() {
		_, err := coordinator.Wait(context.Background(), pid)
		waitErr <- err
	}()
	time.Sleep(50 * time.Millisecond)

	started := time.Now()
	coordCtx, coordCancel := context.WithTimeout(context.Background(), time.Second)
	defer coordCancel()
	if _, err := coordinator.Coordinate(coordCtx, privilegedPlan{Intents: []privilegedIntent{}}); err != nil {
		t.Fatalf("coordinate during wait: %v", err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("coordinate blocked for %s", time.Since(started))
	}

	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("wait did not finish")
	}
}

func TestSpawnRejectsUnreconciledUser(t *testing.T) {
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "pass", "", t.TempDir())
	state.host = newMemoryHost()
	_, err := state.spawn(spawnRequest{Agent: "janitor", User: "missing-user"}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not a reconciled") {
		t.Fatalf("error = %v", err)
	}
}

func TestSpawnRejectsPathEscapeAndIdentityMismatch(t *testing.T) {
	_, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "pass", "", dataDir)
	state.host = newMemoryHost()
	state.remember(identity)

	_, err := state.spawn(spawnRequest{
		Agent:   identity.Agent,
		User:    identity.Username,
		Cwd:     identity.Cwd,
		Home:    identity.Home,
		RunDir:  "/etc",
		DshHome: "/etc/passwd",
	}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "data directory") && !strings.Contains(err.Error(), "run_dir") {
		t.Fatalf("escaped run_dir error = %v", err)
	}

	_, err = state.spawn(spawnRequest{
		Agent:   identity.Agent,
		User:    identity.Username,
		Cwd:     "/tmp",
		Home:    identity.Home,
		RunDir:  runDir,
		DshHome: dshHome,
	}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("cwd mismatch error = %v", err)
	}

	_, err = state.spawn(spawnRequest{
		Agent:   "other-agent",
		User:    identity.Username,
		Cwd:     identity.Cwd,
		Home:    identity.Home,
		RunDir:  runDir,
		DshHome: dshHome,
	}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("agent mismatch error = %v", err)
	}
}

func TestSpawnRejectsSystemUID(t *testing.T) {
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "pass", "", t.TempDir())
	state.host = newMemoryHost()
	state.remember(reconciledIdentity{
		Agent: "janitor", Username: "daemon", Home: "/home/daemon", Cwd: "/home/daemon/w",
		UID: 1, GID: 1, Groups: []uint32{1},
	})
	_, err := state.spawn(spawnRequest{Agent: "janitor", User: "daemon", Cwd: "/home/daemon/w", Home: "/home/daemon"}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "system uid") {
		t.Fatalf("system uid error = %v", err)
	}
}

func TestSpawnEnvStripsPrivilegedKeysAndIgnoresParentPath(t *testing.T) {
	t.Setenv("PATH", "/root/evil-bin")
	env := spawnEnv(reconciledIdentity{Username: "workspace-janitor", Home: "/home/workspace-janitor"}, map[string]string{
		privilegedFDEnv: "3",
		syncTokenEnv:    "secret",
		listenerUserEnv: "genesis",
		"LANG":          "C.UTF-8",
		"PATH":          "/agent/bin",
	})
	got := map[string]string{}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		got[key] = value
	}
	if _, ok := got[privilegedFDEnv]; ok {
		t.Fatal("leaked privileged fd")
	}
	if _, ok := got[syncTokenEnv]; ok {
		t.Fatal("leaked sync token")
	}
	if _, ok := got[listenerUserEnv]; ok {
		t.Fatal("leaked listener user")
	}
	if got["PATH"] != "/agent/bin" {
		t.Fatalf("PATH = %q", got["PATH"])
	}
	if got["HOME"] != "/home/workspace-janitor" || got["USER"] != "workspace-janitor" || got["SHELL"] != agentShell {
		t.Fatalf("identity env = %#v", got)
	}

	blocked := spawnEnv(reconciledIdentity{Username: "workspace-janitor", Home: "/home/workspace-janitor"}, map[string]string{
		"SSH_AUTH_SOCK": "/tmp/attacker.sock",
		"GIT_SSH":       "/tmp/askpass",
		"GITHUB_TOKEN":  "ghs_attacker_token_value",
		"GH_TOKEN":      "ghp_attacker_token_value",
		"LEAK":          "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----",
	})
	for _, item := range blocked {
		key, value, _ := strings.Cut(item, "=")
		if key == "SSH_AUTH_SOCK" || key == "GIT_SSH" || key == "GITHUB_TOKEN" || key == "GH_TOKEN" || strings.Contains(value, "PRIVATE KEY") {
			t.Fatalf("spawn env kept %s", item)
		}
	}

	plain := spawnEnv(reconciledIdentity{Username: "workspace-janitor", Home: "/home/workspace-janitor"}, nil)
	plainMap := map[string]string{}
	for _, item := range plain {
		key, value, _ := strings.Cut(item, "=")
		plainMap[key] = value
	}
	if plainMap["PATH"] != "/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("default PATH = %q", plainMap["PATH"])
	}
}

func TestSpawnedAgentUmaskLeavesWorkspaceReadable(t *testing.T) {
	umaskMu.Lock()
	previous := unix.Umask(0o077)
	umaskMu.Unlock()
	t.Cleanup(func() {
		umaskMu.Lock()
		unix.Umask(previous)
		umaskMu.Unlock()
	})

	_, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "import os\nos.mkdir('created-dir')\nopen('created-file','w').close()\n", "", dataDir)
	state.host = newMemoryHost()
	state.remember(identity)

	pid, err := state.spawn(spawnRequest{
		Agent:   identity.Agent,
		User:    identity.Username,
		Cwd:     identity.Cwd,
		Home:    identity.Home,
		RunDir:  runDir,
		DshHome: dshHome,
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	umaskMu.Lock()
	restored := unix.Umask(0o077)
	unix.Umask(0o077)
	umaskMu.Unlock()
	if restored != 0o077 {
		t.Fatalf("parent umask after spawn = %#o, want 077", restored)
	}
	code, err := state.wait(pid)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	dirInfo, err := os.Stat(filepath.Join(identity.Cwd, "created-dir"))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o755 {
		t.Fatalf("created directory mode = %#o, want 0755", dirInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(filepath.Join(identity.Cwd, "created-file"))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o644 {
		t.Fatalf("created file mode = %#o, want 0644", fileInfo.Mode().Perm())
	}
	dshInfo, err := os.Stat(dshHome)
	if err != nil {
		t.Fatal(err)
	}
	if dshInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dsh_home mode = %#o, want 0700", dshInfo.Mode().Perm())
	}
}

func testSpawnIdentity(t *testing.T) (*user.User, reconciledIdentity, string, string, string) {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.ParseUint(account.Uid, 10, 32)
	gid, _ := strconv.ParseUint(account.Gid, 10, 32)
	home := t.TempDir()
	cwd := filepath.Join(home, "workspace")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, runsDirName, "gen_spawn")
	dshHome := stableDshHome(dataDir, "gen_spawn")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dshHome, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := reconciledIdentity{
		Agent:    "janitor",
		Username: account.Username,
		Home:     home,
		Cwd:      cwd,
		UID:      uint32(uid),
		GID:      uint32(gid),
		Groups:   []uint32{uint32(gid)},
	}
	return account, identity, dataDir, runDir, dshHome
}
