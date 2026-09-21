package main

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func runLaunch(logger *slog.Logger, listen, agentsDir, rulesDir, syncToken string) {
	if syncToken == "" {
		generated, err := generateSyncToken()
		if err != nil {
			fail(logger, "generate sync token", err)
		}
		syncToken = generated
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

	command := exec.Command(executable, "listen",
		"-listen", listen,
		"-agents", absoluteAgentsDir,
		"-rules", absoluteRulesDir,
	)
	command.Env = launchChildEnv(syncToken)
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

	logger.Info("genesis orchestrator started",
		"pid", os.Getpid(),
		"listener_pid", command.Process.Pid,
		"address", listen,
		"agents", absoluteAgentsDir,
		"rules", absoluteRulesDir,
	)

	go servePrivilegedParent(parent, logger)
	code := superviseListener(command, logger)
	_ = parent.Close()
	os.Exit(code)
}

func launchChildEnv(syncToken string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == privilegedFDEnv || key == syncTokenEnv {
			continue
		}
		env = append(env, item)
	}
	return append(env,
		syncTokenEnv+"="+syncToken,
		privilegedFDEnv+"=3",
	)
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

func generateSyncToken() (string, error) {
	return newIPCRequestID()
}
