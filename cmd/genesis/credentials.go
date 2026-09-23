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
}

type credentialStore struct {
	root   string
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
	if err := verifyDir(info, credentialDirMode); err != nil {
		return nil, fmt.Errorf("credential store: %w", err)
	}
	if err := os.Chmod(cleaned, credentialDirMode); err != nil {
		return nil, err
	}
	for _, name := range []string{"grants", "locks"} {
		path := filepath.Join(cleaned, name)
		if err := mkdirExclusive(path, credentialDirMode); err != nil {
			return nil, err
		}
	}
	socketMode := os.FileMode(credentialDirMode)
	if os.Geteuid() == 0 {
		socketMode = credentialSocketDirMode
	}
	if err := mkdirMode(filepath.Join(cleaned, "sockets"), socketMode); err != nil {
		return nil, err
	}
	if err := store.sweepSockets(); err != nil {
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
	for _, path := range []string{s.bounds.AgentsDir, s.bounds.RulesDir, s.bounds.ReposDir, s.bounds.ProvidersDir, s.bounds.DataDir} {
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
	sockets := filepath.Join(s.root, "sockets")
	entries, err := os.ReadDir(sockets)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(sockets, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		if err := removeTreeNoFollow(path); err != nil {
			return err
		}
	}
	return nil
}

func mkdirMode(path string, mode os.FileMode) error {
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a real directory", path)
	}
	uid, err := fileUID(info)
	if err != nil {
		return err
	}
	if uid != os.Geteuid() {
		return fmt.Errorf("%s owner %d is not the coordinator uid %d", path, uid, os.Geteuid())
	}
	perm := info.Mode().Perm()
	if perm != credentialDirMode && perm != mode {
		if perm&0o022 != 0 {
			return fmt.Errorf("%s permissions are %o", path, perm)
		}
	}
	return os.Chmod(path, mode)
}

func mkdirExclusive(path string, mode os.FileMode) error {
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	err := os.Mkdir(path, mode)
	if err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if err := verifyDir(info, mode); err != nil && mode == credentialDirMode {
		perm := info.Mode().Perm()
		if perm&0o077 != 0 {
			return fmt.Errorf("%s permissions are %o", path, perm)
		}
	}
	return os.Chmod(path, mode)
}

func verifyDir(info os.FileInfo, mode os.FileMode) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("directory is a symlink")
	}
	if !info.IsDir() {
		return errors.New("path is not a directory")
	}
	perm := info.Mode().Perm()
	if mode == credentialDirMode && perm&0o077 != 0 {
		return fmt.Errorf("directory permissions are %o", perm)
	}
	uid, err := fileUID(info)
	if err != nil {
		return err
	}
	if uid != os.Geteuid() {
		return fmt.Errorf("directory owner %d is not the coordinator uid %d", uid, os.Geteuid())
	}
	return nil
}

func verifyPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm() != credentialFileMode {
		return fmt.Errorf("%s permissions are %o", path, info.Mode().Perm())
	}
	uid, err := fileUID(info)
	if err != nil {
		return err
	}
	if uid != os.Geteuid() {
		return fmt.Errorf("%s owner %d is not the coordinator uid %d", path, uid, os.Geteuid())
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

func (s *credentialStore) grantDir(id string) (string, error) {
	if !grantIDPattern.MatchString(id) {
		return "", errors.New("grant id is invalid")
	}
	return filepath.Join(s.root, "grants", id), nil
}

func (s *credentialStore) withGrantLock(id string, fn func() error) error {
	if !grantIDPattern.MatchString(id) {
		return errors.New("grant id is invalid")
	}
	lockPath := filepath.Join(s.root, "locks", id+".lock")
	if err := rejectSymlinkPath(lockPath); err != nil {
		return err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, credentialFileMode)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }()
	if err := verifyPrivateFile(lockPath); err != nil {
		info, statErr := os.Lstat(lockPath)
		if statErr != nil {
			return err
		}
		if info.Mode().Perm() != credentialFileMode {
			if chmodErr := os.Chmod(lockPath, credentialFileMode); chmodErr != nil {
				return chmodErr
			}
		}
		if err := verifyPrivateFile(lockPath); err != nil {
			return err
		}
	}
	return fn()
}

func (s *credentialStore) retained(active map[string]struct{}) ([]string, error) {
	grants := filepath.Join(s.root, "grants")
	entries, err := os.ReadDir(grants)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(grants, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("grant path %s is a symlink", name)
		}
		if !grantIDPattern.MatchString(name) || !info.IsDir() {
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
		if current.KeyMaterial != keyMaterialLocal {
			record = current
			return nil
		}
		if registrar == nil {
			registrar = githubRegistrar{}
		}
		private, err := s.readPrivateKey(id, current.Fingerprint)
		if err != nil {
			return err
		}
		signer, err := ssh.NewSignerFromKey(private)
		if err != nil {
			return err
		}
		public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
		result, err := registrar.Register(ctx, grantRegistration{
			GrantID:     id,
			Agent:       intent.Agent,
			Repository:  intent.Repository,
			Fingerprint: current.Fingerprint,
			PublicKey:   public,
		})
		if err != nil {
			return err
		}
		switch result.Status {
		case remoteRegistrationUnsupported, remoteRegistrationNone, remoteStatusReady, remoteStatusPending:
		default:
			return errUnknownRegistrationStatus
		}
		current.RemoteStatus = result.Status
		if result.Status == remoteStatusReady {
			if result.RemoteKeyID == "" {
				return errors.New("ready registration did not return a remote key id")
			}
			current.RemoteKeyID = result.RemoteKeyID
		}
		if current.Generation != grantGenerationInitial {
			return errGrantRotationRefused
		}
		if err := s.saveRecord(current); err != nil {
			return err
		}
		record = current
		return nil
	})
	return record, err
}

func (s *credentialStore) ensureLocalKey(intent privilegedIntent, id string) (grantRecord, error) {
	dir, err := s.grantDir(id)
	if err != nil {
		return grantRecord{}, err
	}
	if err := mkdirExclusive(dir, credentialDirMode); err != nil {
		return grantRecord{}, err
	}
	keyPath := filepath.Join(dir, privateKeyFileName)
	info, err := os.Lstat(keyPath)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return grantRecord{}, fmt.Errorf("grant key is not a regular file")
		}
		if err := verifyPrivateFile(keyPath); err != nil {
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
	if err := createExclusiveFile(keyPath, encoded); err != nil {
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
	dir, err := s.grantDir(id)
	if err != nil {
		return grantRecord{}, err
	}
	if err := mkdirExclusive(dir, credentialDirMode); err != nil {
		return grantRecord{}, err
	}
	keyPath := filepath.Join(dir, privateKeyFileName)
	if _, err := os.Lstat(keyPath); err == nil {
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
	dir, err := s.grantDir(id)
	if err != nil {
		return grantRecord{}, err
	}
	path := filepath.Join(dir, grantStateFileName)
	if err := verifyPrivateFile(path); err != nil {
		return grantRecord{}, err
	}
	payload, err := os.ReadFile(path)
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
	dir, err := s.grantDir(record.GrantID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if bytes.Contains(payload, []byte("PRIVATE KEY")) {
		return errors.New("refusing to persist key material in grant state")
	}
	return writeAtomicNoFollow(filepath.Join(dir, grantStateFileName), payload, credentialFileMode)
}

func (s *credentialStore) readPrivateKey(id, wantFingerprint string) (ed25519.PrivateKey, error) {
	dir, err := s.grantDir(id)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, privateKeyFileName)
	if err := verifyPrivateFile(path); err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parsed, err := ssh.ParseRawPrivateKey(payload)
	if err != nil {
		return nil, err
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

func createExclusiveFile(path string, data []byte) error {
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, credentialFileMode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	success := false
	defer func() {
		file.Close()
		if !success {
			_ = os.Remove(path)
		}
	}()
	if err := unix.Fchmod(fd, credentialFileMode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := verifyPrivateFile(path); err != nil {
		return err
	}
	success = true
	return nil
}

func writeAtomicNoFollow(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := rejectSymlinkPath(dir); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".tmp-"+randomToken())
	if err := createExclusiveFile(tmp, data); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return verifyPrivateFile(path)
}

func randomToken() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func removeTreeNoFollow(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.Remove(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeTreeNoFollow(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
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
		record, err := s.credentials.ensureRegistration(context.Background(), intent, s.grantDriver())
		if err != nil {
			return grantDecision{}, err
		}
		obs := record.observation()
		decision := grantDecision{GrantID: record.GrantID, Observation: &obs}
		if record.KeyMaterial == keyMaterialLocal && record.RemoteStatus == remoteStatusReady {
			decision.Applied = intent.Kind + ":" + intent.Agent
			s.logGrant(obs)
			return decision, nil
		}
		reason := remoteRegistrationReason
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
