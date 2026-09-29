package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	eventsURLEnv     = "GENESIS_EVENTS_URL"
	genesisAgentEnv  = "GENESIS_AGENT"
	defaultEventsURL = "http://127.0.0.1:8787/events"
	defaultAgentPATH = "/usr/local/bin:/usr/bin:/bin"
	genesisStateName = ".genesis"

	jobSourceURN      = "urn:genesis:job"
	scheduleSourceURN = "urn:genesis:schedule"
	jobExitedType     = "dev.genesis.job.exited"
	scheduleFiredType = "dev.genesis.schedule.fired"
)

// eventRetryDelay is the pause between listener 503s and network errors.
// Tests shorten it. Production stays at one second, matching Retry-After.
var eventRetryDelay = time.Second

// agentDeniedEnv is material an agent process must never receive: the sync
// token, the webhook secret, and the privileged listener fd.
func agentDeniedEnv(key string) bool {
	switch key {
	case syncTokenEnv, webhookSecretName, privilegedFDEnv:
		return true
	default:
		return false
	}
}

func leakedSecretStore(value string) bool {
	if value == "" {
		return false
	}
	cleaned := filepath.Clean(value)
	root := filepath.Clean(secretsDockerPath)
	return cleaned == root || strings.HasPrefix(cleaned, root+string(os.PathSeparator))
}

func genesisExecutableDir() string {
	path, err := os.Executable()
	if err != nil || path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != "" {
		path = resolved
	}
	return filepath.Dir(path)
}

func pathHasDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	dir = filepath.Clean(dir)
	for _, entry := range filepath.SplitList(path) {
		if filepath.Clean(entry) == dir {
			return true
		}
	}
	return false
}

// ensureGenesisOnPATH puts the running genesis binary's directory on PATH.
// An omitted PATH becomes the image default, which already contains
// /usr/local/bin. A declared PATH that dropped that directory still gains it.
func ensureGenesisOnPATH(path string) string {
	if strings.TrimSpace(path) == "" {
		path = defaultAgentPATH
	}
	dir := genesisExecutableDir()
	if dir == "" || pathHasDir(path, dir) {
		return path
	}
	return dir + string(os.PathListSeparator) + path
}

func eventsURLFromListen(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return defaultEventsURL
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/events"
}

func listenerEventsURL() string {
	raw := strings.TrimSpace(os.Getenv(eventsURLEnv))
	if raw == "" {
		return defaultEventsURL
	}
	return raw
}

func defaultAgentSource() string {
	id := strings.TrimSpace(os.Getenv(genesisAgentEnv))
	if id == "" || strings.Contains(id, "/") || strings.Contains(id, `\`) {
		return ""
	}
	return agentSource(id)
}

// agentProcessEnv is the environment for a job command and for the detached
// job and schedule processes. It keeps the agent PATH and the listener URL
// and drops the sync token, webhook secret, privileged fd, and secret store.
func agentProcessEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		key, value, _ := strings.Cut(item, "=")
		if agentDeniedEnv(key) || droppedChildEnv(key) || containsPrivateKey(value) || leakedSecretStore(value) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func composeCloudEvent(template json.RawMessage, defaults, overlay, dataOverlay map[string]any) (cloudEvent, error) {
	fields := map[string]any{}
	if len(bytes.TrimSpace(template)) > 0 {
		if err := json.Unmarshal(template, &fields); err != nil {
			return nil, errors.New("event must be a JSON object")
		}
	}
	for key, value := range defaults {
		if blankEventField(fields[key]) {
			fields[key] = value
		}
	}
	for key, value := range overlay {
		fields[key] = value
	}
	data := map[string]any{}
	if raw, ok := fields["data"]; ok && raw != nil {
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("event data must be a JSON object")
		}
		for key, value := range object {
			data[key] = value
		}
	}
	for key, value := range dataOverlay {
		data[key] = value
	}
	fields["data"] = data
	return marshalCloudEvent(fields)
}

// stampedCLIEvent is the job and schedule accept path. It matches harness
// emit: the caller does not choose id or source, and the finished and
// session-continue types stay reserved even if a control file is edited.
func stampedCLIEvent(template json.RawMessage, fallbackSource, fallbackType, fallbackSubject string, now time.Time, dataOverlay map[string]any) (cloudEvent, error) {
	if err := rejectReservedEmit(template); err != nil {
		return nil, err
	}
	id, err := newLifecycleEventID()
	if err != nil {
		return nil, err
	}
	source := defaultAgentSource()
	if source == "" {
		source = fallbackSource
	}
	return composeCloudEvent(template, map[string]any{
		"specversion": cloudEventSpecVersion,
		"type":        fallbackType,
		"subject":     fallbackSubject,
	}, map[string]any{
		"id":              id,
		"source":          source,
		"time":            now.UTC().Format(time.RFC3339Nano),
		"datacontenttype": "application/json",
	}, dataOverlay)
}

func reservedCLIEventType(eventType string) bool {
	return eventType == agentFinishedType || eventType == sessionContinueType
}

func rejectReservedEmit(template json.RawMessage) error {
	if len(bytes.TrimSpace(template)) == 0 {
		return nil
	}
	var object map[string]any
	if err := json.Unmarshal(template, &object); err != nil {
		return errors.New("event must be a JSON object")
	}
	raw, ok := object["type"]
	if !ok || raw == nil {
		return nil
	}
	text, ok := raw.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return errors.New("event type must be a non-empty string")
	}
	if reservedCLIEventType(text) {
		return fmt.Errorf("type %s is reserved", text)
	}
	return nil
}

func blankEventField(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) == ""
}

func parseEmitObject(raw string) (json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("--emit must be one JSON object")
	}
	if len(raw) > maxEventBytes {
		return nil, fmt.Errorf("--emit exceeds %d bytes", maxEventBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, errors.New("--emit must be one JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("--emit must be one JSON object")
	}
	for _, key := range []string{"specversion", "id", "source", "type", "subject"} {
		value, ok := object[key]
		if !ok || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("--emit %s must be a non-empty string", key)
		}
		if key == "specversion" && text != cloudEventSpecVersion {
			return nil, errors.New(`--emit specversion must be "1.0"`)
		}
	}
	if text, ok := object["type"].(string); ok && reservedCLIEventType(text) {
		return nil, fmt.Errorf("type %s is reserved", text)
	}
	if rawData, ok := object["data"]; ok && rawData != nil {
		if _, ok := rawData.(map[string]any); !ok {
			return nil, errors.New("--emit data must be a JSON object")
		}
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

// postCloudEvent is the agent-facing accept path: POST /events with no
// bearer. GENESIS_SYNC_TOKEN is never read and never sent.
func postCloudEvent(event cloudEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(eventRetryDelay)
		}
		status, err := postCloudEventOnce(client, payload)
		if err != nil {
			last = err
			continue
		}
		switch status {
		case http.StatusAccepted, http.StatusNoContent:
			return nil
		case http.StatusServiceUnavailable:
			last = errors.New("sync in progress")
			continue
		default:
			return fmt.Errorf("listener returned %d", status)
		}
	}
	if last == nil {
		last = errors.New("listener did not accept the event")
	}
	return last
}

func postCloudEventOnce(client *http.Client, payload []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, listenerEventsURL(), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", cloudEventsJSON)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func genesisStateDir() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil || resolved == "" {
			return "", errors.New("HOME is required")
		}
		home = resolved
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("HOME must be absolute")
	}
	dir := filepath.Join(home, genesisStateName)
	if err := mkdirPrivate(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func mkdirPrivate(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s must be a real directory", filepath.Base(path))
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func readJSONFile(path string, dest any) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), filepath.Base(path))
	defer file.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	payload, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, dest); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}
