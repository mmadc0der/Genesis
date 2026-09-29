package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	minScheduleInterval = time.Minute
	maxScheduleInterval = 168 * time.Hour
	schedulerPIDName    = "scheduler.pid"
	schedulerLockName   = "scheduler.lock"
)

var errSchedulerBusy = errors.New("scheduler is already running")

type scheduleState struct {
	ID        string          `json:"id"`
	Each      string          `json:"each"`
	Event     json.RawMessage `json:"event"`
	NextDue   time.Time       `json:"next_due"`
	Created   time.Time       `json:"created"`
	LastFire  *time.Time      `json:"last_fire,omitempty"`
	LastError string          `json:"last_error,omitempty"`
}

func runSchedule(logger *slog.Logger, args []string) {
	if err := dispatchSchedule(args); err != nil {
		exitCLI(logger, "schedule", err)
	}
}

func dispatchSchedule(args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "list":
			if len(args) != 1 {
				return usageError{msg: "genesis schedule list takes no arguments"}
			}
			text, err := scheduleList()
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, text)
			return nil
		case "cancel":
			if len(args) != 2 {
				return usageError{msg: "genesis schedule cancel <id>"}
			}
			return scheduleCancel(args[1])
		case "run":
			if len(args) != 1 {
				return usageError{msg: "genesis schedule run takes no arguments"}
			}
			return runSchedulerProcess()
		default:
			return usageError{msg: "genesis schedule: unknown command"}
		}
	}
	id, err := scheduleCreate(args)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, id)
	return nil
}

func scheduleCreate(args []string) (string, error) {
	flags := flag.NewFlagSet("schedule", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	eachFlag := flags.String("each", "", "repeat interval, for example 15m")
	emitFlag := flags.String("emit", "", "one CloudEvent JSON object")
	if err := flags.Parse(args); err != nil {
		return "", usageError{msg: err.Error()}
	}
	if flags.NArg() != 0 {
		return "", usageError{msg: "genesis schedule --each=<duration> --emit=<CloudEvent>"}
	}
	if strings.TrimSpace(*eachFlag) == "" || strings.TrimSpace(*emitFlag) == "" {
		return "", usageError{msg: "genesis schedule requires --each and --emit"}
	}
	every, err := parseEach(*eachFlag)
	if err != nil {
		return "", err
	}
	event, err := parseEmitObject(*emitFlag)
	if err != nil {
		return "", err
	}
	if err := requireEmitType(event); err != nil {
		return "", err
	}
	id, err := newCLIID("sch_")
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	state := scheduleState{
		ID:      id,
		Each:    every.String(),
		Event:   event,
		NextDue: now.Add(every),
		Created: now,
	}
	if err := writeSchedule(state); err != nil {
		return "", err
	}
	if err := notifyScheduler(true); err != nil {
		return "", err
	}
	return id, nil
}

func scheduleList() (string, error) {
	specs, err := loadSchedules()
	if err != nil {
		return "", err
	}
	if len(specs) > 0 {
		if err := notifyScheduler(true); err != nil {
			return "", err
		}
	}
	var b strings.Builder
	for _, spec := range specs {
		eventType, subject := scheduleLabels(spec.Event)
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\n", spec.ID, spec.Each, spec.NextDue.UTC().Format(time.RFC3339), eventType, subject, spec.LastError)
	}
	return b.String(), nil
}

func scheduleCancel(id string) error {
	if err := validateCLIID(id); err != nil {
		return err
	}
	path, err := schedulePath(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("schedule %s not found", id)
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	remaining, err := loadSchedules()
	if err != nil {
		return err
	}
	return notifyScheduler(len(remaining) > 0)
}

func parseEach(raw string) (time.Duration, error) {
	every, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("--each: %w", err)
	}
	if every < minScheduleInterval || every > maxScheduleInterval || every%time.Second != 0 {
		return 0, fmt.Errorf("--each must be a whole number of seconds from %s to %s", minScheduleInterval, maxScheduleInterval)
	}
	return every, nil
}

func requireEmitType(event json.RawMessage) error {
	var object map[string]any
	if err := json.Unmarshal(event, &object); err != nil {
		return errors.New("--emit must be one JSON object")
	}
	text, _ := object["type"].(string)
	if strings.TrimSpace(text) == "" {
		return errors.New("--emit type is required")
	}
	return nil
}

func scheduleLabels(event json.RawMessage) (string, string) {
	var object map[string]any
	if err := json.Unmarshal(event, &object); err != nil {
		return "", ""
	}
	eventType, _ := object["type"].(string)
	subject, _ := object["subject"].(string)
	return eventType, subject
}

// runSchedulerProcess is the detached ticker. It is not a rules.d interval
// clock. Each due schedule POSTs one CloudEvent to the listener /events path.
func runSchedulerProcess() error {
	dir, err := genesisStateDir()
	if err != nil {
		return err
	}
	lock, err := lockScheduler(dir)
	if errors.Is(err, errSchedulerBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()

	sigs := make(chan os.Signal, 4)
	signalNotify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signalStop(sigs)
	pidPath := filepath.Join(dir, schedulerPIDName)
	if err := writePID(pidPath, os.Getpid()); err != nil {
		return err
	}
	defer os.Remove(pidPath)
	return schedulerLoop(sigs)
}

func schedulerLoop(sigs <-chan os.Signal) error {
	for {
		specs, err := loadSchedules()
		if err != nil {
			return err
		}
		if len(specs) == 0 {
			return nil
		}
		wait := time.Until(specs[0].NextDue)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			_ = fireDueSchedules(time.Now())
		case sig := <-sigs:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if sig == syscall.SIGTERM || sig == syscall.SIGINT {
				return nil
			}
		}
	}
}

func fireDueSchedules(now time.Time) error {
	specs, err := loadSchedules()
	if err != nil {
		return err
	}
	var first error
	for _, spec := range specs {
		if spec.NextDue.After(now) {
			continue
		}
		if err := fireSchedule(&spec, now); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func fireSchedule(spec *scheduleState, now time.Time) error {
	event, err := scheduleFireEvent(*spec, now)
	if err != nil {
		spec.LastError = err.Error()
	} else if err = postCloudEvent(event); err != nil {
		spec.LastError = err.Error()
	} else {
		spec.LastError = ""
		fired := now.UTC()
		spec.LastFire = &fired
	}
	every, parseErr := parseEach(spec.Each)
	if parseErr != nil {
		if spec.LastError == "" {
			spec.LastError = parseErr.Error()
		}
		every = minScheduleInterval
	}
	next := spec.NextDue.Add(every)
	if !next.After(now) {
		// Missed more than one slot while down. Catch up once, then wait a full interval.
		next = now.Add(every)
	}
	spec.NextDue = next.UTC()
	if writeErr := writeSchedule(*spec); writeErr != nil {
		return writeErr
	}
	return err
}

func scheduleFireEvent(spec scheduleState, now time.Time) (cloudEvent, error) {
	source := defaultAgentSource()
	if source == "" {
		source = scheduleSourceURN
	}
	return composeCloudEvent(spec.Event, map[string]any{
		"specversion": cloudEventSpecVersion,
		"id":          spec.ID + "-" + strconv.FormatInt(now.Unix(), 10),
		"source":      source,
		"type":        scheduleFiredType,
		"subject":     spec.ID,
	}, map[string]any{
		"time":            now.UTC().Format(time.RFC3339Nano),
		"datacontenttype": "application/json",
	}, map[string]any{
		"schedule": spec.ID,
		"each":     spec.Each,
	})
}

func notifyScheduler(startIfIdle bool) error {
	if pid, alive := liveScheduler(); alive {
		if err := syscall.Kill(pid, syscall.SIGHUP); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	if !startIfIdle {
		return nil
	}
	if _, err := startDetached("schedule", "run"); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, alive := liveScheduler(); alive {
			if err := syscall.Kill(pid, syscall.SIGHUP); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("scheduler did not start")
}

func liveScheduler() (int, bool) {
	dir, err := genesisStateDir()
	if err != nil {
		return 0, false
	}
	pid, err := readPID(filepath.Join(dir, schedulerPIDName))
	if err != nil || !processAlive(pid) {
		return 0, false
	}
	return pid, true
}

func lockScheduler(dir string) (*os.File, error) {
	path := filepath.Join(dir, schedulerLockName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, dataFileMode)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errSchedulerBusy
		}
		return nil, err
	}
	return file, nil
}

func loadSchedules() ([]scheduleState, error) {
	dir, err := schedulesRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	specs := make([]scheduleState, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var spec scheduleState
		if err := readJSONFile(filepath.Join(dir, entry.Name()), &spec); err != nil {
			return nil, err
		}
		if err := validateCLIID(spec.ID); err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].NextDue.Equal(specs[j].NextDue) {
			return specs[i].ID < specs[j].ID
		}
		return specs[i].NextDue.Before(specs[j].NextDue)
	})
	return specs, nil
}

func writeSchedule(spec scheduleState) error {
	path, err := schedulePath(spec.ID)
	if err != nil {
		return err
	}
	return writeJSONFile(path, spec)
}

func schedulePath(id string) (string, error) {
	if err := validateCLIID(id); err != nil {
		return "", err
	}
	dir, err := schedulesRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

func schedulesRoot() (string, error) {
	root, err := genesisStateDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "schedules")
	if err := mkdirPrivate(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func writePID(path string, pid int) error {
	return writeJSONFile(path, pid)
}

func readPID(path string) (int, error) {
	var pid int
	if err := readJSONFile(path, &pid); err != nil {
		return 0, err
	}
	if pid <= 1 {
		return 0, errors.New("scheduler pid is missing")
	}
	return pid, nil
}

func processAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func startDetached(args ...string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	dir, err := genesisStateDir()
	if err != nil {
		return 0, err
	}
	command := exec.Command(exe, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer devnull.Close()
	logFile, err := openPrivateLog(filepath.Join(dir, "detach.log"))
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	command.Stdin = devnull
	command.Stdout = logFile
	command.Stderr = logFile
	command.Env = agentProcessEnv()
	if err := command.Start(); err != nil {
		return 0, err
	}
	go func() { _ = command.Wait() }()
	return command.Process.Pid, nil
}
