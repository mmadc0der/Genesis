package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLocateSessionLogPrefersHighestVersion(t *testing.T) {
	home := t.TempDir()
	older := filepath.Join(home, "sessions", "proj", "session-a")
	newer := filepath.Join(home, "sessions", "proj", "session-b")
	if err := os.MkdirAll(older, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newer, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(older, "session.jsonl"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(newer, "session.v3.jsonl")
	if err := os.WriteFile(want, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newer, "session.v1.jsonl"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := locateSessionLog(home)
	abs, err := filepath.Abs(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != abs {
		t.Fatalf("session log = %s, want %s", got, abs)
	}
}

func TestGrantSessionReadOpensListenerOwnedLog(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "proj", "session-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "session.v3.jsonl")
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	if err := grantSessionRead(home, uid, true, 0, false); err != nil {
		t.Fatal(err)
	}
	logInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if logInfo.Mode().Perm() != sessionLogMode {
		t.Fatalf("session log mode = %o", logInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm()&0o010 == 0 {
		t.Fatalf("session dir mode = %o, want group execute", dirInfo.Mode().Perm())
	}
	otherInfo, err := os.Stat(other)
	if err != nil {
		t.Fatal(err)
	}
	if otherInfo.Mode().Perm() != 0o600 {
		t.Fatalf("non-session file mode = %o", otherInfo.Mode().Perm())
	}
}

func TestRecordedSessionPathStaysInsideHome(t *testing.T) {
	home := t.TempDir()
	runDir := t.TempDir()
	logPath := filepath.Join(home, "session.v3.jsonl")
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSessionPath(runDir, abs, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if got := readRecordedSessionPath(runDir, home); got != abs {
		t.Fatalf("recorded = %s", got)
	}
	outside := filepath.Join(t.TempDir(), "session.v3.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideAbs, err := filepath.Abs(outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, sessionPathFileName), []byte(outsideAbs+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readRecordedSessionPath(runDir, home); got != "" {
		t.Fatalf("outside path was accepted: %s", got)
	}
}

func TestNamedUserACLRecordsOracle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.v3.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Getuid())
	if err := grantNamedUserACL(path, 0o600, uid, aclRead); err != nil {
		t.Skipf("filesystem rejected posix acl: %v", err)
	}
	buffer := make([]byte, 128)
	n, err := unix.Getxattr(path, "system.posix_acl_access", buffer)
	if err != nil {
		t.Fatal(err)
	}
	if n < 12 {
		t.Fatalf("acl length = %d", n)
	}
	found := false
	for off := 4; off+8 <= n; off += 8 {
		tag := binary.LittleEndian.Uint16(buffer[off : off+2])
		id := binary.LittleEndian.Uint32(buffer[off+4 : off+8])
		if tag == aclUser && id == uid {
			found = true
		}
	}
	if !found {
		t.Fatal("named user entry missing")
	}
}
