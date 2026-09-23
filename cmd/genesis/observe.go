package main

import (
	"encoding/json"
	"net/http"
	"slices"
)

// Read-only listener views. They do not reload YAML, swap the cache, or
// accept commands. POST /events and POST /sync keep their existing semantics.

type agentView struct {
	ID           string            `json:"id"`
	Instructions string            `json:"instructions"`
	Cwd          string            `json:"cwd"`
	Home         string            `json:"home"`
	User         string            `json:"user,omitempty"`
	Setup        *agentSetup       `json:"setup,omitempty"`
	Env          map[string]string `json:"env"`
	Secrets      []string          `json:"secrets"`
	GitHub       *agentGitHubView  `json:"github,omitempty"`
}

type ruleView struct {
	Name  string            `json:"name"`
	Match map[string]string `json:"match"`
	Agent string            `json:"agent"`
}

type generationView struct {
	Digest             string                 `json:"digest"`
	Agents             []agentView            `json:"agents"`
	Rules              []ruleView             `json:"rules"`
	Repositories       []repositoryDefinition `json:"repositories"`
	RepositoriesActive bool                   `json:"repositories_active"`
	// Listener flags. Omitted on a file snapshot, which is not a process.
	SyncConfigured *bool `json:"sync_configured,omitempty"`
	Syncing        *bool `json:"syncing,omitempty"`
}

func (s *eventServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method must be GET", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *eventServer) handleGeneration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method must be GET", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	current := s.generation
	syncing := s.syncing
	configured := s.syncToken != ""
	s.mu.RUnlock()
	if current == nil {
		http.Error(w, "generation is not loaded", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, generationSnapshot(current, configured, syncing))
}

func generationSnapshot(current *generation, syncConfigured, syncing bool) generationView {
	view := generationView{
		Digest:             current.digest,
		Agents:             make([]agentView, 0, len(current.agents)),
		Rules:              make([]ruleView, 0, len(current.rules)),
		Repositories:       canonicalRepositories(current.repos),
		RepositoriesActive: current.reposActive,
		SyncConfigured:     boolPtr(syncConfigured),
		Syncing:            boolPtr(syncing),
	}
	ids := make([]string, 0, len(current.agents))
	for id := range current.agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		view.Agents = append(view.Agents, agentViewFrom(current.agents[id]))
	}
	for _, candidate := range current.rules {
		view.Rules = append(view.Rules, ruleView{
			Name:  candidate.name,
			Match: candidate.Match,
			Agent: candidate.Agent,
		})
	}
	return view
}

func boolPtr(value bool) *bool {
	return &value
}

func agentViewFrom(definition agentDefinition) agentView {
	env := definition.Env
	if env == nil {
		env = map[string]string{}
	}
	secrets := definition.Secrets
	if secrets == nil {
		secrets = []string{deepSeekAPIKey}
	}
	return agentView{
		ID:           definition.id,
		Instructions: definition.Instructions,
		Cwd:          definition.Cwd,
		Home:         definition.Home,
		User:         definition.User,
		Setup:        cloneAgentSetup(definition.Setup),
		Env:          env,
		Secrets:      append([]string(nil), secrets...),
		GitHub:       gitHubViewFrom(definition),
	}
}

func cloneAgentSetup(setup *agentSetup) *agentSetup {
	if setup == nil {
		return nil
	}
	copied := *setup
	if setup.Groups != nil {
		copied.Groups = append([]string(nil), setup.Groups...)
	}
	return &copied
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
