package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *eventServer) dispatchSessionContinue(w http.ResponseWriter, event cloudEvent, current *generation) {
	if s.runner == nil {
		s.log().Error("runner is not configured")
		http.Error(w, "runner is unavailable", http.StatusInternalServerError)
		return
	}
	if s.store == nil {
		s.log().Error("run storage is not configured")
		http.Error(w, "run storage is unavailable", http.StatusInternalServerError)
		return
	}

	subject, _ := event.stringAttribute("subject")
	if err := validateRunID(subject); err != nil {
		s.refuseContinuation("", errors.New("continuation subject is invalid"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	message, ok := continuationMessage(event)
	if !ok {
		s.refuseContinuation(subject, errors.New("continuation message is required"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cited, err := s.store.loadCitedSession(subject)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.refuseContinuation(subject, errors.New("continuation run is unknown"))
		} else {
			s.refuseContinuation(subject, err)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !cited.Ended {
		s.refuseContinuation(subject, errors.New("continuation run has not ended"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	emitter, ok := continuationEmitter(event)
	if !ok || (emitter != oracleAgentID && emitter != cited.Agent) {
		s.refuseContinuation(subject, errors.New("continuation emitter is not allowed"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	definition, ok := current.agents[cited.Agent]
	if !ok {
		s.refuseContinuation(subject, fmt.Errorf("continuation agent %q is not in the active generation", cited.Agent))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	record := cited.Record
	if record.SessionID == "" || record.DshHome == "" || record.CorrelationID == "" || record.Cwd == "" {
		s.refuseContinuation(subject, errors.New("continuation session record is incomplete"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if filepath.Clean(definition.Cwd) != filepath.Clean(record.Cwd) {
		s.refuseContinuation(subject, errors.New("continuation cwd does not match the agent"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := validateStableDshHome(s.store.dataDir, record.DshHome); err != nil {
		s.refuseContinuation(subject, err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	info, err := os.Stat(record.DshHome)
	if err != nil || !info.IsDir() {
		s.refuseContinuation(subject, errors.New("continuation home is missing"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	hops, err := s.store.continuationHops(subject)
	if err != nil {
		s.refuseContinuation(subject, err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if hops >= maxContinuationHops {
		s.refuseContinuation(subject, fmt.Errorf("continuation would be hop %d", hops+1))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	putStringAttribute(event, "correlationid", record.CorrelationID)
	newRunID := s.newRunID
	if newRunID == nil {
		newRunID = newGenesisRunID
	}
	runID, err := newRunID()
	if err != nil {
		s.log().Error("create Genesis run ID", "error", err)
		http.Error(w, "failed to create run ID", http.StatusInternalServerError)
		return
	}
	document := snapshotInvocation(event, rule{name: continuationRuleName, Agent: definition.id}, definition, runID, s.secrets)
	document.SessionID = record.SessionID
	document.DshHome = record.DshHome
	document.CorrelationID = record.CorrelationID
	document.ContinuedFrom = subject
	document.UserMessage = message
	if err := s.store.Accept(&document, secretValues(s.secrets, definition.Secrets, document.Env)); err != nil {
		s.log().Error("create run storage", "run_id", runID, "error", err)
		http.Error(w, "failed to create run storage", http.StatusInternalServerError)
		return
	}
	s.startAgent(document, definition.parallelLimit(), definition.Secrets)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		Runs []acceptedRun `json:"runs"`
	}{Runs: []acceptedRun{{
		Rule:  continuationRuleName,
		Agent: definition.id,
		RunID: runID,
	}}})
}

func (s *eventServer) refuseContinuation(runID string, reason error) {
	if reason == nil {
		reason = errors.New("continuation refused")
	}
	s.log().Info("session continuation refused", "run_id", runID, "error", reason.Error())
	if s.store == nil || runID == "" {
		return
	}
	journal := s.store.journal(runID)
	if journal == nil || !journal.writable() {
		return
	}
	_ = journal.Publish(lifecycleTypeError, originGenesis, errorPayload(continuationRefusedType, reason.Error()))
}

func continuationEmitter(event cloudEvent) (string, bool) {
	source, ok := event.stringAttribute("source")
	if !ok {
		return "", false
	}
	const prefix = "urn:genesis:agent:"
	if !strings.HasPrefix(source, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(source, prefix)
	if !agentIDPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

func continuationMessage(event cloudEvent) (string, bool) {
	raw, ok := event["data"]
	if !ok {
		return "", false
	}
	var data struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", false
	}
	if strings.TrimSpace(data.Message) == "" {
		return "", false
	}
	return data.Message, true
}
