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

	mu    sync.Mutex
	conns []net.Conn
	once  sync.Once
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

func startRunSSHAgent(root string, runID string, uid, gid int, private ed25519.PrivateKey, home string) (*runSSHAgent, error) {
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	if private == nil {
		return nil, errors.New("ssh agent requires a grant key")
	}
	dir := filepath.Join(root, "sockets", runID)
	socketPath := filepath.Join(dir, "agent.sock")
	if len(socketPath) >= maxSocketPath {
		return nil, fmt.Errorf("ssh agent socket path is too long")
	}
	if home != "" && (pathWithin(socketPath, home) || pathWithin(dir, home)) {
		return nil, errors.New("ssh agent socket must not be inside the agent home")
	}
	if err := prepareSocketDir(dir, uid); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(socketPath); err != nil {
				return nil, err
			}
		} else if err := os.Remove(socketPath); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := listenSocket(socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, credentialFileMode); err != nil {
		listener.Close()
		_ = os.Remove(socketPath)
		return nil, err
	}
	if err := os.Chown(socketPath, uid, gid); err != nil {
		listener.Close()
		_ = os.Remove(socketPath)
		return nil, err
	}
	if err := verifySocket(socketPath, uid); err != nil {
		listener.Close()
		_ = os.Remove(socketPath)
		return nil, err
	}

	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: private, Comment: grantComment}); err != nil {
		listener.Close()
		_ = os.Remove(socketPath)
		return nil, err
	}
	extended, ok := ring.(agent.ExtendedAgent)
	if !ok {
		listener.Close()
		_ = os.Remove(socketPath)
		return nil, errors.New("ssh agent keyring is not sealable")
	}
	served := sealedAgent{inner: extended}
	run := &runSSHAgent{
		listener: listener,
		socket:   socketPath,
		dir:      dir,
		done:     make(chan struct{}),
	}
	go run.serve(served, uid)
	return run, nil
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
		a.mu.Lock()
		a.conns = append(a.conns, conn)
		a.mu.Unlock()
		go func(conn net.Conn) {
			defer conn.Close()
			_ = agent.ServeAgent(served, conn)
		}(conn)
	}
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
		if a.listener != nil {
			_ = a.listener.Close()
		}
		a.mu.Lock()
		conns := a.conns
		a.conns = nil
		a.mu.Unlock()
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
			} else {
				_ = os.Remove(a.socket)
			}
		}
		if a.dir != "" {
			_ = os.Remove(a.dir)
		}
	})
}

func prepareSocketDir(dir string, uid int) error {
	parent := filepath.Dir(dir)
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("ssh socket parent is not a real directory")
	}
	if os.Geteuid() == 0 && uid != os.Geteuid() {
		if info.Mode().Perm()&0o001 == 0 {
			return errors.New("ssh socket parent is not traversable by the agent user")
		}
	}
	if err := os.Mkdir(dir, credentialDirMode); err != nil && !os.IsExist(err) {
		return err
	}
	current, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() {
		return errors.New("ssh socket directory is not a real directory")
	}
	mode := os.FileMode(credentialDirMode)
	if os.Geteuid() == 0 && uid != os.Geteuid() {
		mode = credentialSocketDirMode
	}
	if err := os.Chmod(dir, mode); err != nil {
		return err
	}
	owner, err := fileUID(current)
	if err != nil {
		return err
	}
	if owner != os.Geteuid() {
		return errors.New("ssh socket directory is not owned by the coordinator")
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

func verifySocket(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return errors.New("ssh agent listener is not a socket")
	}
	if info.Mode().Perm() != credentialFileMode {
		return fmt.Errorf("ssh agent socket permissions are %o", info.Mode().Perm())
	}
	owner, err := fileUID(info)
	if err != nil {
		return err
	}
	if owner != uid {
		return errors.New("ssh agent socket owner does not match the agent user")
	}
	return nil
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
