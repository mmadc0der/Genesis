package main

import (
	"bytes"
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
	jobSupervisorLock   = "supervisor.lock"
)

var (
	errSchedulerBusy   = errors.New("scheduler is already running")
	errScheduleRemoved = errors.New("schedule was removed")
	errJobBusy         = errors.New("job supervisor is already running")
)

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
	path, err := schedulePath(spec.ID)
	if err != nil {
		return err
	}
	// Hold the existing inode. A rename would recreate the name after cancel
	// unlinked it, and the ticker would keep firing a schedule that is gone.
	file, err := openScheduleFile(path)
	if err != nil {
		if errors.Is(err, errScheduleRemoved) {
			return nil
		}
		return err
	}
	defer file.Close()
	if err := scheduleStillLinked(file); err != nil {
		if errors.Is(err, errScheduleRemoved) {
			return nil
		}
		return err
	}

	event, postErr := scheduleFireEvent(*spec, now)
	if postErr != nil {
		spec.LastError = postErr.Error()
	} else if postErr = postCloudEvent(event); postErr != nil {
		spec.LastError = postErr.Error()
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
	if writeErr := rewriteOpenJSON(file, *spec); writeErr != nil {
		if errors.Is(writeErr, errScheduleRemoved) {
			return nil
		}
		return writeErr
	}
	return postErr
}

func scheduleFireEvent(spec scheduleState, now time.Time) (cloudEvent, error) {
	return stampedCLIEvent(spec.Event, scheduleSourceURN, scheduleFiredType, spec.ID, now, map[string]any{
		"schedule": spec.ID,
		"each":     spec.Each,
	})
}

func notifyScheduler(startIfIdle bool) error {
	if pid, alive := liveScheduler(); alive {
		if err := syscall.Kill(pid, syscall.SIGHUP); err == nil {
			return nil
		}
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
			_ = syscall.Kill(pid, syscall.SIGHUP)
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
	if err != nil || !schedulerProcess(pid) {
		return 0, false
	}
	return pid, true
}

// lockScheduler flocks the state directory, not only scheduler.lock. Unlinking
// the lock file must not let a second schedule run flock a new inode.
func lockScheduler(dir string) (*fileLock, error) {
	return lockHeldDir(dir, schedulerLockName, "scheduler lock must not be a symlink", errSchedulerBusy)
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
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		if err := validateCLIID(stem); err != nil {
			continue
		}
		var spec scheduleState
		if err := readJSONFile(filepath.Join(dir, name), &spec); err != nil {
			continue
		}
		if err := validateCLIID(spec.ID); err != nil || spec.ID != stem {
			continue
		}
		if _, err := parseEach(spec.Each); err != nil {
			continue
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
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return writeJSONFile(path, pid)
}

func readPID(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	file := os.NewFile(uintptr(fd), filepath.Base(path))
	defer file.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return 0, errors.New("scheduler pid is not a regular file")
	}
	payload, err := io.ReadAll(io.LimitReader(file, 64))
	if err != nil {
		return 0, err
	}
	var pid int
	if err := json.Unmarshal(payload, &pid); err != nil {
		return 0, err
	}
	if pid <= 1 {
		return 0, errors.New("scheduler pid is missing")
	}
	return pid, nil
}

func openScheduleFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || os.IsNotExist(err) {
			return nil, errScheduleRemoved
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Base(path))
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		file.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		return nil, errors.New("schedule is not a regular file")
	}
	if st.Nlink == 0 {
		file.Close()
		return nil, errScheduleRemoved
	}
	return file, nil
}

func scheduleStillLinked(file *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return err
	}
	if st.Nlink == 0 {
		return errScheduleRemoved
	}
	return nil
}

func rewriteOpenJSON(file *os.File, value any) error {
	if err := scheduleStillLinked(file); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	fd := int(file.Fd())
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		return err
	}
	if err := writeFull(fd, payload); err != nil {
		return err
	}
	if err := unix.Ftruncate(fd, int64(len(payload))); err != nil {
		return err
	}
	return scheduleStillLinked(file)
}

func writeFull(fd int, payload []byte) error {
	for len(payload) > 0 {
		n, err := unix.Write(fd, payload)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

type fileLock struct {
	dir  *os.File
	file *os.File
}

func (l *fileLock) Close() error {
	if l == nil {
		return nil
	}
	var err error
	if l.dir != nil {
		if uerr := unix.Flock(int(l.dir.Fd()), unix.LOCK_UN); uerr != nil {
			err = uerr
		}
	}
	if l.file != nil {
		if cerr := l.file.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if l.dir != nil {
		if cerr := l.dir.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

func lockHeldDir(dir, name, symlinkMsg string, busy error) (*fileLock, error) {
	parent, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%s must not be a symlink", filepath.Base(dir))
		}
		return nil, err
	}
	if err := unix.Flock(int(parent.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		parent.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, busy
		}
		return nil, err
	}
	release := func() {
		_ = unix.Flock(int(parent.Fd()), unix.LOCK_UN)
		parent.Close()
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, dataFileMode)
	if err != nil {
		release()
		if errors.Is(err, unix.ELOOP) || errors.Is(err, syscall.ELOOP) {
			return nil, errors.New(symlinkMsg)
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	var opened, named unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		file.Close()
		release()
		return nil, err
	}
	if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		file.Close()
		release()
		return nil, err
	}
	if opened.Ino != named.Ino || opened.Dev != named.Dev || opened.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		release()
		return nil, fmt.Errorf("%s changed while locking", name)
	}
	return &fileLock{dir: parent, file: file}, nil
}

func lockJobSupervisor(dir string) (*fileLock, error) {
	return lockHeldDir(dir, jobSupervisorLock, "supervisor lock must not be a symlink", errJobBusy)
}

func schedulerProcess(pid int) bool {
	return commandMatches(pid, "schedule", "run")
}

func jobSupervisorProcess(pid int, dir string) bool {
	if pid <= 1 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	got, err := processCmdline(pid)
	if err != nil || len(got) != 4 {
		return false
	}
	exe, err := os.Executable()
	if err != nil || !sameExecutable(got[0], exe) {
		return false
	}
	return got[1] == "job" && got[2] == "supervise" && filepath.Clean(got[3]) == filepath.Clean(dir)
}

func commandMatches(pid int, args ...string) bool {
	if pid <= 1 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	got, err := processCmdline(pid)
	if err != nil || len(got) != len(args)+1 {
		return false
	}
	exe, err := os.Executable()
	if err != nil || !sameExecutable(got[0], exe) {
		return false
	}
	for i, arg := range args {
		if got[i+1] != arg {
			return false
		}
	}
	return true
}

func processCmdline(pid int) ([]string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 && raw[len(raw)-1] == 0 {
		raw = raw[:len(raw)-1]
	}
	if len(raw) == 0 {
		return nil, errors.New("empty command line")
	}
	parts := bytes.Split(raw, []byte{0})
	args := make([]string, len(parts))
	for i, part := range parts {
		args[i] = string(part)
	}
	return args, nil
}

func sameExecutable(path, exe string) bool {
	resolve := func(p string) string {
		p = filepath.Clean(p)
		if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != "" {
			return filepath.Clean(resolved)
		}
		return p
	}
	return path != "" && exe != "" && resolve(path) == resolve(exe)
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
