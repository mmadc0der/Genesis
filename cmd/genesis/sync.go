package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

const (
	syncTokenEnv   = "GENESIS_SYNC_TOKEN"
	maxSyncBytes   = 64 << 10
	syncIPCTimeout = 30 * time.Second
	syncRetryAfter = "1"
	scopeAgents    = "agents"
	scopeRules     = "rules"
	scopeRepos     = "repos"
)

type syncScope struct {
	Agents bool
	Rules  bool
	Repos  bool
}

type syncRequest struct {
	Scope []string `json:"scope"`
}

type privilegedStatus struct {
	Attached     bool                `json:"attached"`
	HostMutation string              `json:"host_mutation"`
	Applied      []string            `json:"applied"`
	Unsupported  []unsupportedChange `json:"unsupported"`
}

type syncResponse struct {
	Digest         string           `json:"digest"`
	Scope          []string         `json:"scope"`
	Agents         int              `json:"agents"`
	Rules          int              `json:"rules"`
	Repositories   int              `json:"repositories"`
	Privileged     privilegedStatus `json:"privileged"`
	RepositoryPlan repositoryPlan   `json:"repository_plan"`
}

func parseScope(values []string) (syncScope, error) {
	if len(values) == 0 {
		return syncScope{Agents: true, Rules: true, Repos: true}, nil
	}
	var scope syncScope
	seen := map[string]struct{}{}
	for _, item := range values {
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		switch item {
		case scopeAgents:
			scope.Agents = true
		case scopeRules:
			scope.Rules = true
		case scopeRepos:
			scope.Repos = true
		default:
			return syncScope{}, fmt.Errorf("unknown sync scope %q", item)
		}
	}
	if !scope.Agents && !scope.Rules && !scope.Repos {
		return syncScope{}, errors.New("sync scope must include agents, rules, and/or repos")
	}
	return scope, nil
}

func (s syncScope) list() []string {
	out := make([]string, 0, 3)
	if s.Agents {
		out = append(out, scopeAgents)
	}
	if s.Rules {
		out = append(out, scopeRules)
	}
	if s.Repos {
		out = append(out, scopeRepos)
	}
	return out
}

func (s *eventServer) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
		return
	}
	if s.syncToken == "" {
		http.Error(w, "sync is disabled", http.StatusUnauthorized)
		return
	}
	if !s.authorizedSync(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	scope, err := decodeSyncRequest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.syncing {
		s.mu.Unlock()
		http.Error(w, "sync already in progress", http.StatusConflict)
		return
	}
	s.syncing = true
	current := s.generation
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.syncing = false
		s.mu.Unlock()
	}()

	next, err := loadScopedGeneration(s.agentsDir, s.rulesDir, s.reposDir, scope, current)
	if err != nil {
		s.log().Error("sync load", "error", err, "scope", scope.list())
		http.Error(w, syncLoadMessage(err), http.StatusInternalServerError)
		return
	}

	repositoryPlan, err := buildRepositoryPlan(next.repos, next.reposActive, scope.Repos)
	if err != nil {
		s.log().Error("sync repository plan", "error", err, "digest", next.digest)
		http.Error(w, "repository plan is invalid", http.StatusInternalServerError)
		return
	}
	plan := buildPlan(next.agents, scope.Agents)
	ctx, cancel := context.WithTimeout(r.Context(), syncIPCTimeout)
	defer cancel()
	result, attached, err := s.coordinate(ctx, plan)
	if err != nil {
		s.log().Error("sync privileged coordinate", "error", err, "digest", next.digest)
		http.Error(w, "privileged coordination failed", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.generation = next
	s.mu.Unlock()

	s.log().Info("genesis synced",
		"digest", next.digest,
		"scope", scope.list(),
		"agents", len(next.agents),
		"rules", len(next.rules),
		"repositories", len(next.repos),
		"repositories_active", next.reposActive,
		"privileged_attached", attached,
		"host_mutation", result.HostMutation,
		"applied", len(result.Applied),
		"unsupported", len(result.Unsupported),
		"retained", len(result.Retained),
		"repository_remote_mutation", repositoryPlan.RemoteMutation,
		"repository_unsupported", len(repositoryPlan.Unsupported),
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(syncResponse{
		Digest:       next.digest,
		Scope:        scope.list(),
		Agents:       len(next.agents),
		Rules:        len(next.rules),
		Repositories: len(next.repos),
		Privileged: privilegedStatus{
			Attached:     attached,
			HostMutation: result.HostMutation,
			Applied:      result.Applied,
			Unsupported:  result.Unsupported,
		},
		RepositoryPlan: repositoryPlan,
	})
}

func (s *eventServer) authorizedSync(r *http.Request) bool {
	if s.syncToken == "" {
		return false
	}
	header := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	provided := []byte(token)
	expected := []byte(s.syncToken)
	if len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(provided, expected) == 1
}

func decodeSyncRequest(w http.ResponseWriter, r *http.Request) (syncScope, error) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSyncBytes))
	if err != nil {
		return syncScope{}, err
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return parseScope(nil)
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return syncScope{}, errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var body syncRequest
	if err := decoder.Decode(&body); err != nil {
		return syncScope{}, fmt.Errorf("invalid sync JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return syncScope{}, errors.New("sync body must contain one JSON object")
		}
		return syncScope{}, fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return parseScope(body.Scope)
}

func (s *eventServer) coordinate(ctx context.Context, plan privilegedPlan) (coordinateResult, bool, error) {
	if s.coordinator == nil {
		result, err := evaluatePlan(plan)
		return result, false, err
	}
	result, err := s.coordinator.Coordinate(ctx, plan)
	return result, true, err
}

func syncLoadMessage(err error) string {
	var loadErr *generationLoadError
	if errors.As(err, &loadErr) {
		switch loadErr.kind {
		case "rules":
			return "rules are invalid"
		case "repos":
			return "repositories are invalid"
		}
	}
	return "agents are invalid"
}
