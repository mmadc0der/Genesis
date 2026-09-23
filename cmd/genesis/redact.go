package main

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
)

const (
	sshAuthSockEnv = "SSH_AUTH_SOCK"
	sshAgentPIDEnv = "SSH_AGENT_PID"
	gitSSHEnv      = "GIT_SSH"
	gitSSHCommand  = "GIT_SSH_COMMAND"
)

var privateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

func redactPrivateKeys(input []byte) []byte {
	if len(input) == 0 || !bytes.Contains(input, []byte("PRIVATE KEY")) {
		return input
	}
	return privateKeyPattern.ReplaceAll(input, []byte(redactedSecret))
}

func containsPrivateKey(value string) bool {
	return strings.Contains(value, "PRIVATE KEY") || strings.Contains(value, "OPENSSH PRIVATE KEY")
}

func droppedChildEnv(key string) bool {
	switch key {
	case sshAuthSockEnv, sshAgentPIDEnv, gitSSHEnv, gitSSHCommand:
		return true
	default:
		return false
	}
}

const redactedSecret = "[redacted]"

type redactor struct {
	values []string
}

func newRedactor(values []string) *redactor {
	filtered := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		if len(value) < 6 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		filtered = append(filtered, value)
	}
	slices.SortFunc(filtered, func(a, b string) int {
		if diff := len(b) - len(a); diff != 0 {
			return diff
		}
		return strings.Compare(a, b)
	})
	return &redactor{values: filtered}
}

func (r *redactor) bytes(input []byte) []byte {
	input = redactPrivateKeys(input)
	if r == nil || len(r.values) == 0 || len(input) == 0 {
		return input
	}
	text := string(input)
	changed := false
	for _, value := range r.values {
		if !strings.Contains(text, value) {
			continue
		}
		text = strings.ReplaceAll(text, value, redactedSecret)
		changed = true
	}
	if !changed {
		return input
	}
	return []byte(text)
}

func (r *redactor) text(input string) string {
	return string(r.bytes([]byte(input)))
}

func (r *redactor) value(input any) any {
	if r == nil || input == nil {
		return input
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return input
	}
	redacted := r.bytes(payload)
	if bytes.Equal(redacted, payload) {
		return input
	}
	var out any
	if err := json.Unmarshal(redacted, &out); err != nil {
		return string(redacted)
	}
	return out
}

func (r *redactor) writer(dst io.Writer) io.Writer {
	if r == nil || dst == nil {
		return dst
	}
	return redactWriter{Writer: dst, redactor: r}
}

type redactWriter struct {
	io.Writer
	redactor *redactor
}

func (w redactWriter) Write(p []byte) (int, error) {
	_, err := w.Writer.Write(w.redactor.bytes(p))
	return len(p), err
}

func secretValues(processEnv map[string]string, names []string, runtimeEnv map[string]string) []string {
	wanted := map[string]struct{}{deepSeekAPIKey: {}}
	for _, name := range names {
		if name != "" {
			wanted[name] = struct{}{}
		}
	}
	values := make([]string, 0, len(wanted))
	for name := range wanted {
		if processEnv != nil {
			values = append(values, processEnv[name])
		}
		if runtimeEnv != nil {
			values = append(values, runtimeEnv[name])
		}
	}
	return values
}
