package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// sessionLogMode is group-readable for session logs the listener owns.
	// The oracle account is a member of the listener group, so 0640 is enough
	// for those files. Logs owned by the agent uid get a named-user ACL instead.
	sessionLogMode os.FileMode = 0o640

	aclUserObj     = 0x01
	aclUser        = 0x02
	aclGroupObj    = 0x04
	aclMask        = 0x10
	aclOther       = 0x20
	aclRead        = 0x04
	aclWrite       = 0x02
	aclExecute     = 0x01
	aclUndefinedID = 0xFFFFFFFF
	aclVersion     = 0x0002
)

// sessionLogName matches the DeepSeek JSONL names the pinned SDK writes.
// session.jsonl is version 0. session.vN.jsonl is version N.
var sessionLogName = regexp.MustCompile(`^session(?:\.v([1-9][0-9]*))?\.jsonl$`)

func isSessionLogName(name string) bool {
	return sessionLogName.MatchString(name)
}

func sessionLogVersion(name string) int {
	match := sessionLogName.FindStringSubmatch(name)
	if match == nil {
		return -1
	}
	if match[1] == "" {
		return 0
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		return -1
	}
	return version
}

// locateSessionLog returns the absolute path of the session log under
// dshHome. The highest version wins. A tie keeps the later path in walk order.
func locateSessionLog(dshHome string) string {
	if strings.TrimSpace(dshHome) == "" {
		return ""
	}
	dshHome = filepath.Clean(dshHome)
	if dshHome == "." {
		return ""
	}
	bestPath := ""
	bestVersion := -1
	_ = filepath.WalkDir(dshHome, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !isSessionLogName(entry.Name()) {
			return nil
		}
		version := sessionLogVersion(entry.Name())
		if version > bestVersion {
			bestVersion = version
			bestPath = path
		}
		return nil
	})
	if bestPath == "" {
		return ""
	}
	absolute, err := filepath.Abs(bestPath)
	if err != nil {
		return bestPath
	}
	return absolute
}

// grantSessionRead lets the oracle read a finished DSH home.
// Listener-owned session logs become mode 0640 and listener-owned directories
// gain group execute. When the oracle uid is known, session logs and
// directories also get a named-user ACL, including files the listener owns,
// so membership in the listener group is not required. Symlinks are not
// followed. EPERM is ignored so a later chmod by the listener does not fail
// the grant.
func grantSessionRead(root string, listenerUID uint32, haveListener bool, oracleUID uint32, haveOracle bool) error {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	var first error
	note := func(err error) {
		if err == nil || first != nil || ignorablePerm(err) {
			return
		}
		first = err
	}
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			note(err)
			return nil
		}
		if entry == nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			note(err)
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat == nil {
			return nil
		}
		ownedByListener := haveListener && stat.Uid == listenerUID
		if entry.IsDir() {
			mode := info.Mode().Perm()
			if ownedByListener {
				mode |= 0o010
				if info.Mode().Perm() != mode {
					note(os.Chmod(path, mode))
				}
			}
			if haveOracle {
				note(grantNamedUserACL(path, mode, oracleUID, aclRead|aclExecute))
			}
			return nil
		}
		if !entry.Type().IsRegular() || !isSessionLogName(entry.Name()) {
			return nil
		}
		mode := info.Mode().Perm()
		if ownedByListener {
			mode = sessionLogMode
			if info.Mode().Perm() != mode {
				note(os.Chmod(path, mode))
			}
		}
		if haveOracle {
			note(grantNamedUserACL(path, mode, oracleUID, aclRead))
		}
		return nil
	})
	if walkErr != nil && first == nil && !ignorablePerm(walkErr) {
		first = walkErr
	}
	return first
}

func ignorablePerm(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// grantNamedUserACL adds one named user to the access ACL. The mask is the
// union of the existing group class and the named permission, so the new
// entry is not masked off. Owner and other permissions are copied from mode.
func grantNamedUserACL(path string, mode os.FileMode, uid uint32, named uint16) error {
	owner := uint16((mode >> 6) & 0o7)
	group := uint16((mode >> 3) & 0o7)
	other := uint16(mode & 0o7)
	mask := group | named
	buffer := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(buffer[0:4], aclVersion)
	writeACLEntry(buffer[4:], aclUserObj, owner, aclUndefinedID)
	writeACLEntry(buffer[12:], aclUser, named, uid)
	writeACLEntry(buffer[20:], aclGroupObj, group, aclUndefinedID)
	writeACLEntry(buffer[28:], aclMask, mask, aclUndefinedID)
	writeACLEntry(buffer[36:], aclOther, other, aclUndefinedID)
	return unix.Setxattr(path, "system.posix_acl_access", buffer, 0)
}

func writeACLEntry(dest []byte, tag, perm uint16, id uint32) {
	binary.LittleEndian.PutUint16(dest[0:2], tag)
	binary.LittleEndian.PutUint16(dest[2:4], perm)
	binary.LittleEndian.PutUint32(dest[4:8], id)
}

func writeSessionPath(runDir, sessionPath string, uid, gid int) error {
	if runDir == "" || sessionPath == "" {
		return nil
	}
	path := filepath.Join(runDir, sessionPathFileName)
	if err := os.WriteFile(path, []byte(sessionPath+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func readRecordedSessionPath(runDir, dshHome string) string {
	if runDir == "" {
		return ""
	}
	path := filepath.Join(runDir, sessionPathFileName)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ""
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	recorded := strings.TrimSpace(string(payload))
	if !filepath.IsAbs(recorded) {
		return ""
	}
	if dshHome != "" && !pathHasPrefix(recorded, filepath.Clean(dshHome)) {
		return ""
	}
	target, err := os.Lstat(recorded)
	if err != nil || target.Mode()&os.ModeSymlink != 0 || !target.Mode().IsRegular() {
		return ""
	}
	if !isSessionLogName(filepath.Base(recorded)) {
		return ""
	}
	return recorded
}

func agentDefinitionPath(dir, id string) string {
	if dir == "" || id == "" || strings.Contains(id, "/") || strings.Contains(id, `\`) {
		return ""
	}
	for _, ext := range []string{".yaml", ".yml"} {
		path := filepath.Join(dir, id+ext)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return path
		}
		return absolute
	}
	return ""
}
