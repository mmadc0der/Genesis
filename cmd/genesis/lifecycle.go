package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

const (
	cloudEventSpecVersion = "1.0"
	sequenceTypeInteger   = "Integer"

	lifecycleTypeAccepted       = "dev.genesis.run.accepted"
	lifecycleTypeStart          = "dev.genesis.run.start"
	lifecycleTypeSessionCreated = "dev.genesis.run.session.created"
	lifecycleTypeTurn           = "dev.genesis.run.turn"
	lifecycleTypeTool           = "dev.genesis.run.tool"
	lifecycleTypeResult         = "dev.genesis.run.result"
	lifecycleTypeError          = "dev.genesis.run.error"
	lifecycleTypeEnd            = "dev.genesis.run.end"

	originGenesis   = "genesis"
	originPython    = "python"
	originSDKEvent  = "sdk.session.event"
	originSDKStatus = "sdk.session.status"
	originSDKOther  = "sdk.other"

	pythonFrameSessionCreated = "session.created"
	pythonFrameNotification   = "notification"
	pythonFrameResult         = "result"

	endStateCompleted = "completed"
	endStateFailed    = "failed"
)

type lifecycleEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject,omitempty"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype,omitempty"`
	Sequence        string          `json:"sequence"`
	SequenceType    string          `json:"sequencetype"`
	RunID           string          `json:"runid"`
	AgentID         string          `json:"agentid"`
	Rulefile        string          `json:"rulefile"`
	SessionID       string          `json:"sessionid,omitempty"`
	CauseID         string          `json:"causeid"`
	CauseSource     string          `json:"causesource"`
	CauseType       string          `json:"causetype,omitempty"`
	Origin          string          `json:"origin"`
	Data            json.RawMessage `json:"data,omitempty"`
}

type pythonFrame struct {
	V                 int             `json:"v"`
	Type              string          `json:"type"`
	RunID             string          `json:"run_id"`
	SessionID         string          `json:"session_id"`
	Method            string          `json:"method"`
	Payload           json.RawMessage `json:"payload"`
	DeepSeekSessionID *string         `json:"deepseek_session_id"`
	FinishReason      *string         `json:"finish_reason"`
	FinalResponse     *string         `json:"final_response"`
	Error             *runnerFailure  `json:"error"`
	Diagnostics       any             `json:"diagnostics"`
}

type sdkNotificationPayload struct {
	SessionID string          `json:"sessionId"`
	Event     json.RawMessage `json:"event"`
	Status    string          `json:"status"`
}

type sdkSessionEvent struct {
	Type string          `json:"type"`
	Seq  json.RawMessage `json:"seq"`
	Data json.RawMessage `json:"data"`
}

func genesisSource(runID string) string {
	return "urn:genesis:run:" + runID
}

func eventTime(now time.Time) string {
	return now.UTC().Format("2006-01-02T15:04:05.000000Z")
}

func marshalData(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return payload
}

func acceptedData(event cloudEvent) map[string]any {
	eventID, _ := event.stringAttribute("id")
	eventSource, _ := event.stringAttribute("source")
	eventType, _ := event.stringAttribute("type")
	return map[string]any{
		"event_id":     eventID,
		"event_source": eventSource,
		"event_type":   eventType,
	}
}

func causeFromEvent(event cloudEvent) (id, source, eventType string) {
	id, _ = event.stringAttribute("id")
	source, _ = event.stringAttribute("source")
	eventType, _ = event.stringAttribute("type")
	return id, source, eventType
}

func mapSDKNotification(sessionID, method string, payload json.RawMessage) (eventType, origin string, data map[string]any, ok bool) {
	switch method {
	case "session.event":
		origin = originSDKEvent
	case "session.status":
		return "", originSDKStatus, nil, false
	default:
		return "", originSDKOther, nil, false
	}

	var envelope sdkNotificationPayload
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", origin, nil, false
	}
	if sessionID == "" || envelope.SessionID != sessionID {
		return "", originSDKOther, nil, false
	}

	var sessionEvent sdkSessionEvent
	if err := json.Unmarshal(envelope.Event, &sessionEvent); err != nil {
		return "", origin, nil, false
	}

	var decodedPayload any
	if err := json.Unmarshal(payload, &decodedPayload); err != nil {
		decodedPayload = json.RawMessage(payload)
	}
	raw := map[string]any{
		"method":  method,
		"payload": decodedPayload,
	}
	switch sessionEvent.Type {
	case "turn/start":
		return lifecycleTypeTurn, origin, turnPayload("start", sessionEvent, raw), true
	case "turn/end":
		body := turnPayload("end", sessionEvent, raw)
		if kind := reasonKind(sessionEvent.Data); kind != "" {
			body["reason_kind"] = kind
		}
		return lifecycleTypeTurn, origin, body, true
	case "tool/call":
		return lifecycleTypeTool, origin, toolPayload("call", sessionEvent, raw), true
	case "tool/result":
		return lifecycleTypeTool, origin, toolPayload("result", sessionEvent, raw), true
	default:
		return "", origin, nil, false
	}
}

func turnPayload(phase string, event sdkSessionEvent, raw map[string]any) map[string]any {
	payload := map[string]any{
		"phase": phase,
		"raw":   raw,
	}
	if seq := decodeSDKSeq(event.Seq); seq != nil {
		payload["sdk_seq"] = seq
	}
	return payload
}

func toolPayload(phase string, event sdkSessionEvent, raw map[string]any) map[string]any {
	payload := map[string]any{
		"phase": phase,
		"raw":   raw,
	}
	if seq := decodeSDKSeq(event.Seq); seq != nil {
		payload["sdk_seq"] = seq
	}
	var data map[string]any
	if len(event.Data) > 0 && json.Unmarshal(event.Data, &data) == nil {
		if name, ok := data["name"].(string); ok && name != "" {
			payload["name"] = name
		}
		for _, key := range []string{"toolCallId", "tool_call_id", "id"} {
			if id, ok := data[key].(string); ok && id != "" {
				payload["tool_call_id"] = id
				break
			}
		}
	}
	return payload
}

func decodeSDKSeq(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if integer, err := number.Int64(); err == nil {
			return integer
		}
		if floating, err := number.Float64(); err == nil {
			return floating
		}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	return nil
}

func reasonKind(raw json.RawMessage) string {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	reason, _ := data["reason"].(map[string]any)
	kind, _ := reason["kind"].(string)
	return kind
}

func resultPayload(result runnerResult) map[string]any {
	return map[string]any{
		"finish_reason":       optionalString(result.FinishReason),
		"final_response":      optionalString(result.FinalResponse),
		"deepseek_session_id": optionalString(result.DeepSeekSessionID),
	}
}

func errorPayload(errorType, message string) map[string]any {
	return map[string]any{
		"error_type": errorType,
		"message":    message,
	}
}

func endPayload(exitCode int, state string) map[string]any {
	return map[string]any{
		"exit_code": exitCode,
		"state":     state,
	}
}

func parsePythonFrame(line []byte) (pythonFrame, error) {
	var frame pythonFrame
	if err := json.Unmarshal(line, &frame); err != nil {
		return pythonFrame{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if frame.V != 1 {
		return pythonFrame{}, fmt.Errorf("unknown frame version %s", formatFrameVersion(frame.V))
	}
	switch frame.Type {
	case pythonFrameSessionCreated, pythonFrameNotification, pythonFrameResult:
	default:
		return pythonFrame{}, fmt.Errorf("unknown frame type %q", frame.Type)
	}
	return frame, nil
}

func formatFrameVersion(version int) string {
	if version == 0 {
		return "0"
	}
	return strconv.Itoa(version)
}

func (f pythonFrame) result() runnerResult {
	return runnerResult{
		DeepSeekSessionID: f.DeepSeekSessionID,
		FinishReason:      f.FinishReason,
		FinalResponse:     f.FinalResponse,
		Error:             f.Error,
		Diagnostics:       f.Diagnostics,
	}
}
