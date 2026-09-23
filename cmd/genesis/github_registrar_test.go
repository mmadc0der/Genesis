package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestUnconfiguredRegistrarDoesNotUseTheNetwork(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unconfigured registrar used the network")
		return nil, errors.New("network")
	})
	for _, reg := range []githubRegistrar{{}, {transport: transport}} {
		result, err := reg.Register(context.Background(), grantRegistration{PublicKey: "ssh-ed25519 AAAA", Git: gitWrite})
		if err != nil || result.Status != remoteRegistrationUnsupported || result.RemoteKeyID != "" {
			t.Fatalf("registrar = %#v %v", result, err)
		}
	}
	source, err := os.ReadFile("registrar.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"net/http", "api.github.com", "http.Get", "http.Post", "BEGIN PRIVATE KEY"} {
		if bytes.Contains(source, []byte(forbidden)) {
			t.Fatalf("registrar source contains %q", forbidden)
		}
	}
}

func TestPublicAndGitNoneMakeNoGitHubCalls(t *testing.T) {
	store := openTestCredentialStore(t)
	reg := githubRegistrar{
		transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("github was called")
			return nil, errors.New("called")
		}),
		resolve: func(context.Context, grantRegistration) (githubAppBinding, error) {
			t.Fatal("reconciler secret was resolved")
			return githubAppBinding{}, errors.New("resolved")
		},
		now: time.Now,
	}
	for _, intent := range []privilegedIntent{
		sampleGrant(intentEnsureRemoteRegistration, "reader", "public", gitRead, credentialNone),
		sampleGrant(intentEnsureRemoteRegistration, "planner", "manager", gitNone, credentialPending),
	} {
		cred := intent
		cred.Kind = intentEnsureCredential
		if _, err := store.ensureCredential(cred); err != nil {
			t.Fatal(err)
		}
		record, err := store.ensureRegistration(context.Background(), intent, reg)
		if err != nil {
			t.Fatal(err)
		}
		if record.RemoteStatus == remoteStatusReady || record.KeyMaterial == keyMaterialLocal {
			t.Fatalf("grant became a deploy key: %#v", record)
		}
	}
}

func TestDeployKeyRegistrationAdoptsCreatesAndFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	public, fingerprint := testDeployPublic(t)
	grantID := "g0123456789abcdef0123456789abcdef"
	title := "genesis-" + grantID

	t.Run("create write and read", func(t *testing.T) {
		for _, gitAccess := range []string{gitWrite, gitRead} {
			fake := newGitHubFake(t, now)
			reg := fake.registrar()
			result, err := reg.Register(context.Background(), grantRegistration{
				GrantID: grantID, Repository: "lab", Fingerprint: fingerprint, PublicKey: public, Git: gitAccess,
			})
			if err != nil || result.Status != remoteStatusReady || result.RemoteKeyID != "77" {
				t.Fatalf("register %s = %#v %v", gitAccess, result, err)
			}
			if fake.postCalls != 1 || fake.deleteCalls != 0 {
				t.Fatalf("posts=%d deletes=%d", fake.postCalls, fake.deleteCalls)
			}
			if gitAccess == gitWrite && fake.lastReadOnly {
				t.Fatal("write grant was registered read-only")
			}
			if gitAccess == gitRead && !fake.lastReadOnly {
				t.Fatal("read grant was registered writable")
			}
			if fake.lastTitle != title || !strings.HasPrefix(fake.lastKey, "ssh-ed25519 ") {
				t.Fatalf("posted title=%q key=%q", fake.lastTitle, fake.lastKey)
			}
			if fake.keyTokenPosts != 1 || fake.discoveryPosts != 1 {
				t.Fatalf("discovery=%d key-token=%d", fake.discoveryPosts, fake.keyTokenPosts)
			}
			again, err := reg.Register(context.Background(), grantRegistration{
				GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitAccess, RemoteKeyID: "77",
			})
			if err != nil || again.RemoteKeyID != "77" || fake.postCalls != 1 {
				t.Fatalf("second register = %#v %v posts=%d", again, err, fake.postCalls)
			}
		}
	})

	t.Run("existing match", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.preload(77, title, public, true)
		reg := fake.registrar()
		result, err := reg.Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if err != nil || result.RemoteKeyID != "77" || fake.postCalls != 0 {
			t.Fatalf("adopt = %#v %v posts=%d", result, err, fake.postCalls)
		}
	})

	t.Run("title collision", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		other, _ := testDeployPublic(t)
		fake.preload(9, title, other, false)
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitWrite,
		})
		if !githubKind(err, "collision") || fake.postCalls != 0 {
			t.Fatalf("collision = %v posts=%d", err, fake.postCalls)
		}
	})

	t.Run("fingerprint collision", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.preload(9, "other-title", public, false)
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitWrite,
		})
		if !githubKind(err, "collision") || fake.postCalls != 0 {
			t.Fatalf("collision = %v posts=%d", err, fake.postCalls)
		}
	})

	t.Run("mode mismatch", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.preload(77, title, public, true)
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitWrite, RemoteKeyID: "77",
		})
		if !githubKind(err, "collision") || fake.postCalls != 0 || fake.deleteCalls != 0 {
			t.Fatalf("mode = %v posts=%d deletes=%d", err, fake.postCalls, fake.deleteCalls)
		}
	})

	t.Run("auth then success", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.authFails = 1
		result, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitWrite,
		})
		if err != nil || result.RemoteKeyID != "77" || fake.tokenCalls < 2 {
			t.Fatalf("auth retry = %#v %v calls=%d", result, err, fake.tokenCalls)
		}
	})

	t.Run("rate limit retries inside the cap", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.listAfter = "1"
		fake.listFailOnce = true
		var slept []time.Duration
		reg := fake.registrar()
		reg.sleep = func(delay time.Duration) { slept = append(slept, delay) }
		result, err := reg.Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if err != nil || result.Status != remoteStatusReady {
			t.Fatalf("rate retry = %#v %v", result, err)
		}
		if len(slept) != 1 || slept[0] != time.Second {
			t.Fatalf("sleeps = %v", slept)
		}
	})

	t.Run("long rate limit fails closed", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.listAfter = "30"
		fake.listFailOnce = true
		var slept []time.Duration
		reg := fake.registrar()
		reg.sleep = func(delay time.Duration) { slept = append(slept, delay) }
		_, err := reg.Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if !githubKind(err, "rate") || len(slept) != 0 {
			t.Fatalf("long rate = %v sleeps=%v", err, slept)
		}
	})

	t.Run("unavailable then success", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.repoFailOnce = true
		result, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitWrite,
		})
		if err != nil || result.RemoteKeyID != "77" || fake.repoCalls < 2 {
			t.Fatalf("retry = %#v %v repos=%d", result, err, fake.repoCalls)
		}
	})

	t.Run("missing repository", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.repoStatus = http.StatusNotFound
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if !githubKind(err, "missing") || fake.listCalls != 0 || fake.postCalls != 0 {
			t.Fatalf("missing = %v lists=%d posts=%d", err, fake.listCalls, fake.postCalls)
		}
	})

	t.Run("collision response can adopt a raced key", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.postStatus = http.StatusUnprocessableEntity
		fake.adoptOnPost = true
		result, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if err != nil || result.RemoteKeyID != "77" {
			t.Fatalf("raced adopt = %#v %v", result, err)
		}
	})

	t.Run("unmatched collision stays closed", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.postStatus = http.StatusUnprocessableEntity
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitWrite,
		})
		if !githubKind(err, "collision") {
			t.Fatalf("collision = %v", err)
		}
	})

	t.Run("partial create", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.emptyCreate = true
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitWrite,
		})
		if !githubKind(err, "partial") {
			t.Fatalf("partial = %v", err)
		}
	})

	t.Run("unscoped token", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.selection = "all"
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if !githubKind(err, "partial") || fake.listCalls != 0 {
			t.Fatalf("unscoped = %v lists=%d", err, fake.listCalls)
		}
	})

	t.Run("extra token permission is refused", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.extraPermission = "contents"
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if !githubKind(err, "auth") || fake.repoCalls != 0 || fake.listCalls != 0 || fake.postCalls != 0 || strings.Contains(err.Error(), "ghs_") {
			t.Fatalf("extra permission = %v repos=%d", err, fake.repoCalls)
		}
	})

	t.Run("short lived token is refused", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.expiresSoon = true
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if !githubKind(err, "auth") || fake.repoCalls != 0 || strings.Contains(err.Error(), "ghs_") {
			t.Fatalf("expiry = %v", err)
		}
	})

	t.Run("token without full name is refused", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.omitTokenFullName = true
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if !githubKind(err, "partial") || fake.repoCalls != 0 || fake.postCalls != 0 {
			t.Fatalf("missing full name = %v repos=%d", err, fake.repoCalls)
		}
	})

	t.Run("repository owner must match the declaration", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.owner = "evil"
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if !githubKind(err, "partial") || fake.listCalls != 0 || fake.postCalls != 0 {
			t.Fatalf("owner mismatch = %v lists=%d posts=%d", err, fake.listCalls, fake.postCalls)
		}
	})

	t.Run("token repository id must match the repository", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.tokenID = 999
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if !githubKind(err, "partial") || fake.keyTokenPosts != 0 || fake.postCalls != 0 {
			t.Fatalf("id mismatch = %v key-tokens=%d", err, fake.keyTokenPosts)
		}
	})

	t.Run("canonical name case still matches", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.owner = "Octo-Org"
		fake.repoName = "Lab-Widget"
		fake.tokenName = "Lab-Widget"
		fake.tokenFullName = "Octo-Org/Lab-Widget"
		result, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if err != nil || result.RemoteKeyID != "77" {
			t.Fatalf("case = %#v %v", result, err)
		}
	})

	t.Run("unrelated key does not block registration", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.preload(9, "someone-else", testRSAPublic(t), false)
		result, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitWrite,
		})
		if err != nil || result.RemoteKeyID != "77" || fake.postCalls != 1 {
			t.Fatalf("unrelated = %#v %v posts=%d", result, err, fake.postCalls)
		}
	})

	t.Run("non-ed25519 title collision fails closed", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.preload(9, title, testRSAPublic(t), false)
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitWrite,
		})
		if !githubKind(err, "collision") || fake.postCalls != 0 || fake.deleteCalls != 0 {
			t.Fatalf("rsa title = %v posts=%d", err, fake.postCalls)
		}
	})

	t.Run("error body does not leak tokens", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.repoStatus = http.StatusInternalServerError
		fake.leakBody = true
		_, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if err == nil || strings.Contains(err.Error(), "ghs_") || strings.Contains(err.Error(), "eyJ") || strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "MIIE") {
			t.Fatalf("leak = %v", err)
		}
	})

	t.Run("second page is searched before create", func(t *testing.T) {
		fake := newGitHubFake(t, now)
		fake.pageTwo = true
		fake.preload(77, title, public, true)
		result, err := fake.registrar().Register(context.Background(), grantRegistration{
			GrantID: grantID, Fingerprint: fingerprint, PublicKey: public, Git: gitRead,
		})
		if err != nil || result.RemoteKeyID != "77" || fake.postCalls != 0 || fake.listCalls != 2 {
			t.Fatalf("page = %#v %v lists=%d posts=%d", result, err, fake.listCalls, fake.postCalls)
		}
	})
}

func TestReadyDriftRefusesTheSocket(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	store := openTestCredentialStore(t)
	var logs bytes.Buffer
	fake := newGitHubFake(t, now)
	state := &privilegedState{
		logger:      slog.New(slog.NewJSONHandler(&logs, nil)),
		host:        newMemoryHost(),
		mutate:      true,
		credentials: store,
		registrar:   fake.registrar(),
	}
	plan := privilegedPlan{Agents: true, Intents: grantTrio("worker", gitWrite, credentialPending)}
	applied, err := state.apply(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Grants) != 1 || applied.Grants[0].RemoteStatus != remoteStatusReady || applied.Grants[0].RemoteKeyID != "77" {
		t.Fatalf("grants = %#v", applied.Grants)
	}
	if _, ok := state.readyHeld("worker"); !ok {
		t.Fatal("ready grant was not held")
	}
	payload := mustRead(t, filepath.Join(store.root, "grants", applied.Grants[0].GrantID, grantStateFileName))
	if bytes.Contains(payload, []byte("PRIVATE KEY")) || bytes.Contains(payload, []byte("eyJ")) || bytes.Contains(payload, []byte("ghs_")) {
		t.Fatalf("state leaked material: %s", payload)
	}
	fake.repoStatus = http.StatusNotFound
	if _, err := state.apply(plan); err == nil || !githubKind(err, "missing") {
		t.Fatalf("drift = %v", err)
	}
	if _, ok := state.readyHeld("worker"); ok {
		t.Fatal("drifted grant stayed deliverable")
	}
	refused := mustRead(t, filepath.Join(store.root, "grants", applied.Grants[0].GrantID, grantStateFileName))
	if !bytes.Contains(refused, []byte(`"remote_status":"refused"`)) {
		t.Fatalf("state = %s", refused)
	}
	if _, _, err := store.privateKeyForGrant(applied.Grants[0].GrantID); err == nil {
		t.Fatal("refused grant still delivered a key")
	}
	if bytes.Contains(logs.Bytes(), []byte("eyJ")) || bytes.Contains(logs.Bytes(), []byte("ghs_")) || bytes.Contains(logs.Bytes(), []byte("PRIVATE KEY")) || bytes.Contains(logs.Bytes(), []byte(fake.pemMarker)) {
		t.Fatalf("logs leaked material: %s", logs.String())
	}
}

func TestReconcilerSecretResolutionIsRootNamedAndFailClosed(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	reposDir := t.TempDir()
	providersDir := t.TempDir()
	writeRepoFile(t, reposDir, "lab.yaml", validRepositoryYAML("octo-org", "lab-widget"))
	secretName := "GITHUB_APP_RECONCILER_PEM"
	writeRepoFile(t, providersDir, "github.yaml", reconcilerProviderYAML(secretName))
	writeGrantAgent(t, agentsDir, "worker", programmerIdentity, "lab", gitRead, map[string]string{"contents": "read", "metadata": "read"})
	key := testAppKey(t)
	secrets := openTestSecretStore(t)
	writeSecretPEM(t, secrets.root, secretName, key)
	state := &privilegedState{
		secrets:      secrets,
		agentsDir:    agentsDir,
		rulesDir:     rulesDir,
		reposDir:     reposDir,
		providersDir: providersDir,
	}
	request := grantRegistration{
		Agent: "worker", Repository: "lab", Identity: programmerIdentity, Git: gitRead,
		Permissions: map[string]string{"contents": "read", "metadata": "read"},
	}
	binding, err := state.resolveReconcilerApp(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Org != "octo-org" || binding.Repo != "lab-widget" || binding.AppID != "100001" || binding.InstallationID != "100002" {
		t.Fatalf("binding = %#v", binding)
	}
	if binding.Key == nil || binding.Key.N.Cmp(key.N) != 0 {
		t.Fatal("resolved key did not match the reconciler secret")
	}
	writeRepoFile(t, providersDir, "other.yaml", `provider: github
org: evil-org
identities:
  - name: outsider
    role: programmer
    credential: app
    secret: EVIL_APP_PRIVATE_KEY_PEM
  - name: evil-bot
    role: reconciler
    credential: app
    secret: EVIL_APP_RECONCILER_PEM
    app_id: "300001"
    installation_id: "300002"
`)
	evilKey := testAppKey(t)
	writeSecretPEM(t, secrets.root, "EVIL_APP_RECONCILER_PEM", evilKey)
	cross := request
	cross.Identity = "outsider"
	if _, err := state.resolveReconcilerApp(context.Background(), cross); err == nil || strings.Contains(err.Error(), "EVIL") || strings.Contains(err.Error(), secretName) {
		t.Fatalf("cross-org identity error = %v", err)
	}
	wrongGit := request
	wrongGit.Git = gitWrite
	wrongGit.Permissions = map[string]string{"contents": "write", "metadata": "read"}
	if _, err := state.resolveReconcilerApp(context.Background(), wrongGit); err == nil {
		t.Fatal("undeclared git write was accepted")
	}
	if _, err := state.resolveReconcilerApp(context.Background(), grantRegistration{Repository: "lab"}); err == nil {
		t.Fatal("missing agent identity was accepted")
	}
	again, err := state.resolveReconcilerApp(context.Background(), request)
	if err != nil || again.Key == nil || again.Key.N.Cmp(key.N) != 0 || again.Key.N.Cmp(evilKey.N) == 0 {
		t.Fatalf("reconciler key changed after the cross-org probe: %v", err)
	}
	if err := os.Remove(filepath.Join(secrets.root, secretName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(secrets.root, secretName)); err != nil {
		t.Fatal(err)
	}
	_, err = state.resolveReconcilerApp(context.Background(), grantRegistration{Repository: "lab"})
	if err == nil || strings.Contains(err.Error(), secretName) || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("symlink error = %v", err)
	}
	if _, err := secrets.read("../" + secretName); err == nil || strings.Contains(err.Error(), "..") {
		t.Fatalf("traversal error = %v", err)
	}
}

func TestSecretStoreRejectsLooseFilesAndOverlap(t *testing.T) {
	root := t.TempDir()
	loose := filepath.Join(root, "secrets")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loose, "GITHUB_APP_RECONCILER_PEM"), []byte("pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openSecretStore(loose, secretBounds{}); err == nil || strings.Contains(err.Error(), "GITHUB_APP") {
		t.Fatalf("loose file error = %v", err)
	}
	store := openTestSecretStore(t)
	if err := os.WriteFile(filepath.Join(store.root, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read("GITHUB_APP_RECONCILER_PEM"); err == nil || strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("unexpected entry error = %v", err)
	}
	credentials := openTestCredentialStore(t)
	if _, err := openSecretStore(credentials.root, secretBounds{CredentialsDir: credentials.root}); err == nil {
		t.Fatal("secret store accepted the credential directory")
	}
	if _, err := openSecretStore("relative/secrets", secretBounds{}); err == nil {
		t.Fatal("relative secret store accepted")
	}
}

func TestReconcilerAppIDsAreRequiredOnlyThere(t *testing.T) {
	agentsDir := t.TempDir()
	rulesDir := t.TempDir()
	reposDir := t.TempDir()
	providersDir := t.TempDir()
	without := strings.ReplaceAll(validProviderYAML(), "\n    app_id: \"910000000000000001\"\n    installation_id: \"910000000000000002\"", "")
	writeRepoFile(t, providersDir, "github.yaml", without)
	if _, _, err := loadProviders(providersDir, agentsDir, rulesDir, reposDir); err == nil || strings.Contains(err.Error(), providerSecretRef) {
		t.Fatalf("missing app id error = %v", err)
	}
	withProgrammer := strings.Replace(validProviderYAML(), "secret: "+programmerSecret, "secret: "+programmerSecret+"\n    app_id: \"5\"", 1)
	writeRepoFile(t, providersDir, "github.yaml", withProgrammer)
	if _, _, err := loadProviders(providersDir, agentsDir, rulesDir, reposDir); err == nil || !strings.Contains(err.Error(), "reconciler") {
		t.Fatalf("programmer app id error = %v", err)
	}
}

func TestListenerArgsDoNotCarrySecretStores(t *testing.T) {
	args := strings.Join(listenerArgs("127.0.0.1:9", "/agents", "/rules", "/repos", "/providers", "/data"), " ")
	if strings.Contains(args, "-secrets") || strings.Contains(args, "-credentials") || strings.Contains(args, "genesis-secrets") {
		t.Fatalf("listener args = %s", args)
	}
}

func TestNextKeyPageRejectsForeignHosts(t *testing.T) {
	origin, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/octo-org/lab-widget/keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	header.Set("Link", `<https://evil.example/repos/octo-org/lab-widget/keys?page=2>; rel="next"`)
	if _, err := nextKeyPage(origin.URL, header, "octo-org", "lab-widget"); err == nil {
		t.Fatal("foreign link was accepted")
	}
	header.Set("Link", `<https://api.github.com/repos/octo-org/lab-widget/keys?page=2&access_token=ghs_secret>; rel="next"`)
	if _, err := nextKeyPage(origin.URL, header, "octo-org", "lab-widget"); err == nil {
		t.Fatal("token query was accepted")
	}
	header.Set("Link", `</repos/octo-org/lab-widget/keys?page=2>; rel="next", <//evil.example/repos/octo-org/lab-widget/keys?page=3>; rel="next"`)
	if _, err := nextKeyPage(origin.URL, header, "octo-org", "lab-widget"); err == nil {
		t.Fatal("protocol-relative link was accepted")
	}
}

func TestSSHCheckScriptIsGuarded(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "scripts", "wsl-docker-ssh-check.sh")
	textBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(textBytes)
	for _, want := range []string{"GITHUB_TOKEN", "GH_TOKEN", "ssh -V", "65532", "chmod 0700", "/mnt/"} {
		if !strings.Contains(text, want) {
			t.Fatalf("ssh check missing %q", want)
		}
	}
	for _, forbidden := range []string{"git@github.com", "down -v", "curl "} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("ssh check contains %q", forbidden)
		}
	}
	if !strings.Contains(text, "grep 'OpenSSH_'") {
		t.Fatal("ssh check does not print the OpenSSH version")
	}
	docBytes, err := os.ReadFile(filepath.Join(root, "docs", "ssh-client-verification.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(docBytes)
	for _, want := range []string{
		"No-credential smoke",
		"Administration: Read and write",
		"Metadata: Read-only",
		"GITHUB_APP_RECONCILER_PEM",
		"credential_active",
		"remote_key_id",
		"SSH_AUTH_SOCK",
		"git: none",
		"refused",
		"Do not print",
		"ssh client check passed",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("verification checklist missing %q", want)
		}
	}
	if strings.Contains(doc, "-----BEGIN") {
		t.Fatal("verification checklist contains a PEM block")
	}
}

func TestVerificationChecklistYAMLLoads(t *testing.T) {
	root := repoRoot(t)
	docBytes, err := os.ReadFile(filepath.Join(root, "docs", "ssh-client-verification.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(docBytes)
	provider := checklistTemplate(t, doc, "Path(\"/tmp/genesis-probe-provider.yaml\").write_text(f\"\"\"", "\"\"\")")
	repository := checklistTemplate(t, doc, "Path(\"/tmp/genesis-probe-repo.yaml\").write_text(f\"\"\"", "\"\"\")")
	replacer := strings.NewReplacer("{org}", "octo-org", "{repo}", "genesis-deploy-probe", "{app}", "100001", "{inst}", "100002")
	provider = replacer.Replace(provider)
	repository = replacer.Replace(repository)
	agents := checklistAgents(t, doc)
	if len(agents) != 3 {
		t.Fatalf("agent samples = %d", len(agents))
	}
	rulesSource, err := os.ReadFile(filepath.Join(root, "rules.d", "example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{agents[0], agents[1]} {
		dir := t.TempDir()
		agentsDir := filepath.Join(dir, "agents")
		rulesDir := filepath.Join(dir, "rules")
		reposDir := filepath.Join(dir, "repos")
		providersDir := filepath.Join(dir, "providers")
		for _, path := range []string{agentsDir, rulesDir, reposDir, providersDir} {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(agentsDir, "workspace-janitor.yaml"), []byte(agent), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rulesDir, "example.yaml"), rulesSource, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(reposDir, "probe.yaml"), []byte(repository), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(providersDir, "probe.yaml"), []byte(provider), 0o640); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadGeneration(agentsDir, rulesDir, reposDir, providersDir)
		if err != nil {
			t.Fatalf("checklist YAML: %v\nprovider:\n%s\nrepo:\n%s\nagent:\n%s", err, provider, repository, agent)
		}
		if !loaded.providersActive || !loaded.reposActive {
			t.Fatalf("active providers=%v repos=%v", loaded.providersActive, loaded.reposActive)
		}
		janitor := loaded.agents["workspace-janitor"]
		if janitor.GitHub == nil || janitor.GitHub.Repository != "probe" {
			t.Fatalf("grant = %#v", janitor.GitHub)
		}
	}
	if !strings.Contains(agents[0], "git: write") || !strings.Contains(agents[1], "git: none") || agents[0] != agents[2] {
		t.Fatal("write, none, and restored agent samples drifted")
	}
}

func checklistTemplate(t *testing.T, doc, start, end string) string {
	t.Helper()
	_, after, ok := strings.Cut(doc, start)
	if !ok {
		t.Fatalf("checklist missing %s", start)
	}
	body, _, ok := strings.Cut(after, end)
	if !ok || strings.TrimSpace(body) == "" {
		t.Fatal("checklist template was empty")
	}
	return body
}

func checklistAgents(t *testing.T, doc string) []string {
	t.Helper()
	const marker = "cat > /var/lib/genesis/config/agents.d/workspace-janitor.yaml' <<'EOF'\n"
	var agents []string
	rest := doc
	for {
		_, after, ok := strings.Cut(rest, marker)
		if !ok {
			break
		}
		body, tail, ok := strings.Cut(after, "\nEOF")
		if !ok {
			t.Fatal("agent sample was not closed")
		}
		agents = append(agents, body+"\n")
		rest = tail
	}
	return agents
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type gitHubFake struct {
	t                 *testing.T
	server            *httptest.Server
	key               *rsa.PrivateKey
	now               time.Time
	mu                sync.Mutex
	keys              []gitHubDeployKey
	tokenCalls        int
	discoveryPosts    int
	keyTokenPosts     int
	repoCalls         int
	listCalls         int
	postCalls         int
	deleteCalls       int
	lastTitle         string
	lastKey           string
	lastReadOnly      bool
	authFails         int
	repoStatus        int
	repoFailOnce      bool
	listAfter         string
	listFailOnce      bool
	listFailed        bool
	postStatus        int
	adoptOnPost       bool
	emptyCreate       bool
	selection         string
	pageTwo           bool
	pemMarker         string
	owner             string
	repoName          string
	fullName          string
	tokenID           int64
	tokenName         string
	tokenFullName     string
	omitTokenFullName bool
	omitRepoFullName  bool
	extraPermission   string
	expiresSoon       bool
	leakBody          bool
}

func newGitHubFake(t *testing.T, now time.Time) *gitHubFake {
	t.Helper()
	fake := &gitHubFake{t: t, key: testAppKey(t), now: now, selection: "selected", pemMarker: "BEGIN RSA PRIVATE KEY"}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *gitHubFake) registrar() githubRegistrar {
	return githubRegistrar{
		baseURL:   f.server.URL,
		transport: f.server.Client().Transport,
		now:       func() time.Time { return f.now },
		resolve: func(context.Context, grantRegistration) (githubAppBinding, error) {
			return githubAppBinding{
				Org: "octo-org", Repo: "lab-widget", AppID: "100001", InstallationID: "100002", Key: f.key,
			}, nil
		},
	}
}

func (f *gitHubFake) preload(id int64, title, public string, readOnly bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, gitHubDeployKey{ID: id, Title: title, Key: public, ReadOnly: githubBool(readOnly)})
}

func (f *gitHubFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodDelete {
		f.deleteCalls++
		http.Error(w, "delete unsupported", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case r.URL.Path == "/app/installations/100002/access_tokens" && r.Method == http.MethodPost:
		f.serveToken(w, r)
	case r.URL.Path == "/repos/octo-org/lab-widget" && r.Method == http.MethodGet:
		f.serveRepo(w, r)
	case r.URL.Path == "/repos/octo-org/lab-widget/keys" && r.Method == http.MethodGet:
		f.serveKeys(w, r)
	case r.URL.Path == "/repos/octo-org/lab-widget/keys" && r.Method == http.MethodPost:
		f.serveCreate(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *gitHubFake) serveToken(w http.ResponseWriter, r *http.Request) {
	f.tokenCalls++
	f.verifyJWT(r.Header.Get("Authorization"))
	if f.authFails > 0 {
		f.authFails--
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"bad credentials"}`))
		return
	}
	var request gitHubTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		f.t.Errorf("token body: %v", err)
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	token := "ghs_test_discovery_token_value"
	switch {
	case len(request.Repositories) == 1 && request.Repositories[0] == "lab-widget" && request.Permissions.Metadata == "read" && request.Permissions.Administration == "":
		f.discoveryPosts++
	case len(request.RepositoryIDs) == 1 && request.RepositoryIDs[0] == 4242 && request.Permissions.Administration == "write":
		f.keyTokenPosts++
		token = "ghs_test_repository_token_value"
	default:
		f.t.Errorf("unexpected token request")
		http.Error(w, "bad scope", http.StatusBadRequest)
		return
	}
	name := f.tokenName
	if name == "" {
		name = "lab-widget"
	}
	full := f.tokenFullName
	if full == "" {
		full = "octo-org/" + name
	}
	if f.omitTokenFullName {
		full = ""
	}
	id := int64(4242)
	if f.tokenID != 0 {
		id = f.tokenID
	}
	repos := []gitHubRepoRef{{ID: id, Name: name, FullName: full}}
	if f.selection == "all" {
		repos = nil
	}
	permissions := map[string]string{"metadata": "read"}
	if token != "ghs_test_discovery_token_value" {
		permissions["administration"] = "write"
	}
	if f.extraPermission != "" {
		permissions[f.extraPermission] = "write"
	}
	expires := f.now.Add(time.Hour)
	if f.expiresSoon {
		expires = f.now.Add(10 * time.Second)
	}
	_ = json.NewEncoder(w).Encode(gitHubTokenResponse{
		Token:               token,
		ExpiresAt:           expires,
		Permissions:         permissions,
		RepositorySelection: f.selection,
		Repositories:        repos,
	})
}

func (f *gitHubFake) serveRepo(w http.ResponseWriter, r *http.Request) {
	f.repoCalls++
	if f.repoFailOnce {
		f.repoFailOnce = false
		w.WriteHeader(http.StatusInternalServerError)
		if f.leakBody {
			_, _ = w.Write([]byte(`{"token":"ghs_leak_token_value","pem":"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----","jwt":"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"}`))
		}
		return
	}
	if r.Header.Get("Authorization") != "Bearer ghs_test_discovery_token_value" {
		f.t.Errorf("repo token was not the discovery token")
	}
	if f.repoStatus != 0 {
		w.WriteHeader(f.repoStatus)
		if f.leakBody {
			_, _ = w.Write([]byte(`{"token":"ghs_leak_token_value","pem":"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----","jwt":"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"}`))
		}
		return
	}
	name := f.repoName
	if name == "" {
		name = "lab-widget"
	}
	owner := f.owner
	if owner == "" {
		owner = "octo-org"
	}
	full := f.fullName
	if full == "" {
		full = owner + "/" + name
	}
	if f.omitRepoFullName {
		full = ""
	}
	_ = json.NewEncoder(w).Encode(gitHubRepo{ID: 4242, Name: name, FullName: full, Owner: struct {
		Login string `json:"login"`
	}{Login: owner}})
}

func (f *gitHubFake) serveKeys(w http.ResponseWriter, r *http.Request) {
	f.listCalls++
	if r.Header.Get("Authorization") != "Bearer ghs_test_repository_token_value" {
		f.t.Errorf("key list used the wrong token")
	}
	if f.listFailOnce && !f.listFailed {
		f.listFailed = true
		if f.listAfter != "" {
			w.Header().Set("Retry-After", f.listAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	keys := f.keys
	if f.pageTwo && r.URL.Query().Get("page") == "" {
		w.Header().Set("Link", "<"+f.server.URL+"/repos/octo-org/lab-widget/keys?per_page=100&page=2>; rel=\"next\"")
		_ = json.NewEncoder(w).Encode([]gitHubDeployKey{})
		return
	}
	_ = json.NewEncoder(w).Encode(keys)
}

func (f *gitHubFake) serveCreate(w http.ResponseWriter, r *http.Request) {
	f.postCalls++
	if r.Header.Get("Authorization") != "Bearer ghs_test_repository_token_value" {
		f.t.Errorf("create used the wrong token")
	}
	var body struct {
		Title    string `json:"title"`
		Key      string `json:"key"`
		ReadOnly bool   `json:"read_only"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("create body: %v", err)
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	f.lastTitle = body.Title
	f.lastKey = body.Key
	f.lastReadOnly = body.ReadOnly
	if strings.Contains(body.Key, "PRIVATE KEY") {
		f.t.Errorf("create received private material")
	}
	created := gitHubDeployKey{ID: 77, Title: body.Title, Key: body.Key, ReadOnly: githubBool(body.ReadOnly)}
	if f.adoptOnPost {
		f.keys = append(f.keys, created)
	}
	if f.postStatus != 0 {
		w.WriteHeader(f.postStatus)
		_, _ = w.Write([]byte(`{"message":"Validation Failed"}`))
		return
	}
	if f.emptyCreate {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":77}`))
		return
	}
	f.keys = append(f.keys, created)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

func (f *gitHubFake) verifyJWT(header string) {
	f.t.Helper()
	raw := strings.TrimPrefix(header, "Bearer ")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		f.t.Errorf("jwt was not sent")
		return
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		f.t.Errorf("jwt signature: %v", err)
		return
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
		f.t.Errorf("jwt verify: %v", err)
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		f.t.Errorf("jwt payload: %v", err)
		return
	}
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		f.t.Errorf("jwt claims: %v", err)
		return
	}
	if claims.Iss != "100001" || claims.Iat != f.now.Add(-time.Minute).Unix() || claims.Exp != f.now.Add(8*time.Minute).Unix() {
		f.t.Errorf("jwt claims were not short-lived app claims")
	}
	if bytes.Contains(payload, []byte("PRIVATE")) || bytes.Contains(payload, []byte(f.pemMarker)) {
		f.t.Errorf("jwt payload contained key material")
	}
}

func testRSAPublic(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))
}

func testDeployPublic(t *testing.T) (string, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), ssh.FingerprintSHA256(signer.PublicKey())
}

func testAppKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func openTestSecretStore(t *testing.T) *secretStore {
	t.Helper()
	root := t.TempDir()
	store, err := openSecretStore(filepath.Join(root, "secrets"), secretBounds{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.dir.Close() })
	return store
}

func writeSecretPEM(t *testing.T, root, name string, key *rsa.PrivateKey) {
	t.Helper()
	payload := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(root, name), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func reconcilerProviderYAML(secret string) string {
	return `provider: github
org: octo-org
identities:
  - name: reconciler
    role: reconciler
    credential: app
    secret: ` + secret + `
    app_id: "100001"
    installation_id: "100002"
  - name: ` + programmerIdentity + `
    role: programmer
    credential: app
    secret: ` + programmerSecret + `
`
}

func githubBool(value bool) *bool {
	return &value
}

func TestEntrypointSecretDirectoryStaysPrivate(t *testing.T) {
	root := repoRoot(t)
	config := t.TempDir()
	data := t.TempDir()
	defaults := t.TempDir()
	credentials := t.TempDir()
	secrets := filepath.Join(t.TempDir(), "secrets")
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
			"GENESIS_CREDENTIALS_DIR="+credentials,
			"GENESIS_SECRETS_DIR="+path,
		)
		return cmd.CombinedOutput()
	}
	output, err := run(secrets, "own-config")
	if err != nil {
		t.Fatalf("own-config: %v\n%s", err, output)
	}
	info, err := os.Stat(secrets)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("secrets mode = %o", info.Mode().Perm())
	}
	if err := os.Symlink(outside, filepath.Join(secrets, "alias")); err != nil {
		t.Fatal(err)
	}
	output, err = run(secrets, "own-config")
	if err != nil {
		t.Fatalf("own-config with inner symlink: %v\n%s", err, output)
	}
	outsideInfo, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if outsideInfo.Mode().Perm() != 0o644 {
		t.Fatalf("secrets lock followed a symlink: %o", outsideInfo.Mode().Perm())
	}
	output, err = run(filepath.Join(config, "secrets"), "own-config")
	if err == nil || !bytes.Contains(output, []byte("outside config")) {
		t.Fatalf("config secrets error = %v\n%s", err, output)
	}
}
