package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

//go:embed runner.py
var embeddedPythonRunner string

type processRunner struct {
	pythonPath string
	source     string
	logger     *slog.Logger
	store      *runStore
	spawner    privilegedSpawner
	// agentsDir is the listener's agent definition directory. The finish
	// event names the reviewed agent's file so the oracle does not search.
	agentsDir string
	// dispatch is POST /events. Nil in runner unit tests that only journal.
	dispatch func(cloudEvent) int
}

type runnerResult struct {
	DeepSeekSessionID *string        `json:"deepseek_session_id"`
	FinishReason      *string        `json:"finish_reason"`
	FinalResponse     *string        `json:"final_response"`
	Error             *runnerFailure `json:"error"`
	Diagnostics       any            `json:"diagnostics"`
}

type runnerFailure struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type streamState struct {
	gotSession         bool
	gotResult          bool
	result             runnerResult
	failed             bool
	errorPublished     bool
	errorType          string
	errorMsg           string
	turnEndFailure     bool
	turnFailureMessage string
}

func (r processRunner) Run(document invocation) {
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("Genesis run started",
		"genesis_run_id", document.RunID,
		"rule", document.Rule,
		"agent", document.Agent,
		"user", document.User,
	)

	journal := (*runJournal)(nil)
	if r.store != nil {
		journal = r.store.journal(document.RunID)
	}
	if journal == nil {
		r.logResult(logger, document, runnerResult{}, errors.New("run journal is missing"), "")
		return
	}

	input, err := json.Marshal(document)
	if err != nil {
		r.finish(logger, journal, document, streamState{}, err, nil, -1, "")
		return
	}

	if document.User == "" && grantNeedsAPIToken(document.Git, document.Credential) {
		err := errors.New("installation token was refused")
		r.finish(logger, journal, document, streamState{
			failed:    true,
			errorType: processErrorType,
			errorMsg:  err.Error(),
		}, err, err, -1, "")
		return
	}

	if document.User != "" {
		r.runDedicated(logger, journal, document, input)
		return
	}

	command := exec.Command(r.pythonPath, "-c", r.source)
	command.Env = sanitizedChildEnv()
	command.Stdin = bytes.NewReader(input)
	stdout, err := command.StdoutPipe()
	if err != nil {
		r.finish(logger, journal, document, streamState{}, err, nil, -1, "")
		return
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		r.finish(logger, journal, document, streamState{}, err, nil, -1, "")
		return
	}

	if err := command.Start(); err != nil {
		r.finish(logger, journal, document, streamState{
			failed:    true,
			errorType: processErrorType,
			errorMsg:  err.Error(),
		}, nil, err, -1, "")
		return
	}

	if err := journal.Publish(lifecycleTypeStart, originGenesis, map[string]any{"pid": command.Process.Pid}); err != nil {
		logger.Error("persist start event", "genesis_run_id", document.RunID, "error", err)
		r.recordStorageFailure(journal, nil, logger, document.RunID, err)
	}

	stderrPath := filepath.Join(document.RunDir, stderrFileName)
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, dataFileMode)
	if err != nil {
		logger.Error("create stderr log", "genesis_run_id", document.RunID, "error", err)
		r.recordStorageFailure(journal, nil, logger, document.RunID, err)
	}
	var stderrBuf bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		writer := io.Writer(&stderrBuf)
		if stderrFile != nil {
			defer stderrFile.Close()
			writer = io.MultiWriter(journal.redactor.writer(stderrFile), &stderrBuf)
		}
		// Keep only the capped prefix in the journal/log, but always drain
		// the remainder to EOF. Stopping at the LimitReader cap leaves the
		// OS pipe full and can deadlock Wait.
		copyCappedThenDrain(writer, stderr, maxFrameBytes)
	}()

	state := r.consumeStdout(journal, document, stdout)
	// Cmd.Wait closes stdout/stderr pipes. Finish draining both before Wait,
	// otherwise the last stderr bytes can be lost. Stderr is already being
	// copied concurrently, so waiting for that copy after stdout EOF cannot
	// fill the stdout pipe.
	<-stderrDone
	waitErr := command.Wait()
	stderrText := journal.redactor.text(strings.TrimSpace(stderrBuf.String()))
	r.finish(logger, journal, document, state, nil, waitErr, exitStatus(waitErr, command), stderrText)
}

func (r processRunner) consumeStdout(journal *runJournal, document invocation, stdout io.Reader) streamState {
	reader := bufio.NewReaderSize(stdout, 64*1024)
	var state streamState
	for {
		line, tooLong, err := readCappedLine(reader, maxFrameBytes)
		if tooLong {
			state.failed = true
			state.errorType = payloadTooLarge
			state.errorMsg = "Python runner emitted a frame larger than 16MiB"
			_ = r.publishError(journal, &state, state.errorType, state.errorMsg)
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				state.errorType = protocolError
				state.errorMsg = err.Error()
				_ = r.publishError(journal, &state, state.errorType, state.errorMsg)
				break
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			state.failed = true
			state.errorType = protocolError
			state.errorMsg = err.Error()
			_ = r.publishError(journal, &state, state.errorType, state.errorMsg)
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		r.handleFrame(journal, document, line, &state)
	}
	return state
}

func (r processRunner) handleFrame(journal *runJournal, document invocation, line []byte, state *streamState) {
	frame, err := parsePythonFrame(line)
	if err != nil {
		state.failed = true
		state.errorType = protocolError
		state.errorMsg = err.Error()
		_ = r.publishError(journal, state, state.errorType, state.errorMsg)
		return
	}
	if state.gotResult && frame.Type != pythonFrameResult {
		state.failed = true
		state.errorType = protocolError
		state.errorMsg = "Python runner emitted a frame after the result"
		_ = r.publishError(journal, state, state.errorType, state.errorMsg)
		return
	}

	switch frame.Type {
	case pythonFrameEmit:
		r.handleAgentEmit(journal, document, frame.Event)
	case pythonFrameSessionCreated:
		if state.gotSession {
			state.failed = true
			state.errorType = protocolError
			state.errorMsg = "Python runner emitted multiple session.created frames"
			_ = r.publishError(journal, state, state.errorType, state.errorMsg)
			return
		}
		state.gotSession = true
		journal.rememberSession(frame.SessionID)
		if r.store != nil {
			if err := r.store.noteSessionID(document.RunID, frame.SessionID); err != nil && r.logger != nil {
				r.logger.Error("record session id", "genesis_run_id", document.RunID, "error", err)
			}
		}
		if err := journal.Publish(lifecycleTypeSessionCreated, originPython, map[string]any{
			"session_id": frame.SessionID,
		}); err != nil {
			r.recordStorageFailure(journal, state, r.logger, document.RunID, err)
		}
	case pythonFrameNotification:
		r.handleNotification(journal, document, frame, state)
	case pythonFrameResult:
		if state.gotResult {
			state.failed = true
			state.errorType = protocolError
			state.errorMsg = "Python runner emitted multiple result frames"
			_ = r.publishError(journal, state, state.errorType, state.errorMsg)
			return
		}
		state.gotResult = true
		state.result = frame.result()
		if message, ok := diagnosticsTurnFailure(state.result.Diagnostics); ok {
			state.turnEndFailure = true
			if state.turnFailureMessage == "" {
				state.turnFailureMessage = message
			}
		}
		if state.turnEndFailure && isFinishWrapper(state.result.Error) {
			state.result.Error = nil
		}
		if err := journal.Publish(lifecycleTypeResult, originPython, resultPayload(state.result)); err != nil {
			r.recordStorageFailure(journal, state, r.logger, document.RunID, err)
			state.failed = true
			state.errorType = diskErrorType
			state.errorMsg = err.Error()
		}
		if err := writeResultFile(document.RunDir, state.result, journal.redactor); err != nil {
			if r.logger != nil {
				r.logger.Error("write result file", "genesis_run_id", document.RunID, "error", err)
			}
		}
	}
}

func (r processRunner) handleNotification(journal *runJournal, document invocation, frame pythonFrame, state *streamState) {
	// genesis.emit is ingress. It is not a lifecycle observation and is not
	// published on the journal bus.
	if frame.Method == genesisEmitMethod {
		r.handleAgentEmit(journal, document, frame.Payload)
		return
	}
	eventType, origin, data, ok := mapSDKNotification(journal.session(), frame.Method, frame.Payload)
	if !ok {
		return
	}
	if eventType == lifecycleTypeTurn {
		if message, failed := mappedTurnFailure(data); failed {
			state.turnEndFailure = true
			if message != "" {
				state.turnFailureMessage = message
			}
		}
	}
	if err := journal.Publish(eventType, origin, data); err != nil {
		r.recordStorageFailure(journal, state, r.logger, document.RunID, err)
		state.failed = true
		state.errorType = diskErrorType
		state.errorMsg = err.Error()
	}
}

func (r processRunner) finish(
	logger *slog.Logger,
	journal *runJournal,
	document invocation,
	state streamState,
	internalErr error,
	processErr error,
	exitCode int,
	stderr string,
) {
	result := state.result
	if internalErr != nil && !state.gotResult {
		result = runnerResult{}
	}
	if state.turnEndFailure && isFinishWrapper(result.Error) {
		result.Error = nil
	}
	incomplete := state.gotResult && (result.FinishReason == nil || *result.FinishReason != endStateCompleted)
	if result.Error == nil && incomplete {
		if state.turnEndFailure {
			state.failed = true
		} else {
			reason := "<missing>"
			errorType := "DeepSeekRunIncomplete"
			if result.FinishReason != nil {
				reason = *result.FinishReason
				if reason == "error" {
					errorType = "DeepSeekRunError"
				}
			}
			message := fmt.Sprintf("DeepSeek run finished with finish_reason=%q", reason)
			if result.FinalResponse == nil || *result.FinalResponse == "" {
				message += " and an empty final response"
			}
			result.Error = &runnerFailure{Type: errorType, Message: message}
		}
	}
	if processErr != nil && result.Error == nil && !state.turnEndFailure {
		result.Error = &runnerFailure{Type: processErrorType, Message: processErr.Error()}
	}
	if processErr != nil && state.turnEndFailure {
		state.failed = true
	}
	if !state.gotResult && internalErr == nil && processErr == nil {
		state.failed = true
		state.errorType = protocolError
		state.errorMsg = "Python runner exited without a result frame"
	}
	if internalErr != nil {
		state.failed = true
		if state.errorType == "" {
			state.errorType = runnerErrorType
			state.errorMsg = internalErr.Error()
		}
	}
	if result.Error == nil && state.failed && state.errorMsg != "" {
		result.Error = &runnerFailure{Type: state.errorType, Message: state.errorMsg}
	}

	skipTurnWrapper := state.turnEndFailure && result.Error == nil && state.errorMsg == "" && internalErr == nil
	if !state.errorPublished && !skipTurnWrapper && (state.failed || result.Error != nil) {
		errorType := state.errorType
		message := state.errorMsg
		if result.Error != nil {
			if errorType == "" {
				errorType = result.Error.Type
			}
			if message == "" {
				message = result.Error.Message
			}
		}
		if errorType == "" {
			errorType = runnerErrorType
		}
		if message == "" && internalErr != nil {
			message = internalErr.Error()
		}
		_ = r.publishError(journal, &state, errorType, message)
	}

	stateName := endStateCompleted
	if state.failed || result.Error != nil || internalErr != nil || processErr != nil {
		stateName = endStateFailed
	}
	if err := journal.Publish(lifecycleTypeEnd, originGenesis, endPayload(exitCode, stateName)); err != nil {
		logger.Error("persist end event", "genesis_run_id", document.RunID, "error", err)
	}
	r.publishAgentFinished(journal, document, stateName)

	logErr := internalErr
	if logErr == nil && !state.gotResult && processErr != nil && !state.turnEndFailure {
		logErr = processErr
	}
	if result.Error == nil && state.turnEndFailure {
		message := state.turnFailureMessage
		if message == "" {
			message = "turn/end recorded a provider failure"
		}
		result.Error = &runnerFailure{Type: "DeepSeekRunError", Message: message}
	}
	logged := result
	if journal != nil && journal.redactor != nil {
		if logged.FinalResponse != nil {
			redacted := journal.redactor.text(*logged.FinalResponse)
			logged.FinalResponse = &redacted
		}
		logged.Diagnostics = journal.redactor.value(logged.Diagnostics)
	}
	r.logResult(logger, document, logged, logErr, stderr)
}

func (r processRunner) publishAgentFinished(journal *runJournal, document invocation, stateName string) {
	if r.dispatch == nil || journal == nil || r.store == nil {
		return
	}
	transcript, err := r.store.writeTranscript(document.RunID)
	if err != nil {
		_ = journal.Publish(lifecycleTypeError, originGenesis, errorPayload(diskErrorType, err.Error()))
		return
	}
	// Dedicated runs were granted by the privileged parent. This covers a
	// listener-owned log, and EPERM leaves an agent-owned ACL in place.
	if err := grantSessionRead(document.DshHome, uint32(os.Geteuid()), true, 0, false); err != nil && r.logger != nil {
		r.logger.Error("session read grant", "genesis_run_id", document.RunID, "error", err)
	}
	sessionPath := readRecordedSessionPath(document.RunDir, document.DshHome)
	if sessionPath == "" {
		sessionPath = locateSessionLog(document.DshHome)
	}
	outcome := outcomeOK
	if stateName != endStateCompleted {
		outcome = outcomeError
	}
	event, err := newAgentFinishedEvent(document.Agent, document.RunID, outcome, transcript, sessionPath, agentDefinitionPath(r.agentsDir, document.Agent))
	if err != nil {
		_ = journal.Publish(lifecycleTypeError, originGenesis, errorPayload(runnerErrorType, err.Error()))
		return
	}
	// One shot. 503 means sync holds the generation. Retrying would busy-loop
	// or start a session against a stale or half-applied generation.
	r.noteDispatch(journal, r.dispatch(event))
}

func (r processRunner) handleAgentEmit(journal *runJournal, document invocation, raw json.RawMessage) {
	event, err := agentEmitEvent(document.Agent, raw)
	if err != nil {
		if journal != nil {
			_ = journal.Publish(lifecycleTypeError, originGenesis, errorPayload(protocolError, err.Error()))
		}
		return
	}
	if eventType, _ := event.stringAttribute("type"); eventType == sessionContinueType {
		stampCause(event, document.Event)
	}
	if r.dispatch == nil {
		if journal != nil {
			_ = journal.Publish(lifecycleTypeError, originGenesis, errorPayload(runnerErrorType, "event dispatch is not configured"))
		}
		return
	}
	r.noteDispatch(journal, r.dispatch(event))
}

func (r processRunner) noteDispatch(journal *runJournal, code int) {
	if journal == nil {
		return
	}
	switch code {
	case http.StatusAccepted, http.StatusNoContent:
		return
	case http.StatusServiceUnavailable:
		_ = journal.Publish(lifecycleTypeError, originGenesis, errorPayload(syncInProgressType, "sync in progress"))
	default:
		if code >= 400 {
			_ = journal.Publish(lifecycleTypeError, originGenesis, errorPayload(runnerErrorType, fmt.Sprintf("event dispatch returned %d", code)))
		}
	}
}

func (r processRunner) publishError(journal *runJournal, state *streamState, errorType, message string) error {
	if journal == nil || errorType == "" {
		return nil
	}
	if state != nil {
		state.errorPublished = true
		state.failed = true
		if state.errorType == "" {
			state.errorType = errorType
			state.errorMsg = message
		}
	}
	err := journal.Publish(lifecycleTypeError, originGenesis, errorPayload(errorType, message))
	if err != nil && r.logger != nil {
		r.logger.Error("persist error event", "error", err, "error_type", errorType)
	}
	return err
}

func (r processRunner) recordStorageFailure(journal *runJournal, state *streamState, logger *slog.Logger, runID string, err error) {
	if logger != nil {
		logger.Error("persist lifecycle event", "genesis_run_id", runID, "error", err)
	}
	_ = r.publishError(journal, state, diskErrorType, err.Error())
}

func (r processRunner) logResult(
	logger *slog.Logger,
	document invocation,
	result runnerResult,
	internalErr error,
	stderr string,
) {
	fields := []any{
		"genesis_run_id", document.RunID,
		"rule", document.Rule,
		"agent", document.Agent,
		"user", document.User,
		"run_dir", document.RunDir,
		"deepseek_session_id", optionalString(result.DeepSeekSessionID),
		"finish_reason", optionalString(result.FinishReason),
		"final_response", optionalString(result.FinalResponse),
		"diagnostics", result.Diagnostics,
		"stderr", strings.TrimSpace(stderr),
	}

	if internalErr != nil {
		fields = append(fields, "error_type", runnerErrorType, "error", internalErr.Error())
		logger.Error("Genesis run finished", fields...)
		return
	}
	if result.Error != nil {
		fields = append(fields, "error_type", result.Error.Type, "error", result.Error.Message)
		logger.Error("Genesis run finished", fields...)
		return
	}
	fields = append(fields, "error_type", nil, "error", nil)
	logger.Info("Genesis run finished", fields...)
}

func optionalString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func copyCappedThenDrain(dst io.Writer, src io.Reader, limit int64) {
	if src == nil {
		return
	}
	if dst == nil {
		dst = io.Discard
	}
	if limit > 0 {
		_, _ = io.Copy(dst, io.LimitReader(src, limit))
	}
	_, _ = io.Copy(io.Discard, src)
}

func (r processRunner) runDedicated(logger *slog.Logger, journal *runJournal, document invocation, input []byte) {
	if r.spawner == nil {
		err := errors.New("dedicated OS user requires genesis launch privileged spawn")
		r.finish(logger, journal, document, streamState{
			failed:    true,
			errorType: processErrorType,
			errorMsg:  err.Error(),
		}, err, nil, -1, "")
		return
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		r.finish(logger, journal, document, streamState{}, err, nil, -1, "")
		return
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		r.finish(logger, journal, document, streamState{}, err, nil, -1, "")
		return
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		r.finish(logger, journal, document, streamState{}, err, nil, -1, "")
		return
	}
	for _, file := range []*os.File{stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW} {
		syscall.CloseOnExec(int(file.Fd()))
	}

	pid, err := r.spawner.Spawn(context.Background(), spawnRequest{
		Agent:      document.Agent,
		User:       document.User,
		Cwd:        document.Cwd,
		Home:       document.Home,
		RunDir:     document.RunDir,
		DshHome:    document.DshHome,
		Env:        document.Env,
		NeedsToken: grantNeedsAPIToken(document.Git, document.Credential),
	}, stdinR, stdoutW, stderrW)
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		stdinW.Close()
		stdoutR.Close()
		stderrR.Close()
		r.finish(logger, journal, document, streamState{
			failed:    true,
			errorType: processErrorType,
			errorMsg:  err.Error(),
		}, err, err, -1, "")
		return
	}

	if err := journal.Publish(lifecycleTypeStart, originGenesis, map[string]any{"pid": pid, "user": document.User}); err != nil {
		logger.Error("persist start event", "genesis_run_id", document.RunID, "error", err)
		r.recordStorageFailure(journal, nil, logger, document.RunID, err)
	}

	stderrPath := filepath.Join(document.RunDir, stderrFileName)
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, dataFileMode)
	if err != nil {
		logger.Error("create stderr log", "genesis_run_id", document.RunID, "error", err)
		r.recordStorageFailure(journal, nil, logger, document.RunID, err)
	}
	var stderrBuf bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		defer stderrR.Close()
		writer := io.Writer(&stderrBuf)
		if stderrFile != nil {
			defer stderrFile.Close()
			writer = io.MultiWriter(journal.redactor.writer(stderrFile), &stderrBuf)
		}
		copyCappedThenDrain(writer, stderrR, maxFrameBytes)
	}()

	go func() {
		_, _ = stdinW.Write(input)
		stdinW.Close()
	}()

	state := r.consumeStdout(journal, document, stdoutR)
	stdoutR.Close()
	<-stderrDone
	exitCode, waitErr := r.spawner.Wait(context.Background(), pid)
	if waitErr == nil && exitCode != 0 {
		waitErr = fmt.Errorf("process exited with status %d", exitCode)
	}
	stderrText := journal.redactor.text(strings.TrimSpace(stderrBuf.String()))
	r.finish(logger, journal, document, state, nil, waitErr, exitCode, stderrText)
}

func sanitizedChildEnv() []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment))
	for _, item := range environment {
		key, _, _ := strings.Cut(item, "=")
		if key == privilegedFDEnv || key == syncTokenEnv || droppedChildEnv(key) || containsPrivateKey(item) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func exitStatus(waitErr error, command *exec.Cmd) int {
	if waitErr == nil {
		if command != nil && command.ProcessState != nil {
			return command.ProcessState.ExitCode()
		}
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
		return exitErr.ExitCode()
	}
	return -1
}
