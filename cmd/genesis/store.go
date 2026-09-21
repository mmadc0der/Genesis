package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	runsDirName      = "runs"
	eventsFileName   = "events.jsonl"
	stderrFileName   = "stderr.log"
	resultFileName   = "result.json"
	dshHomeDirName   = "dsh_home"
	dataDirMode      = 0o700
	dataFileMode     = 0o600
	maxFrameBytes    = 16 << 20
	interruptedType  = "Interrupted"
	interruptedMsg   = "interrupted"
	payloadTooLarge  = "PayloadTooLarge"
	protocolError    = "ProtocolError"
	processErrorType = "ProcessError"
	runnerErrorType  = "RunnerError"
	diskErrorType    = "StorageError"
)

type runStore struct {
	dataDir string
	bus     *eventBus
	logger  *slog.Logger
	now     func() time.Time
	newID   func() (string, error)
	alive   func(int) bool

	mu       sync.Mutex
	journals map[string]*runJournal
}

type runJournal struct {
	mu        sync.Mutex
	runID     string
	agent     string
	rule      string
	causeID   string
	causeSrc  string
	causeType string
	sessionID string
	seq       uint64
	runDir    string
	dshHome   string
	file      *os.File
	bus       *eventBus
	redactor  *redactor
	now       func() time.Time
	newID     func() (string, error)
	ended     bool
}

func prepareDataDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("data directory is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve data directory: %w", err)
	}
	if !filepath.IsAbs(absolute) {
		return "", errors.New("data directory must be an absolute path")
	}
	info, err := os.Stat(absolute)
	if err == nil && !info.IsDir() {
		return "", fmt.Errorf("data path %q is not a directory", absolute)
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	runs := filepath.Join(absolute, runsDirName)
	if err := os.MkdirAll(runs, dataDirMode); err != nil {
		return "", fmt.Errorf("create data directory: %w", err)
	}
	if err := os.Chmod(absolute, dataDirMode); err != nil {
		return "", fmt.Errorf("chmod data directory: %w", err)
	}
	if err := os.Chmod(runs, dataDirMode); err != nil {
		return "", fmt.Errorf("chmod runs directory: %w", err)
	}
	return absolute, nil
}

func newRunStore(dataDir string, bus *eventBus, logger *slog.Logger) *runStore {
	if logger == nil {
		logger = slog.Default()
	}
	if bus == nil {
		bus = newEventBus()
	}
	return &runStore{
		dataDir:  dataDir,
		bus:      bus,
		logger:   logger,
		now:      time.Now,
		newID:    newLifecycleEventID,
		alive:    pidAlive,
		journals: map[string]*runJournal{},
	}
}

func (s *runStore) Accept(inv *invocation, secretValues []string) error {
	if inv == nil {
		return errors.New("invocation is required")
	}
	if err := validateRunID(inv.RunID); err != nil {
		return err
	}
	runDir := filepath.Join(s.dataDir, runsDirName, inv.RunID)
	dshHome := filepath.Join(runDir, dshHomeDirName)
	if err := os.MkdirAll(dshHome, dataDirMode); err != nil {
		return fmt.Errorf("create run directory: %w", err)
	}
	if err := os.Chmod(runDir, dataDirMode); err != nil {
		return fmt.Errorf("chmod run directory: %w", err)
	}
	if err := os.Chmod(dshHome, dataDirMode); err != nil {
		return fmt.Errorf("chmod dsh_home: %w", err)
	}

	eventsPath := filepath.Join(runDir, eventsFileName)
	file, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, dataFileMode)
	if err != nil {
		return fmt.Errorf("create events journal: %w", err)
	}
	causeID, causeSource, causeType := causeFromEvent(inv.Event)
	journal := &runJournal{
		runID:     inv.RunID,
		agent:     inv.Agent,
		rule:      inv.Rule,
		causeID:   causeID,
		causeSrc:  causeSource,
		causeType: causeType,
		runDir:    runDir,
		dshHome:   dshHome,
		file:      file,
		bus:       s.bus,
		redactor:  newRedactor(secretValues),
		now:       s.now,
		newID:     s.newID,
	}
	if err := journal.Publish(lifecycleTypeAccepted, originGenesis, acceptedData(inv.Event)); err != nil {
		_ = file.Close()
		return err
	}

	inv.RunDir = runDir
	inv.DshHome = dshHome

	s.mu.Lock()
	s.journals[inv.RunID] = journal
	s.mu.Unlock()
	return nil
}

func (s *runStore) journal(runID string) *runJournal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journals[runID]
}

func (s *runStore) release(runID string) {
	s.mu.Lock()
	journal := s.journals[runID]
	delete(s.journals, runID)
	s.mu.Unlock()
	if journal != nil {
		_ = journal.Close()
	}
}

func (s *runStore) failAccept(runID, message string) {
	journal := s.journal(runID)
	if journal == nil {
		return
	}
	_ = journal.Publish(lifecycleTypeError, originGenesis, errorPayload(diskErrorType, message))
	_ = journal.Publish(lifecycleTypeEnd, originGenesis, endPayload(-1, endStateFailed))
	s.release(runID)
}

func (s *runStore) Recover() error {
	runs := filepath.Join(s.dataDir, runsDirName)
	entries, err := os.ReadDir(runs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := s.recoverRun(entry.Name()); err != nil {
			s.logger.Error("recover run journal", "run_id", entry.Name(), "error", err)
		}
	}
	return nil
}

func (s *runStore) recoverRun(runID string) error {
	if err := validateRunID(runID); err != nil {
		return nil
	}
	runDir := filepath.Join(s.dataDir, runsDirName, runID)
	eventsPath := filepath.Join(runDir, eventsFileName)
	if err := dropTornTail(eventsPath); err != nil {
		return err
	}
	events, err := readJournalPrefix(eventsPath)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	last := events[len(events)-1]
	if last.Type == lifecycleTypeEnd {
		return nil
	}

	pid := startPID(events)
	if pid > 0 && s.alive(pid) {
		s.logger.Info("orphaned Genesis run left untouched",
			"genesis_run_id", runID,
			"pid", pid,
		)
		return nil
	}

	file, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, dataFileMode)
	if err != nil {
		return err
	}
	first := events[0]
	journal := &runJournal{
		runID:     runID,
		agent:     first.AgentID,
		rule:      first.Rulefile,
		causeID:   first.CauseID,
		causeSrc:  first.CauseSource,
		causeType: first.CauseType,
		sessionID: lastSessionID(events),
		seq:       lastSequence(events),
		runDir:    runDir,
		dshHome:   filepath.Join(runDir, dshHomeDirName),
		file:      file,
		bus:       s.bus,
		redactor:  newRedactor(nil),
		now:       s.now,
		newID:     s.newID,
	}
	if err := journal.Publish(lifecycleTypeError, originGenesis, errorPayload(interruptedType, interruptedMsg)); err != nil {
		_ = journal.Close()
		return err
	}
	if err := journal.Publish(lifecycleTypeEnd, originGenesis, endPayload(-1, endStateFailed)); err != nil {
		_ = journal.Close()
		return err
	}
	return journal.Close()
}

func (j *runJournal) Publish(eventType, origin string, data any) error {
	if j == nil {
		return errors.New("run journal is not open")
	}
	j.mu.Lock()
	if j.file == nil {
		j.mu.Unlock()
		return errors.New("run journal is closed")
	}
	eventID, err := j.newID()
	if err != nil {
		j.mu.Unlock()
		return fmt.Errorf("create lifecycle event ID: %w", err)
	}
	j.seq++
	event := lifecycleEvent{
		SpecVersion:     cloudEventSpecVersion,
		ID:              eventID,
		Source:          genesisSource(j.runID),
		Type:            eventType,
		Subject:         j.runID,
		Time:            eventTime(j.now()),
		DataContentType: "application/json",
		Sequence:        strconv.FormatUint(j.seq, 10),
		SequenceType:    sequenceTypeInteger,
		RunID:           j.runID,
		AgentID:         j.agent,
		Rulefile:        j.rule,
		SessionID:       j.sessionID,
		CauseID:         j.causeID,
		CauseSource:     j.causeSrc,
		CauseType:       j.causeType,
		Origin:          origin,
		Data:            marshalData(data),
	}
	if eventType == lifecycleTypeSessionCreated {
		if payload, ok := data.(map[string]any); ok {
			if sessionID, ok := payload["session_id"].(string); ok {
				j.sessionID = sessionID
				event.SessionID = sessionID
			}
		}
	}
	if eventType == lifecycleTypeEnd {
		j.ended = true
	}
	payload, err := json.Marshal(event)
	if err != nil {
		j.seq--
		j.mu.Unlock()
		return fmt.Errorf("encode lifecycle event: %w", err)
	}
	payload = j.redactor.bytes(payload)
	var published lifecycleEvent
	if err := json.Unmarshal(payload, &published); err != nil {
		j.seq--
		j.mu.Unlock()
		return fmt.Errorf("redacted lifecycle event is not JSON: %w", err)
	}
	if _, err := j.file.Write(append(payload, '\n')); err != nil {
		j.seq--
		j.mu.Unlock()
		return fmt.Errorf("append lifecycle event: %w", err)
	}
	if err := j.file.Sync(); err != nil {
		j.mu.Unlock()
		return fmt.Errorf("flush lifecycle event: %w", err)
	}
	j.mu.Unlock()
	j.bus.publish(published)
	return nil
}

func (j *runJournal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

func (j *runJournal) rememberSession(sessionID string) {
	if j == nil || sessionID == "" {
		return
	}
	j.mu.Lock()
	j.sessionID = sessionID
	j.mu.Unlock()
}

func (j *runJournal) session() string {
	if j == nil {
		return ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.sessionID
}

func writeResultFile(runDir string, result runnerResult, redactor *redactor) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	payload = redactor.bytes(payload)
	tmp := filepath.Join(runDir, resultFileName+".tmp")
	if err := os.WriteFile(tmp, payload, dataFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(runDir, resultFileName))
}

func dropTornTail(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	consumed := 0
	rest := data
	for len(rest) > 0 {
		idx := bytes.IndexByte(rest, '\n')
		var line []byte
		next := len(rest)
		if idx >= 0 {
			line = rest[:idx]
			next = idx + 1
		} else {
			line = rest
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			consumed += next
			rest = rest[next:]
			continue
		}
		if !json.Valid(trimmed) {
			break
		}
		consumed += next
		if idx < 0 {
			break
		}
		rest = rest[next:]
	}
	if consumed == len(data) {
		return nil
	}
	return os.Truncate(path, int64(consumed))
}

func readJournalPrefix(path string) ([]lifecycleEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxFrameBytes)
	var events []lifecycleEvent
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event lifecycleEvent
		if err := json.Unmarshal(line, &event); err != nil {
			break
		}
		events = append(events, event)
	}
	return events, nil
}

func lastSequence(events []lifecycleEvent) uint64 {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Sequence == "" {
			continue
		}
		value, err := strconv.ParseUint(events[i].Sequence, 10, 64)
		if err == nil {
			return value
		}
	}
	return 0
}

func lastSessionID(events []lifecycleEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].SessionID != "" {
			return events[i].SessionID
		}
	}
	return ""
}

func startPID(events []lifecycleEvent) int {
	for _, event := range events {
		if event.Type != lifecycleTypeStart || len(event.Data) == 0 {
			continue
		}
		var payload struct {
			PID int `json:"pid"`
		}
		if err := json.Unmarshal(event.Data, &payload); err == nil && payload.PID > 0 {
			return payload.PID
		}
	}
	return 0
}

func validateRunID(runID string) error {
	if runID == "" {
		return errors.New("run id is required")
	}
	if strings.ContainsRune(runID, os.PathSeparator) || strings.Contains(runID, "..") ||
		strings.ContainsRune(runID, '\x00') {
		return fmt.Errorf("invalid run id %q", runID)
	}
	cleaned := filepath.Base(runID)
	if cleaned != runID {
		return fmt.Errorf("invalid run id %q", runID)
	}
	return nil
}

func newLifecycleEventID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "evt_" + hex.EncodeToString(random[:]), nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

func readCappedLine(reader *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	var buf bytes.Buffer
	for {
		part, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			if buf.Len()+len(part) > max {
				if drainErr := drainLine(reader); drainErr != nil && !errors.Is(drainErr, io.EOF) {
					return nil, true, drainErr
				}
				return nil, true, nil
			}
			buf.Write(part)
			continue
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, false, readErr
		}
		chunk := part
		if n := len(chunk); n > 0 && chunk[n-1] == '\n' {
			chunk = chunk[:n-1]
			if n := len(chunk); n > 0 && chunk[n-1] == '\r' {
				chunk = chunk[:n-1]
			}
		}
		if buf.Len()+len(chunk) > max {
			return nil, true, nil
		}
		if buf.Len() == 0 {
			if errors.Is(readErr, io.EOF) && len(part) == 0 {
				return nil, false, io.EOF
			}
			return bytes.Clone(chunk), false, eofIfComplete(readErr, part)
		}
		buf.Write(chunk)
		return buf.Bytes(), false, eofIfComplete(readErr, part)
	}
}

func drainLine(reader *bufio.Reader) error {
	for {
		part, err := reader.ReadSlice('\n')
		if err == nil || (errors.Is(err, io.EOF) && bytes.Contains(part, []byte{'\n'})) {
			return nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func eofIfComplete(err error, part []byte) error {
	if errors.Is(err, io.EOF) && !bytes.Contains(part, []byte{'\n'}) {
		if len(part) == 0 {
			return io.EOF
		}
		return nil
	}
	return nil
}
