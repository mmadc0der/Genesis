package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
)

const (
	cloudEventsJSON    = "application/cloudevents+json"
	deepSeekAPIKey     = "DEEPSEEK_API_KEY"
	homeEnvKey         = "HOME"
	systemPromptEnvKey = "DSH_SYSTEM_PROMPT"
	maxEventBytes      = 1 << 20
)

var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type cloudEvent map[string]json.RawMessage

// agentDefinition is a file-backed identity. cwd is the SDK workspace; home is
// the child process Unix HOME. They separate workspace from environment under
// the shared Genesis UID and are not a security sandbox.
type agentDefinition struct {
	Instructions string            `yaml:"instructions"`
	Cwd          string            `yaml:"cwd"`
	Home         string            `yaml:"home"`
	Env          map[string]string `yaml:"env,omitempty"`
	Secrets      []string          `yaml:"secrets,omitempty"`
	id           string
}

type rule struct {
	Match map[string]string `yaml:"match"`
	Agent string            `yaml:"agent"`
	name  string
}

type invocation struct {
	Event        cloudEvent        `json:"event"`
	Rule         string            `json:"rule"`
	Agent        string            `json:"agent"`
	RunID        string            `json:"run_id"`
	Cwd          string            `json:"cwd"`
	Home         string            `json:"home"`
	Instructions string            `json:"instructions"`
	Env          map[string]string `json:"env"`
	RunDir       string            `json:"run_dir"`
	DshHome      string            `json:"dsh_home"`
}

type acceptedRun struct {
	Rule  string `json:"rule"`
	Agent string `json:"agent"`
	RunID string `json:"run_id"`
}

type invocationRunner interface {
	Run(invocation)
}

type eventServer struct {
	agentsDir   string
	rulesDir    string
	runner      invocationRunner
	newRunID    func() (string, error)
	secrets     map[string]string
	logger      *slog.Logger
	syncToken   string
	coordinator privilegedCoordinator
	store       *runStore

	mu         sync.RWMutex
	syncing    bool
	generation *generation
}

func (s *eventServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/events":
		s.handleEvents(w, r)
	case "/sync":
		s.handleSync(w, r)
	case "/health":
		s.handleHealth(w, r)
	case "/generation":
		s.handleGeneration(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *eventServer) loadInitialGeneration() error {
	generation, err := loadGeneration(s.agentsDir, s.rulesDir)
	if err != nil {
		return err
	}
	s.generation = generation
	return nil
}

func (s *eventServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
		return
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != cloudEventsJSON {
		http.Error(w, "Content-Type must be application/cloudevents+json", http.StatusUnsupportedMediaType)
		return
	}

	event, err := decodeCloudEvent(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	if s.syncing {
		s.mu.RUnlock()
		w.Header().Set("Retry-After", syncRetryAfter)
		http.Error(w, "sync in progress", http.StatusServiceUnavailable)
		return
	}
	current := s.generation
	s.mu.RUnlock()
	if current == nil {
		http.Error(w, "generation is not loaded", http.StatusInternalServerError)
		return
	}

	matches := make([]rule, 0, len(current.rules))
	for _, candidate := range current.rules {
		if candidate.matches(event) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
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

	newRunID := s.newRunID
	if newRunID == nil {
		newRunID = newGenesisRunID
	}
	invocations := make([]invocation, 0, len(matches))
	accepted := make([]acceptedRun, 0, len(matches))
	for _, matched := range matches {
		definition, ok := current.agents[matched.Agent]
		if !ok {
			s.log().Error("matched rule is missing its agent", "rule", matched.name, "agent", matched.Agent)
			http.Error(w, "rules are invalid", http.StatusInternalServerError)
			return
		}
		runID, err := newRunID()
		if err != nil {
			s.log().Error("create Genesis run ID", "rule", matched.name, "agent", definition.id, "error", err)
			http.Error(w, "failed to create run ID", http.StatusInternalServerError)
			return
		}
		document := snapshotInvocation(event, matched, definition, runID, s.secrets)
		if err := s.store.Accept(&document, secretValues(s.secrets, definition.Secrets, document.Env)); err != nil {
			s.log().Error("create run storage", "rule", matched.name, "agent", definition.id, "run_id", runID, "error", err)
			for _, previous := range invocations {
				s.store.failAccept(previous.RunID, err.Error())
			}
			http.Error(w, "failed to create run storage", http.StatusInternalServerError)
			return
		}
		invocations = append(invocations, document)
		accepted = append(accepted, acceptedRun{Rule: matched.name, Agent: definition.id, RunID: runID})
	}

	for _, document := range invocations {
		go func(document invocation) {
			defer s.store.release(document.RunID)
			s.runner.Run(document)
		}(document)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		Runs []acceptedRun `json:"runs"`
	}{Runs: accepted})
}

func (s *eventServer) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func snapshotInvocation(
	event cloudEvent,
	matched rule,
	definition agentDefinition,
	runID string,
	secrets map[string]string,
) invocation {
	return invocation{
		Event:        event,
		Rule:         matched.name,
		Agent:        definition.id,
		RunID:        runID,
		Cwd:          definition.Cwd,
		Home:         definition.Home,
		Instructions: definition.Instructions,
		Env:          runtimeEnvironment(definition, secrets),
	}
}

func newGenesisRunID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "gen_" + hex.EncodeToString(random[:]), nil
}

func inheritedEnvironment() map[string]string {
	environment := map[string]string{}
	for _, item := range os.Environ() {
		key, value, found := strings.Cut(item, "=")
		if !found || key == "" {
			continue
		}
		environment[key] = value
	}
	return environment
}

func runtimeEnvironment(definition agentDefinition, secrets map[string]string) map[string]string {
	environment := make(map[string]string, len(definition.Env)+len(definition.Secrets)+2)
	for key, value := range definition.Env {
		environment[key] = value
	}
	for _, name := range definition.Secrets {
		if value := secrets[name]; value != "" {
			environment[name] = value
		}
	}
	environment[homeEnvKey] = definition.Home
	environment[systemPromptEnvKey] = definition.Instructions
	return environment
}

func decodeCloudEvent(w http.ResponseWriter, r *http.Request) (cloudEvent, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEventBytes))
	var event cloudEvent
	if err := decoder.Decode(&event); err != nil {
		return nil, fmt.Errorf("invalid CloudEvent JSON: %w", err)
	}
	if event == nil {
		return nil, errors.New("CloudEvent must be a JSON object")
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("CloudEvent body must contain one JSON object")
		}
		return nil, fmt.Errorf("invalid trailing JSON: %w", err)
	}

	specVersion, ok := event.stringAttribute("specversion")
	if !ok || specVersion != "1.0" {
		return nil, errors.New(`CloudEvent "specversion" must be "1.0"`)
	}
	for _, attribute := range []string{"id", "source", "type"} {
		value, ok := event.stringAttribute(attribute)
		if !ok || value == "" {
			return nil, fmt.Errorf("CloudEvent %q must be a non-empty string", attribute)
		}
	}
	return event, nil
}

func (e cloudEvent) stringAttribute(name string) (string, bool) {
	raw, ok := e[name]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func (r rule) matches(event cloudEvent) bool {
	for attribute, expected := range r.Match {
		actual, ok := event.stringAttribute(attribute)
		if !ok || actual != expected {
			return false
		}
	}
	return true
}

func loadYAMLDocument(path string, destination any) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	name := filepath.Base(path)
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s: file must contain one YAML document", name)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func yamlEntries(directory string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	filtered := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if entry.IsDir() || (extension != ".yaml" && extension != ".yml") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered, nil
}

func loadAgents(directory string) (map[string]agentDefinition, error) {
	entries, err := yamlEntries(directory)
	if err != nil {
		return nil, err
	}

	agents := make(map[string]agentDefinition, len(entries))
	for _, entry := range entries {
		id, err := agentIDFromFilename(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if _, exists := agents[id]; exists {
			return nil, fmt.Errorf("%s: duplicate agent id %q", entry.Name(), id)
		}

		var loaded agentDefinition
		if err := loadYAMLDocument(filepath.Join(directory, entry.Name()), &loaded); err != nil {
			return nil, err
		}
		if loaded.Env == nil {
			loaded.Env = map[string]string{}
		}
		if loaded.Secrets == nil {
			loaded.Secrets = []string{deepSeekAPIKey}
		}
		loaded.id = id
		if err := loaded.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		agents[id] = loaded
	}
	return agents, nil
}

func loadRules(directory string, agents map[string]agentDefinition) ([]rule, error) {
	entries, err := yamlEntries(directory)
	if err != nil {
		return nil, err
	}

	rules := make([]rule, 0, len(entries))
	for _, entry := range entries {
		var loaded rule
		if err := loadYAMLDocument(filepath.Join(directory, entry.Name()), &loaded); err != nil {
			return nil, err
		}
		loaded.name = entry.Name()
		if err := loaded.validate(agents); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		rules = append(rules, loaded)
	}
	return rules, nil
}

func agentIDFromFilename(name string) (string, error) {
	extension := filepath.Ext(name)
	id := strings.TrimSuffix(name, extension)
	if !agentIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid agent id %q", id)
	}
	if strings.ContainsAny(id, `/\`) || strings.ContainsRune(id, '\x00') {
		return "", fmt.Errorf("invalid agent id %q", id)
	}
	return id, nil
}

func (a agentDefinition) validate() error {
	if strings.TrimSpace(a.Instructions) == "" {
		return errors.New("instructions is required")
	}
	if strings.ContainsRune(a.Instructions, '\x00') {
		return errors.New("instructions must contain no NUL")
	}
	if err := validateAbsolutePath("cwd", a.Cwd); err != nil {
		return err
	}
	if err := validateAbsolutePath("home", a.Home); err != nil {
		return err
	}
	if filepath.Clean(a.Cwd) == filepath.Clean(a.Home) {
		return errors.New("cwd and home must be distinct paths")
	}

	seenSecrets := make(map[string]struct{}, len(a.Secrets))
	for _, name := range a.Secrets {
		if err := validateEnvKey(name); err != nil {
			return fmt.Errorf("invalid secret name %q", name)
		}
		if _, duplicate := seenSecrets[name]; duplicate {
			return fmt.Errorf("duplicate secret %q", name)
		}
		if source, reserved := reservedEnvKey(name); reserved {
			return fmt.Errorf("secrets must not declare %s; Genesis sets it from %s", name, source)
		}
		seenSecrets[name] = struct{}{}
	}

	for key, value := range a.Env {
		if err := validateEnvKey(key); err != nil {
			return fmt.Errorf("invalid env key %q", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("env[%q] must contain no NUL", key)
		}
		if key == deepSeekAPIKey {
			return fmt.Errorf("env must not declare %s; named secrets are copied from the Genesis process", key)
		}
		if _, secret := seenSecrets[key]; secret {
			return fmt.Errorf("env must not declare %s; it is listed in secrets", key)
		}
		if source, reserved := reservedEnvKey(key); reserved {
			return fmt.Errorf("env must not declare %s; Genesis sets it from %s", key, source)
		}
	}
	return nil
}

func (r rule) validate(agents map[string]agentDefinition) error {
	if len(r.Match) == 0 {
		return errors.New("match must contain at least one attribute")
	}
	for attribute := range r.Match {
		if attribute == "" || strings.ContainsRune(attribute, '\x00') {
			return errors.New("match attribute names must be non-empty and contain no NUL")
		}
	}
	if r.Agent == "" {
		return errors.New("agent is required")
	}
	if _, ok := agents[r.Agent]; !ok {
		return fmt.Errorf("agent %q is not defined", r.Agent)
	}
	return nil
}

func reservedEnvKey(name string) (string, bool) {
	switch name {
	case homeEnvKey:
		return "home", true
	case systemPromptEnvKey:
		return "instructions", true
	default:
		return "", false
	}
}

func validateAbsolutePath(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path", field)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s must contain no NUL", field)
	}
	return nil
}

func validateEnvKey(key string) error {
	if key == "" || strings.Contains(key, "=") || strings.ContainsRune(key, '\x00') {
		return fmt.Errorf("invalid env key %q", key)
	}
	return nil
}
