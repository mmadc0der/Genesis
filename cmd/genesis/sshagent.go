package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sys/unix"
)

const maxSocketPath = 104

var umaskMu sync.Mutex

// runSSHAgent is a per-run ssh-agent. It holds one grant key in memory and
// serves it on a short-lived Unix socket. The child receives only
// SSH_AUTH_SOCK. Stop closes the listener, drops the key, and removes the
// socket on every completion, failure, and cancel path.
type runSSHAgent struct {
	listener net.Listener
	socket   string
	dir      string
	done     chan struct{}
	keyring  agent.Agent
	release  func()

	mu      sync.Mutex
	conns   []net.Conn
	stopped bool
	once    sync.Once
}

// sealedAgent refuses add, remove, and lock so a child cannot change the
// single grant key that root loaded.
type sealedAgent struct {
	inner agent.ExtendedAgent
}

func (s sealedAgent) List() ([]*agent.Key, error) { return s.inner.List() }
func (s sealedAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return s.inner.Sign(key, data)
}
func (s sealedAgent) Signers() ([]ssh.Signer, error) { return s.inner.Signers() }
func (s sealedAgent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	return s.inner.SignWithFlags(key, data, flags)
}
func (s sealedAgent) Extension(string, []byte) ([]byte, error) {
	return nil, agent.ErrExtensionUnsupported
}
func (s sealedAgent) Add(agent.AddedKey) error { return errors.New("ssh agent is sealed") }
func (s sealedAgent) Remove(ssh.PublicKey) error {
	return errors.New("ssh agent is sealed")
}
func (s sealedAgent) RemoveAll() error    { return errors.New("ssh agent is sealed") }
func (s sealedAgent) Lock([]byte) error   { return errors.New("ssh agent is sealed") }
func (s sealedAgent) Unlock([]byte) error { return errors.New("ssh agent is sealed") }

func startRunSSHAgent(store *credentialStore, runID string, uid, gid int, private ed25519.PrivateKey, home string) (*runSSHAgent, error) {
	if store == nil {
		return nil, errors.New("ssh agent requires a credential store")
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	if private == nil {
		return nil, errors.New("ssh agent requires a grant key")
	}
	if err := store.confirmBound(); err != nil {
		return nil, err
	}
	dir := filepath.Join(store.root, "sockets", runID)
	socketPath := filepath.Join(dir, "agent.sock")
	if len(socketPath) >= maxSocketPath {
		return nil, fmt.Errorf("ssh agent socket path is too long")
	}
	if home != "" && (pathWithin(socketPath, home) || pathWithin(dir, home)) {
		return nil, errors.New("ssh agent socket must not be inside the agent home")
	}
	socketRel := "sockets/" + runID
	if err := store.mkdirRel(socketRel, socketDirMode(uid)); err != nil {
		return nil, err
	}
	if err := store.ensureTraversable(dir, uid); err != nil {
		_ = store.removeTree(socketRel)
		return nil, err
	}
	if err := store.unlinkRel(socketRel + "/agent.sock"); err != nil {
		_ = store.removeTree(socketRel)
		return nil, err
	}
	if err := store.confirmBound(); err != nil {
		_ = store.removeTree(socketRel)
		return nil, err
	}
	listener, err := listenSocket(socketPath)
	if err != nil {
		_ = store.removeTree(socketRel)
		return nil, err
	}
	if err := store.ownSocket(socketRel+"/agent.sock", uid, gid); err != nil {
		discardSocket(listener, store, socketRel)
		return nil, err
	}

	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: private, Comment: grantComment}); err != nil {
		_ = ring.RemoveAll()
		discardSocket(listener, store, socketRel)
		return nil, err
	}
	extended, ok := ring.(agent.ExtendedAgent)
	if !ok {
		_ = ring.RemoveAll()
		discardSocket(listener, store, socketRel)
		return nil, errors.New("ssh agent keyring is not sealable")
	}
	served := sealedAgent{inner: extended}
	run := &runSSHAgent{
		listener: listener,
		socket:   socketPath,
		dir:      dir,
		done:     make(chan struct{}),
		keyring:  ring,
	}
	go run.serve(served, uid)
	return run, nil
}

func socketDirMode(uid int) os.FileMode {
	if os.Geteuid() == 0 && uid != os.Geteuid() {
		return credentialSocketDirMode
	}
	return credentialDirMode
}

func discardSocket(listener net.Listener, store *credentialStore, socketRel string) {
	if listener != nil {
		_ = listener.Close()
	}
	if store != nil && socketRel != "" {
		_ = store.removeTree(socketRel)
	}
}

func (a *runSSHAgent) serve(served agent.Agent, uid int) {
	defer close(a.done)
	for {
		conn, err := a.listener.Accept()
		if err != nil {
			return
		}
		peer, err := peerUID(conn)
		if err != nil || (peer != uid && peer != os.Geteuid()) {
			conn.Close()
			continue
		}
		if !a.track(conn) {
			conn.Close()
			continue
		}
		go func(conn net.Conn) {
			defer conn.Close()
			_ = agent.ServeAgent(served, conn)
		}(conn)
	}
}

func (a *runSSHAgent) track(conn net.Conn) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return false
	}
	a.conns = append(a.conns, conn)
	return true
}

func (a *runSSHAgent) Socket() string {
	if a == nil {
		return ""
	}
	return a.socket
}

func (a *runSSHAgent) stop() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		a.mu.Lock()
		a.stopped = true
		keyring := a.keyring
		conns := a.conns
		a.conns = nil
		a.mu.Unlock()
		if keyring != nil {
			_ = keyring.RemoveAll()
		}
		if a.listener != nil {
			_ = a.listener.Close()
		}
		for _, conn := range conns {
			_ = conn.Close()
		}
		if a.done != nil {
			<-a.done
		}
		if a.socket != "" {
			info, err := os.Lstat(a.socket)
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				_ = os.Remove(a.socket)
			} else if err == nil {
				_ = os.Remove(a.socket)
			}
		}
		if a.dir != "" {
			_ = os.Remove(a.dir)
		}
		if a.release != nil {
			a.release()
		}
	})
}

func (s *credentialStore) ownSocket(rel string, uid, gid int) error {
	dir, name, err := splitRel(rel)
	if err != nil {
		return err
	}
	parent, err := s.openDir(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return errors.New("ssh agent listener is not a socket")
	}
	if err := unix.Fchmodat(int(parent.Fd()), name, credentialFileMode, 0); err != nil {
		return err
	}
	if err := unix.Fchownat(int(parent.Fd()), name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if after.Mode&unix.S_IFMT != unix.S_IFSOCK || after.Ino != before.Ino || after.Dev != before.Dev {
		return errors.New("ssh agent socket changed while it was tightened")
	}
	if os.FileMode(after.Mode).Perm() != credentialFileMode || int(after.Uid) != uid {
		return errors.New("ssh agent socket owner does not match the agent user")
	}
	return nil
}

func listenSocket(path string) (net.Listener, error) {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	old := unix.Umask(0o077)
	defer unix.Umask(old)
	return net.Listen("unix", path)
}

func peerUID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, errors.New("ssh agent connection is not a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return -1, err
	}
	var uid int
	var ctrlErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			ctrlErr = err
			return
		}
		uid = int(cred.Uid)
	}); err != nil {
		return -1, err
	}
	if ctrlErr != nil {
		return -1, ctrlErr
	}
	return uid, nil
}
