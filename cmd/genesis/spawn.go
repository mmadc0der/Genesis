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
	"sync"
	"syscall"
	"time"
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

	mu      sync.Mutex
	users   map[string]reconciledIdentity
	spawned map[int]*spawnedChild
}

func newPrivilegedState(logger *slog.Logger, pythonPath, source, listenerUser string) *privilegedState {
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
	if err := prepareSpawnDirs(s.host, req, identity); err != nil {
		return 0, err
	}

	command := exec.Command(s.pythonPath, "-c", s.source)
	command.Dir = req.Cwd
	command.Env = spawnEnv(identity, req.Env)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 || int(identity.UID) != os.Geteuid() || int(identity.GID) != os.Getegid() {
		command.SysProcAttr.Credential = &syscall.Credential{
			Uid:    identity.UID,
			Gid:    identity.GID,
			Groups: append([]uint32(nil), identity.Groups...),
		}
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

func prepareSpawnDirs(host hostAPI, req spawnRequest, identity reconciledIdentity) error {
	if host == nil {
		return errors.New("privileged host is not configured")
	}
	if err := validateAbsolutePath("run_dir", req.RunDir); err != nil {
		return err
	}
	if err := validateAbsolutePath("dsh_home", req.DshHome); err != nil {
		return err
	}
	if !pathHasPrefix(req.DshHome, req.RunDir) {
		return errors.New("dsh_home must be inside run_dir")
	}
	uid := int(identity.UID)
	gid := int(identity.GID)
	for _, path := range []string{filepath.Dir(filepath.Dir(req.RunDir)), filepath.Dir(req.RunDir), req.RunDir} {
		if err := host.Chmod(path, 0o711); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	if err := host.Chown(req.DshHome, uid, gid); err != nil {
		return fmt.Errorf("chown dsh_home: %w", err)
	}
	if err := host.Chmod(req.DshHome, 0o700); err != nil {
		return fmt.Errorf("chmod dsh_home: %w", err)
	}
	return nil
}

func spawnEnv(identity reconciledIdentity, extra map[string]string) []string {
	env := map[string]string{
		"HOME":    identity.Home,
		"USER":    identity.Username,
		"LOGNAME": identity.Username,
		"SHELL":   agentShell,
		"PATH":    os.Getenv("PATH"),
	}
	if env["PATH"] == "" {
		env["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	for key, value := range extra {
		if key == privilegedFDEnv || key == syncTokenEnv {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	unixConn, err := unixConnOf(c.conn)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	id, err := newIPCRequestID()
	if err != nil {
		return 0, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := unixConn.SetDeadline(deadline); err != nil {
			return 0, err
		}
		defer func() { _ = unixConn.SetDeadline(time.Time{}) }()
	}
	if err := writeIPCMessageWithFDs(unixConn, ipcEnvelope{ID: id, Op: ipcOpSpawn, Payload: payload}, stdin, stdout, stderr); err != nil {
		return 0, err
	}
	var reply ipcEnvelope
	if err := readIPCMessage(unixConn, &reply); err != nil {
		return 0, err
	}
	if reply.ID != id {
		return 0, fmt.Errorf("privileged ipc id mismatch")
	}
	if reply.Error != "" {
		return 0, errors.New(reply.Error)
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
	c.mu.Lock()
	defer c.mu.Unlock()
	payload, err := json.Marshal(waitRequest{PID: pid})
	if err != nil {
		return -1, err
	}
	id, err := newIPCRequestID()
	if err != nil {
		return -1, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.conn.SetDeadline(deadline); err != nil {
			return -1, err
		}
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}
	if err := writeIPCMessage(c.conn, ipcEnvelope{ID: id, Op: ipcOpWait, Payload: payload}); err != nil {
		return -1, err
	}
	var reply ipcEnvelope
	if err := readIPCMessage(c.conn, &reply); err != nil {
		return -1, err
	}
	if reply.ID != id {
		return -1, fmt.Errorf("privileged ipc id mismatch")
	}
	if reply.Error != "" {
		return -1, errors.New(reply.Error)
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
