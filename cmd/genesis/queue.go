package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

const queueFileName = "queue.json"

// queuedRun is the accepted run waiting for its own agent's slot.
// Secret values are not stored. They are copied back from the listener
// when the run is started, including after a restart.
type queuedRun struct {
	Event         cloudEvent        `json:"event"`
	Rule          string            `json:"rule"`
	Agent         string            `json:"agent"`
	RunID         string            `json:"run_id"`
	User          string            `json:"user,omitempty"`
	Cwd           string            `json:"cwd"`
	Home          string            `json:"home"`
	Instructions  string            `json:"instructions"`
	Env           map[string]string `json:"env,omitempty"`
	RunDir        string            `json:"run_dir"`
	DshHome       string            `json:"dsh_home,omitempty"`
	SessionID     string            `json:"session_id,omitempty"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	ContinuedFrom string            `json:"continued_from,omitempty"`
	UserMessage   string            `json:"user_message,omitempty"`
	Git           string            `json:"git,omitempty"`
	Credential    string            `json:"credential,omitempty"`
	SecretNames   []string          `json:"secret_names,omitempty"`
	acceptedAt    string
}

func queuedRunPath(runDir string) string {
	return filepath.Join(runDir, queueFileName)
}

func hasQueuedRun(runDir string) bool {
	info, err := os.Lstat(queuedRunPath(runDir))
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func writeQueuedRun(runDir string, document invocation, secretNames []string) error {
	if runDir == "" {
		return errors.New("run directory is required")
	}
	env := make(map[string]string, len(document.Env))
	secret := map[string]struct{}{}
	for _, name := range secretNames {
		if name != "" {
			secret[name] = struct{}{}
		}
	}
	for key, value := range document.Env {
		if _, skip := secret[key]; skip {
			continue
		}
		env[key] = value
	}
	names := append([]string(nil), secretNames...)
	slices.Sort(names)
	return writeJSONFile(queuedRunPath(runDir), queuedRun{
		Event:         document.Event,
		Rule:          document.Rule,
		Agent:         document.Agent,
		RunID:         document.RunID,
		User:          document.User,
		Cwd:           document.Cwd,
		Home:          document.Home,
		Instructions:  document.Instructions,
		Env:           env,
		RunDir:        runDir,
		DshHome:       document.DshHome,
		SessionID:     document.SessionID,
		CorrelationID: document.CorrelationID,
		ContinuedFrom: document.ContinuedFrom,
		UserMessage:   document.UserMessage,
		Git:           document.Git,
		Credential:    document.Credential,
		SecretNames:   names,
	})
}

func readQueuedRun(runDir string) (queuedRun, error) {
	path := queuedRunPath(runDir)
	info, err := os.Lstat(path)
	if err != nil {
		return queuedRun{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return queuedRun{}, fmt.Errorf("%s is not a regular file", path)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return queuedRun{}, err
	}
	var record queuedRun
	if err := json.Unmarshal(payload, &record); err != nil {
		return queuedRun{}, fmt.Errorf("decode queued run: %w", err)
	}
	if record.RunDir == "" {
		record.RunDir = runDir
	}
	return record, nil
}

func removeQueuedRun(runDir string) error {
	if runDir == "" {
		return nil
	}
	err := os.Remove(queuedRunPath(runDir))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

func (record queuedRun) invocation(secrets map[string]string) invocation {
	env := make(map[string]string, len(record.Env)+len(record.SecretNames))
	for key, value := range record.Env {
		env[key] = value
	}
	for _, name := range record.SecretNames {
		if value := secrets[name]; value != "" {
			env[name] = value
		}
	}
	return invocation{
		Event:         record.Event,
		Rule:          record.Rule,
		Agent:         record.Agent,
		RunID:         record.RunID,
		User:          record.User,
		Cwd:           record.Cwd,
		Home:          record.Home,
		Instructions:  record.Instructions,
		Env:           env,
		RunDir:        record.RunDir,
		DshHome:       record.DshHome,
		SessionID:     record.SessionID,
		CorrelationID: record.CorrelationID,
		ContinuedFrom: record.ContinuedFrom,
		UserMessage:   record.UserMessage,
		Git:           record.Git,
		Credential:    record.Credential,
	}
}

func (s *runStore) listQueued() ([]queuedRun, error) {
	runs := filepath.Join(s.dataDir, runsDirName)
	entries, err := os.ReadDir(runs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var queued []queuedRun
	for _, entry := range entries {
		if !entry.IsDir() || validateRunID(entry.Name()) != nil {
			continue
		}
		runDir := filepath.Join(runs, entry.Name())
		if !hasQueuedRun(runDir) {
			continue
		}
		events, err := readJournalPrefix(filepath.Join(runDir, eventsFileName))
		if err != nil || len(events) == 0 || journalHasEnd(events) || journalHasStart(events) {
			continue
		}
		record, err := readQueuedRun(runDir)
		if err != nil {
			s.logger.Error("read queued run", "run_id", entry.Name(), "error", err)
			continue
		}
		record.acceptedAt = events[0].Time
		queued = append(queued, record)
	}
	slices.SortFunc(queued, func(left, right queuedRun) int {
		if left.acceptedAt != right.acceptedAt {
			if left.acceptedAt < right.acceptedAt {
				return -1
			}
			return 1
		}
		if left.RunID < right.RunID {
			return -1
		}
		if left.RunID > right.RunID {
			return 1
		}
		return 0
	})
	return queued, nil
}

func (s *runStore) reopenRun(runID string) error {
	if err := validateRunID(runID); err != nil {
		return err
	}
	s.mu.Lock()
	if _, open := s.journals[runID]; open {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	runDir := filepath.Join(s.dataDir, runsDirName, runID)
	events, err := readJournalPrefix(filepath.Join(runDir, eventsFileName))
	if err != nil {
		return err
	}
	if len(events) == 0 || journalHasEnd(events) {
		return fmt.Errorf("queued run %s is not open", runID)
	}
	file, err := os.OpenFile(filepath.Join(runDir, eventsFileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, dataFileMode)
	if err != nil {
		return err
	}
	first := events[0]
	correlationID := first.CorrelationID
	if correlationID == "" {
		correlationID = runID
	}
	dshHome := stableDshHome(s.dataDir, runID)
	if record, err := readSessionRecord(filepath.Join(runDir, sessionFileName)); err == nil && record.DshHome != "" {
		dshHome = record.DshHome
	}
	journal := &runJournal{
		runID:         runID,
		agent:         first.AgentID,
		rule:          first.Rulefile,
		causeID:       first.CauseID,
		causeSrc:      first.CauseSource,
		causeType:     first.CauseType,
		correlationID: correlationID,
		sessionID:     lastSessionID(events),
		seq:           lastSequence(events),
		runDir:        runDir,
		dshHome:       dshHome,
		file:          file,
		bus:           s.bus,
		redactor:      newRedactor(nil),
		now:           s.now,
		newID:         s.newID,
	}
	s.mu.Lock()
	if existing, open := s.journals[runID]; open {
		s.mu.Unlock()
		_ = journal.Close()
		_ = existing
		return nil
	}
	s.journals[runID] = journal
	s.mu.Unlock()
	return nil
}

func (s *eventServer) restoreQueuedRuns() error {
	if s == nil || s.store == nil {
		return nil
	}
	queued, err := s.store.listQueued()
	if err != nil {
		return err
	}
	for _, record := range queued {
		document := record.invocation(s.secrets)
		if s.generation == nil {
			s.finishQueued(document, "generation is not loaded")
			continue
		}
		definition, ok := s.generation.agents[document.Agent]
		if !ok {
			s.finishQueued(document, "queued agent is not in the active generation")
			continue
		}
		if err := s.store.reopenRun(document.RunID); err != nil {
			s.log().Error("reopen queued run", "run_id", document.RunID, "error", err)
			continue
		}
		if journal := s.store.journal(document.RunID); journal != nil {
			journal.redactor = newRedactor(secretValues(s.secrets, record.SecretNames, document.Env))
		}
		s.slots.start(document.Agent, definition.parallelLimit(), func() {
			_ = removeQueuedRun(document.RunDir)
			defer s.store.release(document.RunID)
			s.runner.Run(document)
		})
	}
	return nil
}

func (s *eventServer) finishQueued(document invocation, message string) {
	if err := s.store.reopenRun(document.RunID); err != nil {
		s.log().Error("finish queued run", "run_id", document.RunID, "error", err)
		_ = removeQueuedRun(document.RunDir)
		return
	}
	journal := s.store.journal(document.RunID)
	if journal != nil {
		_ = journal.Publish(lifecycleTypeError, originGenesis, errorPayload(runnerErrorType, message))
		_ = journal.Publish(lifecycleTypeEnd, originGenesis, endPayload(-1, endStateFailed))
	}
	_ = removeQueuedRun(document.RunDir)
	s.store.release(document.RunID)
}

func writeJSONFile(path string, value any) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temp := path + ".tmp"
	if err := os.WriteFile(temp, payload, dataFileMode); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := os.Chmod(temp, dataFileMode); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
