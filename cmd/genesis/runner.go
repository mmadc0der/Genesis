package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	gotSession     bool
	gotResult      bool
	result         runnerResult
	failed         bool
	errorPublished bool
	errorType      string
	errorMsg       string
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
		_, _ = io.Copy(writer, io.LimitReader(stderr, maxFrameBytes))
	}()

	state := r.consumeStdout(journal, document, stdout)
	waitErr := command.Wait()
	<-stderrDone
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
	eventType, origin, data, ok := mapSDKNotification(journal.session(), frame.Method, frame.Payload)
	if !ok {
		return
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
	if result.Error == nil &&
		state.gotResult &&
		(result.FinishReason == nil || *result.FinishReason != "completed") {
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
	if processErr != nil && result.Error == nil {
		result.Error = &runnerFailure{Type: processErrorType, Message: processErr.Error()}
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

	if !state.errorPublished && (state.failed || result.Error != nil) {
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

	logErr := internalErr
	if logErr == nil && !state.gotResult && processErr != nil {
		logErr = processErr
	}
	logged := result
	if journal != nil && journal.redactor != nil && logged.FinalResponse != nil {
		redacted := journal.redactor.text(*logged.FinalResponse)
		logged.FinalResponse = &redacted
	}
	r.logResult(logger, document, logged, logErr, stderr)
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

func sanitizedChildEnv() []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment))
	for _, item := range environment {
		key, _, _ := strings.Cut(item, "=")
		if key == privilegedFDEnv || key == syncTokenEnv {
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
