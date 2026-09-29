package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
)

type generation struct {
	agents          map[string]agentDefinition
	rules           []rule
	repos           map[string]repositoryDefinition
	reposActive     bool
	providers       map[string]providerDefinition
	providersActive bool
	digest          string
}

func loadGeneration(agentsDir, rulesDir, reposDir, providersDir string) (*generation, error) {
	return loadScopedGeneration(agentsDir, rulesDir, reposDir, providersDir, syncScope{Agents: true, Rules: true, Repos: true}, nil)
}

func loadScopedGeneration(agentsDir, rulesDir, reposDir, providersDir string, scope syncScope, current *generation) (*generation, error) {
	if !scope.Agents && !scope.Rules && !scope.Repos {
		return nil, fmt.Errorf("sync scope must include agents, rules, and/or repos")
	}
	if current == nil && (!scope.Agents || !scope.Rules) {
		return nil, fmt.Errorf("initial generation must load agents and rules")
	}

	agents := map[string]agentDefinition{}
	var rules []rule
	repos := map[string]repositoryDefinition{}
	reposActive := false
	providers := map[string]providerDefinition{}
	providersActive := false
	if current != nil {
		agents = current.agents
		rules = current.rules
		repos = current.repos
		reposActive = current.reposActive
		providers = current.providers
		providersActive = current.providersActive
	}

	if scope.Agents {
		loaded, err := loadAgents(agentsDir)
		if err != nil {
			return nil, &generationLoadError{kind: "agents", err: err}
		}
		agents = loaded
		if !scope.Rules {
			for _, candidate := range rules {
				if err := candidate.validate(agents); err != nil {
					return nil, &generationLoadError{kind: "rules", err: err}
				}
			}
		}
	}
	if scope.Rules {
		loaded, err := loadRules(rulesDir, agents)
		if err != nil {
			return nil, &generationLoadError{kind: "rules", err: err}
		}
		rules = loaded
	}
	if scope.Repos {
		loaded, active, err := loadRepositories(reposDir)
		if err != nil {
			return nil, &generationLoadError{kind: "repos", err: err}
		}
		repos = loaded
		reposActive = active
	}
	if current == nil || scope.Agents || scope.Repos {
		loaded, active, err := loadProviders(providersDir, agentsDir, rulesDir, reposDir)
		if err != nil {
			return nil, &generationLoadError{kind: "providers", err: err}
		}
		providers = loaded
		providersActive = active
	}
	bound, err := bindGrants(agents, repos, reposActive, providers, providersActive)
	if err != nil {
		return nil, &generationLoadError{kind: "agents", err: err}
	}
	agents = bound

	digest, err := digestGeneration(agents, rules, repos, reposActive, providers, providersActive)
	if err != nil {
		kind := "agents"
		if reposActive {
			kind = "repos"
		}
		if providersActive {
			kind = "providers"
		}
		return nil, &generationLoadError{kind: kind, err: err}
	}
	return &generation{
		agents:          agents,
		rules:           rules,
		repos:           repos,
		reposActive:     reposActive,
		providers:       providers,
		providersActive: providersActive,
		digest:          digest,
	}, nil
}

type generationLoadError struct {
	kind string
	err  error
}

func (e *generationLoadError) Error() string {
	return e.err.Error()
}

func (e *generationLoadError) Unwrap() error {
	return e.err
}

func digestGeneration(agents map[string]agentDefinition, rules []rule, repos map[string]repositoryDefinition, reposActive bool, providers map[string]providerDefinition, providersActive bool) (string, error) {
	if err := generationUTF8(agents, rules, repos, reposActive, providers, providersActive); err != nil {
		return "", err
	}
	type agentDigest struct {
		ID              string            `json:"id"`
		Instructions    string            `json:"instructions"`
		Cwd             string            `json:"cwd"`
		Home            string            `json:"home"`
		User            string            `json:"user"`
		Setup           *agentSetup       `json:"setup"`
		Env             map[string]string `json:"env"`
		Secrets         []string          `json:"secrets"`
		GitHub          *digestGitHub     `json:"github,omitempty"`
		MaxParallel     *int              `json:"max_parallel,omitempty"`
		ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	}
	type ruleDigest struct {
		Name  string            `json:"name"`
		Match map[string]string `json:"match"`
		Agent string            `json:"agent"`
	}

	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	agentDigests := make([]agentDigest, 0, len(ids))
	for _, id := range ids {
		definition := agents[id]
		agentDigests = append(agentDigests, agentDigest{
			ID:              id,
			Instructions:    definition.Instructions,
			Cwd:             definition.Cwd,
			Home:            definition.Home,
			User:            definition.User,
			Setup:           definition.Setup,
			Env:             definition.Env,
			Secrets:         definition.Secrets,
			GitHub:          gitHubDigest(definition),
			MaxParallel:     definition.MaxParallel,
			ReasoningEffort: definition.ReasoningEffort,
		})
	}
	ruleDigests := make([]ruleDigest, 0, len(rules))
	for _, candidate := range rules {
		ruleDigests = append(ruleDigests, ruleDigest{
			Name:  candidate.name,
			Match: candidate.Match,
			Agent: candidate.Agent,
		})
	}
	if !reposActive && !providersActive {
		payload, err := json.Marshal(struct {
			Agents []agentDigest `json:"agents"`
			Rules  []ruleDigest  `json:"rules"`
		}{Agents: agentDigests, Rules: ruleDigests})
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(payload)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	if reposActive && !providersActive {
		payload, err := json.Marshal(struct {
			Agents       []agentDigest          `json:"agents"`
			Rules        []ruleDigest           `json:"rules"`
			Repositories []repositoryDefinition `json:"repositories"`
		}{Agents: agentDigests, Rules: ruleDigests, Repositories: canonicalRepositories(repos)})
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(payload)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	if !reposActive {
		payload, err := json.Marshal(struct {
			Agents    []agentDigest    `json:"agents"`
			Rules     []ruleDigest     `json:"rules"`
			Providers []digestProvider `json:"providers"`
		}{Agents: agentDigests, Rules: ruleDigests, Providers: canonicalProviderDigests(providers)})
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(payload)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	payload, err := json.Marshal(struct {
		Agents       []agentDigest          `json:"agents"`
		Rules        []ruleDigest           `json:"rules"`
		Repositories []repositoryDefinition `json:"repositories"`
		Providers    []digestProvider       `json:"providers"`
	}{
		Agents:       agentDigests,
		Rules:        ruleDigests,
		Repositories: canonicalRepositories(repos),
		Providers:    canonicalProviderDigests(providers),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func generationUTF8(agents map[string]agentDefinition, rules []rule, repos map[string]repositoryDefinition, reposActive bool, providers map[string]providerDefinition, providersActive bool) error {
	for id, definition := range agents {
		if err := requireUTF8(id, definition.Instructions, definition.Cwd, definition.Home, definition.User); err != nil {
			return err
		}
		if definition.Setup != nil {
			if err := requireUTF8(definition.Setup.Workspace); err != nil {
				return err
			}
			if err := requireUTF8(definition.Setup.Groups...); err != nil {
				return err
			}
		}
		for key, value := range definition.Env {
			if err := requireUTF8(key, value); err != nil {
				return err
			}
		}
		if err := requireUTF8(definition.Secrets...); err != nil {
			return err
		}
		if definition.GitHub != nil {
			if err := requireUTF8(definition.GitHub.Repository, definition.GitHub.Identity, definition.GitHub.Git); err != nil {
				return err
			}
			for name, level := range definition.GitHub.Permissions {
				if err := requireUTF8(name, level); err != nil {
					return err
				}
			}
		}
	}
	for _, candidate := range rules {
		if err := requireUTF8(candidate.name, candidate.Agent); err != nil {
			return err
		}
		for key, value := range candidate.Match {
			if err := requireUTF8(key, value); err != nil {
				return err
			}
		}
	}
	if reposActive {
		for _, repository := range repos {
			if err := repositoryUTF8(repository); err != nil {
				return err
			}
		}
	}
	if !providersActive {
		return nil
	}
	return providerUTF8(providers)
}

func repositoryUTF8(repository repositoryDefinition) error {
	values := []string{
		repository.ID, repository.Provider, repository.Org, repository.Name,
		repository.Lifecycle.Remove, repository.Lifecycle.Existing,
		repository.Settings.Visibility, repository.Settings.Description, repository.Settings.DefaultBranch,
		repository.Actions.Allowed,
	}
	if repository.Bootstrap != nil {
		values = append(values, repository.Bootstrap.Template)
	}
	values = append(values, repository.Actions.Selected...)
	if repository.Secrets != nil {
		values = append(values, repository.Secrets.Repository...)
		for _, environment := range repository.Secrets.Environments {
			values = append(values, environment.Name)
			values = append(values, environment.Secrets...)
		}
	}
	if repository.Protection != nil {
		values = append(values, repository.Protection.Ruleset.Name)
		values = append(values, repository.Protection.Ruleset.RequiredChecks...)
	}
	for _, identity := range repository.Identities {
		values = append(values, identity.Name, identity.Role)
	}
	return requireUTF8(values...)
}

func requireUTF8(values ...string) error {
	for _, value := range values {
		if !utf8.ValidString(value) {
			return errors.New("generation contains invalid UTF-8")
		}
	}
	return nil
}
