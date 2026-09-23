package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	defaultSecretsDir = "genesis-secrets"
	secretsDockerPath = "/var/lib/genesis/secrets"
	maxSecretBytes    = 64 << 10
)

var errReconcilerUnavailable = errors.New("reconciler app identity is unavailable")

// secretBounds are trees the root secret directory must not overlap.
type secretBounds struct {
	AgentsDir      string
	RulesDir       string
	ReposDir       string
	ProvidersDir   string
	DataDir        string
	ConfigRoot     string
	CredentialsDir string
}

// secretStore is the root-only directory of GitHub App private keys.
// Names are secret references, never paths. Listener, control, and agents
// do not receive this directory.
type secretStore struct {
	root   string
	dir    *os.File
	bounds secretBounds
}

func openSecretStore(root string, bounds secretBounds) (*secretStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("secret store path is required")
	}
	cleaned := filepath.Clean(root)
	if !filepath.IsAbs(cleaned) {
		return nil, errors.New("secret store must be absolute")
	}
	if err := rejectSymlinkPath(cleaned); err != nil {
		return nil, errors.New("secret store must not be a symlink")
	}
	if err := os.MkdirAll(cleaned, credentialDirMode); err != nil {
		return nil, err
	}
	if err := rejectSymlinkPath(cleaned); err != nil {
		return nil, errors.New("secret store must not be a symlink")
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(resolved) != cleaned {
		return nil, errors.New("secret store path resolves through a symlink")
	}
	store := &secretStore{root: cleaned, bounds: bounds}
	if err := store.rejectOverlap(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(cleaned)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("secret store is not a real directory")
	}
	uid, err := fileUID(info)
	if err != nil {
		return nil, err
	}
	if uid != os.Geteuid() {
		return nil, errors.New("secret store owner is not the coordinator")
	}
	if err := rejectPrivateParent(cleaned, "secret store"); err != nil {
		return nil, err
	}
	if err := os.Chmod(cleaned, credentialDirMode); err != nil {
		return nil, err
	}
	fd, err := unix.Open(cleaned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), cleaned)
	store.dir = file
	if err := store.confirm(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return store, nil
}

func (s *secretStore) rejectOverlap() error {
	forbidden := []string{
		"/home",
		"/root",
		designerWritableConfigRoot,
		credentialsDockerPath,
	}
	if s.bounds.ConfigRoot != "" {
		forbidden = append(forbidden, s.bounds.ConfigRoot)
	}
	for _, path := range []string{s.bounds.AgentsDir, s.bounds.RulesDir, s.bounds.ReposDir, s.bounds.ProvidersDir, s.bounds.DataDir, s.bounds.CredentialsDir} {
		if strings.TrimSpace(path) != "" {
			forbidden = append(forbidden, path)
		}
	}
	if s.root != secretsDockerPath && (pathWithin(s.root, secretsDockerPath) || pathWithin(secretsDockerPath, s.root)) {
		return errors.New("secret store overlaps the image secret path")
	}
	for _, path := range forbidden {
		cleaned := filepath.Clean(path)
		if cleaned == "." || cleaned == "" {
			continue
		}
		if pathWithin(s.root, cleaned) || pathWithin(cleaned, s.root) {
			return errors.New("secret store overlaps a forbidden tree")
		}
		resolved, err := filepath.EvalSymlinks(cleaned)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if pathWithin(s.root, resolved) || pathWithin(resolved, s.root) {
			return errors.New("secret store overlaps a forbidden tree")
		}
	}
	return nil
}

func rejectPrivateParent(path, label string) error {
	parent := filepath.Dir(filepath.Clean(path))
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s parent is not a real directory", label)
	}
	uid, err := fileUID(info)
	if err != nil {
		return err
	}
	if uid != os.Geteuid() && uid != 0 {
		return fmt.Errorf("%s parent is owned by another user", label)
	}
	perm := info.Mode().Perm()
	if perm&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%s parent is writable by another user", label)
	}
	return nil
}

func (s *secretStore) confirm() error {
	if s == nil || s.dir == nil {
		return errors.New("secret store is not open")
	}
	var pinned unix.Stat_t
	if err := unix.Fstat(int(s.dir.Fd()), &pinned); err != nil {
		return err
	}
	var current unix.Stat_t
	if err := unix.Lstat(s.root, &current); err != nil {
		return err
	}
	if current.Mode&unix.S_IFMT == unix.S_IFLNK || pinned.Ino != current.Ino || pinned.Dev != current.Dev {
		return errors.New("secret store path no longer names the opened directory")
	}
	if os.FileMode(current.Mode).Perm()&0o077 != 0 {
		return errors.New("secret store is not private")
	}
	if err := rejectPrivateParent(s.root, "secret store"); err != nil {
		return err
	}
	return s.audit()
}

func (s *secretStore) audit() error {
	entries, err := s.dir.ReadDir(-1)
	if err != nil {
		return err
	}
	if _, err := s.dir.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." {
			continue
		}
		if !secretNamePattern.MatchString(name) || strings.Contains(name, "/") || strings.Contains(name, "..") {
			return errors.New("secret store contains an unexpected entry")
		}
		if err := s.statPrivate(name); err != nil {
			return err
		}
	}
	return nil
}

func (s *secretStore) statPrivate(name string) error {
	fd, err := unix.Openat(int(s.dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("secret store entry is not a regular file")
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return errors.New("secret store entry is not a regular file")
	}
	if int(st.Uid) != os.Geteuid() {
		return errors.New("secret store entry owner is not the coordinator")
	}
	perm := os.FileMode(st.Mode).Perm()
	if perm&0o077 != 0 || perm&0o400 == 0 {
		return errors.New("secret store entry is not private")
	}
	if st.Size <= 0 || st.Size > maxSecretBytes {
		return errors.New("secret store entry has an unexpected size")
	}
	return nil
}

func (s *secretStore) read(name string) ([]byte, error) {
	if !secretNamePattern.MatchString(name) || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return nil, errors.New("secret name is invalid")
	}
	if err := s.confirm(); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(s.dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("reconciler private key is unavailable")
	}
	file := os.NewFile(uintptr(fd), "secret")
	defer file.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, errors.New("reconciler private key is unavailable")
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
		return nil, errors.New("reconciler private key is unavailable")
	}
	perm := os.FileMode(st.Mode).Perm()
	if perm&0o077 != 0 || perm&0o400 == 0 || st.Size <= 0 || st.Size > maxSecretBytes {
		return nil, errors.New("reconciler private key is unavailable")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxSecretBytes+1))
	if err != nil || len(payload) == 0 || len(payload) > maxSecretBytes {
		return nil, errors.New("reconciler private key is unavailable")
	}
	return payload, nil
}

func parseAppPrivateKey(payload []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(payload)
	if block == nil || len(bytesTrimSpace(rest)) != 0 {
		return nil, errReconcilerUnavailable
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errReconcilerUnavailable
		}
		key = parsed
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errReconcilerUnavailable
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errReconcilerUnavailable
		}
		key = rsaKey
	default:
		return nil, errReconcilerUnavailable
	}
	if err := key.Validate(); err != nil {
		return nil, errReconcilerUnavailable
	}
	return key, nil
}

func bytesTrimSpace(payload []byte) []byte {
	return []byte(strings.TrimSpace(string(payload)))
}
