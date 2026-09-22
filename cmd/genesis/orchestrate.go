package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const listenerUserEnv = "GENESIS_LISTENER_USER"

type listenerIdentity struct {
	Username string
	Home     string
	Uid      uint32
	Gid      uint32
	Groups   []uint32
}

func runLaunch(logger *slog.Logger, listen, agentsDir, rulesDir, dataDir, syncToken, listenerUser string) {
	identity, err := resolveListenerIdentity(os.Geteuid(), listenerUser)
	if err != nil {
		fail(logger, "resolve listener user", err)
	}

	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		fail(logger, "find python3 in PATH", err)
	}
	pythonPath, err = filepath.Abs(pythonPath)
	if err != nil {
		fail(logger, "resolve python3 path", err)
	}

	parent, child, err := privilegedSocketpair()
	if err != nil {
		fail(logger, "create privileged socketpair", err)
	}

	executable, err := os.Executable()
	if err != nil {
		child.Close()
		parent.Close()
		fail(logger, "resolve genesis executable", err)
	}
	absoluteAgentsDir, err := filepath.Abs(agentsDir)
	if err != nil {
		child.Close()
		parent.Close()
		fail(logger, "resolve agents directory", err)
	}
	absoluteRulesDir, err := filepath.Abs(rulesDir)
	if err != nil {
		child.Close()
		parent.Close()
		fail(logger, "resolve rules directory", err)
	}
	absoluteDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		child.Close()
		parent.Close()
		fail(logger, "resolve data directory", err)
	}

	listenerName := ""
	if identity != nil {
		listenerName = identity.Username
	}
	state := newPrivilegedState(logger, pythonPath, embeddedPythonRunner, listenerName, absoluteDataDir)
	agents, err := loadAgents(absoluteAgentsDir)
	if err != nil {
		child.Close()
		parent.Close()
		fail(logger, "load agents for privileged reconcile", err)
	}
	if _, err := executePlan(buildPlan(agents, true), state); err != nil {
		child.Close()
		parent.Close()
		fail(logger, "reconcile dedicated agent users", err)
	}

	command := exec.Command(executable, "listen",
		"-listen", listen,
		"-agents", absoluteAgentsDir,
		"-rules", absoluteRulesDir,
		"-data", absoluteDataDir,
	)
	command.Env = launchChildEnv(syncToken, identity)
	applyListenerIdentity(command, identity)
	command.ExtraFiles = []*os.File{child}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	if err := command.Start(); err != nil {
		child.Close()
		parent.Close()
		fail(logger, "start listener", err)
	}
	child.Close()

	attrs := []any{
		"pid", os.Getpid(),
		"uid", os.Geteuid(),
		"listener_pid", command.Process.Pid,
		"address", listen,
		"agents", absoluteAgentsDir,
		"rules", absoluteRulesDir,
		"data", absoluteDataDir,
		"sync_configured", syncToken != "",
	}
	if identity != nil {
		attrs = append(attrs, "listener_user", identity.Username, "listener_uid", identity.Uid)
	} else {
		attrs = append(attrs, "listener_uid", os.Geteuid())
	}
	logger.Info("genesis orchestrator started", attrs...)

	go servePrivilegedParent(parent, logger, state)
	code := superviseListener(command, logger)
	state.killAll()
	_ = parent.Close()
	os.Exit(code)
}

func resolveListenerIdentity(euid int, requested string) (*listenerIdentity, error) {
	if euid == 0 {
		if requested == "" {
			return nil, errors.New("genesis launch as root requires -listener-user or GENESIS_LISTENER_USER")
		}
		return lookupListenerIdentity(requested)
	}
	if requested == "" {
		return nil, nil
	}
	identity, err := lookupListenerIdentity(requested)
	if err != nil {
		return nil, err
	}
	if int(identity.Uid) != euid {
		return nil, fmt.Errorf("genesis launch cannot switch to user %q from uid %d", requested, euid)
	}
	return nil, nil
}

func lookupListenerIdentity(name string) (*listenerIdentity, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("listener user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("listener user %q uid: %w", name, err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("listener user %q gid: %w", name, err)
	}
	identity := &listenerIdentity{
		Username: account.Username,
		Home:     account.HomeDir,
		Uid:      uint32(uid),
		Gid:      uint32(gid),
		Groups:   []uint32{uint32(gid)},
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return identity, nil
	}
	seen := map[uint32]struct{}{identity.Gid: {}}
	for _, raw := range groupIDs {
		value, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			continue
		}
		gid := uint32(value)
		if _, exists := seen[gid]; exists {
			continue
		}
		seen[gid] = struct{}{}
		identity.Groups = append(identity.Groups, gid)
	}
	return identity, nil
}

func applyListenerIdentity(command *exec.Cmd, identity *listenerIdentity) {
	if command == nil || identity == nil {
		return
	}
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Credential = &syscall.Credential{
		Uid:    identity.Uid,
		Gid:    identity.Gid,
		Groups: append([]uint32{}, identity.Groups...),
	}
}

func launchChildEnv(syncToken string, identity *listenerIdentity) []string {
	skip := map[string]struct{}{
		privilegedFDEnv: {},
		syncTokenEnv:    {},
	}
	if identity != nil {
		skip["HOME"] = struct{}{}
		skip["USER"] = struct{}{}
		skip["LOGNAME"] = struct{}{}
		skip["USERNAME"] = struct{}{}
	}
	env := make([]string, 0, len(os.Environ())+5)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, drop := skip[key]; drop {
			continue
		}
		env = append(env, item)
	}
	if syncToken != "" {
		env = append(env, syncTokenEnv+"="+syncToken)
	}
	env = append(env, privilegedFDEnv+"=3")
	if identity != nil {
		env = append(env, "USER="+identity.Username, "LOGNAME="+identity.Username)
		if identity.Home != "" {
			env = append(env, "HOME="+identity.Home)
		}
	}
	return env
}

func superviseListener(command *exec.Cmd, logger *slog.Logger) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()

	select {
	case sig := <-signals:
		logger.Info("genesis orchestrator stopping", "signal", sig.String())
		_ = command.Process.Signal(sig)
		err := <-waited
		return exitCode(err)
	case err := <-waited:
		if err != nil {
			logger.Error("genesis listener exited", "error", err)
		} else {
			logger.Info("genesis listener exited")
		}
		return exitCode(err)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
		return exitErr.ExitCode()
	}
	return 1
}
