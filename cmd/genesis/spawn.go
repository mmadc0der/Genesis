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
)

type spawnRequest struct {
	Agent   string            `json:"agent"`
	User    string            `json:"user"`
	Cwd     string            `json:"cwd"`
	Home    string            `json:"home"`
	RunDir  string            `json:"run_dir"`
	DshHome string            `json:"dsh_home"`
	Env     map[string]string `json:"env"`
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
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

type privilegedState struct {
	logger       *slog.Logger
	host         hostAPI
	mutate       bool
	pythonPath   string
	source       string
	listenerUser string
	dataDir      string

	mu      sync.Mutex
	users   map[string]reconciledIdentity
	spawned map[int]*spawnedChild
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

	command := exec.Command(s.pythonPath, "-c", s.source)
	command.Dir = identity.Cwd
	command.Env = spawnEnv(identity, req.Env)
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
	if err := command.Start(); err != nil {
		return 0, err
	}
	child := &spawnedChild{cmd: command, done: make(chan struct{})}
	s.mu.Lock()
	if s.spawned == nil {
		s.spawned = map[int]*spawnedChild{}
	}
	s.spawned[command.Process.Pid] = child
	s.mu.Unlock()
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
	}
	s.mu.Lock()
	s.spawned = map[int]*spawnedChild{}
	s.mu.Unlock()
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
	if dshHome != filepath.Join(runDir, dshHomeDirName) {
		return errors.New("dsh_home must be the dsh_home directory inside run_dir")
	}
	uid := int(identity.UID)
	gid := int(identity.GID)
	for _, path := range []string{dataDir, filepath.Join(dataDir, runsDirName), runDir} {
		if err := chmodExistingDir(host, path, 0o711); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	if err := chownExistingDir(host, dshHome, uid, gid); err != nil {
		return fmt.Errorf("chown dsh_home: %w", err)
	}
	if err := chmodExistingDir(host, dshHome, 0o700); err != nil {
		return fmt.Errorf("chmod dsh_home: %w", err)
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
		if key == privilegedFDEnv || key == syncTokenEnv || key == listenerUserEnv {
			continue
		}
		if key == "" || strings.Contains(key, "=") || strings.ContainsRune(key, '\x00') {
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
