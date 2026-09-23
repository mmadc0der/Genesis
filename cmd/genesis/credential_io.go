package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func storeRootMode(euid int) os.FileMode {
	if euid == 0 {
		// Other users may traverse to sockets/<run>/agent.sock. They cannot
		// list the store or write in it. grants/ and locks/ stay 0700.
		return credentialSocketDirMode
	}
	return credentialDirMode
}

func acceptableRootPerm(perm os.FileMode) bool {
	if perm&0o700 != 0o700 {
		return false
	}
	return perm&0o066 == 0
}

func rejectStoreParent(store string) error {
	parent := filepath.Dir(filepath.Clean(store))
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("credential store parent is not a real directory")
	}
	uid, err := fileUID(info)
	if err != nil {
		return err
	}
	if uid != os.Geteuid() && uid != 0 {
		return fmt.Errorf("credential store parent is owned by uid %d", uid)
	}
	perm := info.Mode().Perm()
	if perm&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("credential store parent permissions are %o", perm)
	}
	return nil
}

func (s *credentialStore) pin() error {
	if s == nil {
		return errors.New("credential store is not open")
	}
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), s.root)
	if err := s.boundTo(file); err != nil {
		file.Close()
		return err
	}
	s.dir = file
	return nil
}

func (s *credentialStore) confirmBound() error {
	if s == nil || s.dir == nil {
		return errors.New("credential store is not open")
	}
	if err := s.boundTo(s.dir); err != nil {
		return err
	}
	return rejectStoreParent(s.root)
}

func (s *credentialStore) boundTo(file *os.File) error {
	var pinned unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &pinned); err != nil {
		return err
	}
	var current unix.Stat_t
	if err := unix.Lstat(s.root, &current); err != nil {
		return err
	}
	if current.Mode&unix.S_IFMT == unix.S_IFLNK || pinned.Ino != current.Ino || pinned.Dev != current.Dev {
		return errors.New("credential store path no longer names the opened directory")
	}
	return nil
}

func grantFileRel(id, name string) (string, error) {
	if !grantIDPattern.MatchString(id) {
		return "", errors.New("grant id is invalid")
	}
	switch name {
	case privateKeyFileName, grantStateFileName:
	default:
		return "", errors.New("grant file is invalid")
	}
	return "grants/" + id + "/" + name, nil
}

func relParts(rel string) ([]string, error) {
	cleaned := filepath.Clean(rel)
	if cleaned == "." || cleaned == "" || filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return nil, errors.New("invalid credential relative path")
	}
	parts := strings.Split(cleaned, string(os.PathSeparator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("invalid credential relative path")
		}
	}
	return parts, nil
}

func splitRel(rel string) (string, string, error) {
	parts, err := relParts(rel)
	if err != nil {
		return "", "", err
	}
	name := parts[len(parts)-1]
	if len(parts) == 1 {
		return ".", name, nil
	}
	return strings.Join(parts[:len(parts)-1], "/"), name, nil
}

func (s *credentialStore) openDir(rel string) (*os.File, error) {
	if s == nil || s.dir == nil {
		return nil, errors.New("credential store is not open")
	}
	if rel == "." || rel == "" {
		fd, err := unix.Dup(int(s.dir.Fd()))
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), s.root), nil
	}
	return s.openRel(rel, unix.O_RDONLY|unix.O_DIRECTORY, 0)
}

func (s *credentialStore) openRel(rel string, flags int, mode uint32) (*os.File, error) {
	if s == nil || s.dir == nil {
		return nil, errors.New("credential store is not open")
	}
	parts, err := relParts(rel)
	if err != nil {
		return nil, err
	}
	current := int(s.dir.Fd())
	var closers []int
	defer func() {
		for _, fd := range closers {
			_ = unix.Close(fd)
		}
	}()
	for i, part := range parts {
		openFlags := unix.O_NOFOLLOW | unix.O_CLOEXEC
		last := i == len(parts)-1
		if last {
			openFlags |= flags
		} else {
			openFlags |= unix.O_RDONLY | unix.O_DIRECTORY
		}
		next, err := unix.Openat(current, part, openFlags, mode)
		if err != nil {
			return nil, err
		}
		if last {
			return os.NewFile(uintptr(next), rel), nil
		}
		closers = append(closers, next)
		current = next
	}
	return nil, errors.New("invalid credential relative path")
}

func (s *credentialStore) statRel(rel string) (unix.Stat_t, error) {
	dir, name, err := splitRel(rel)
	if err != nil {
		return unix.Stat_t{}, err
	}
	parent, err := s.openDir(dir)
	if err != nil {
		return unix.Stat_t{}, err
	}
	defer parent.Close()
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unix.Stat_t{}, err
	}
	return st, nil
}

func privateRegular(st unix.Stat_t) error {
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("credential file is not a regular file")
	}
	if os.FileMode(st.Mode).Perm() != credentialFileMode {
		return fmt.Errorf("credential file permissions are %o", os.FileMode(st.Mode).Perm())
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("credential file owner %d is not the coordinator uid %d", st.Uid, os.Geteuid())
	}
	return nil
}

func (s *credentialStore) readRel(rel string) ([]byte, error) {
	file, err := s.openRel(rel, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return nil, err
	}
	if err := privateRegular(st); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

func (s *credentialStore) createRel(rel string, data []byte) error {
	dir, name, err := splitRel(rel)
	if err != nil {
		return err
	}
	parent, err := s.openDir(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return createAt(parent, name, data)
}

func createAt(parent *os.File, name string, data []byte) error {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, credentialFileMode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	success := false
	defer func() {
		file.Close()
		if !success {
			_ = unix.Unlinkat(int(parent.Fd()), name, 0)
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
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if err := privateRegular(st); err != nil {
		return err
	}
	success = true
	return nil
}

func (s *credentialStore) writeAtomicRel(rel string, data []byte) error {
	dir, name, err := splitRel(rel)
	if err != nil {
		return err
	}
	parent, err := s.openDir(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	tmp := ".tmp-" + randomToken()
	if err := createAt(parent, tmp, data); err != nil {
		return err
	}
	if err := unix.Renameat(int(parent.Fd()), tmp, int(parent.Fd()), name); err != nil {
		_ = unix.Unlinkat(int(parent.Fd()), tmp, 0)
		return err
	}
	st, err := s.statRel(rel)
	if err != nil {
		return err
	}
	return privateRegular(st)
}

func (s *credentialStore) mkdirRel(rel string, mode os.FileMode) error {
	dir, name, err := splitRel(rel)
	if err != nil {
		return err
	}
	parent, err := s.openDir(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := unix.Mkdirat(int(parent.Fd()), name, uint32(mode)); err != nil && err != unix.EEXIST {
		return err
	}
	child, err := s.openRel(rel, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer child.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(child.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%s is not a directory", rel)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s owner %d is not the coordinator uid %d", rel, st.Uid, os.Geteuid())
	}
	perm := os.FileMode(st.Mode).Perm()
	if mode == credentialDirMode {
		if perm&0o077 != 0 {
			return fmt.Errorf("%s permissions are %o", rel, perm)
		}
	} else if perm&0o066 != 0 {
		return fmt.Errorf("%s permissions are %o", rel, perm)
	}
	return unix.Fchmod(int(child.Fd()), uint32(mode))
}

func (s *credentialStore) readDir(rel string) ([]os.DirEntry, error) {
	file, err := s.openDir(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.ReadDir(-1)
}

func (s *credentialStore) unlinkRel(rel string) error {
	dir, name, err := splitRel(rel)
	if err != nil {
		return err
	}
	parent, err := s.openDir(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	err = unix.Unlinkat(int(parent.Fd()), name, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func (s *credentialStore) removeTree(rel string) error {
	st, err := s.statRel(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dir, name, err := splitRel(rel)
	if err != nil {
		return err
	}
	parent, err := s.openDir(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(int(parent.Fd()), name, 0)
	}
	entries, err := s.readDir(rel)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := entry.Name()
		if child == "." || child == ".." || strings.Contains(child, "/") || strings.Contains(child, "..") {
			return fmt.Errorf("unexpected credential entry %s", child)
		}
		if err := s.removeTree(rel + "/" + child); err != nil {
			return err
		}
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}

func (s *credentialStore) ensureTraversable(dir string, uid int) error {
	if os.Geteuid() != 0 || uid == os.Geteuid() {
		return nil
	}
	current := filepath.Clean(dir)
	root := filepath.Clean(s.root)
	for {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("ssh socket path is not a real directory")
		}
		perm := info.Mode().Perm()
		if perm&0o001 == 0 {
			return errors.New("ssh socket path is not traversable by the agent user")
		}
		if perm&0o066 != 0 {
			return errors.New("ssh socket path is listable or writable by other users")
		}
		if current == root {
			return nil
		}
		next := filepath.Dir(current)
		if next == current || !pathWithin(current, root) && current != root {
			return errors.New("ssh socket path escaped the credential store")
		}
		current = next
	}
}
