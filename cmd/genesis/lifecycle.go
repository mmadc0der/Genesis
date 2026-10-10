package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	cloudEventSpecVersion = "1.0"
	sequenceTypeInteger   = "Integer"

	lifecycleTypeAccepted       = "dev.genesis.run.accepted"
	lifecycleTypeStart          = "dev.genesis.run.start"
	lifecycleTypeSessionCreated = "dev.genesis.run.session.created"
	lifecycleTypeTurn           = "dev.genesis.run.turn"
	lifecycleTypeTool           = "dev.genesis.run.tool"
	lifecycleTypeAssistant      = "dev.genesis.run.assistant"
	lifecycleTypeUser           = "dev.genesis.run.user"
	lifecycleTypeStatus         = "dev.genesis.run.status"
	lifecycleTypeSessionEvent   = "dev.genesis.run.session.event"
	lifecycleTypeSDK            = "dev.genesis.run.sdk"
	lifecycleTypeRetry          = "dev.genesis.run.retry"
	lifecycleTypeRestart        = "dev.genesis.run.restart"
	lifecycleTypeChunk          = "dev.genesis.run.chunk"
	lifecycleTypeResult         = "dev.genesis.run.result"
	lifecycleTypeError          = "dev.genesis.run.error"
	lifecycleTypeEnd            = "dev.genesis.run.end"

	originGenesis   = "genesis"
	originPython    = "python"
	originSDKEvent  = "sdk.session.event"
	originSDKStatus = "sdk.session.status"
	originSDKChunk  = "sdk.session.on_chunk"
	originSDKOther  = "sdk.other"

	pythonFrameSessionCreated = "session.created"
	pythonFrameNotification   = "notification"
	pythonFrameResult         = "result"
	pythonFrameEmit           = "emit"

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
	CorrelationID   string          `json:"correlationid,omitempty"`
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
	Event             json.RawMessage `json:"event,omitempty"`
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

type sdkNotice struct {
	Type   string
	Origin string
	Data   map[string]any
	OK     bool
	Apply  bool
}

// isEphemeralEvent returns true for events that should stream live but not be stored on disk.
// Token delta chunks (text-delta, reasoning-delta, tool-call-delta, block-start/end) are ephemeral.
// Usage frames are persistent rollup data; they either write usage.json or stay recorded.
func isEphemeralEvent(eventType string, data any) bool {
	if eventType != lifecycleTypeChunk {
		return false
	}
	if data == nil {
		return true
	}
	raw, ok := data.(json.RawMessage)
	if !ok {
		encoded, err := json.Marshal(data)
		if err != nil {
			return true
		}
		raw = encoded
	}
	// If it's a usage frame, it is not ephemeral (or writes to usage.json)
	if _, ok := parseUsageFrame(raw); ok {
		return false
	}
	return true
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
	eventSubject, _ := event.stringAttribute("subject")
	data := map[string]any{
		"event_id":     eventID,
		"event_source": eventSource,
		"event_type":   eventType,
	}
	if eventSubject != "" {
		data["event_subject"] = eventSubject
	}
	// The text that started the run, so a reader of the journal can see what
	// the agent was asked. It is the same text the agent receives; the journal
	// redacts secrets on append.
	if message, ok := continuationMessage(event); ok {
		data["message"] = clipMessage(message, acceptedMessageLimit)
	}
	return data
}

// acceptedMessageLimit is the most bytes of a trigger's message that the
// accepted event keeps.
const acceptedMessageLimit = 16 * 1024

// clipMessage cuts text to at most limit bytes on a rune boundary.
func clipMessage(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func causeFromEvent(event cloudEvent) (id, source, eventType string) {
	id, _ = event.stringAttribute("id")
	source, _ = event.stringAttribute("source")
	eventType, _ = event.stringAttribute("type")
	return id, source, eventType
}

// mapSDKNotification classifies an SDK notification. ok is false only for the
// skip list: non-usage chunk frames. Every other notification is durable.
// apply is true when the event belongs to this run and existing handlers
// should run after it has been appended.
func mapSDKNotification(sessionID, method string, payload json.RawMessage) sdkNotice {
	switch method {
	case "session.event":
		return mapSessionEvent(sessionID, method, payload)
	case "on_chunk":
		return mapOnChunk(sessionID, payload)
	case "session.status":
		return sdkNotice{
			Type:   lifecycleTypeStatus,
			Origin: originSDKStatus,
			Data:   statusPayload(method, payload),
			OK:     true,
			Apply:  false,
		}
	default:
		return sdkNotice{
			Type:   lifecycleTypeSDK,
			Origin: originSDKOther,
			Data:   sdkMethodPayload(method, payload),
			OK:     true,
			Apply:  false,
		}
	}
}

func mapSessionEvent(sessionID, method string, payload json.RawMessage) sdkNotice {
	origin := originSDKEvent
	raw := sdkEnvelope(method, payload)
	var envelope sdkNotificationPayload
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return sdkNotice{Type: lifecycleTypeSessionEvent, Origin: origin, Data: raw, OK: true, Apply: false}
	}
	bound := sessionID != "" && envelope.SessionID == sessionID
	noteForeignSession(raw, sessionID, envelope.SessionID)

	var sessionEvent sdkSessionEvent
	if err := json.Unmarshal(envelope.Event, &sessionEvent); err != nil {
		return sdkNotice{Type: lifecycleTypeSessionEvent, Origin: origin, Data: raw, OK: true, Apply: false}
	}

	switch sessionEvent.Type {
	case "turn/start":
		return sdkNotice{Type: lifecycleTypeTurn, Origin: origin, Data: turnPayload("start", sessionEvent, raw), OK: true, Apply: bound}
	case "turn/end":
		body := turnPayload("end", sessionEvent, raw)
		if kind, failure, failed := reasonFailure(sessionEvent.Data); kind != "" {
			body["reason_kind"] = kind
			if failed {
				body["turn_failure"] = true
				if message, ok := failure["message"].(string); ok {
					body["error_message"] = message
				}
				if code, ok := failure["code"].(string); ok {
					body["error_code"] = code
				}
				if status, ok := jsonNumberInt(failure["status"]); ok {
					body["error_status"] = status
				}
			}
		}
		return sdkNotice{Type: lifecycleTypeTurn, Origin: origin, Data: body, OK: true, Apply: bound}
	case "tool/call":
		return sdkNotice{Type: lifecycleTypeTool, Origin: origin, Data: toolPayload("call", sessionEvent, raw), OK: true, Apply: bound}
	case "tool/result":
		return sdkNotice{Type: lifecycleTypeTool, Origin: origin, Data: toolPayload("result", sessionEvent, raw), OK: true, Apply: bound}
	case "assistant/message":
		return sdkNotice{Type: lifecycleTypeAssistant, Origin: origin, Data: assistantPayload("message", sessionEvent, raw), OK: true, Apply: bound}
	case "assistant/attempt":
		return sdkNotice{Type: lifecycleTypeAssistant, Origin: origin, Data: assistantPayload("attempt", sessionEvent, raw), OK: true, Apply: bound}
	case "user/message":
		return sdkNotice{Type: lifecycleTypeUser, Origin: origin, Data: userPayload(sessionEvent, raw), OK: true, Apply: bound}
	case "llm/retry":
		return sdkNotice{Type: lifecycleTypeRetry, Origin: origin, Data: retryPayload(sessionEvent, raw), OK: true, Apply: bound}
	default:
		return sdkNotice{Type: lifecycleTypeSessionEvent, Origin: origin, Data: sessionEventPayload(sessionEvent, raw), OK: true, Apply: false}
	}
}

func sdkEnvelope(method string, payload json.RawMessage) map[string]any {
	return map[string]any{
		"method":  method,
		"payload": decodeJSONValue(payload),
	}
}

func decodeJSONValue(payload json.RawMessage) any {
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return string(payload)
	}
	return decoded
}

func sdkMethodPayload(method string, payload json.RawMessage) map[string]any {
	decoded := decodeJSONValue(payload)
	body := map[string]any{
		"method":   method,
		"sdk_type": method,
	}
	if fields, ok := decoded.(map[string]any); ok {
		for key, value := range fields {
			body[key] = value
		}
		if sid, ok := fields["sessionId"].(string); ok && sid != "" {
			body["sdk_session_id"] = sid
		}
	} else {
		body["payload"] = decoded
	}
	return body
}

func statusPayload(method string, payload json.RawMessage) map[string]any {
	body := sdkMethodPayload(method, payload)
	if status, ok := body["status"].(string); ok && status != "" {
		body["status"] = status
	}
	return body
}

// noteForeignSession records the session id the payload actually carries when
// it is not this run's session. It does not copy that id onto the run.
func noteForeignSession(raw map[string]any, runSession, payloadSession string) {
	if payloadSession == "" || payloadSession == runSession {
		return
	}
	raw["sdk_session_id"] = payloadSession
}

func mapOnChunk(sessionID string, payload json.RawMessage) sdkNotice {
	var frame map[string]any
	if err := json.Unmarshal(payload, &frame); err != nil {
		return sdkNotice{
			Type:   lifecycleTypeSDK,
			Origin: originSDKChunk,
			Data:   sdkMethodPayload("on_chunk", payload),
			OK:     true,
			Apply:  false,
		}
	}
	sid, _ := frame["sessionId"].(string)
	bound := sessionID != "" && sid == sessionID
	frameType, _ := frame["type"].(string)
	switch frameType {
	case "start", "chunk", "end":
	default:
		body := sdkMethodPayload("on_chunk", payload)
		noteForeignSession(body, sessionID, sid)
		return sdkNotice{Type: lifecycleTypeSDK, Origin: originSDKChunk, Data: body, OK: true, Apply: false}
	}
	decoded := decodeJSONValue(payload)
	rawBody, _ := decoded.(map[string]any)
	if rawBody == nil {
		rawBody = map[string]any{"value": decoded}
	}
	body := map[string]any{
		"frame": frameType,
		"raw": map[string]any{
			"payload": rawBody,
		},
	}
	if attempt, ok := frame["attemptId"].(string); ok && attempt != "" {
		body["attempt_id"] = attempt
	}
	chunkType := ""
	if frameType == "chunk" {
		if chunk, ok := frame["chunk"].(map[string]any); ok {
			if kind, ok := chunk["type"].(string); ok && kind != "" {
				chunkType = kind
				body["chunk_type"] = kind
			}
		}
	}
	noteForeignSession(body, sessionID, sid)
	if !bound && chunkType != "usage" {
		return sdkNotice{Origin: originSDKChunk, OK: false, Apply: false}
	}
	return sdkNotice{Type: lifecycleTypeChunk, Origin: originSDKChunk, Data: body, OK: true, Apply: bound}
}

func turnPayload(phase string, event sdkSessionEvent, raw map[string]any) map[string]any {
	payload := map[string]any{
		"phase": phase,
		"raw":   raw,
	}
	if seq := decodeSDKSeq(event.Seq); seq != nil {
		payload["sdk_seq"] = seq
	}
	liftSession(payload, raw)
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
		for _, key := range []string{"callId", "toolCallId", "tool_call_id", "id"} {
			if id, ok := data[key].(string); ok && id != "" {
				payload["tool_call_id"] = id
				break
			}
		}
		if phase == "call" {
			if args, ok := data["arguments"].(string); ok {
				payload["arguments"] = args
			}
		}
		if phase == "result" {
			payload["output"] = toolResultOutput(data)
		}
	}
	liftSession(payload, raw)
	return payload
}

func toolResultOutput(data map[string]any) string {
	message, _ := data["message"].(map[string]any)
	content, _ := message["content"].([]any)
	var output strings.Builder
	for _, part := range content {
		block, _ := part.(map[string]any)
		if block["type"] != "tool-result" {
			continue
		}
		inner, _ := block["content"].([]any)
		for _, item := range inner {
			segment, _ := item.(map[string]any)
			if segment["type"] == "text" {
				if text, ok := segment["text"].(string); ok {
					output.WriteString(text)
				}
			}
		}
	}
	return output.String()
}

func liftSession(payload, raw map[string]any) {
	sid, _ := raw["sdk_session_id"].(string)
	if sid == "" {
		return
	}
	payload["sdk_session_id"] = sid
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

func assistantPayload(phase string, event sdkSessionEvent, raw map[string]any) map[string]any {
	payload := map[string]any{
		"phase": phase,
		"raw":   raw,
	}
	if seq := decodeSDKSeq(event.Seq); seq != nil {
		payload["sdk_seq"] = seq
	}
	liftSession(payload, raw)
	return payload
}

func userPayload(event sdkSessionEvent, raw map[string]any) map[string]any {
	payload := map[string]any{
		"phase":    "message",
		"sdk_type": "user/message",
		"raw":      raw,
	}
	if seq := decodeSDKSeq(event.Seq); seq != nil {
		payload["sdk_seq"] = seq
	}
	liftSession(payload, raw)
	return payload
}

func sessionEventPayload(event sdkSessionEvent, raw map[string]any) map[string]any {
	payload := map[string]any{
		"sdk_type": event.Type,
		"raw":      raw,
	}
	if seq := decodeSDKSeq(event.Seq); seq != nil {
		payload["sdk_seq"] = seq
	}
	liftSession(payload, raw)
	return payload
}

func retryPayload(event sdkSessionEvent, raw map[string]any) map[string]any {
	payload := map[string]any{"raw": raw}
	if seq := decodeSDKSeq(event.Seq); seq != nil {
		payload["sdk_seq"] = seq
	}
	liftSession(payload, raw)
	var data map[string]any
	if len(event.Data) == 0 || json.Unmarshal(event.Data, &data) != nil {
		return payload
	}
	if retry, ok := jsonNumberInt(data["retry"]); ok {
		payload["retry"] = retry
	}
	failure, _ := data["failure"].(map[string]any)
	if failure == nil {
		return payload
	}
	if message, ok := failure["message"].(string); ok {
		payload["message"] = message
	}
	if code, ok := failure["code"].(string); ok {
		payload["code"] = code
	}
	if status, ok := jsonNumberInt(failure["status"]); ok {
		payload["status"] = status
	}
	return payload
}

func reasonFailure(raw json.RawMessage) (kind string, failure map[string]any, ok bool) {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", nil, false
	}
	reason, _ := data["reason"].(map[string]any)
	if reason == nil {
		return "", nil, false
	}
	kind, _ = reason["kind"].(string)
	failure, _ = reason["error"].(map[string]any)
	return kind, failure, kind == "error" && failure != nil
}

func jsonNumberInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		integer, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(integer), true
	default:
		return 0, false
	}
}

func journalTurnFailure(raw json.RawMessage) (string, bool) {
	var data map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
		return "", false
	}
	return mappedTurnFailure(data)
}

func mappedTurnFailure(data map[string]any) (message string, ok bool) {
	if data == nil {
		return "", false
	}
	failed, _ := data["turn_failure"].(bool)
	if !failed {
		return "", false
	}
	message, _ = data["error_message"].(string)
	return message, true
}

func transportFailure(state streamState) bool {
	if state.result.FinishReason != nil && *state.result.FinishReason == endStateCompleted {
		return false
	}
	if state.turnFailureCode == transportErrorCode {
		return true
	}
	return diagnosticsErrorCode(state.result.Diagnostics) == transportErrorCode
}

func restartSessionID(state streamState, journal *runJournal, document invocation) string {
	if state.result.DeepSeekSessionID != nil && *state.result.DeepSeekSessionID != "" {
		return *state.result.DeepSeekSessionID
	}
	if journal != nil && journal.session() != "" {
		return journal.session()
	}
	return document.SessionID
}

func diagnosticsErrorCode(diagnostics any) string {
	root, ok := diagnostics.(map[string]any)
	if !ok || root == nil {
		return ""
	}
	if code := turnEndErrorCode(root["turn_end"]); code != "" {
		return code
	}
	events, _ := root["events"].([]any)
	for index := len(events) - 1; index >= 0; index-- {
		if code := turnEndErrorCode(events[index]); code != "" {
			return code
		}
	}
	return ""
}

func turnEndErrorCode(value any) string {
	event, ok := value.(map[string]any)
	if !ok || event == nil {
		return ""
	}
	if eventType, _ := event["type"].(string); eventType != "" && eventType != "turn/end" {
		return ""
	}
	data, _ := event["data"].(map[string]any)
	reason, _ := data["reason"].(map[string]any)
	if reason == nil {
		return ""
	}
	if kind, _ := reason["kind"].(string); kind != "" && kind != "error" {
		return ""
	}
	failure, _ := reason["error"].(map[string]any)
	if failure == nil {
		return ""
	}
	code, _ := failure["code"].(string)
	return code
}

func diagnosticsTurnFailure(diagnostics any) (string, bool) {
	root, ok := diagnostics.(map[string]any)
	if !ok || root == nil {
		return "", false
	}
	if message, found := turnEndFailureMessage(root["turn_end"]); found {
		return message, true
	}
	events, _ := root["events"].([]any)
	for index := len(events) - 1; index >= 0; index-- {
		if message, found := turnEndFailureMessage(events[index]); found {
			return message, true
		}
	}
	return "", false
}

func turnEndFailureMessage(value any) (string, bool) {
	event, ok := value.(map[string]any)
	if !ok || event == nil {
		return "", false
	}
	if eventType, _ := event["type"].(string); eventType != "" && eventType != "turn/end" {
		return "", false
	}
	data, _ := event["data"].(map[string]any)
	reason, _ := data["reason"].(map[string]any)
	if reason == nil {
		return "", false
	}
	if kind, _ := reason["kind"].(string); kind != "error" {
		return "", false
	}
	failure, ok := reason["error"].(map[string]any)
	if !ok || failure == nil {
		return "", false
	}
	message, _ := failure["message"].(string)
	return message, true
}

func isFinishWrapper(err *runnerFailure) bool {
	if err == nil || err.Message == "" {
		return false
	}
	return strings.Contains(err.Message, "DeepSeek run finished with finish_reason=")
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
	case pythonFrameSessionCreated, pythonFrameNotification, pythonFrameResult, pythonFrameEmit:
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
