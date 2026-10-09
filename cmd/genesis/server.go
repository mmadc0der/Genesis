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
	"slices"
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
// the child process Unix HOME. When user is set, launch / POST /sync reconcile
// a dedicated OS account and the runner executes as that account. Without
// user, cwd/home remain a workspace split under the listener UID and are not
// a security sandbox.
type agentDefinition struct {
	Instructions string            `yaml:"instructions"`
	Cwd          string            `yaml:"cwd"`
	Home         string            `yaml:"home"`
	User         string            `yaml:"user,omitempty"`
	Setup        *agentSetup       `yaml:"setup,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	Secrets      []string          `yaml:"secrets,omitempty"`
	GitHub       *agentGitHub      `yaml:"github,omitempty" json:"-"`
	// MaxParallel is the most runs of this agent that may execute at once.
	// Nil means 1. It never limits a different agent.
	MaxParallel *int `yaml:"max_parallel,omitempty" json:"max_parallel,omitempty"`
	// ReasoningEffort is the DSH thinking level for this agent's harness.
	// Empty means the adapter default, high. Allowed: off, low, high, max.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
	id              string
	credentialMode  string
}

// agentSetup is the allowlisted host contract for a dedicated OS user.
// Unknown keys fail closed. There is no command, script, or package field.
type agentSetup struct {
	Groups    []string `yaml:"groups,omitempty" json:"groups,omitempty"`
	Workspace string   `yaml:"workspace,omitempty" json:"workspace,omitempty"`
}

type rule struct {
	Match map[string]string `yaml:"match"`
	Agent string            `yaml:"agent"`
	name  string
}

type invocation struct {
	Event         cloudEvent        `json:"event"`
	Rule          string            `json:"rule"`
	Agent         string            `json:"agent"`
	RunID         string            `json:"run_id"`
	User          string            `json:"user,omitempty"`
	Cwd           string            `json:"cwd"`
	Home          string            `json:"home"`
	Instructions  string            `json:"instructions"`
	Env           map[string]string `json:"env"`
	RunDir        string            `json:"run_dir"`
	DshHome       string            `json:"dsh_home"`
	SessionID     string            `json:"session_id,omitempty"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	ContinuedFrom string            `json:"continued_from,omitempty"`
	UserMessage   string            `json:"user_message,omitempty"`
	// TransportRestart resumes the previous DSH session after a TRANSPORT
	// failure. It is not a session.continue event and carries no user message.
	TransportRestart bool   `json:"transport_restart,omitempty"`
	Git              string `json:"-"`
	Credential       string `json:"-"`
	// ReasoningEffort is copied from the agent at accept time. Empty omits
	// the harness argument so DSH keeps its default.
	ReasoningEffort     string `json:"reasoning_effort,omitempty"`
	Provider            string `json:"provider,omitempty"`
	Model               string `json:"model,omitempty"`
	ContextWindow       int    `json:"context_window,omitempty"`
	MaxRetries          int    `json:"max_retries,omitempty"`
	MaxBackoffMs        int    `json:"max_backoff_ms,omitempty"`
	StreamIdleTimeoutMs int    `json:"stream_idle_timeout_ms,omitempty"`
	APIKey              string `json:"api_key,omitempty"`
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
	agentsDir       string
	rulesDir        string
	reposDir        string
	providersDir    string
	runner          invocationRunner
	newRunID        func() (string, error)
	secrets         map[string]string
	logger          *slog.Logger
	syncToken       string
	eventsURL       string
	coordinator     privilegedCoordinator
	store           *runStore
	verifyWebhook   webhookVerifier
	repositoryBound repositoryBinder

	mu         sync.RWMutex
	syncing    bool
	generation *generation
	slots      parallelGate

	pubMu sync.Mutex
	pubs  *publicationRegister
}

func (s *eventServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/events":
		s.handleEvents(w, r)
	case webhookPath:
		s.handleGitHubWebhook(w, r)
	case "/sync":
		s.handleSync(w, r)
	case "/health":
		s.handleHealth(w, r)
	case "/generation":
		s.handleGeneration(w, r)
	case "/live":
		s.handleLiveEvents(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *eventServer) loadInitialGeneration() error {
	generation, err := loadGeneration(s.agentsDir, s.rulesDir, s.reposDir, s.providersDir)
	if err != nil {
		return err
	}
	if s.coordinator == nil {
		if _, err := evaluatePlan(buildPlan(generation.agents, true)); err != nil {
			return err
		}
	}
	s.generation = generation
	return nil
}

func (s *eventServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleGetEvents(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method must be GET or POST", http.StatusMethodNotAllowed)
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
	s.dispatchCloudEvent(w, event)
}

func (s *eventServer) dispatchCloudEvent(w http.ResponseWriter, event cloudEvent) {
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
	if eventType, ok := event.stringAttribute("type"); ok && eventType == publicationSubmittedType {
		s.handlePublication(w, event, current)
		return
	}
	if eventType, ok := event.stringAttribute("type"); ok && eventType == sessionContinueType {
		s.dispatchSessionContinue(w, event, current)
		return
	}

	matches := make([]rule, 0, len(current.rules))
	for _, candidate := range current.rules {
		if !candidate.matches(event) {
			continue
		}
		// Only dev.genesis.agent.finished skips a rule whose agent is the
		// subject. That is the agent that just finished. Other rules still run.
		if skipFinishedSelf(candidate, event) {
			continue
		}
		matches = append(matches, candidate)
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
		document := snapshotInvocation(event, matched, definition, runID, s.secrets, s.eventsURL, s.agentsDir)
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
		limit := 1
		var secretNames []string
		if definition, ok := current.agents[document.Agent]; ok {
			limit = definition.parallelLimit()
			secretNames = definition.Secrets
		}
		s.startAgent(document, limit, secretNames)
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

func findModelConfigFile(agentsDir string) string {
	candidates := []string{}
	if agentsDir != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(agentsDir), "model.json"))
	}
	candidates = append(candidates, "/var/lib/genesis/config/model.json", "model.json")
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func snapshotInvocation(
	event cloudEvent,
	matched rule,
	definition agentDefinition,
	runID string,
	secrets map[string]string,
	eventsURL string,
	agentsDir string,
) invocation {
	modelPath := findModelConfigFile(agentsDir)
	var modelCfg modelConfig
	var hasModelCfg bool
	if modelPath != "" {
		if cfg, err := loadModelConfigFile(modelPath); err == nil {
			modelCfg = cfg
			hasModelCfg = true
			if modelCfg.APIKey != "" {
				if secrets != nil {
					secrets[deepSeekAPIKey] = modelCfg.APIKey
				}
				_ = os.Setenv(deepSeekAPIKey, modelCfg.APIKey)
			}
		}
	}

	gitAccess := ""
	credential := ""
	if definition.GitHub != nil {
		gitAccess = definition.GitHub.Git
		credential = definition.credentialMode
		if credential != credentialNone {
			credential = credentialPending
		}
	}
	doc := invocation{
		Event:           event,
		Rule:            matched.name,
		Agent:           definition.id,
		RunID:           runID,
		User:            definition.User,
		Cwd:             definition.Cwd,
		Home:            definition.Home,
		Instructions:    definition.Instructions,
		Env:             runtimeEnvironment(definition, secrets, eventsURL),
		Git:             gitAccess,
		Credential:      credential,
		ReasoningEffort: definition.ReasoningEffort,
	}
	if hasModelCfg {
		if doc.ReasoningEffort == "" && modelCfg.ReasoningEffort != "" {
			doc.ReasoningEffort = modelCfg.ReasoningEffort
		}
		if modelCfg.Provider != "" {
			doc.Provider = modelCfg.Provider
		}
		if modelCfg.Model != "" {
			doc.Model = modelCfg.Model
		}
		if modelCfg.ContextWindow > 0 {
			doc.ContextWindow = modelCfg.ContextWindow
		}
		if modelCfg.MaxRetries >= 0 {
			doc.MaxRetries = modelCfg.MaxRetries
		}
		if modelCfg.MaxBackoffMs > 0 {
			doc.MaxBackoffMs = modelCfg.MaxBackoffMs
		}
		if modelCfg.StreamIdleTimeoutMs > 0 {
			doc.StreamIdleTimeoutMs = modelCfg.StreamIdleTimeoutMs
		}
		if modelCfg.APIKey != "" {
			doc.APIKey = modelCfg.APIKey
			if doc.Env == nil {
				doc.Env = map[string]string{}
			}
			doc.Env[deepSeekAPIKey] = modelCfg.APIKey
		}
	}
	return doc
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

func runtimeEnvironment(definition agentDefinition, secrets map[string]string, eventsURL string) map[string]string {
	environment := make(map[string]string, len(definition.Env)+len(definition.Secrets)+8)
	for key, value := range definition.Env {
		if agentDeniedEnv(key) || leakedSecretStore(value) || containsPrivateKey(value) {
			continue
		}
		environment[key] = value
	}
	for _, name := range definition.Secrets {
		if agentDeniedEnv(name) {
			continue
		}
		value := secrets[name]
		if value == "" || leakedSecretStore(value) || containsPrivateKey(value) {
			continue
		}
		environment[name] = value
	}
	environment[homeEnvKey] = definition.Home
	environment[systemPromptEnvKey] = definition.Instructions
	if definition.User != "" {
		environment["USER"] = definition.User
		environment["LOGNAME"] = definition.User
		environment["SHELL"] = agentShell
	}
	environment["PATH"] = ensureGenesisOnPATH(environment["PATH"])
	if strings.TrimSpace(eventsURL) == "" {
		eventsURL = defaultEventsURL
	}
	environment[eventsURLEnv] = eventsURL
	if definition.id != "" && !strings.ContainsAny(definition.id, `/\`) {
		environment[genesisAgentEnv] = definition.id
	}
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
		if !ok {
			return false
		}
		if attribute == "subject" {
			if !subjectMatches(expected, actual) {
				return false
			}
			continue
		}
		if actual != expected {
			return false
		}
	}
	return true
}

// subjectMatches compares a subject pattern to a CloudEvent subject.
// A pattern with no '*' is exact equality, including values that contain
// slashes. Otherwise the pattern is a '/'-separated list of segments. A
// segment that is exactly "*" matches one non-empty subject segment and
// does not match across '/'. Any other segment is exact, so "**", "?",
// and character classes are not wildcards. An empty pattern is exact, not
// match-all.
func subjectMatches(pattern, subject string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == subject
	}
	patternSegments := strings.Split(pattern, "/")
	subjectSegments := strings.Split(subject, "/")
	if len(patternSegments) != len(subjectSegments) {
		return false
	}
	for i, segment := range patternSegments {
		if segment == "*" {
			if subjectSegments[i] == "" {
				return false
			}
			continue
		}
		if segment != subjectSegments[i] {
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
	return decodeYAMLBytes(filepath.Base(path), contents, destination)
}

func decodeYAMLBytes(name string, contents []byte, destination any) error {
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
	if err := validateUniqueAgentUsers(agents); err != nil {
		return nil, err
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
	if a.User != "" {
		if err := validateOSUsername(a.User); err != nil {
			return fmt.Errorf("user: %w", err)
		}
		home := agentHomeDir(a.User)
		if filepath.Clean(a.Home) != home {
			return fmt.Errorf("home must be %s", home)
		}
		cwd := filepath.Clean(a.Cwd)
		kind, err := classifyHostPath(cwd)
		if err != nil {
			return fmt.Errorf("cwd: %w", err)
		}
		switch kind {
		case pathStickyShared:
		case pathManaged:
			if !pathHasPrefix(cwd, home) {
				return fmt.Errorf("cwd must be inside %s", home)
			}
		default:
			return fmt.Errorf("cwd %q is not a permitted workspace", a.Cwd)
		}
	}
	if err := a.setupContract().validate(a.User != ""); err != nil {
		return err
	}
	if err := a.validateGitHubShape(); err != nil {
		return err
	}
	if a.MaxParallel != nil && *a.MaxParallel < 1 {
		return errors.New("max_parallel must be at least 1")
	}
	if err := validateReasoningEffort(a.ReasoningEffort); err != nil {
		return err
	}

	seenSecrets := make(map[string]struct{}, len(a.Secrets))
	for _, name := range a.Secrets {
		if err := validateEnvKey(name); err != nil {
			return fmt.Errorf("invalid secret name %q", name)
		}
		if _, duplicate := seenSecrets[name]; duplicate {
			return fmt.Errorf("duplicate secret %q", name)
		}
		if agentDeniedEnv(name) {
			return fmt.Errorf("secrets must not declare %s", name)
		}
		if source, reserved := reservedEnvKey(name, a.User != ""); reserved {
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
		if leakedSecretStore(value) {
			return fmt.Errorf("env[%q] must not name the secret store", key)
		}
		if key == deepSeekAPIKey {
			return fmt.Errorf("env must not declare %s; named secrets are copied from the Genesis process", key)
		}
		if agentDeniedEnv(key) {
			return fmt.Errorf("env must not declare %s", key)
		}
		if _, secret := seenSecrets[key]; secret {
			return fmt.Errorf("env must not declare %s; it is listed in secrets", key)
		}
		if source, reserved := reservedEnvKey(key, a.User != ""); reserved {
			return fmt.Errorf("env must not declare %s; Genesis sets it from %s", key, source)
		}
	}
	return nil
}

func validateReasoningEffort(value string) error {
	switch value {
	case "", "off", "low", "high", "max":
		return nil
	default:
		return errors.New("reasoning_effort must be off, low, high, or max")
	}
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

func reservedEnvKey(name string, dedicated bool) (string, bool) {
	switch name {
	case homeEnvKey:
		return "home", true
	case systemPromptEnvKey:
		return "instructions", true
	case "USER", "LOGNAME", "SHELL":
		if dedicated {
			return "user", true
		}
		return "", false
	case githubTokenEnv, ghTokenEnv:
		return "the run installation token", true
	case eventsURLEnv:
		return "the listener address", true
	case genesisAgentEnv:
		return "the agent id", true
	default:
		return "", false
	}
}

func (a agentDefinition) setupContract() agentSetup {
	if a.Setup == nil {
		return agentSetup{}
	}
	return *a.Setup
}

func (s agentSetup) validate(dedicated bool) error {
	if !dedicated {
		if len(s.Groups) > 0 || s.Workspace != "" {
			return errors.New("setup requires user")
		}
		return nil
	}
	switch s.Workspace {
	case "", workspacePrivate, workspaceSharedRead, workspaceSharedWrite:
	default:
		return fmt.Errorf("setup.workspace must be %s, %s, or %s", workspacePrivate, workspaceSharedRead, workspaceSharedWrite)
	}
	seen := make(map[string]struct{}, len(s.Groups))
	for _, name := range s.Groups {
		if err := validateOSGroupName(name); err != nil {
			return fmt.Errorf("setup.groups: %w", err)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("setup.groups: duplicate group %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func (s agentSetup) workspaceMode() string {
	if s.Workspace == "" {
		return workspacePrivate
	}
	return s.Workspace
}

func validateUniqueAgentUsers(agents map[string]agentDefinition) error {
	seen := map[string]string{}
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		name := agents[id].User
		if name == "" {
			continue
		}
		if other, exists := seen[name]; exists {
			return fmt.Errorf("agents %q and %q share OS user %q", other, id, name)
		}
		seen[name] = id
	}
	return nil
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
