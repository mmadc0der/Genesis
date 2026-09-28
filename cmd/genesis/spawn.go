package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// agentSpawnUmask is 022 so a spawned agent creates directories as 0755 and
// files as 0644. umask 077 would hide those file contents from other agent
// accounts even when the workspace directory is 0755.
const agentSpawnUmask = 0o022

type spawnRequest struct {
	Agent      string            `json:"agent"`
	User       string            `json:"user"`
	Cwd        string            `json:"cwd"`
	Home       string            `json:"home"`
	RunDir     string            `json:"run_dir"`
	DshHome    string            `json:"dsh_home"`
	Env        map[string]string `json:"env"`
	NeedsToken bool              `json:"needs_token,omitempty"`
}

type spawnReply struct {
	PID int `json:"pid"`
}

type waitRequest struct {
	PID int `json:"pid"`
}

type waitReply struct {
	ExitCode int `json:"exit_code"`
}

type privilegedSpawner interface {
	Spawn(ctx context.Context, req spawnRequest, stdin, stdout, stderr *os.File) (int, error)
	Wait(ctx context.Context, pid int) (int, error)
}

type spawnedChild struct {
	cmd              *exec.Cmd
	done             chan struct{}
	err              error
	ssh              *runSSHAgent
	agent            string
	grantID          string
	dshHome          string
	token            string
	credentialActive bool
}

type privilegedState struct {
	logger       *slog.Logger
	host         hostAPI
	mutate       bool
	pythonPath   string
	source       string
	listenerUser string
	dataDir      string

	mu            sync.Mutex
	users         map[string]reconciledIdentity
	spawned       map[int]*spawnedChild
	credentials   *credentialStore
	secrets       *secretStore
	agentsDir     string
	rulesDir      string
	reposDir      string
	providersDir  string
	registrar     grantRegistrar
	installations map[string]string // org -> installation id; memory only, never disk or YAML
	held          map[string]heldGrant
	uidGrants     map[uint32]*uidGrantHold
	tokens        runTokenMinter
	active        map[string]grantRecord
	gitRemote     func(org, repo string) (string, error)
	gitBin        string
	observeGit    func(env, args []string)
}

type uidGrantHold struct {
	grantID string
	refs    int
}

func newPrivilegedState(logger *slog.Logger, pythonPath, source, listenerUser, dataDir string) *privilegedState {
	if logger == nil {
		logger = slog.Default()
	}
	return &privilegedState{
		logger:       logger,
		host:         unixHost{},
		mutate:       os.Geteuid() == 0,
		pythonPath:   pythonPath,
		source:       source,
		listenerUser: listenerUser,
		dataDir:      dataDir,
		users:        map[string]reconciledIdentity{},
		spawned:      map[int]*spawnedChild{},
	}
}

func (s *privilegedState) spawn(req spawnRequest, stdin, stdout, stderr *os.File) (int, error) {
	if s == nil {
		return 0, errors.New("privileged spawn is not configured")
	}
	if s.pythonPath == "" || s.source == "" {
		return 0, errors.New("privileged spawn is missing python")
	}
	if req.Agent == "" || req.User == "" {
		return 0, errors.New("spawn requires agent and user")
	}
	identity, ok := s.lookupUser(req.User)
	if !ok {
		return 0, fmt.Errorf("OS user %q is not a reconciled agent identity", req.User)
	}
	if identity.UID < minRegularUID {
		return 0, fmt.Errorf("refusing to spawn system uid %d for %q", identity.UID, req.User)
	}
	if req.Agent == "" || req.Agent != identity.Agent {
		return 0, fmt.Errorf("spawn agent %q does not match reconciled identity", req.Agent)
	}
	if filepath.Clean(req.Cwd) != filepath.Clean(identity.Cwd) {
		return 0, errors.New("spawn cwd does not match reconciled identity")
	}
	if filepath.Clean(req.Home) != filepath.Clean(identity.Home) {
		return 0, errors.New("spawn home does not match reconciled identity")
	}
	if err := prepareSpawnDirs(s.host, s.dataDir, req, identity); err != nil {
		return 0, err
	}
	sshAgent, grantID, err := s.deliverGrantSocket(req, identity)
	if err != nil {
		return 0, err
	}
	token, err := s.prepareRun(req, identity, sshAgent)
	if err != nil {
		if sshAgent != nil {
			sshAgent.stop()
		}
		return 0, redactTokenError(err, token)
	}

	command := exec.Command(s.pythonPath, "-c", s.source)
	command.Dir = identity.Cwd
	command.Env = spawnEnv(identity, req.Env)
	if sshAgent != nil {
		command.Env = appendAuthSock(command.Env, sshAgent.Socket())
	}
	if token != "" {
		command.Env = appendOneEnv(command.Env, githubTokenEnv, token)
	}
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cred := &syscall.Credential{
		Uid:    identity.UID,
		Gid:    identity.GID,
		Groups: append([]uint32{}, identity.Groups...),
	}
	if os.Geteuid() == 0 || int(identity.UID) != os.Geteuid() || int(identity.GID) != os.Getegid() {
		command.SysProcAttr.Credential = cred
	}
	child := &spawnedChild{
		cmd: command, done: make(chan struct{}), ssh: sshAgent, agent: req.Agent, grantID: grantID, dshHome: req.DshHome,
	}
	if err := startAgentCommand(command); err != nil {
		if sshAgent != nil {
			sshAgent.stop()
			child.ssh = nil
		}
		return 0, err
	}
	if token != "" {
		child.token = token
		child.credentialActive = true
		s.logCredential(req.Agent, true)
	}
	s.mu.Lock()
	if s.spawned == nil {
		s.spawned = map[int]*spawnedChild{}
	}
	s.spawned[command.Process.Pid] = child
	revoke := false
	if sshAgent != nil {
		current, ok := s.held[req.Agent]
		revoke = !ok || !current.Ready || current.ID != grantID
	}
	s.mu.Unlock()
	if revoke {
		sshAgent.stop()
	}
	go func() {
		child.err = command.Wait()
		close(child.done)
	}()
	return command.Process.Pid, nil
}

func (s *privilegedState) wait(pid int) (int, error) {
	s.mu.Lock()
	child := s.spawned[pid]
	s.mu.Unlock()
	if child == nil {
		return -1, fmt.Errorf("unknown spawned pid %d", pid)
	}
	<-child.done
	s.finishChild(child)
	s.mu.Lock()
	delete(s.spawned, pid)
	s.mu.Unlock()
	return exitStatus(child.err, child.cmd), nil
}

func (s *privilegedState) killAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	children := make([]*spawnedChild, 0, len(s.spawned))
	for _, child := range s.spawned {
		children = append(children, child)
	}
	s.mu.Unlock()
	for _, child := range children {
		if child.cmd != nil && child.cmd.Process != nil {
			_ = child.cmd.Process.Kill()
		}
	}
	for _, child := range children {
		<-child.done
		s.finishChild(child)
	}
	s.mu.Lock()
	s.spawned = map[int]*spawnedChild{}
	s.mu.Unlock()
}

// startAgentCommand forks the agent with umask 022. The parent umask is
// restored after Start so credential sockets and the run journal stay private.
func startAgentCommand(command *exec.Cmd) error {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	previous := unix.Umask(agentSpawnUmask)
	defer unix.Umask(previous)
	return command.Start()
}

func prepareSpawnDirs(host hostAPI, dataDir string, req spawnRequest, identity reconciledIdentity) error {
	if host == nil {
		return errors.New("privileged host is not configured")
	}
	if dataDir == "" {
		return errors.New("privileged data directory is not configured")
	}
	dataDir = filepath.Clean(dataDir)
	if !filepath.IsAbs(dataDir) {
		return errors.New("data directory must be absolute")
	}
	if err := validateAbsolutePath("run_dir", req.RunDir); err != nil {
		return err
	}
	if err := validateAbsolutePath("dsh_home", req.DshHome); err != nil {
		return err
	}
	runDir := filepath.Clean(req.RunDir)
	dshHome := filepath.Clean(req.DshHome)
	runID := filepath.Base(runDir)
	if err := validateRunID(runID); err != nil {
		return err
	}
	wantRunDir := filepath.Join(dataDir, runsDirName, runID)
	if runDir != wantRunDir {
		return errors.New("run_dir must be a run directory under the configured data directory")
	}
	if err := validateStableDshHome(dataDir, dshHome); err != nil {
		return err
	}
	uid := int(identity.UID)
	gid := int(identity.GID)
	sessionDir := filepath.Dir(dshHome)
	for _, path := range []string{dataDir, filepath.Join(dataDir, runsDirName), runDir, filepath.Dir(sessionDir), sessionDir} {
		if err := chmodExistingDir(host, path, dataRootMode); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	// The listener creates this tree, so an earlier run can leave files owned
	// by the listener at mode 0600. Chown the entries, not only the directory,
	// or the agent cannot replace them on continuation. Symlinks are not followed.
	if err := chownDshHomeTree(dshHome, uid, gid); err != nil {
		return fmt.Errorf("chown dsh_home: %w", err)
	}
	if err := chmodExistingDir(host, dshHome, 0o700); err != nil {
		return fmt.Errorf("chmod dsh_home: %w", err)
	}
	return nil
}

// chownDshHomeTree gives the agent uid every real entry under dsh_home.
// Open and chown use NOFOLLOW so a symlink cannot retarget the walk.
func chownDshHomeTree(root string, uid, gid int) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return err
	}
	return chownTreeChildren(fd, uid, gid)
}

func chownTreeChildren(dirfd int, uid, gid int) error {
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(dup), "dsh-home")
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." {
			continue
		}
		if err := unix.Fchownat(dirfd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		var info unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if info.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		child, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		err = chownTreeChildren(child, uid, gid)
		closeErr := unix.Close(child)
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func chmodExistingDir(host hostAPI, path string, mode os.FileMode) error {
	if err := refuseSymlinkStat(host, path); err != nil {
		return err
	}
	return host.Chmod(path, mode)
}

func chownExistingDir(host hostAPI, path string, uid, gid int) error {
	if err := refuseSymlinkStat(host, path); err != nil {
		return err
	}
	return host.Chown(path, uid, gid)
}

func refuseSymlinkStat(host hostAPI, path string) error {
	info, err := host.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	return nil
}

func spawnEnv(identity reconciledIdentity, extra map[string]string) []string {
	env := map[string]string{
		"HOME":    identity.Home,
		"USER":    identity.Username,
		"LOGNAME": identity.Username,
		"SHELL":   agentShell,
		"PATH":    "/usr/local/bin:/usr/bin:/bin",
	}
	for key, value := range extra {
		if key == privilegedFDEnv || key == syncTokenEnv || key == listenerUserEnv || droppedChildEnv(key) {
			continue
		}
		if key == "" || strings.Contains(key, "=") || strings.ContainsRune(key, '\x00') {
			continue
		}
		if containsPrivateKey(value) {
			continue
		}
		env[key] = value
	}
	env["HOME"] = identity.Home
	env["USER"] = identity.Username
	env["LOGNAME"] = identity.Username
	env["SHELL"] = agentShell
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}

func appendAuthSock(env []string, socket string) []string {
	filtered := make([]string, 0, len(env)+1)
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if key == sshAuthSockEnv {
			continue
		}
		filtered = append(filtered, item)
	}
	if socket != "" {
		filtered = append(filtered, sshAuthSockEnv+"="+socket)
	}
	return filtered
}

func (s *privilegedState) deliverGrantSocket(req spawnRequest, identity reconciledIdentity) (*runSSHAgent, string, error) {
	held, ok := s.readyHeld(req.Agent)
	if !ok || s.credentials == nil {
		return nil, "", nil
	}
	if err := s.reserveGrantUID(identity.UID, held.ID); err != nil {
		return nil, "", err
	}
	private, _, err := s.credentials.privateKeyForGrant(held.ID)
	if err != nil {
		s.releaseGrantUID(identity.UID)
		return nil, "", err
	}
	runID := filepath.Base(filepath.Clean(req.RunDir))
	agent, err := startRunSSHAgent(s.credentials, runID, int(identity.UID), int(identity.GID), private, identity.Home)
	if err != nil {
		s.releaseGrantUID(identity.UID)
		return nil, "", err
	}
	agent.release = func() { s.releaseGrantUID(identity.UID) }
	return agent, held.ID, nil
}

func (s *privilegedState) reserveGrantUID(uid uint32, grantID string) error {
	if s == nil || grantID == "" {
		return errors.New("grant socket is incomplete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uidGrants == nil {
		s.uidGrants = map[uint32]*uidGrantHold{}
	}
	hold := s.uidGrants[uid]
	if hold == nil {
		s.uidGrants[uid] = &uidGrantHold{grantID: grantID, refs: 1}
		return nil
	}
	if hold.grantID != grantID {
		return errors.New("shared uid cannot receive a second grant socket")
	}
	hold.refs++
	return nil
}

func (s *privilegedState) releaseGrantUID(uid uint32) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hold := s.uidGrants[uid]
	if hold == nil {
		return
	}
	hold.refs--
	if hold.refs <= 0 {
		delete(s.uidGrants, uid)
	}
}

func (c *ipcCoordinator) Spawn(ctx context.Context, req spawnRequest, stdin, stdout, stderr *os.File) (int, error) {
	if stdin == nil || stdout == nil || stderr == nil {
		return 0, errors.New("spawn requires stdin, stdout, and stderr files")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	reply, err := c.roundTrip(ctx, ipcOpSpawn, payload, stdin, stdout, stderr)
	if err != nil {
		return 0, err
	}
	var result spawnReply
	if err := decodeExactJSON(reply.Payload, &result); err != nil {
		return 0, fmt.Errorf("privileged ipc spawn: %w", err)
	}
	if result.PID <= 0 {
		return 0, errors.New("privileged spawn returned no pid")
	}
	return result.PID, nil
}

func (c *ipcCoordinator) Wait(ctx context.Context, pid int) (int, error) {
	payload, err := json.Marshal(waitRequest{PID: pid})
	if err != nil {
		return -1, err
	}
	reply, err := c.roundTrip(ctx, ipcOpWait, payload)
	if err != nil {
		return -1, err
	}
	var result waitReply
	if err := decodeExactJSON(reply.Payload, &result); err != nil {
		return -1, fmt.Errorf("privileged ipc wait: %w", err)
	}
	return result.ExitCode, nil
}

func asSpawner(coordinator privilegedCoordinator) privilegedSpawner {
	spawner, _ := coordinator.(privilegedSpawner)
	return spawner
}
