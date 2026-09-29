package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	jobStatusStarting = "starting"
	jobStatusRunning  = "running"
	jobStatusExited   = "exited"
)

type usageError struct {
	msg string
}

func (e usageError) Error() string { return e.msg }

type jobState struct {
	ID        string          `json:"id"`
	Command   []string        `json:"command"`
	Dir       string          `json:"dir"`
	Emit      json.RawMessage `json:"emit,omitempty"`
	Status    string          `json:"status"`
	PID       int             `json:"pid,omitempty"`
	ExitCode  *int            `json:"exit_code,omitempty"`
	EmitError string          `json:"emit_error,omitempty"`
	Created   time.Time       `json:"created"`
	Ended     *time.Time      `json:"ended,omitempty"`
}

func runJob(logger *slog.Logger, args []string) {
	if err := dispatchJob(args); err != nil {
		exitCLI(logger, "job", err)
	}
}

func dispatchJob(args []string) error {
	if len(args) == 0 {
		return usageError{msg: "genesis job requires start, list, logs, or stop"}
	}
	switch args[0] {
	case "start":
		id, err := jobStart(args[1:])
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, id)
		return nil
	case "list":
		if len(args) != 1 {
			return usageError{msg: "genesis job list takes no arguments"}
		}
		text, err := jobList()
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stdout, text)
		return nil
	case "logs":
		if len(args) != 2 {
			return usageError{msg: "genesis job logs <id>"}
		}
		return jobLogs(args[1])
	case "stop":
		if len(args) != 2 {
			return usageError{msg: "genesis job stop <id>"}
		}
		return jobStop(args[1])
	case "supervise":
		if len(args) != 2 {
			return usageError{msg: "genesis job supervise <dir>"}
		}
		return jobSupervise(args[1])
	default:
		return usageError{msg: "genesis job requires start, list, logs, or stop"}
	}
}

func jobStart(args []string) (string, error) {
	flags := flag.NewFlagSet("job start", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	idFlag := flags.String("id", "", "job id; default is generated")
	emitFlag := flags.String("emit", "", "CloudEvent JSON posted when the command exits")
	if err := flags.Parse(args); err != nil {
		return "", usageError{msg: err.Error()}
	}
	command := flags.Args()
	if len(command) == 0 {
		return "", usageError{msg: "genesis job start requires a command after --"}
	}
	emit, err := parseOptionalEmit(*emitFlag)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(*idFlag)
	if id == "" {
		id, err = newCLIID("job_")
		if err != nil {
			return "", err
		}
	}
	if err := validateCLIID(id); err != nil {
		return "", err
	}
	workdir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(workdir) {
		return "", errors.New("job working directory must be absolute")
	}
	argv := append([]string(nil), command...)
	resolved, err := exec.LookPath(argv[0])
	if err != nil {
		return "", fmt.Errorf("command: %w", err)
	}
	argv[0] = resolved

	dir, err := prepareJobDir(id)
	if err != nil {
		return "", err
	}
	state := jobState{
		ID:      id,
		Command: argv,
		Dir:     workdir,
		Emit:    emit,
		Status:  jobStatusStarting,
		Created: time.Now().UTC(),
	}
	if err := writeJobState(dir, state); err != nil {
		return "", err
	}
	if _, err := startDetached("job", "supervise", dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return id, nil
}

func parseOptionalEmit(raw string) (json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	return parseEmitObject(raw)
}

func jobList() (string, error) {
	root, err := jobsRoot()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var b strings.Builder
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := readJobState(filepath.Join(root, entry.Name()))
		if err != nil {
			return "", err
		}
		exit := "-"
		if state.ExitCode != nil {
			exit = strconv.Itoa(*state.ExitCode)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\n", state.ID, state.Status, exit, strings.Join(state.Command, " "), state.EmitError)
	}
	return b.String(), nil
}

func jobLogs(id string) error {
	dir, err := existingJobDir(id)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "log")
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("job %s has no log yet", id)
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(os.Stdout, file)
	return err
}

func jobStop(id string) error {
	dir, err := existingJobDir(id)
	if err != nil {
		return err
	}
	state, err := readJobState(dir)
	if err != nil {
		return err
	}
	if state.Status == jobStatusExited {
		return nil
	}
	deadline := time.Now().Add(10 * time.Second)
	pid := state.PID
	for pid <= 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		state, err = readJobState(dir)
		if err != nil {
			return err
		}
		if state.Status == jobStatusExited {
			return nil
		}
		pid = state.PID
	}
	if pid <= 1 {
		return fmt.Errorf("job %s has no supervisor pid", id)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	for time.Now().Before(deadline) {
		state, err = readJobState(dir)
		if err != nil {
			return err
		}
		if state.Status == jobStatusExited {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("job %s did not exit", id)
}

func jobSupervise(dir string) error {
	dir = filepath.Clean(dir)
	state, err := readJobState(dir)
	if err != nil {
		return err
	}
	if len(state.Command) == 0 {
		return errors.New("job command is empty")
	}
	sigs := make(chan os.Signal, 1)
	signalNotify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signalStop(sigs)

	state.Status = jobStatusRunning
	state.PID = os.Getpid()
	if err := writeJobState(dir, state); err != nil {
		return err
	}

	logFile, err := openPrivateLog(filepath.Join(dir, "log"))
	if err != nil {
		return finishJob(dir, state, -1, err)
	}
	defer logFile.Close()

	command := exec.Command(state.Command[0], state.Command[1:]...)
	command.Dir = state.Dir
	command.Env = agentProcessEnv()
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var childPid atomic.Int64
	stopped := atomic.Bool{}
	go func() {
		<-sigs
		stopped.Store(true)
		if pid := int(childPid.Load()); pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
	}()

	if err := command.Start(); err != nil {
		return finishJob(dir, state, -1, err)
	}
	childPid.Store(int64(command.Process.Pid))
	if stopped.Load() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
	waitErr := command.Wait()
	return finishJob(dir, state, exitStatus(waitErr, command), nil)
}

func finishJob(dir string, state jobState, code int, startErr error) error {
	now := time.Now().UTC()
	state.Status = jobStatusExited
	state.ExitCode = &code
	state.Ended = &now
	event, err := jobExitEvent(state, code, now)
	if err != nil {
		state.EmitError = err.Error()
	} else if postErr := postCloudEvent(event); postErr != nil {
		state.EmitError = postErr.Error()
	}
	if startErr != nil && state.EmitError == "" {
		state.EmitError = startErr.Error()
	}
	if writeErr := writeJobState(dir, state); writeErr != nil {
		return writeErr
	}
	if startErr != nil {
		return startErr
	}
	return nil
}

func jobExitEvent(state jobState, code int, now time.Time) (cloudEvent, error) {
	source := defaultAgentSource()
	if source == "" {
		source = jobSourceURN
	}
	return composeCloudEvent(state.Emit, map[string]any{
		"specversion": cloudEventSpecVersion,
		"id":          state.ID + "-" + strconv.FormatInt(now.Unix(), 10),
		"source":      source,
		"type":        jobExitedType,
		"subject":     state.ID,
	}, map[string]any{
		"time":            now.Format(time.RFC3339Nano),
		"datacontenttype": "application/json",
	}, map[string]any{
		"job":       state.ID,
		"exit_code": code,
		"command":   state.Command,
	})
}

func prepareJobDir(id string) (string, error) {
	root, err := jobsRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, id)
	if _, err := os.Lstat(dir); err == nil {
		return "", fmt.Errorf("job %s already exists", id)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := mkdirPrivate(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func existingJobDir(id string) (string, error) {
	if err := validateCLIID(id); err != nil {
		return "", err
	}
	root, err := jobsRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, id)
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("job %s not found", id)
		}
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("job %s is not a real directory", id)
	}
	return dir, nil
}

func jobsRoot() (string, error) {
	root, err := genesisStateDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "jobs")
	if err := mkdirPrivate(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func jobStatePath(dir string) string {
	return filepath.Join(dir, "state.json")
}

func writeJobState(dir string, state jobState) error {
	return writeJSONFile(jobStatePath(dir), state)
}

func readJobState(dir string) (jobState, error) {
	var state jobState
	if err := readJSONFile(jobStatePath(dir), &state); err != nil {
		return jobState{}, err
	}
	if err := validateCLIID(state.ID); err != nil {
		return jobState{}, err
	}
	return state, nil
}

func openPrivateLog(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, dataFileMode)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(dataFileMode); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func newCLIID(prefix string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}

func validateCLIID(id string) error {
	if id == "" || len(id) > 64 || strings.Contains(id, "/") || strings.Contains(id, `\`) || id == "." || id == ".." {
		return fmt.Errorf("invalid id %q", id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("invalid id %q", id)
		}
	}
	return nil
}

func exitCLI(logger *slog.Logger, command string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error(command, "error", err)
	var usage usageError
	if errors.As(err, &usage) {
		os.Exit(2)
	}
	os.Exit(1)
}

// signalNotify and signalStop are vars so tests can observe supervise without
// installing a process-wide handler. Production uses the real signal package.
var signalNotify = func(c chan<- os.Signal, signals ...os.Signal) {
	// assigned in init via the real implementation below
}

var signalStop = func(c chan<- os.Signal) {}

func notifyOS(c chan<- os.Signal, signals ...os.Signal) {
	signal.Notify(c, signals...)
}

func stopOS(c chan<- os.Signal) {
	signal.Stop(c)
}

func init() {
	signalNotify = notifyOS
	signalStop = stopOS
}
