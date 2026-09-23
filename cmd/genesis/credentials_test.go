package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestRootLaunchStoreIsTraversableAndGrantDirsStayPrivate(t *testing.T) {
	root := storeRootMode(0)
	if root&0o001 == 0 || root&0o066 != 0 || root&0o700 != 0o700 {
		t.Fatalf("root launch mode = %o", root)
	}
	if storeRootMode(1000) != credentialDirMode {
		t.Fatalf("unprivileged mode = %o", storeRootMode(1000))
	}
	if credentialDirMode&0o077 != 0 {
		t.Fatal("grant directories are not private")
	}
}

func TestCredentialStoreRejectsRenamableParentAndReplacedPath(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := openCredentialStore(filepath.Join(parent, "credentials"), credentialBounds{}); err == nil {
		t.Fatal("store in a world-writable parent was accepted")
	}

	safe := t.TempDir()
	root := filepath.Join(safe, "credentials")
	store, err := openCredentialStore(root, credentialBounds{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.dir.Close() })
	if err := os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	reopened, err := openCredentialStore(root, credentialBounds{})
	if err != nil {
		t.Fatalf("traversable store root was rejected: %v", err)
	}
	t.Cleanup(func() { _ = reopened.dir.Close() })
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != storeRootMode(os.Geteuid()) {
		t.Fatalf("reopened mode = %o", info.Mode().Perm())
	}
	grants, err := os.Lstat(filepath.Join(root, "grants"))
	if err != nil {
		t.Fatal(err)
	}
	if grants.Mode().Perm() != credentialDirMode {
		t.Fatalf("grants mode = %o", grants.Mode().Perm())
	}

	real := filepath.Join(safe, "real")
	if err := os.Rename(root, real); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(safe, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	intent := sampleGrant(intentEnsureCredential, "worker", "programmer", gitWrite, credentialPending)
	if _, err := store.ensureCredential(intent); err == nil {
		t.Fatal("replaced store path was accepted")
	}
	if _, err := startRunSSHAgent(store, "gen_replaced", os.Getuid(), os.Getgid(), ed25519.PrivateKey("not-a-key"), ""); err == nil {
		t.Fatal("replaced store path bound a socket")
	}
	for _, dir := range []string{outside, real} {
		walk := bytes.Buffer{}
		if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			payload, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			walk.Write(payload)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(walk.Bytes(), []byte("PRIVATE KEY")) || bytes.Contains(walk.Bytes(), []byte("agent.sock")) {
			t.Fatalf("replaced path received credential material in %s", dir)
		}
	}
}

func TestPrivateKeyErrorsDoNotEchoMaterial(t *testing.T) {
	store := openTestCredentialStore(t)
	intent := sampleGrant(intentEnsureCredential, "worker", "programmer", gitWrite, credentialPending)
	record, err := store.ensureCredential(intent)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(store.root, "grants", record.GrantID, privateKeyFileName)
	const marker = "SUPERSECRETKEYMATERIAL"
	if err := os.WriteFile(keyPath, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.ensureCredential(intent)
	if err == nil || strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("parse error echoed material: %v", err)
	}
}

func TestProductionRegistrarCannotReportReady(t *testing.T) {
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "print(1)", "genesis", t.TempDir())
	result, err := state.grantDriver().Register(context.Background(), grantRegistration{PublicKey: "ssh-ed25519 AAAA"})
	if err != nil || result.Status != remoteRegistrationUnsupported || result.RemoteKeyID != "" {
		t.Fatalf("production registrar = %#v %v", result, err)
	}
	if _, ok := state.grantDriver().(githubRegistrar); !ok {
		t.Fatalf("production driver = %T", state.grantDriver())
	}
}

func TestIPCReplyRedactsPrivateKeys(t *testing.T) {
	reply := ipcEnvelope{
		Error:   "boom -----BEGIN OPENSSH PRIVATE KEY-----\nSECRET\n-----END OPENSSH PRIVATE KEY-----",
		Payload: []byte(`{"note":"-----BEGIN OPENSSH PRIVATE KEY-----\nSECRET\n-----END OPENSSH PRIVATE KEY-----"}`),
	}
	sanitizeIPCReply(&reply)
	if strings.Contains(reply.Error, "SECRET") || bytes.Contains(reply.Payload, []byte("SECRET")) || strings.Contains(reply.Error, "PRIVATE KEY") {
		t.Fatalf("ipc leaked key: %#v", reply)
	}
}

func TestSSHAgentStopDropsKeys(t *testing.T) {
	store := openTestCredentialStore(t)
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	run, err := startRunSSHAgent(store, "gen_stop", os.Getuid(), os.Getgid(), private, home)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", run.Socket())
	if err != nil {
		t.Fatal(err)
	}
	client := agent.NewClient(conn)
	keys, err := client.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %d", len(keys))
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-stop
			for range 20 {
				extra, err := net.Dial("unix", run.Socket())
				if err != nil {
					return
				}
				_, _ = agent.NewClient(extra).List()
				extra.Close()
			}
		}()
	}
	close(stop)
	run.stop()
	wg.Wait()
	if _, err := net.Dial("unix", run.Socket()); err == nil {
		t.Fatal("socket accepted after stop")
	}
	if _, err := client.List(); err == nil {
		t.Fatal("agent still served a key after stop")
	}
	if _, err := os.Lstat(run.Socket()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remained: %v", err)
	}
}

func TestChangedOrRefusedGrantDropsTheLiveSocket(t *testing.T) {
	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	store := openTestCredentialStore(t)
	report := filepath.Join(t.TempDir(), "sock")
	source := "import os,time\nopen(os.environ['REPORT'],'w').write(os.environ.get('SSH_AUTH_SOCK') or 'NONE')\ntime.sleep(60)\n"
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", source, account.Username, dataDir)
	state.host = newMemoryHost()
	state.mutate = true
	state.credentials = store
	fake := &fakeRegistrar{Status: remoteStatusReady, KeyID: "live-key"}
	state.registrar = fake
	state.remember(identity)
	t.Cleanup(func() { state.killAll() })
	plan := privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, gitWrite, credentialPending)}
	if _, err := state.apply(plan); err != nil {
		t.Fatal(err)
	}
	pid, err := state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome, Env: map[string]string{"REPORT": report},
	}, devNull(t), devNull(t), devNull(t))
	if err != nil {
		t.Fatal(err)
	}
	socket := strings.TrimSpace(waitFile(t, report))
	if socket == "" || socket == "NONE" {
		t.Fatalf("socket = %q", socket)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := state.apply(plan); err != nil {
		t.Fatal(err)
	}
	conn, err = net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("idempotent sync dropped the socket: %v", err)
	}
	conn.Close()
	fake.Err = errors.New("github missing")
	changed := privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, gitRead, credentialPending)}
	if _, err := state.apply(changed); err == nil {
		t.Fatal("changed grant registration succeeded")
	}
	if _, ok := state.readyHeld(identity.Agent); ok {
		t.Fatal("changed grant stayed deliverable")
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revoked socket remained: %v", err)
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err != nil {
		t.Fatal("child died when the socket was revoked")
	}
	again := filepath.Join(t.TempDir(), "again")
	otherRun := filepath.Join(dataDir, runsDirName, "gen_after")
	otherDsh := filepath.Join(otherRun, dshHomeDirName)
	if err := os.MkdirAll(otherDsh, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: otherRun, DshHome: otherDsh, Env: map[string]string{"REPORT": again},
	}, devNull(t), devNull(t), devNull(t)); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(waitFile(t, again)); got != "NONE" {
		t.Fatalf("replacement spawn received %q", got)
	}
}

func TestSharedUIDDoesNotReceiveASecondGrantSocket(t *testing.T) {
	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	store := openTestCredentialStore(t)
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", "import time\ntime.sleep(30)\n", account.Username, dataDir)
	state.host = newMemoryHost()
	state.mutate = true
	state.credentials = store
	state.registrar = &fakeRegistrar{Status: remoteStatusReady, KeyID: "k"}
	state.remember(identity)
	other := identity
	other.Agent = "other"
	other.Username = "other-user"
	state.remember(other)
	second := sampleGrant(intentEnsureCredential, other.Agent, "reviewer", gitRead, credentialPending)
	plan := privilegedPlan{Agents: true, Intents: append(grantTrio(identity.Agent, gitWrite, credentialPending),
		sampleGrant(intentEnsureRepositoryGrant, other.Agent, "reviewer", gitRead, credentialPending),
		second,
		sampleGrant(intentEnsureRemoteRegistration, other.Agent, "reviewer", gitRead, credentialPending),
	)}
	if _, err := state.apply(plan); err != nil {
		t.Fatal(err)
	}
	pid, err := state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome,
	}, devNull(t), devNull(t), devNull(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.killAll() })
	otherRun := filepath.Join(dataDir, runsDirName, "gen_other")
	otherDsh := filepath.Join(otherRun, dshHomeDirName)
	if err := os.MkdirAll(otherDsh, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := state.spawn(spawnRequest{
		Agent: other.Agent, User: other.Username, Cwd: other.Cwd, Home: other.Home,
		RunDir: otherRun, DshHome: otherDsh,
	}, devNull(t), devNull(t), devNull(t)); err == nil {
		t.Fatal("shared uid received a second grant socket")
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err != nil {
		t.Fatal("first child died")
	}
	entries, err := os.ReadDir(filepath.Join(store.root, "sockets"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("sockets = %v", entries)
	}
}

func TestConcurrentRunsOfOneGrantGetDistinctSockets(t *testing.T) {
	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	store := openTestCredentialStore(t)
	reportA := filepath.Join(t.TempDir(), "a")
	reportB := filepath.Join(t.TempDir(), "b")
	source := "import os,time\nopen(os.environ['REPORT'],'w').write(os.environ.get('SSH_AUTH_SOCK') or '')\ntime.sleep(30)\n"
	state := newPrivilegedState(discardLogger(), "/usr/bin/python3", source, account.Username, dataDir)
	state.host = newMemoryHost()
	state.mutate = true
	state.credentials = store
	state.registrar = &fakeRegistrar{Status: remoteStatusReady, KeyID: "k"}
	state.remember(identity)
	t.Cleanup(func() { state.killAll() })
	if _, err := state.apply(privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, gitWrite, credentialPending)}); err != nil {
		t.Fatal(err)
	}
	spawn := func(run, dsh, report string) int {
		t.Helper()
		if err := os.MkdirAll(dsh, 0o700); err != nil {
			t.Fatal(err)
		}
		pid, err := state.spawn(spawnRequest{
			Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
			RunDir: run, DshHome: dsh, Env: map[string]string{"REPORT": report},
		}, devNull(t), devNull(t), devNull(t))
		if err != nil {
			t.Fatal(err)
		}
		return pid
	}
	otherRun := filepath.Join(dataDir, runsDirName, "gen_spawn_b")
	otherDsh := filepath.Join(otherRun, dshHomeDirName)
	spawn(runDir, dshHome, reportA)
	spawn(otherRun, otherDsh, reportB)
	sockA := strings.TrimSpace(waitFile(t, reportA))
	sockB := strings.TrimSpace(waitFile(t, reportB))
	if sockA == "" || sockB == "" || sockA == sockB {
		t.Fatalf("sockets = %q %q", sockA, sockB)
	}
	keysOf := func(path string) []*agent.Key {
		t.Helper()
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		keys, err := agent.NewClient(conn).List()
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}
	left := keysOf(sockA)
	right := keysOf(sockB)
	if len(left) != 1 || len(right) != 1 || !bytes.Equal(left[0].Marshal(), right[0].Marshal()) {
		t.Fatalf("concurrent keys differ or multiplied: %d %d", len(left), len(right))
	}
	state.killAll()
	if _, err := os.Lstat(sockA); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first socket remained: %v", err)
	}
	if _, err := os.Lstat(sockB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second socket remained: %v", err)
	}
}

func TestCredentialStoreRejectsBoundaryViolations(t *testing.T) {
	data := t.TempDir()
	providers := t.TempDir()
	if _, err := openCredentialStore(filepath.Join(data, "credentials"), credentialBounds{DataDir: data}); err == nil {
		t.Fatal("store inside the data directory was accepted")
	}
	if _, err := openCredentialStore(filepath.Join(providers, "credentials"), credentialBounds{ProvidersDir: providers}); err == nil {
		t.Fatal("store inside providers was accepted")
	}
	parent := t.TempDir()
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(data, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := openCredentialStore(filepath.Join(alias, "credentials"), credentialBounds{DataDir: data}); err == nil {
		t.Fatal("store through a parent symlink was accepted")
	}
	link := filepath.Join(t.TempDir(), "linked-store")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := openCredentialStore(link, credentialBounds{}); err == nil {
		t.Fatal("symlink store was accepted")
	}
	if _, err := openCredentialStore("relative/credentials", credentialBounds{}); err == nil {
		t.Fatal("relative store was accepted")
	}
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openCredentialStore(loose, credentialBounds{}); err == nil {
		t.Fatal("group-readable store was accepted")
	}
	if err := (&credentialStore{root: "/home/agent/credentials"}).rejectOverlap(); err == nil {
		t.Fatal("store inside an agent home was accepted")
	}
}

func TestLocalEd25519GrantIsIdempotentAndRetained(t *testing.T) {
	store := openTestCredentialStore(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "note"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_APP_PEM", "-----BEGIN PRIVATE KEY-----\nnot-read\n-----END PRIVATE KEY-----")
	intent := sampleGrant(intentEnsureCredential, "worker", "programmer", gitWrite, credentialPending)
	first, err := store.ensureCredential(intent)
	if err != nil {
		t.Fatal(err)
	}
	if first.KeyMaterial != keyMaterialLocal || first.Generation != grantGenerationInitial || first.Fingerprint == "" {
		t.Fatalf("record = %#v", first)
	}
	keyPath := filepath.Join(store.root, "grants", first.GrantID, privateKeyFileName)
	original, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(original, []byte(os.Getenv("GITHUB_APP_PEM"))) {
		t.Fatal("key was derived from an App PEM")
	}
	info, err := os.Lstat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("key mode = %o", info.Mode())
	}
	stateRaw, err := os.ReadFile(filepath.Join(store.root, "grants", first.GrantID, grantStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateRaw, []byte("PRIVATE KEY")) || bytes.Contains(stateRaw, original) {
		t.Fatal("state file contains key material")
	}
	second, err := store.ensureCredential(intent)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fingerprint != first.Fingerprint || second.Generation != 1 || !bytes.Equal(original, retry) {
		t.Fatal("retry rotated the grant key")
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ensureCredential(intent); err == nil {
		t.Fatal("loose key was reused")
	}
	loose, err := os.Lstat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if loose.Mode().Perm() != 0o644 || !bytes.Equal(original, mustRead(t, keyPath)) {
		t.Fatal("failed check changed the loose key")
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ensureCredential(intent); err == nil {
		t.Fatal("symlink key was followed")
	}
	if string(mustRead(t, outside)) != "keep" {
		t.Fatal("symlink target was modified")
	}
	walk := bytes.Buffer{}
	if err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		walk.Write(payload)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(walk.Bytes(), []byte("PRIVATE KEY")) {
		t.Fatal("agent home contains key material")
	}

	public := sampleGrant(intentEnsureCredential, "reader", "public-bot", gitRead, credentialNone)
	public.Permissions = map[string]string{"contents": "read", "metadata": "read"}
	none, err := store.ensureCredential(public)
	if err != nil {
		t.Fatal(err)
	}
	if none.KeyMaterial != keyMaterialNone || none.Generation != 0 || none.Fingerprint != "" {
		t.Fatalf("public record = %#v", none)
	}
	if _, err := os.Lstat(filepath.Join(store.root, "grants", none.GrantID, privateKeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("public grant created a key: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	fresh := sampleGrant(intentEnsureCredential, "racer", "programmer", gitRead, credentialPending)
	fresh.Permissions = map[string]string{"contents": "read", "metadata": "read"}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ensureCredential(fresh)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	raced, err := store.ensureCredential(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if raced.Generation != 1 {
		t.Fatalf("raced generation = %d", raced.Generation)
	}

	changed := fresh
	changed.Git = gitWrite
	changed.Permissions = map[string]string{"contents": "write", "metadata": "read"}
	rotated, err := store.ensureCredential(changed)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.GrantID == raced.GrantID {
		t.Fatal("changed access grant reused the previous id")
	}
	oldKey := mustRead(t, filepath.Join(store.root, "grants", raced.GrantID, privateKeyFileName))
	newKey := mustRead(t, filepath.Join(store.root, "grants", rotated.GrantID, privateKeyFileName))
	if bytes.Equal(oldKey, newKey) || rotated.Generation != 1 {
		t.Fatal("access change rotated the old key instead of retaining it")
	}
}

func TestGitHubRegistrarStaysUnsupportedAndFakeCanReportReady(t *testing.T) {
	source, err := os.ReadFile("registrar.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"net/http", "api.github.com", "http.Get", "http.Post"} {
		if bytes.Contains(source, []byte(forbidden)) {
			t.Fatalf("registrar source contains %q", forbidden)
		}
	}
	result, err := githubRegistrar{}.Register(context.Background(), grantRegistration{
		GrantID: "g0123456789abcdef0123456789abcdef", PublicKey: "ssh-ed25519 AAAA comment", Fingerprint: "SHA256:abc",
	})
	if err != nil || result.Status != remoteRegistrationUnsupported || result.RemoteKeyID != "" {
		t.Fatalf("github registrar = %#v %v", result, err)
	}

	store := openTestCredentialStore(t)
	fake := &fakeRegistrar{Status: remoteStatusReady, KeyID: "fake-key-1"}
	state := &privilegedState{logger: discardLogger(), host: newMemoryHost(), mutate: true, credentials: store, registrar: fake}
	intent := sampleGrant(intentEnsureCredential, "worker", "programmer", gitWrite, credentialPending)
	plan := privilegedPlan{Agents: true, Intents: []privilegedIntent{
		sampleGrant(intentEnsureRepositoryGrant, "worker", "programmer", gitWrite, credentialPending),
		intent,
		sampleGrant(intentEnsureRemoteRegistration, "worker", "programmer", gitWrite, credentialPending),
	}}
	applied, err := state.apply(plan)
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls != 1 || strings.Contains(fake.Last.PublicKey, "PRIVATE KEY") || !strings.HasPrefix(fake.Last.PublicKey, "ssh-ed25519 ") {
		t.Fatalf("registration call = %d key=%q", fake.Calls, fake.Last.PublicKey)
	}
	if len(applied.Grants) != 1 || applied.Grants[0].RemoteStatus != remoteStatusReady || applied.Grants[0].RemoteKeyID != "fake-key-1" || applied.Grants[0].Generation != 1 {
		t.Fatalf("grants = %#v", applied.Grants)
	}
	if !slices.Contains(applied.Applied, intentEnsureRemoteRegistration+":worker") {
		t.Fatalf("applied = %#v", applied.Applied)
	}
	empty := privilegedPlan{Agents: true, Intents: []privilegedIntent{}}
	removed, err := state.apply(empty)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed.RetainedGrants) != 1 || removed.RetainedGrants[0] != applied.Grants[0].GrantID {
		t.Fatalf("retained = %#v", removed.RetainedGrants)
	}
	if _, err := os.Lstat(filepath.Join(store.root, "grants", applied.Grants[0].GrantID, privateKeyFileName)); err != nil {
		t.Fatal("YAML removal deleted grant material")
	}
	if _, ok := state.readyHeld("worker"); ok {
		t.Fatal("removed grant stayed ready for delivery")
	}

	provisional, err := buildGrantPlan(plan, true)
	if err != nil {
		t.Fatal(err)
	}
	attached := attachGrantMaterial(provisional, applied)
	if attached.CredentialActive || attached.KeyMaterial == "" {
		t.Fatalf("attached plan activated a credential: %#v", attached)
	}
	attached.CredentialActive = false
	if err := grantPlanIsInactive(attached); err != nil {
		t.Fatal(err)
	}
	planted := applied
	planted.Grants[0].Fingerprint = "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n-----END OPENSSH PRIVATE KEY-----"
	redacted := attachGrantMaterial(provisional, planted)
	encoded, _ := json.Marshal(redacted)
	if bytes.Contains(encoded, []byte("secret")) || bytes.Contains(encoded, []byte("PRIVATE KEY")) {
		t.Fatalf("material leaked a private key: %s", encoded)
	}
}

func TestGrantSocketStaysClosedUntilReadyAndCleansEveryExit(t *testing.T) {
	account, identity, dataDir, runDir, dshHome := testSpawnIdentity(t)
	store := openTestCredentialStore(t)
	var logs bytes.Buffer
	state := newPrivilegedState(slog.New(slog.NewJSONHandler(&logs, nil)), "/usr/bin/python3", "import os\nprint(os.environ.get('SSH_AUTH_SOCK',''))", account.Username, dataDir)
	state.host = newMemoryHost()
	state.mutate = true
	state.credentials = store
	state.remember(identity)
	plan := privilegedPlan{Agents: true, Intents: grantTrio(identity.Agent, gitWrite, credentialPending)}
	if _, err := state.apply(plan); err != nil {
		t.Fatal(err)
	}
	before := childPIDs(t, os.Getpid())
	pid, err := state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome, Env: map[string]string{"SSH_AUTH_SOCK": "/tmp/attacker.sock"},
	}, devNull(t), devNull(t), devNull(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.wait(pid); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "PRIVATE KEY") {
		t.Fatalf("logs contained key material: %s", logs.String())
	}
	entries, err := os.ReadDir(filepath.Join(store.root, "sockets"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("socket left behind: %v", entries)
	}
	assertNoNewChildren(t, before)

	fake := &fakeRegistrar{Status: remoteStatusReady, KeyID: "fake-key-9"}
	state.registrar = fake
	if _, err := state.apply(plan); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.readyHeld(identity.Agent); !ok {
		t.Fatal("ready grant was not held")
	}
	reportPath := filepath.Join(t.TempDir(), "report")
	state.source = "import os,time\n" +
		"values=chr(10).join(os.environ.values())\n" +
		"bad='BAD' if ('PRIVATE KEY' in values or 'GIT_SSH' in os.environ or 'SSH_AGENT_PID' in os.environ) else 'OK'\n" +
		"open(" + strconv.Quote(reportPath) + ",'w').write((os.environ.get('SSH_AUTH_SOCK') or '')+chr(10)+bad+chr(10))\n" +
		"time.sleep(30)\n"
	pid, err = state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome,
	}, devNull(t), devNull(t), devNull(t))
	if err != nil {
		t.Fatal(err)
	}
	report := waitFile(t, reportPath)
	output, marker, _ := strings.Cut(report, "\n")
	marker = strings.TrimSpace(marker)
	if output == "" || strings.Contains(output, "attacker") || marker != "OK" {
		t.Fatalf("socket = %q marker = %q", output, marker)
	}
	conn, err := net.Dial("unix", output)
	if err != nil {
		t.Fatal(err)
	}
	client := agent.NewClient(conn)
	keys, err := client.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Type() != ssh.KeyAlgoED25519 || keys[0].Comment != grantComment {
		t.Fatalf("agent keys = %#v", keys)
	}
	if _, err := client.Sign(keys[0], []byte("probe")); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveAll(); err == nil {
		t.Fatal("child could change the sealed agent")
	}
	conn.Close()
	if !pathWithin(output, store.root) || pathWithin(output, identity.Home) {
		t.Fatalf("socket path = %s", output)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if _, err := state.wait(pid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remained after wait: %v", err)
	}

	cancelReport := filepath.Join(t.TempDir(), "cancel-report")
	state.source = "import os,time\nopen(" + strconv.Quote(cancelReport) + ",'w').write(os.environ.get('SSH_AUTH_SOCK') or '')\ntime.sleep(30)\n"
	pid, err = state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome,
	}, devNull(t), devNull(t), devNull(t))
	if err != nil {
		t.Fatal(err)
	}
	sock := strings.TrimSpace(waitFile(t, cancelReport))
	state.killAll()
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remained after cancel: %v", err)
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled child is still running")
	}

	state.pythonPath = "/no/such/python"
	if _, err := state.spawn(spawnRequest{
		Agent: identity.Agent, User: identity.Username, Cwd: identity.Cwd, Home: identity.Home,
		RunDir: runDir, DshHome: dshHome,
	}, devNull(t), devNull(t), devNull(t)); err == nil {
		t.Fatal("missing python started")
	}
	left, err := os.ReadDir(filepath.Join(store.root, "sockets"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("failed spawn left sockets: %v", left)
	}
}

func TestControlRedactsPrivateKeysFromRunStatus(t *testing.T) {
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "genesis")
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(block)
	dataDir := t.TempDir()
	if _, err := prepareDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dataDir, runsDirName, "gen_redact")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	event := lifecycleEvent{
		SpecVersion: cloudEventSpecVersion, ID: "evt", Source: "urn:genesis:run:gen_redact",
		Type: lifecycleTypeAccepted, Time: "2026-01-01T00:00:00.000000Z", Sequence: "1",
		SequenceType: sequenceTypeInteger, RunID: "gen_redact", AgentID: "worker",
		Rulefile: "worker.yaml", CauseID: "cause", CauseSource: "urn:test", Origin: originGenesis,
		Data: json.RawMessage(`{"note":"plain"}`),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, eventsFileName), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, stderrFileName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	control := &controlServer{dataDir: dataDir}
	detail, err := control.readRunDetail("gen_redact")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail.StderrTail, "PRIVATE KEY") || strings.Contains(detail.StderrTail, string(encoded)) {
		t.Fatalf("stderr tail = %s", detail.StderrTail)
	}
	body, _ := json.Marshal(detail)
	if bytes.Contains(body, []byte("PRIVATE KEY")) {
		t.Fatalf("run detail = %s", body)
	}

	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(listener.Close)
	panel := httptest.NewServer(newControlServer(controlConfig{
		listenerURL: listener.URL, agentsDir: t.TempDir(), rulesDir: t.TempDir(),
		reposDir: t.TempDir(), providersDir: t.TempDir(), dataDir: dataDir,
	}, discardLogger()))
	t.Cleanup(panel.Close)
	response, err := http.Get(panel.URL + "/api/runs/gen_redact")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	page, _ := io.ReadAll(response.Body)
	if bytes.Contains(page, []byte("PRIVATE KEY")) || bytes.Contains(page, encoded) {
		t.Fatalf("control status = %s", page)
	}
}

func TestEntrypointCredentialStoreStaysOutsideConfig(t *testing.T) {
	root := repoRoot(t)
	config := t.TempDir()
	data := t.TempDir()
	defaults := t.TempDir()
	credentials := filepath.Join(t.TempDir(), "credentials")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(path, command string) ([]byte, error) {
		cmd := exec.Command("sh", filepath.Join(root, "docker-entrypoint.sh"), command)
		cmd.Env = append(os.Environ(),
			"GENESIS_CONFIG_DIR="+config,
			"GENESIS_DATA_DIR="+data,
			"GENESIS_DEFAULTS_DIR="+defaults,
			"GENESIS_CREDENTIALS_DIR="+path,
		)
		return cmd.CombinedOutput()
	}
	output, err := run(credentials, "own-config")
	if err != nil {
		t.Fatalf("own-config: %v\n%s", err, output)
	}
	info, err := os.Stat(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("credentials mode = %o", info.Mode().Perm())
	}
	if err := os.Symlink(outside, filepath.Join(credentials, "alias")); err != nil {
		t.Fatal(err)
	}
	output, err = run(credentials, "own-config")
	if err != nil {
		t.Fatalf("own-config with inner symlink: %v\n%s", err, output)
	}
	outsideInfo, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if outsideInfo.Mode().Perm() != 0o644 {
		t.Fatalf("credentials lock followed a symlink: %o", outsideInfo.Mode().Perm())
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(credentials, link); err != nil {
		t.Fatal(err)
	}
	output, err = run(link, "own-config")
	if err == nil || !bytes.Contains(output, []byte("must not be a symlink")) {
		t.Fatalf("symlink credentials directory error = %v\n%s", err, output)
	}
	output, err = run(filepath.Join(config, "credentials"), "own-config")
	if err == nil || !bytes.Contains(output, []byte("outside config")) {
		t.Fatalf("config credentials error = %v\n%s", err, output)
	}
}

func openTestCredentialStore(t *testing.T) *credentialStore {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "gcred")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	store, err := openCredentialStore(root, credentialBounds{})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func sampleGrant(kind, agent, identity, gitAccess, credential string) privilegedIntent {
	permissions := map[string]string{"contents": "write", "metadata": "read"}
	if gitAccess == gitRead {
		permissions = map[string]string{"contents": "read", "metadata": "read"}
	}
	return privilegedIntent{
		Kind: kind, Agent: agent, Repository: "lab", Identity: identity,
		Git: gitAccess, Permissions: permissions, Credential: credential,
	}
}

func grantTrio(agent, gitAccess, credential string) []privilegedIntent {
	return []privilegedIntent{
		sampleGrant(intentEnsureRepositoryGrant, agent, "programmer", gitAccess, credential),
		sampleGrant(intentEnsureCredential, agent, "programmer", gitAccess, credential),
		sampleGrant(intentEnsureRemoteRegistration, agent, "programmer", gitAccess, credential),
	}
}

func grantKeyPaths(t *testing.T, store *credentialStore) (string, string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store.root, "grants"))
	if err != nil {
		t.Fatal(err)
	}
	worker, reader := "", ""
	for _, entry := range entries {
		payload, err := os.ReadFile(filepath.Join(store.root, "grants", entry.Name(), grantStateFileName))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(payload, []byte("PRIVATE KEY")) {
			t.Fatal("grant state contains key material")
		}
		var record grantRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			t.Fatal(err)
		}
		keyPath := filepath.Join(store.root, "grants", entry.Name(), privateKeyFileName)
		_, statErr := os.Lstat(keyPath)
		hasKey := statErr == nil
		switch record.Agent {
		case "worker":
			if hasKey {
				worker = keyPath
			}
		case "reader":
			if hasKey {
				reader = keyPath
			}
		default:
			t.Fatalf("unexpected grant agent %s", record.Agent)
		}
	}
	return worker, reader
}

func slicesContainsAll(values []string, want ...string) bool {
	for _, item := range want {
		if !slices.Contains(values, item) {
			return false
		}
	}
	return true
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	file, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		payload, err := os.ReadFile(path)
		if err == nil && len(payload) > 0 {
			return string(payload)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

func childPIDs(t *testing.T, parent int) map[int]struct{} {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	found := map[int]struct{}{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		text := string(stat)
		end := strings.LastIndex(text, ")")
		if end < 0 || end+2 >= len(text) {
			continue
		}
		fields := strings.Fields(text[end+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err == nil && ppid == parent {
			found[pid] = struct{}{}
		}
	}
	return found
}

func assertNoNewChildren(t *testing.T, before map[int]struct{}) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current := childPIDs(t, os.Getpid())
		extra := false
		for pid := range current {
			if _, ok := before[pid]; !ok {
				if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err == nil {
					extra = true
				}
			}
		}
		if !extra {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("child process remained after cleanup")
}
