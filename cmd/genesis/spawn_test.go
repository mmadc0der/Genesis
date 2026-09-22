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

	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.ParseUint(account.Uid, 10, 32)
	gid, _ := strconv.ParseUint(account.Gid, 10, 32)
	work := t.TempDir()
	runDir := filepath.Join(t.TempDir(), "runs", "gen_spawn")
	dshHome := filepath.Join(runDir, "dsh_home")
	if err := os.MkdirAll(dshHome, 0o700); err != nil {
		t.Fatal(err)
	}

	state := newPrivilegedState(slog.New(slog.NewJSONHandler(io.Discard, nil)), "/usr/bin/python3", "import sys; sys.stdout.write(sys.stdin.read())", account.Username)
	state.mutate = false
	state.host = newMemoryHost()
	state.remember(reconciledIdentity{
		Agent:    "janitor",
		Username: account.Username,
		Home:     work,
		Cwd:      work,
		UID:      uint32(uid),
		GID:      uint32(gid),
		Groups:   []uint32{uint32(gid)},
	})
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
		Agent:   "janitor",
		User:    account.Username,
		Cwd:     work,
		Home:    work,
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

func TestSpawnRejectsUnreconciledUser(t *testing.T) {
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "pass", "")
	state.host = newMemoryHost()
	_, err := state.spawn(spawnRequest{Agent: "janitor", User: "missing-user"}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not a reconciled") {
		t.Fatalf("error = %v", err)
	}
}
