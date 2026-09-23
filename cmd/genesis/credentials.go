package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const (
	defaultCredentialsDir   = "genesis-credentials"
	credentialDirMode       = 0o700
	credentialFileMode      = 0o600
	credentialSocketDirMode = 0o711
	keyMaterialLocal        = "local"
	remoteStatusReady       = "ready"
	remoteStatusPending     = "pending"
	remoteStatusRefused     = "refused"
	grantGenerationInitial  = 1
	credentialsDockerPath   = "/var/lib/genesis/credentials"
	privateKeyFileName      = "id_ed25519"
	grantStateFileName      = "state.json"
	grantComment            = "genesis"
)

var (
	grantIDPattern = regexp.MustCompile(`^g[0-9a-f]{32}$`)

	errUnknownRegistrationStatus = errors.New("remote registration status is not supported")
	errGrantRotationRefused      = errors.New("refusing to rotate or replace grant material")
)

// credentialBounds are trees the root credential store must not overlap.
// Agent homes under /home and the designer config root are always included.
type credentialBounds struct {
	AgentsDir    string
	RulesDir     string
	ReposDir     string
	ProvidersDir string
	DataDir      string
	ConfigRoot   string
	SecretsDir   string
}

type credentialStore struct {
	root   string
	dir    *os.File
	bounds credentialBounds
}

type grantRecord struct {
	GrantID      string            `json:"grant_id"`
	Agent        string            `json:"agent"`
	Repository   string            `json:"repository"`
	Identity     string            `json:"identity"`
	Git          string            `json:"git"`
	Permissions  map[string]string `json:"permissions,omitempty"`
	Fingerprint  string            `json:"fingerprint,omitempty"`
	Generation   int               `json:"generation"`
	RemoteKeyID  string            `json:"remote_key_id,omitempty"`
	RemoteStatus string            `json:"remote_status"`
	KeyMaterial  string            `json:"key_material"`
	Credential   string            `json:"credential"`
}

// grantObservation is the non-secret view of a grant. It is safe to return
// across the coordinator socket and in status JSON. It has no identity name,
// secret reference, public key body, or private key.
type grantObservation struct {
	GrantID      string `json:"grant_id"`
	Agent        string `json:"agent"`
	Repository   string `json:"repository"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	Generation   int    `json:"generation,omitempty"`
	RemoteKeyID  string `json:"remote_key_id,omitempty"`
	RemoteStatus string `json:"remote_status"`
	KeyMaterial  string `json:"key_material"`
}

type heldGrant struct {
	ID    string
	Ready bool
}

type grantDecision struct {
	Applied     string
	Unsupported *unsupportedChange
	Observation *grantObservation
	GrantID     string
}

func openCredentialStore(root string, bounds credentialBounds) (*credentialStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("credential store path is required")
	}
	cleaned := filepath.Clean(root)
	if !filepath.IsAbs(cleaned) {
		return nil, errors.New("credential store must be absolute")
	}
	if err := rejectSymlinkPath(cleaned); err != nil {
		return nil, fmt.Errorf("credential store: %w", err)
	}
	if err := os.MkdirAll(cleaned, credentialDirMode); err != nil {
		return nil, err
	}
	if err := rejectSymlinkPath(cleaned); err != nil {
		return nil, fmt.Errorf("credential store: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(resolved) != cleaned {
		return nil, errors.New("credential store path resolves through a symlink")
	}
	store := &credentialStore{root: cleaned, bounds: bounds}
	if err := store.rejectOverlap(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(cleaned)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("credential store is not a real directory")
	}
	uid, err := fileUID(info)
	if err != nil {
		return nil, err
	}
	if uid != os.Geteuid() {
		return nil, fmt.Errorf("credential store owner %d is not the coordinator uid %d", uid, os.Geteuid())
	}
	if !acceptableRootPerm(info.Mode().Perm()) {
		return nil, fmt.Errorf("credential store permissions are %o", info.Mode().Perm())
	}
	if err := rejectStoreParent(cleaned); err != nil {
		return nil, err
	}
	if err := os.Chmod(cleaned, storeRootMode(os.Geteuid())); err != nil {
		return nil, err
	}
	if err := store.pin(); err != nil {
		return nil, err
	}
	socketMode := os.FileMode(credentialDirMode)
	if os.Geteuid() == 0 {
		socketMode = credentialSocketDirMode
	}
	for _, sub := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "grants", mode: credentialDirMode},
		{name: "locks", mode: credentialDirMode},
		{name: "sockets", mode: socketMode},
	} {
		if err := store.mkdirRel(sub.name, sub.mode); err != nil {
			_ = store.dir.Close()
			return nil, err
		}
	}
	if err := store.sweepSockets(); err != nil {
		_ = store.dir.Close()
		return nil, err
	}
	return store, nil
}

func (s *credentialStore) rejectOverlap() error {
	forbidden := []string{
		"/home",
		"/root",
		designerWritableConfigRoot,
	}
	if s.bounds.ConfigRoot != "" {
		forbidden = append(forbidden, s.bounds.ConfigRoot)
	}
	for _, path := range []string{s.bounds.AgentsDir, s.bounds.RulesDir, s.bounds.ReposDir, s.bounds.ProvidersDir, s.bounds.DataDir, s.bounds.SecretsDir} {
		if strings.TrimSpace(path) != "" {
			forbidden = append(forbidden, path)
		}
	}
	if s.root != credentialsDockerPath && (pathWithin(s.root, credentialsDockerPath) || pathWithin(credentialsDockerPath, s.root)) {
		return fmt.Errorf("credential store %s overlaps %s", s.root, credentialsDockerPath)
	}
	for _, path := range forbidden {
		cleaned := filepath.Clean(path)
		if cleaned == "." || cleaned == "" {
			continue
		}
		if pathWithin(s.root, cleaned) || pathWithin(cleaned, s.root) {
			return fmt.Errorf("credential store %s overlaps %s", s.root, cleaned)
		}
		resolved, err := filepath.EvalSymlinks(cleaned)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if pathWithin(s.root, resolved) || pathWithin(resolved, s.root) {
			return fmt.Errorf("credential store %s overlaps %s", s.root, resolved)
		}
	}
	return nil
}

func (s *credentialStore) sweepSockets() error {
	entries, err := s.readDir("sockets")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." || strings.Contains(name, "/") || strings.Contains(name, "..") {
			return fmt.Errorf("unexpected socket entry %s", name)
		}
		if err := s.removeTree("sockets/" + name); err != nil {
			return err
		}
	}
	return nil
}

func fileUID(info os.FileInfo) (int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, errors.New("file owner is unavailable")
	}
	return int(stat.Uid), nil
}

func stableGrantID(agent, repository, identity, gitAccess string, permissions map[string]string) (string, error) {
	if agent == "" || repository == "" || identity == "" {
		return "", errors.New("grant identity is incomplete")
	}
	switch gitAccess {
	case gitNone, gitRead, gitWrite:
	default:
		return "", errors.New("git access must be none, read, or write")
	}
	sum := sha256.New()
	write := func(part string) {
		_, _ = io.WriteString(sum, part)
		_, _ = sum.Write([]byte{'\n'})
	}
	write("genesis-grant-v1")
	write(agent)
	write(repository)
	write(identity)
	write(gitAccess)
	names := make([]string, 0, len(permissions))
	for name := range permissions {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		write(name + "=" + permissions[name])
	}
	return "g" + hex.EncodeToString(sum.Sum(nil)[:16]), nil
}

func (s *credentialStore) withGrantLock(id string, fn func() error) error {
	if err := s.confirmBound(); err != nil {
		return err
	}
	if !grantIDPattern.MatchString(id) {
		return errors.New("grant id is invalid")
	}
	parent, err := s.openDir("locks")
	if err != nil {
		return err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), id+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, credentialFileMode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), id+".lock")
	defer file.Close()
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("grant lock is not a regular file")
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("grant lock owner %d is not the coordinator uid %d", st.Uid, os.Geteuid())
	}
	if os.FileMode(st.Mode).Perm() != credentialFileMode {
		if err := unix.Fchmod(fd, credentialFileMode); err != nil {
			return err
		}
	}
	return fn()
}

func (s *credentialStore) retained(active map[string]struct{}) ([]string, error) {
	if err := s.confirmBound(); err != nil {
		return nil, err
	}
	entries, err := s.readDir("grants")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." || strings.Contains(name, "/") || strings.Contains(name, "..") {
			return nil, fmt.Errorf("unexpected grant entry %s", name)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("grant path %s is a symlink", name)
		}
		if !grantIDPattern.MatchString(name) || !entry.IsDir() {
			return nil, fmt.Errorf("unexpected grant entry %s", name)
		}
		if _, ok := active[name]; ok {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

func (s *credentialStore) ensureCredential(intent privilegedIntent) (grantRecord, error) {
	id, err := grantIDFromIntent(intent)
	if err != nil {
		return grantRecord{}, err
	}
	var record grantRecord
	err = s.withGrantLock(id, func() error {
		switch {
		case intent.Credential == credentialNone:
			record, err = s.writeNoKey(intent, id, keyMaterialNone, remoteRegistrationNone, 0)
		case intent.Git == gitRead || intent.Git == gitWrite:
			record, err = s.ensureLocalKey(intent, id)
		default:
			record, err = s.writeNoKey(intent, id, keyMaterialPending, remoteRegistrationUnsupported, 0)
		}
		return err
	})
	return record, err
}

func (s *credentialStore) ensureRegistration(ctx context.Context, intent privilegedIntent, registrar grantRegistrar) (grantRecord, error) {
	id, err := grantIDFromIntent(intent)
	if err != nil {
		return grantRecord{}, err
	}
	var record grantRecord
	err = s.withGrantLock(id, func() error {
		current, err := s.readRecord(id)
		if err != nil {
			return err
		}
		if current.KeyMaterial != keyMaterialLocal || intent.Credential == credentialNone || intent.Git == gitNone {
			record = current
			return nil
		}
		if current.Generation != grantGenerationInitial {
			return s.refuseReady(current, errGrantRotationRefused)
		}
		if registrar == nil {
			registrar = githubRegistrar{}
		}
		private, err := s.readPrivateKey(id, current.Fingerprint)
		if err != nil {
			return s.refuseReady(current, err)
		}
		signer, err := ssh.NewSignerFromKey(private)
		if err != nil {
			return s.refuseReady(current, err)
		}
		public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
		result, err := registrar.Register(ctx, grantRegistration{
			GrantID:     id,
			Agent:       intent.Agent,
			Repository:  intent.Repository,
			Fingerprint: current.Fingerprint,
			PublicKey:   public,
			Git:         intent.Git,
			RemoteKeyID: current.RemoteKeyID,
		})
		if err != nil {
			return s.refuseReady(current, err)
		}
		switch result.Status {
		case remoteRegistrationUnsupported, remoteRegistrationNone, remoteStatusReady, remoteStatusPending, remoteStatusRefused:
		default:
			return s.refuseReady(current, errUnknownRegistrationStatus)
		}
		if result.Status == remoteStatusReady && result.RemoteKeyID == "" {
			return s.refuseReady(current, errors.New("ready registration did not return a remote key id"))
		}
		current.RemoteStatus = result.Status
		if result.Status == remoteStatusReady {
			current.RemoteKeyID = result.RemoteKeyID
		}
		if err := s.saveRecord(current); err != nil {
			return err
		}
		record = current
		return nil
	})
	return record, err
}

func (s *credentialStore) refuseReady(current grantRecord, cause error) error {
	if current.RemoteStatus != remoteStatusReady {
		return cause
	}
	current.RemoteStatus = remoteStatusRefused
	if err := s.saveRecord(current); err != nil {
		return err
	}
	return cause
}

func (s *credentialStore) ensureLocalKey(intent privilegedIntent, id string) (grantRecord, error) {
	if !grantIDPattern.MatchString(id) {
		return grantRecord{}, errors.New("grant id is invalid")
	}
	if err := s.mkdirRel("grants/"+id, credentialDirMode); err != nil {
		return grantRecord{}, err
	}
	keyRel, err := grantFileRel(id, privateKeyFileName)
	if err != nil {
		return grantRecord{}, err
	}
	info, err := s.statRel(keyRel)
	if err == nil {
		if info.Mode&unix.S_IFMT != unix.S_IFREG {
			return grantRecord{}, errors.New("grant key is not a regular file")
		}
		if err := privateRegular(info); err != nil {
			return grantRecord{}, err
		}
		existing, err := s.readRecord(id)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return grantRecord{}, err
			}
			private, err := s.readPrivateKey(id, "")
			if err != nil {
				return grantRecord{}, err
			}
			fingerprint, err := fingerprintOf(private)
			if err != nil {
				return grantRecord{}, err
			}
			record := recordFromIntent(intent, id, keyMaterialLocal, remoteRegistrationUnsupported, grantGenerationInitial, fingerprint)
			if err := s.saveRecord(record); err != nil {
				return grantRecord{}, err
			}
			return record, nil
		}
		if existing.Generation != grantGenerationInitial || existing.KeyMaterial != keyMaterialLocal {
			return grantRecord{}, errGrantRotationRefused
		}
		if _, err := s.readPrivateKey(id, existing.Fingerprint); err != nil {
			return grantRecord{}, err
		}
		if existing.GrantID != id || existing.Agent != intent.Agent || existing.Repository != intent.Repository || existing.Identity != intent.Identity || existing.Git != intent.Git || existing.Credential != intent.Credential {
			return grantRecord{}, errGrantRotationRefused
		}
		return existing, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return grantRecord{}, err
	}
	if _, err := s.readRecord(id); err == nil {
		return grantRecord{}, errGrantRotationRefused
	} else if !errors.Is(err, os.ErrNotExist) {
		return grantRecord{}, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return grantRecord{}, err
	}
	block, err := ssh.MarshalPrivateKey(private, grantComment)
	if err != nil {
		return grantRecord{}, err
	}
	encoded := pem.EncodeToMemory(block)
	if err := s.createRel(keyRel, encoded); err != nil {
		return grantRecord{}, err
	}
	fingerprint, err := fingerprintOf(private)
	if err != nil {
		return grantRecord{}, err
	}
	record := recordFromIntent(intent, id, keyMaterialLocal, remoteRegistrationUnsupported, grantGenerationInitial, fingerprint)
	if err := s.saveRecord(record); err != nil {
		return grantRecord{}, err
	}
	return record, nil
}

func (s *credentialStore) writeNoKey(intent privilegedIntent, id, material, remote string, generation int) (grantRecord, error) {
	if !grantIDPattern.MatchString(id) {
		return grantRecord{}, errors.New("grant id is invalid")
	}
	if err := s.mkdirRel("grants/"+id, credentialDirMode); err != nil {
		return grantRecord{}, err
	}
	keyRel, err := grantFileRel(id, privateKeyFileName)
	if err != nil {
		return grantRecord{}, err
	}
	if _, err := s.statRel(keyRel); err == nil {
		return grantRecord{}, errGrantRotationRefused
	} else if !errors.Is(err, os.ErrNotExist) {
		return grantRecord{}, err
	}
	if existing, err := s.readRecord(id); err == nil {
		if existing.KeyMaterial != material || existing.Generation != generation {
			return grantRecord{}, errGrantRotationRefused
		}
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return grantRecord{}, err
	}
	record := recordFromIntent(intent, id, material, remote, generation, "")
	if err := s.saveRecord(record); err != nil {
		return grantRecord{}, err
	}
	return record, nil
}

func recordFromIntent(intent privilegedIntent, id, material, remote string, generation int, fingerprint string) grantRecord {
	return grantRecord{
		GrantID:      id,
		Agent:        intent.Agent,
		Repository:   intent.Repository,
		Identity:     intent.Identity,
		Git:          intent.Git,
		Permissions:  clonePermissions(intent.Permissions),
		Fingerprint:  fingerprint,
		Generation:   generation,
		RemoteStatus: remote,
		KeyMaterial:  material,
		Credential:   intent.Credential,
	}
}

func clonePermissions(permissions map[string]string) map[string]string {
	if len(permissions) == 0 {
		return nil
	}
	return mapsClone(permissions)
}

func mapsClone(permissions map[string]string) map[string]string {
	out := make(map[string]string, len(permissions))
	for key, value := range permissions {
		out[key] = value
	}
	return out
}

func (s *credentialStore) readRecord(id string) (grantRecord, error) {
	rel, err := grantFileRel(id, grantStateFileName)
	if err != nil {
		return grantRecord{}, err
	}
	payload, err := s.readRel(rel)
	if err != nil {
		return grantRecord{}, err
	}
	if bytes.Contains(payload, []byte("PRIVATE KEY")) || bytes.Contains(payload, []byte("BEGIN ")) {
		return grantRecord{}, errors.New("grant state contains key material")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var record grantRecord
	if err := decoder.Decode(&record); err != nil {
		return grantRecord{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return grantRecord{}, errors.New("grant state must contain one JSON object")
		}
		return grantRecord{}, err
	}
	if record.GrantID != id {
		return grantRecord{}, errGrantRotationRefused
	}
	recomputed, err := stableGrantID(record.Agent, record.Repository, record.Identity, record.Git, record.Permissions)
	if err != nil || recomputed != id {
		return grantRecord{}, errGrantRotationRefused
	}
	return record, nil
}

func (s *credentialStore) saveRecord(record grantRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if bytes.Contains(payload, []byte("PRIVATE KEY")) {
		return errors.New("refusing to persist key material in grant state")
	}
	rel, err := grantFileRel(record.GrantID, grantStateFileName)
	if err != nil {
		return err
	}
	return s.writeAtomicRel(rel, payload)
}

func (s *credentialStore) readPrivateKey(id, wantFingerprint string) (ed25519.PrivateKey, error) {
	rel, err := grantFileRel(id, privateKeyFileName)
	if err != nil {
		return nil, err
	}
	payload, err := s.readRel(rel)
	if err != nil {
		return nil, err
	}
	parsed, err := ssh.ParseRawPrivateKey(payload)
	if err != nil {
		return nil, errors.New("grant key is not a usable ed25519 private key")
	}
	var private ed25519.PrivateKey
	switch key := parsed.(type) {
	case ed25519.PrivateKey:
		private = key
	case *ed25519.PrivateKey:
		private = *key
	default:
		return nil, errors.New("grant key is not ed25519")
	}
	if wantFingerprint == "" {
		return private, nil
	}
	fingerprint, err := fingerprintOf(private)
	if err != nil {
		return nil, err
	}
	if fingerprint != wantFingerprint {
		return nil, errGrantRotationRefused
	}
	return private, nil
}

func (s *credentialStore) privateKeyForGrant(id string) (ed25519.PrivateKey, grantRecord, error) {
	var private ed25519.PrivateKey
	var record grantRecord
	err := s.withGrantLock(id, func() error {
		current, err := s.readRecord(id)
		if err != nil {
			return err
		}
		if current.KeyMaterial != keyMaterialLocal || current.RemoteStatus != remoteStatusReady {
			return errors.New("grant is not ready for delivery")
		}
		key, err := s.readPrivateKey(id, current.Fingerprint)
		if err != nil {
			return err
		}
		private = key
		record = current
		return nil
	})
	return private, record, err
}

func fingerprintOf(private ed25519.PrivateKey) (string, error) {
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(signer.PublicKey()), nil
}

func grantIDFromIntent(intent privilegedIntent) (string, error) {
	if intent.Credential != credentialNone && intent.Credential != credentialPending {
		return "", errors.New("grant credential is not pending or none")
	}
	return stableGrantID(intent.Agent, intent.Repository, intent.Identity, intent.Git, intent.Permissions)
}

func (record grantRecord) observation() grantObservation {
	return grantObservation{
		GrantID:      record.GrantID,
		Agent:        record.Agent,
		Repository:   record.Repository,
		Fingerprint:  record.Fingerprint,
		Generation:   record.Generation,
		RemoteKeyID:  record.RemoteKeyID,
		RemoteStatus: record.RemoteStatus,
		KeyMaterial:  record.KeyMaterial,
	}
}

func randomToken() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func (s *privilegedState) grantDriver() grantRegistrar {
	if s != nil && s.registrar != nil {
		return s.registrar
	}
	return githubRegistrar{}
}

func (s *privilegedState) consumeGrantIntent(intent privilegedIntent) (grantDecision, error) {
	if s == nil || s.credentials == nil {
		change, ok := classifyGrantIntent(intent)
		if !ok {
			return grantDecision{}, fmt.Errorf("unknown privileged intent kind %q", intent.Kind)
		}
		return grantDecision{Unsupported: &change}, nil
	}
	switch intent.Kind {
	case intentEnsureRepositoryGrant:
		id, err := grantIDFromIntent(intent)
		if err != nil {
			return grantDecision{}, err
		}
		return grantDecision{Applied: intent.Kind + ":" + intent.Agent, GrantID: id}, nil
	case intentEnsureCredential:
		record, err := s.credentials.ensureCredential(intent)
		if err != nil {
			return grantDecision{}, err
		}
		obs := record.observation()
		decision := grantDecision{GrantID: record.GrantID, Observation: &obs}
		if intent.Credential == credentialPending && intent.Git == gitNone {
			decision.Unsupported = &unsupportedChange{
				Kind:   intent.Kind,
				Agent:  intent.Agent,
				Reason: "git none does not receive an SSH identity; Genesis did not mint an App JWT or installation access token",
			}
			return decision, nil
		}
		decision.Applied = intent.Kind + ":" + intent.Agent
		s.logGrant(obs)
		return decision, nil
	case intentEnsureRemoteRegistration:
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		record, err := s.credentials.ensureRegistration(ctx, intent, s.grantDriver())
		if err != nil {
			if id, idErr := grantIDFromIntent(intent); idErr == nil {
				s.dropHeld(intent.Agent, id)
			}
			return grantDecision{}, err
		}
		obs := record.observation()
		decision := grantDecision{GrantID: record.GrantID, Observation: &obs}
		if record.KeyMaterial == keyMaterialLocal && record.RemoteStatus == remoteStatusReady {
			decision.Applied = intent.Kind + ":" + intent.Agent
			s.logGrant(obs)
			return decision, nil
		}
		reason := coordinatorRegistrationReason
		if record.KeyMaterial != keyMaterialLocal {
			reason = "no SSH identity to register; Genesis did not call GitHub"
		}
		decision.Unsupported = &unsupportedChange{Kind: intent.Kind, Agent: intent.Agent, Reason: reason}
		s.logGrant(obs)
		return decision, nil
	default:
		return grantDecision{}, fmt.Errorf("unknown privileged intent kind %q", intent.Kind)
	}
}

func (s *privilegedState) logGrant(obs grantObservation) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Info("repository grant",
		"agent", obs.Agent,
		"repository", obs.Repository,
		"grant_id", obs.GrantID,
		"fingerprint", obs.Fingerprint,
		"generation", obs.Generation,
		"remote_status", obs.RemoteStatus,
		"remote_key_id", obs.RemoteKeyID,
		"key_material", obs.KeyMaterial,
	)
}

func (s *privilegedState) dropHeld(agent, grantID string) {
	if s == nil || agent == "" || grantID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held == nil {
		return
	}
	if current, ok := s.held[agent]; ok && current.ID == grantID {
		delete(s.held, agent)
	}
}

func (s *privilegedState) replaceHeld(next map[string]heldGrant) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if next == nil {
		s.held = map[string]heldGrant{}
		return
	}
	s.held = next
}

func (s *privilegedState) readyHeld(agent string) (heldGrant, bool) {
	if s == nil {
		return heldGrant{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.held[agent]
	if !ok || !grant.Ready || grant.ID == "" {
		return heldGrant{}, false
	}
	return grant, true
}

func isGrantIntent(kind string) bool {
	switch kind {
	case intentEnsureRepositoryGrant, intentEnsureCredential, intentEnsureRemoteRegistration:
		return true
	default:
		return false
	}
}

func sortedObservations(items map[string]grantObservation) []grantObservation {
	out := make([]grantObservation, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	slices.SortFunc(out, func(left, right grantObservation) int {
		if cmp := strings.Compare(left.Agent, right.Agent); cmp != 0 {
			return cmp
		}
		return strings.Compare(left.GrantID, right.GrantID)
	})
	return out
}
