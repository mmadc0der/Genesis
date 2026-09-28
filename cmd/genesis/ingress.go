package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	agentFinishedType   = "dev.genesis.agent.finished"
	sessionContinueType = "dev.genesis.session.continue"
	genesisEmitMethod   = "genesis.emit"
	oracleAgentID       = "oracle"
	// maxContinuationHops is the farthest hop index a run may have.
	// The chain root is hop 0 and its first continuation is hop 1.
	// A new run at hop 9 is refused.
	maxContinuationHops     = 8
	continuationRuleName    = "session.continue"
	continuationRefusedType = "ContinuationRefused"
	syncInProgressType      = "SyncInProgress"
	outcomeOK               = "ok"
	outcomeError            = "error"
)

// statusWriter records the status dispatchCloudEvent would return on POST /events.
type statusWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (w *statusWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *statusWriter) Write(payload []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.body.Write(payload)
}

// dispatchIngress is the in-process path for POST /events. The returned
// status is the same code the HTTP handler would write.
func (s *eventServer) dispatchIngress(event cloudEvent) int {
	writer := &statusWriter{}
	s.dispatchCloudEvent(writer, event)
	if writer.code == 0 {
		return http.StatusOK
	}
	return writer.code
}

// skipFinishedSelf is the self-loop brake for dev.genesis.agent.finished.
// A matching rule whose agent id is the event subject is the agent that
// just finished. Other matching rules still run.
func skipFinishedSelf(candidate rule, event cloudEvent) bool {
	eventType, ok := event.stringAttribute("type")
	if !ok || eventType != agentFinishedType {
		return false
	}
	subject, ok := event.stringAttribute("subject")
	if !ok || subject == "" {
		return false
	}
	return candidate.Agent == subject
}

func agentSource(agentID string) string {
	return "urn:genesis:agent:" + agentID
}

func stampCause(event, cause cloudEvent) {
	if event == nil || cause == nil {
		return
	}
	id, source, eventType := causeFromEvent(cause)
	putStringAttribute(event, "causeid", id)
	putStringAttribute(event, "causesource", source)
	putStringAttribute(event, "causetype", eventType)
}

func putStringAttribute(event cloudEvent, key, value string) {
	if event == nil || key == "" {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	event[key] = raw
}

func newAgentFinishedEvent(agentID, runID, outcome, transcript string) (cloudEvent, error) {
	if agentID == "" || strings.Contains(agentID, "/") {
		return nil, errors.New("finished agent id must be one path segment")
	}
	if outcome != outcomeOK && outcome != outcomeError {
		return nil, errors.New("finished outcome must be ok or error")
	}
	if transcript == "" {
		return nil, errors.New("finished transcript path is required")
	}
	id, err := newLifecycleEventID()
	if err != nil {
		return nil, err
	}
	return marshalCloudEvent(map[string]any{
		"specversion": cloudEventSpecVersion,
		"id":          id,
		"source":      agentSource(agentID),
		"type":        agentFinishedType,
		"subject":     agentID,
		"data": map[string]string{
			"runid":      runID,
			"agent":      agentID,
			"outcome":    outcome,
			"transcript": transcript,
		},
	})
}

// agentEmitEvent builds an ingress CloudEvent from a session emit.
// The runner stamps id and source. type dev.genesis.agent.finished is reserved.
func agentEmitEvent(agentID string, raw json.RawMessage) (cloudEvent, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("emit event must be a JSON object")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("emit event must be a JSON object")
	}
	eventType, ok := jsonStringField(body, "type")
	if !ok || strings.TrimSpace(eventType) == "" {
		return nil, errors.New("emit type is required")
	}
	if eventType == agentFinishedType {
		return nil, errors.New("type dev.genesis.agent.finished is reserved")
	}
	subject, ok := jsonStringField(body, "subject")
	if !ok || strings.TrimSpace(subject) == "" {
		return nil, errors.New("emit subject is required")
	}
	id, err := newLifecycleEventID()
	if err != nil {
		return nil, err
	}
	fields := map[string]any{
		"specversion": cloudEventSpecVersion,
		"id":          id,
		"source":      agentSource(agentID),
		"type":        eventType,
		"subject":     subject,
	}
	if rawData, exists := body["data"]; exists && len(bytes.TrimSpace(rawData)) > 0 {
		if bytes.Equal(bytes.TrimSpace(rawData), []byte("null")) {
			return nil, errors.New("emit data must be a JSON object")
		}
		var generic any
		if err := json.Unmarshal(rawData, &generic); err != nil {
			return nil, errors.New("emit data must be a JSON object")
		}
		if _, isObject := generic.(map[string]any); !isObject {
			return nil, errors.New("emit data must be a JSON object")
		}
		fields["data"] = json.RawMessage(append([]byte(nil), rawData...))
	}
	return marshalCloudEvent(fields)
}

func jsonStringField(body map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := body[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func marshalCloudEvent(fields map[string]any) (cloudEvent, error) {
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxEventBytes {
		return nil, fmt.Errorf("CloudEvent exceeds %d bytes", maxEventBytes)
	}
	var event cloudEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	return event, nil
}
