package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

type generation struct {
	agents map[string]agentDefinition
	rules  []rule
	digest string
}

func loadGeneration(agentsDir, rulesDir string) (*generation, error) {
	return loadScopedGeneration(agentsDir, rulesDir, syncScope{Agents: true, Rules: true}, nil)
}

func loadScopedGeneration(agentsDir, rulesDir string, scope syncScope, current *generation) (*generation, error) {
	if !scope.Agents && !scope.Rules {
		return nil, fmt.Errorf("sync scope must include agents and/or rules")
	}
	if current == nil && (!scope.Agents || !scope.Rules) {
		return nil, fmt.Errorf("initial generation must load agents and rules")
	}

	agents := map[string]agentDefinition{}
	var rules []rule
	if current != nil {
		agents = current.agents
		rules = current.rules
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

	return &generation{
		agents: agents,
		rules:  rules,
		digest: digestGeneration(agents, rules),
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

func digestGeneration(agents map[string]agentDefinition, rules []rule) string {
	type agentDigest struct {
		ID           string            `json:"id"`
		Instructions string            `json:"instructions"`
		Cwd          string            `json:"cwd"`
		Home         string            `json:"home"`
		User         string            `json:"user"`
		Setup        *agentSetup       `json:"setup"`
		Env          map[string]string `json:"env"`
		Secrets      []string          `json:"secrets"`
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
			ID:           id,
			Instructions: definition.Instructions,
			Cwd:          definition.Cwd,
			Home:         definition.Home,
			User:         definition.User,
			Setup:        definition.Setup,
			Env:          definition.Env,
			Secrets:      definition.Secrets,
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
	payload, err := json.Marshal(struct {
		Agents []agentDigest `json:"agents"`
		Rules  []ruleDigest  `json:"rules"`
	}{Agents: agentDigests, Rules: ruleDigests})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
