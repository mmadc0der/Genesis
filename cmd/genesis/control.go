package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	controlDefaultListen   = "127.0.0.1:8790"
	controlDefaultListener = "http://127.0.0.1:8787"
	controlDefaultWeb      = "web/dist"
	controlSource          = "urn:genesis:control"
	maxControlBody         = 1 << 20
	stderrTailBytes        = 8 << 10
	defaultRunLimit        = 40
	maxRunLimit            = 200
	defaultEventLimit      = 200
	// Later journal pages ask for up to this many events. A common run is
	// about 1 KB of JSON per event, so 4000 stays near 4 MB.
	maxEventLimit = 4000

	driftInSync         = "in_sync"
	driftDraft          = "draft"
	driftListenerDown   = "listener_unavailable"
	driftDesiredBad     = "desired_invalid"
	driftGenerationHuge = "generation_too_large"
	maxGenerationBytes  = 32 << 20

	presenceActive     = "active"
	presenceDraft      = "draft"
	presenceActiveOnly = "active_only"
	presenceUnknown    = "unknown"

	runStateOpen      = "open"
	runStateCompleted = "completed"
	runStateFailed    = "failed"
)

type controlConfig struct {
	listen       string
	listenerURL  string
	agentsDir    string
	rulesDir     string
	reposDir     string
	providersDir string
	dataDir      string
	syncToken    string
	webDir       string
}

type controlServer struct {
	agentsDir       string
	rulesDir        string
	reposDir        string
	providersDir    string
	dataDir         string
	listenerURL     string
	syncToken       string
	webDir          string
	logger          *slog.Logger
	listenerClient  *http.Client
	pollEvery       time.Duration
	generationLimit int
	newEventID      func() (string, error)
	journalMu       sync.Mutex
	journals        map[string]journalSnap
	liveMu          sync.Mutex
	liveSet         map[*liveSubs]struct{}
	feedOnce        sync.Once
	feedReadyOnce   sync.Once
	feedReady       chan struct{}
}

type journalSnap struct {
	size    int64
	offset  int64
	summary runSummary
	seen    map[string]struct{}
}

type controlState struct {
	Listener     listenerStatus  `json:"listener"`
	Active       *generationView `json:"active"`
	Desired      *generationView `json:"desired"`
	DesiredError string          `json:"desired_error,omitempty"`
	Drift        string          `json:"drift"`
}

type listenerStatus struct {
	Reachable      bool   `json:"reachable"`
	OK             bool   `json:"ok"`
	SyncConfigured bool   `json:"sync_configured"`
	Syncing        bool   `json:"syncing"`
	Error          string `json:"error,omitempty"`
}

type listedAgent struct {
	agentView
	Presence string `json:"presence"`
}

type listedRule struct {
	ruleView
	Presence string `json:"presence"`
}

type runSummary struct {
	RunID        string       `json:"run_id"`
	Agent        string       `json:"agent"`
	Rule         string       `json:"rule"`
	State        string       `json:"state"`
	AcceptedAt   string       `json:"accepted_at,omitempty"`
	EndedAt      string       `json:"ended_at,omitempty"`
	LastSeq      string       `json:"last_seq"`
	SessionID    string       `json:"session_id,omitempty"`
	CauseID      string       `json:"cause_id,omitempty"`
	CauseType    string       `json:"cause_type,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
	Error        string       `json:"error,omitempty"`
	Usage        tokenAccount `json:"usage"`
}

type runDetail struct {
	runSummary
	EventCount int             `json:"event_count"`
	Result     json.RawMessage `json:"result,omitempty"`
	StderrTail string          `json:"stderr_tail,omitempty"`
}

type eventsPage struct {
	RunID   string           `json:"run_id"`
	Events  []lifecycleEvent `json:"events"`
	Cursor  string           `json:"cursor"`
	HasMore bool             `json:"has_more"`
}

type messageBody struct {
	Message string `json:"message"`
	Rule    string `json:"rule,omitempty"`
	Type    string `json:"type,omitempty"`
	Source  string `json:"source,omitempty"`
	Subject string `json:"subject,omitempty"`
}

func runControl(logger *slog.Logger, args []string) {
	cfg, err := parseControlConfig(args, logger)
	if err != nil {
		fail(logger, "control configuration", err)
	}
	if !listenIsLoopback(cfg.listen) {
		logger.Info("control listen is not loopback; there is no auth beyond the bind", "address", cfg.listen)
	}
	handler := newControlServer(cfg, logger)
	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	logger.Info("genesis control listening",
		"address", cfg.listen,
		"listener", cfg.listenerURL,
		"agents", cfg.agentsDir,
		"rules", cfg.rulesDir,
		"repos", cfg.reposDir,
		"providers", cfg.providersDir,
		"data", cfg.dataDir,
		"web", cfg.webDir,
		"sync_configured", cfg.syncToken != "",
	)
	if err := server.ListenAndServe(); err != nil {
		fail(logger, "serve control", err)
	}
}

func parseControlConfig(args []string, logger *slog.Logger) (controlConfig, error) {
	flags := flag.NewFlagSet("control", flag.ExitOnError)
	listenFlag := flags.String("listen", controlDefaultListen, "control panel listen address")
	listenerFlag := flags.String("listener", controlDefaultListener, "internal Genesis listener base URL")
	agentsFlag := flags.String("agents", "agents.d", "directory containing YAML agent definitions")
	rulesFlag := flags.String("rules", "rules.d", "directory containing YAML rules")
	reposFlag := flags.String("repos", "repos.d", "directory containing YAML repository declarations; missing directory leaves the layer inactive")
	providersFlag := flags.String("providers", defaultProvidersDir, "root-owned directory of provider identity files; missing directory leaves the layer inactive")
	dataFlag := flags.String("data", "genesis-data", "read-only per-run journal directory")
	tokenFlag := flags.String("sync-token", os.Getenv(syncTokenEnv), "bearer token sent only to the listener POST /sync")
	webFlag := flags.String("web", controlDefaultWeb, "directory of built control panel files; missing directory serves the API only")
	parseFlagSet(flags, args, logger)

	listenerURL, err := normalizeListenerURL(*listenerFlag)
	if err != nil {
		return controlConfig{}, err
	}
	agentsDir, err := existingDir(*agentsFlag, "agents")
	if err != nil {
		return controlConfig{}, err
	}
	rulesDir, err := existingDir(*rulesFlag, "rules")
	if err != nil {
		return controlConfig{}, err
	}
	reposDir := strings.TrimSpace(*reposFlag)
	if reposDir != "" {
		reposDir, err = filepath.Abs(reposDir)
		if err != nil {
			return controlConfig{}, fmt.Errorf("resolve repos directory: %w", err)
		}
	}
	providersDir := strings.TrimSpace(*providersFlag)
	if providersDir != "" {
		providersDir, err = filepath.Abs(providersDir)
		if err != nil {
			return controlConfig{}, fmt.Errorf("resolve providers directory: %w", err)
		}
	}
	dataDir, err := existingDir(*dataFlag, "data")
	if err != nil {
		return controlConfig{}, err
	}
	webDir := strings.TrimSpace(*webFlag)
	if webDir != "" {
		absolute, absErr := filepath.Abs(webDir)
		if absErr != nil {
			return controlConfig{}, absErr
		}
		info, statErr := os.Stat(absolute)
		if statErr != nil || !info.IsDir() {
			logger.Info("control ui directory is absent; serving API only", "web", absolute)
			webDir = ""
		} else {
			webDir = absolute
		}
	}
	return controlConfig{
		listen:       *listenFlag,
		listenerURL:  listenerURL,
		agentsDir:    agentsDir,
		rulesDir:     rulesDir,
		reposDir:     reposDir,
		providersDir: providersDir,
		dataDir:      dataDir,
		syncToken:    *tokenFlag,
		webDir:       webDir,
	}, nil
}

func normalizeListenerURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("listener URL must be absolute http or https, got %q", raw)
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func existingDir(path, name string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s directory is required", name)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s directory: %w", name, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("%s directory: %w", name, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s path %q is not a directory", name, absolute)
	}
	return absolute, nil
}

func listenIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newControlServer(cfg controlConfig, logger *slog.Logger) *controlServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &controlServer{
		agentsDir:       cfg.agentsDir,
		rulesDir:        cfg.rulesDir,
		reposDir:        cfg.reposDir,
		providersDir:    cfg.providersDir,
		dataDir:         cfg.dataDir,
		listenerURL:     cfg.listenerURL,
		syncToken:       cfg.syncToken,
		webDir:          cfg.webDir,
		logger:          logger,
		listenerClient:  &http.Client{Timeout: 45 * time.Second},
		pollEvery:       250 * time.Millisecond,
		generationLimit: maxGenerationBytes,
		newEventID:      newControlEventID,
		feedReady:       make(chan struct{}),
	}
}

func (c *controlServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopbackRequest(r) {
		http.Error(w, "host is not loopback", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/api/live" {
		c.handleLive(w, r)
		return
	}
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		c.handleAPI(w, r)
		return
	}
	c.handleStatic(w, r)
}

func (c *controlServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/api/health":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/api/state":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		c.handleState(w, r)
	case "/api/agents":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		c.handleAgents(w, r)
	case "/api/rules":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		c.handleRules(w, r)
	case "/api/repositories":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		c.handleRepositories(w, r)
	case "/api/runs":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		c.handleRuns(w, r)
	case "/api/events":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		c.handlePostEvents(w, r)
	case "/api/messages":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		c.handlePostMessages(w, r)
	case "/api/sync":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		c.handlePostSync(w, r)
	default:
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/agents/"):
			if !requireMethod(w, r, http.MethodGet) {
				return
			}
			c.handleAgent(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/rules/"):
			if !requireMethod(w, r, http.MethodGet) {
				return
			}
			c.handleRule(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/repositories/"):
			if !requireMethod(w, r, http.MethodGet) {
				return
			}
			c.handleRepository(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/runs/"):
			if !requireMethod(w, r, http.MethodGet) {
				return
			}
			c.handleRunRoute(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method must be "+method, http.StatusMethodNotAllowed)
	return false
}

func (c *controlServer) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, c.currentState(r))
}

func (c *controlServer) currentState(r *http.Request) controlState {
	state := controlState{Drift: driftDraft}
	desired, desiredErr := c.loadDesired()
	if desiredErr != nil {
		state.DesiredError = desiredErr.Error()
		state.Drift = driftDesiredBad
	} else {
		state.Desired = desired
	}
	active, listener, err := c.fetchActive(r)
	state.Listener = listener
	if err != nil {
		state.Listener.Error = err.Error()
		var overflow generationTooLargeError
		if errors.As(err, &overflow) {
			state.Listener.Reachable = true
			state.Listener.OK = false
			if state.Drift != driftDesiredBad {
				state.Drift = driftGenerationHuge
			}
			return state
		}
		if state.Drift != driftDesiredBad {
			state.Drift = driftListenerDown
		}
		return state
	}
	state.Active = active
	if active.SyncConfigured != nil {
		state.Listener.SyncConfigured = *active.SyncConfigured
	}
	if active.Syncing != nil {
		state.Listener.Syncing = *active.Syncing
	}
	if state.Drift == driftDesiredBad {
		return state
	}
	if desired != nil && desired.Digest == active.Digest {
		state.Drift = driftInSync
	} else {
		state.Drift = driftDraft
	}
	return state
}

func (c *controlServer) loadDesired() (*generationView, error) {
	loaded, err := loadGeneration(c.agentsDir, c.rulesDir, c.reposDir, c.providersDir)
	if err != nil {
		return nil, err
	}
	view := generationSnapshot(loaded, false, false)
	view.SyncConfigured = nil
	view.Syncing = nil
	return &view, nil
}

func (c *controlServer) fetchActive(r *http.Request) (*generationView, listenerStatus, error) {
	status := listenerStatus{}
	ctx := r.Context()
	healthReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.listenerURL+"/health", nil)
	if err != nil {
		return nil, status, err
	}
	healthResp, err := c.listenerClient.Do(healthReq)
	if err != nil {
		return nil, status, errors.New("listener is unreachable")
	}
	defer healthResp.Body.Close()
	_, _ = io.Copy(io.Discard, healthResp.Body)
	status.Reachable = true
	status.OK = healthResp.StatusCode == http.StatusOK
	if healthResp.StatusCode != http.StatusOK {
		return nil, status, fmt.Errorf("listener health returned %d", healthResp.StatusCode)
	}

	genReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.listenerURL+"/generation", nil)
	if err != nil {
		return nil, status, err
	}
	genResp, err := c.listenerClient.Do(genReq)
	if err != nil {
		return nil, status, errors.New("listener generation is unreachable")
	}
	defer genResp.Body.Close()
	limit := c.generationLimit
	if limit <= 0 {
		limit = maxGenerationBytes
	}
	body, err := readCapped(genResp.Body, limit)
	if err != nil {
		return nil, status, err
	}
	if genResp.StatusCode != http.StatusOK {
		return nil, status, fmt.Errorf("listener generation returned %d", genResp.StatusCode)
	}
	var view generationView
	if err := json.Unmarshal(body, &view); err != nil {
		return nil, status, fmt.Errorf("listener generation: %w", err)
	}
	status.OK = true
	if view.SyncConfigured != nil {
		status.SyncConfigured = *view.SyncConfigured
	}
	if view.Syncing != nil {
		status.Syncing = *view.Syncing
	}
	return &view, status, nil
}

func (c *controlServer) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents, desiredErr := c.listAgents(r)
	payload := map[string]any{"agents": agents}
	if desiredErr != "" {
		payload["desired_error"] = desiredErr
	}
	writeJSON(w, http.StatusOK, payload)
}

func (c *controlServer) handleAgent(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	if strings.Contains(id, "/") || agentIDPattern.FindString(id) != id {
		http.Error(w, "invalid agent id", http.StatusBadRequest)
		return
	}
	agents, _ := c.listAgents(r)
	for _, agent := range agents {
		if agent.ID == id {
			writeJSON(w, http.StatusOK, agent)
			return
		}
	}
	http.NotFound(w, r)
}

func (c *controlServer) listAgents(r *http.Request) ([]listedAgent, string) {
	state := c.currentState(r)
	byID := map[string]listedAgent{}
	if state.Desired != nil {
		for _, agent := range state.Desired.Agents {
			byID[agent.ID] = listedAgent{agentView: agent, Presence: presenceUnknown}
		}
	}
	if state.Active != nil {
		activeByID := map[string]agentView{}
		for _, agent := range state.Active.Agents {
			activeByID[agent.ID] = agent
			if _, ok := byID[agent.ID]; !ok {
				byID[agent.ID] = listedAgent{agentView: agent, Presence: presenceActiveOnly}
			}
		}
		for id, listed := range byID {
			if listed.Presence == presenceActiveOnly {
				continue
			}
			active, ok := activeByID[id]
			if ok && sameAgent(listed.agentView, active) {
				listed.Presence = presenceActive
			} else {
				listed.Presence = presenceDraft
			}
			byID[id] = listed
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]listedAgent, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out, state.DesiredError
}

func (c *controlServer) handleRules(w http.ResponseWriter, r *http.Request) {
	rules, desiredErr := c.listRules(r)
	payload := map[string]any{"rules": rules}
	if desiredErr != "" {
		payload["desired_error"] = desiredErr
	}
	writeJSON(w, http.StatusOK, payload)
}

func (c *controlServer) handleRule(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/rules/")
	if err := safeYAMLName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rules, _ := c.listRules(r)
	for _, candidate := range rules {
		if candidate.Name == name {
			writeJSON(w, http.StatusOK, candidate)
			return
		}
	}
	http.NotFound(w, r)
}

func (c *controlServer) listRules(r *http.Request) ([]listedRule, string) {
	state := c.currentState(r)
	order := make([]string, 0)
	byName := map[string]listedRule{}
	if state.Desired != nil {
		for _, candidate := range state.Desired.Rules {
			order = append(order, candidate.Name)
			byName[candidate.Name] = listedRule{ruleView: candidate, Presence: presenceUnknown}
		}
	}
	if state.Active != nil {
		activeByName := map[string]ruleView{}
		for _, candidate := range state.Active.Rules {
			activeByName[candidate.Name] = candidate
			if _, ok := byName[candidate.Name]; !ok {
				order = append(order, candidate.Name)
				byName[candidate.Name] = listedRule{ruleView: candidate, Presence: presenceActiveOnly}
			}
		}
		for name, listed := range byName {
			if listed.Presence == presenceActiveOnly {
				continue
			}
			active, ok := activeByName[name]
			if ok && sameRule(listed.ruleView, active) {
				listed.Presence = presenceActive
			} else {
				listed.Presence = presenceDraft
			}
			byName[name] = listed
		}
	}
	out := make([]listedRule, 0, len(order))
	seen := map[string]struct{}{}
	for _, name := range order {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, byName[name])
	}
	return out, state.DesiredError
}

type listedRepository struct {
	repositoryDefinition
	Presence    string              `json:"presence"`
	Observation string              `json:"observation,omitempty"`
	Observed    *repositoryObserved `json:"observed,omitempty"`
	Drift       []repositoryDrift   `json:"drift,omitempty"`
}

func (c *controlServer) handleRepositories(w http.ResponseWriter, r *http.Request) {
	repositories, active, desiredErr := c.listRepositories(r)
	payload := map[string]any{
		"repositories":        repositories,
		"repositories_active": active,
	}
	if desiredErr != "" {
		payload["desired_error"] = desiredErr
	}
	writeJSON(w, http.StatusOK, payload)
}

func (c *controlServer) handleRepository(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/repositories/")
	if strings.Contains(id, "/") || agentIDPattern.FindString(id) != id {
		http.Error(w, "invalid repository id", http.StatusBadRequest)
		return
	}
	repositories, _, _ := c.listRepositories(r)
	for _, repository := range repositories {
		if repository.ID == id {
			writeJSON(w, http.StatusOK, repository)
			return
		}
	}
	http.NotFound(w, r)
}

func (c *controlServer) listRepositories(r *http.Request) ([]listedRepository, bool, string) {
	state := c.currentState(r)
	byID := map[string]listedRepository{}
	if state.Desired != nil {
		for _, repository := range state.Desired.Repositories {
			byID[repository.ID] = listedRepository{repositoryDefinition: repository, Presence: presenceUnknown}
		}
	}
	if state.Active != nil {
		activeByID := map[string]repositoryDefinition{}
		for _, repository := range state.Active.Repositories {
			activeByID[repository.ID] = repository
			if _, ok := byID[repository.ID]; !ok {
				byID[repository.ID] = listedRepository{repositoryDefinition: repository, Presence: presenceActiveOnly}
			}
		}
		for id, listed := range byID {
			if listed.Presence == presenceActiveOnly {
				continue
			}
			active, ok := activeByID[id]
			if ok && sameRepository(listed.repositoryDefinition, active) {
				listed.Presence = presenceActive
			} else {
				listed.Presence = presenceDraft
			}
			byID[id] = listed
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]listedRepository, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	out = c.annotateRepositoryObservation(out, state.Active)
	active := false
	switch {
	case state.Desired != nil:
		active = state.Desired.RepositoriesActive
	case state.Active != nil:
		active = state.Active.RepositoriesActive
	default:
		usable, err := repositoryDirectoryUsable(c.reposDir)
		active = err == nil && usable
	}
	return out, active, state.DesiredError
}

func (c *controlServer) annotateRepositoryObservation(listed []listedRepository, active *generationView) []listedRepository {
	if c == nil || active == nil || strings.TrimSpace(c.dataDir) == "" {
		return listed
	}
	journal, err := readRepositoryJournal(c.dataDir)
	if err != nil || journal.Digest == "" {
		return listed
	}
	digest, err := repositoryListDigest(active.Repositories)
	if err != nil || digest != journal.Digest {
		return listed
	}
	byID := observedByID(journal.Observed)
	activeByID := map[string]repositoryDefinition{}
	for _, repository := range active.Repositories {
		activeByID[repository.ID] = repository
	}
	drift, _ := classifyRepositories(active.Repositories, byID)
	driftByID := map[string][]repositoryDrift{}
	for _, item := range drift {
		driftByID[item.ID] = append(driftByID[item.ID], item)
	}
	for i := range listed {
		definition, ok := activeByID[listed[i].ID]
		if !ok || listed[i].Org != definition.Org || listed[i].Name != definition.Name {
			continue
		}
		if observed, found := byID[definition.ID]; found {
			copied := observed
			listed[i].Observed = &copied
			listed[i].Observation = observationObserved
		} else if definition.Lifecycle.Existing != lifecycleExistingAdopt {
			listed[i].Observation = observationObserved
		} else {
			continue
		}
		listed[i].Drift = driftByID[definition.ID]
		if listed[i].Drift == nil {
			listed[i].Drift = []repositoryDrift{}
		}
	}
	return listed
}

func sameRepository(left, right repositoryDefinition) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func sameAgent(left, right agentView) bool {
	return left.ID == right.ID &&
		left.Instructions == right.Instructions &&
		left.Cwd == right.Cwd &&
		left.Home == right.Home &&
		left.User == right.User &&
		sameSetup(left.Setup, right.Setup) &&
		maps.Equal(left.Env, right.Env) &&
		slices.Equal(left.Secrets, right.Secrets) &&
		sameGitHub(left.GitHub, right.GitHub) &&
		sameOptionalInt(left.MaxParallel, right.MaxParallel) &&
		left.ReasoningEffort == right.ReasoningEffort
}

func sameOptionalInt(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameSetup(left, right *agentSetup) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Workspace == right.Workspace && slices.Equal(left.Groups, right.Groups)
}

func sameRule(left, right ruleView) bool {
	return left.Name == right.Name && left.Agent == right.Agent && maps.Equal(left.Match, right.Match)
}

func safeYAMLName(name string) error {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return errors.New("invalid rule name")
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return nil
	default:
		return errors.New("invalid rule name")
	}
}

func (c *controlServer) handleRuns(w http.ResponseWriter, r *http.Request) {
	limit := queryLimit(r, "limit", defaultRunLimit, maxRunLimit)
	runs, err := c.listRuns(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (c *controlServer) handleRunRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/runs/")
	id, tail, _ := strings.Cut(rest, "/")
	if err := validateRunID(id); err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	if tail == "" {
		detail, err := c.readRunDetail(id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, detail)
		return
	}
	if tail != "events" {
		http.NotFound(w, r)
		return
	}
	after, err := parseCursor(r.URL.Query().Get("after"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit := queryLimit(r, "limit", defaultEventLimit, maxEventLimit)
	page, err := c.readEvents(id, after, limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (c *controlServer) listRuns(limit int) ([]runSummary, error) {
	dir := filepath.Join(c.dataDir, runsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []runSummary{}, nil
		}
		return nil, err
	}
	runs := make([]runSummary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := validateRunID(entry.Name()); err != nil {
			continue
		}
		summary, err := c.summarizeCached(entry.Name())
		if err != nil || summary.RunID == "" {
			continue
		}
		runs = append(runs, summary)
	}
	slices.SortFunc(runs, func(left, right runSummary) int {
		if left.AcceptedAt != right.AcceptedAt {
			if left.AcceptedAt < right.AcceptedAt {
				return 1
			}
			return -1
		}
		return strings.Compare(right.RunID, left.RunID)
	})
	if limit >= 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func (c *controlServer) readRunDetail(runID string) (runDetail, error) {
	events, err := c.readRunEvents(runID)
	if err != nil {
		return runDetail{}, err
	}
	if len(events) == 0 {
		return runDetail{}, os.ErrNotExist
	}
	detail := runDetail{runSummary: summarizeRun(events), EventCount: len(events)}
	runDir := filepath.Join(c.dataDir, runsDirName, runID)
	detail.Usage = usageFromRun(runDir, events)
	if result, err := os.ReadFile(filepath.Join(runDir, resultFileName)); err == nil && json.Valid(result) {
		detail.Result = json.RawMessage(redactPrivateKeys(result))
	}
	if tail, err := readFileTail(filepath.Join(runDir, stderrFileName), stderrTailBytes); err == nil && tail != "" {
		detail.StderrTail = tail
	}
	return detail, nil
}

func (c *controlServer) readEvents(runID string, after uint64, limit int) (eventsPage, error) {
	events, err := c.readRunEvents(runID)
	if err != nil {
		return eventsPage{}, err
	}
	if len(events) == 0 {
		return eventsPage{}, os.ErrNotExist
	}
	filtered := make([]lifecycleEvent, 0, len(events))
	for _, event := range events {
		seq, ok := eventSequence(event)
		if !ok || seq <= after {
			continue
		}
		filtered = append(filtered, event)
	}
	page := eventsPage{RunID: runID, Events: []lifecycleEvent{}, Cursor: strconv.FormatUint(after, 10)}
	if limit < len(filtered) {
		page.HasMore = true
		filtered = filtered[:limit]
	}
	if len(filtered) > 0 {
		page.Events = filtered
		if seq, ok := eventSequence(filtered[len(filtered)-1]); ok {
			page.Cursor = strconv.FormatUint(seq, 10)
		}
	}
	return page, nil
}

func (c *controlServer) readRunEvents(runID string) ([]lifecycleEvent, error) {
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	path := filepath.Join(c.dataDir, runsDirName, runID, eventsFileName)
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	events, err := readJournalPrefix(path)
	if err != nil {
		return nil, err
	}
	for index := range events {
		events[index].Data = redactPrivateKeys(events[index].Data)
	}
	return events, nil
}

func (c *controlServer) summarizeCached(runID string) (runSummary, error) {
	path := filepath.Join(c.dataDir, runsDirName, runID, eventsFileName)
	info, err := os.Stat(path)
	if err != nil {
		return runSummary{}, err
	}
	usagePath := filepath.Join(c.dataDir, runsDirName, runID, usageFileName)
	c.journalMu.Lock()
	snap, ok := c.journals[runID]
	c.journalMu.Unlock()
	if ok && snap.size == info.Size() && snap.offset <= info.Size() {
		summary := snap.summary
		if account, _, found := readUsageAccount(usagePath); found {
			summary.Usage = account
		}
		return summary, nil
	}
	offset := int64(0)
	summary := runSummary{State: runStateOpen, LastSeq: "0"}
	if ok && info.Size() >= snap.offset {
		offset = snap.offset
		summary = snap.summary
	}
	events, next, err := readJournalFrom(path, offset)
	if err != nil {
		return runSummary{}, err
	}
	for index := range events {
		events[index].Data = redactPrivateKeys(events[index].Data)
	}
	if offset == 0 {
		summary = summarizeRun(events)
	} else if len(events) > 0 {
		summary = foldRunSummary(summary, events)
	}
	account, seen, fromFile := readUsageAccount(usagePath)
	if fromFile {
		summary.Usage = account
	} else if offset == 0 {
		summary.Usage, seen = foldUsage(tokenAccount{}, nil, events)
	} else if len(events) > 0 {
		summary.Usage, seen = foldUsage(summary.Usage, snap.seen, events)
	} else {
		seen = snap.seen
	}
	c.journalMu.Lock()
	if c.journals == nil {
		c.journals = map[string]journalSnap{}
	}
	c.journals[runID] = journalSnap{size: info.Size(), offset: next, summary: summary, seen: seen}
	c.journalMu.Unlock()
	return summary, nil
}

func summarizeRun(events []lifecycleEvent) runSummary {
	return foldRunSummary(runSummary{State: runStateOpen, LastSeq: "0"}, events)
}

func foldRunSummary(summary runSummary, events []lifecycleEvent) runSummary {
	if len(events) == 0 {
		return summary
	}
	if summary.RunID == "" {
		first := events[0]
		summary.RunID = first.RunID
		summary.Agent = first.AgentID
		summary.Rule = first.Rulefile
		summary.AcceptedAt = first.Time
		summary.CauseID = first.CauseID
		summary.CauseType = first.CauseType
	}
	for _, event := range events {
		if seq, ok := eventSequence(event); ok {
			summary.LastSeq = strconv.FormatUint(seq, 10)
		}
		if event.SessionID != "" {
			summary.SessionID = event.SessionID
		}
		switch event.Type {
		case lifecycleTypeResult:
			var payload struct {
				FinishReason string `json:"finish_reason"`
			}
			_ = json.Unmarshal(event.Data, &payload)
			summary.FinishReason = payload.FinishReason
		case lifecycleTypeTurn:
			if message, ok := journalTurnFailure(event.Data); ok {
				if message == "" {
					message = "turn/end recorded a provider failure"
				}
				summary.Error = message
			}
		case lifecycleTypeError:
			var payload struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(event.Data, &payload)
			if summary.Error != "" && isFinishWrapper(&runnerFailure{Message: payload.Message}) {
				continue
			}
			summary.Error = payload.Message
		case lifecycleTypeEnd:
			summary.EndedAt = event.Time
			var payload struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(event.Data, &payload)
			switch payload.State {
			case endStateCompleted:
				summary.State = runStateCompleted
			default:
				summary.State = runStateFailed
			}
		}
	}
	return summary
}

func eventSequence(event lifecycleEvent) (uint64, bool) {
	if event.Sequence == "" {
		return 0, false
	}
	value, err := strconv.ParseUint(event.Sequence, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func parseCursor(raw string) (uint64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.New("cursor must be an unsigned integer")
	}
	return value, nil
}

func queryLimit(r *http.Request, name string, fallback, max int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

func readFileTail(path string, max int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if info.Size() > int64(max) {
		start = info.Size() - int64(max)
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	buf, err := io.ReadAll(io.LimitReader(file, int64(max)))
	if err != nil {
		return "", err
	}
	return string(redactPrivateKeys(buf)), nil
}

func (c *controlServer) handlePostEvents(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != cloudEventsJSON {
		http.Error(w, "Content-Type must be application/cloudevents+json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBytes))
	if err != nil {
		http.Error(w, "invalid event body", http.StatusBadRequest)
		return
	}
	c.forward(w, r, http.MethodPost, "/events", cloudEventsJSON, body, false)
}

func (c *controlServer) handlePostMessages(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSyncBytes))
	if err != nil {
		http.Error(w, "invalid message body", http.StatusBadRequest)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var body messageBody
	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "invalid message JSON", http.StatusBadRequest)
		return
	}
	event, err := c.messageEvent(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		http.Error(w, "failed to encode event", http.StatusInternalServerError)
		return
	}
	c.forward(w, r, http.MethodPost, "/events", cloudEventsJSON, encoded, false)
}

func (c *controlServer) messageEvent(body messageBody) (map[string]any, error) {
	if strings.TrimSpace(body.Message) == "" {
		return nil, errors.New("message is required")
	}
	attributes := map[string]string{}
	if body.Rule != "" {
		if err := safeYAMLName(body.Rule); err != nil {
			return nil, err
		}
		var loaded rule
		if err := loadYAMLDocument(filepath.Join(c.rulesDir, body.Rule), &loaded); err != nil {
			return nil, fmt.Errorf("rule %s: %w", body.Rule, err)
		}
		if len(loaded.Match) == 0 {
			return nil, fmt.Errorf("rule %s has no match", body.Rule)
		}
		maps.Copy(attributes, loaded.Match)
	}
	if body.Type != "" {
		attributes["type"] = body.Type
	}
	if body.Source != "" {
		attributes["source"] = body.Source
	}
	if body.Subject != "" {
		attributes["subject"] = body.Subject
	}
	return buildMessageEvent(attributes, body.Message, c.newEventID)
}

func buildMessageEvent(attributes map[string]string, message string, newID func() (string, error)) (map[string]any, error) {
	if attributes == nil {
		attributes = map[string]string{}
	}
	if value, ok := attributes["specversion"]; ok && value != "1.0" {
		return nil, fmt.Errorf("rule match %q conflicts with required CloudEvent specversion 1.0", "specversion")
	}
	if _, ok := attributes["data"]; ok {
		return nil, errors.New("rule match \"data\" conflicts with the message payload")
	}
	for _, key := range []string{"id", "type", "source"} {
		if value, ok := attributes[key]; ok && strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("rule match %q must be a non-empty string", key)
		}
	}
	event := map[string]any{
		"specversion": "1.0",
		"data":        map[string]any{"message": message},
	}
	for key, value := range attributes {
		if key == "specversion" {
			continue
		}
		event[key] = value
	}
	if _, ok := event["id"]; !ok {
		if newID == nil {
			newID = newControlEventID
		}
		id, err := newID()
		if err != nil {
			return nil, err
		}
		event["id"] = id
	}
	if _, ok := event["source"]; !ok {
		event["source"] = controlSource
	}
	eventType, _ := event["type"].(string)
	if strings.TrimSpace(eventType) == "" {
		return nil, errors.New("type is required when the rule does not match one")
	}
	source, _ := event["source"].(string)
	if strings.TrimSpace(source) == "" {
		return nil, errors.New("source must be a non-empty string")
	}
	return event, nil
}

func (c *controlServer) handlePostSync(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSyncBytes))
	if err != nil {
		http.Error(w, "invalid sync body", http.StatusBadRequest)
		return
	}
	if err := validateSyncObject(body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.forward(w, r, http.MethodPost, "/sync", "application/json", body, true)
}

func validateSyncObject(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("sync body must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var payload syncRequest
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("invalid sync JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("sync body must contain one JSON object")
		}
		return fmt.Errorf("invalid sync JSON: %w", err)
	}
	if _, err := parseScope(payload.Scope); err != nil {
		return err
	}
	return nil
}

func (c *controlServer) forward(w http.ResponseWriter, r *http.Request, method, path, contentType string, body []byte, authorize bool) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(r.Context(), method, c.listenerURL+path, reader)
	if err != nil {
		http.Error(w, "failed to reach listener", http.StatusBadGateway)
		return
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if authorize && c.syncToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.syncToken)
	}
	response, err := c.listenerClient.Do(request)
	if err != nil {
		http.Error(w, "listener is unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxControlBody))
	if err != nil {
		http.Error(w, "listener response was unreadable", http.StatusBadGateway)
		return
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if retry := response.Header.Get("Retry-After"); retry != "" {
		w.Header().Set("Retry-After", retry)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
}

func (c *controlServer) handleStatic(w http.ResponseWriter, r *http.Request) {
	if c.webDir == "" || r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/" {
		http.ServeFile(w, r, filepath.Join(c.webDir, "index.html"))
		return
	}
	rel := strings.TrimPrefix(pathClean(r.URL.Path), "/")
	if rel == "" || rel == "." {
		http.ServeFile(w, r, filepath.Join(c.webDir, "index.html"))
		return
	}
	full := filepath.Join(c.webDir, rel)
	if !strings.HasPrefix(full, c.webDir+string(os.PathSeparator)) && full != c.webDir {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, full)
}

func pathClean(urlPath string) string {
	return strings.TrimPrefix(path.Clean("/"+urlPath), "/")
}

func loopbackRequest(r *http.Request) bool {
	if !loopbackHost(r.Host) {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || !loopbackHost(parsed.Host) {
		return false
	}
	return true
}

func loopbackHost(hostport string) bool {
	host := hostport
	if split, _, err := net.SplitHostPort(hostport); err == nil {
		host = split
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type generationTooLargeError struct {
	Limit int
}

func (e generationTooLargeError) Error() string {
	return fmt.Sprintf("listener generation is larger than %d bytes", e.Limit)
}

func readCapped(reader io.Reader, max int) ([]byte, error) {
	if max < 1 {
		return nil, errors.New("generation read limit must be positive")
	}
	body, err := io.ReadAll(io.LimitReader(reader, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > max {
		return nil, generationTooLargeError{Limit: max}
	}
	return body, nil
}

func newControlEventID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "ctl_" + hex.EncodeToString(random[:]), nil
}
