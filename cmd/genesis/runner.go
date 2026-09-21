package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

//go:embed runner.py
var embeddedPythonRunner string

type processRunner struct {
	pythonPath string
	source     string
	logger     *slog.Logger
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

	input, err := json.Marshal(document)
	if err != nil {
		r.logResult(logger, document, runnerResult{}, err, "")
		return
	}

	command := exec.Command(r.pythonPath, "-c", r.source)
	command.Env = sanitizedChildEnv()
	command.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	processErr := command.Run()

	result, decodeErr := decodeRunnerResult(stdout.Bytes())
	if decodeErr != nil {
		if processErr != nil {
			decodeErr = fmt.Errorf("%w; process: %v", decodeErr, processErr)
		}
		r.logResult(logger, document, runnerResult{}, decodeErr, stderr.String())
		return
	}
	if result.Error == nil &&
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
	if processErr != nil {
		if result.Error == nil {
			result.Error = &runnerFailure{Type: "ProcessError", Message: processErr.Error()}
		}
	}
	r.logResult(logger, document, result, nil, stderr.String())
}

func decodeRunnerResult(output []byte) (runnerResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	var result runnerResult
	if err := decoder.Decode(&result); err != nil {
		return runnerResult{}, fmt.Errorf("decode Python runner output: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return runnerResult{}, errors.New("Python runner emitted multiple JSON values")
		}
		return runnerResult{}, fmt.Errorf("decode trailing Python runner output: %w", err)
	}
	return result, nil
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
		"deepseek_session_id", optionalString(result.DeepSeekSessionID),
		"finish_reason", optionalString(result.FinishReason),
		"final_response", optionalString(result.FinalResponse),
		"diagnostics", result.Diagnostics,
		"stderr", strings.TrimSpace(stderr),
	}

	if internalErr != nil {
		fields = append(fields, "error_type", "RunnerError", "error", internalErr.Error())
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
