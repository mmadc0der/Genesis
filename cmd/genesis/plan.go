package main

import (
	"fmt"
	"slices"
)

const (
	intentEnsureAgentPaths     = "ensure_agent_paths"
	intentProvisionDeclaredEnv = "provision_declared_env"
	hostMutationNone           = "none"
)

type privilegedIntent struct {
	Kind    string   `json:"kind"`
	Agent   string   `json:"agent,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Home    string   `json:"home,omitempty"`
	EnvKeys []string `json:"env_keys,omitempty"`
}

type privilegedPlan struct {
	Intents []privilegedIntent `json:"intents"`
}

type unsupportedChange struct {
	Kind   string `json:"kind"`
	Agent  string `json:"agent,omitempty"`
	Reason string `json:"reason"`
}

type coordinateResult struct {
	HostMutation string              `json:"host_mutation"`
	Applied      []string            `json:"applied"`
	Unsupported  []unsupportedChange `json:"unsupported"`
}

func buildPlan(agents map[string]agentDefinition, includeAgents bool) privilegedPlan {
	if !includeAgents {
		return privilegedPlan{Intents: []privilegedIntent{}}
	}
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	intents := make([]privilegedIntent, 0, len(ids)*2)
	for _, id := range ids {
		definition := agents[id]
		intents = append(intents, privilegedIntent{
			Kind:  intentEnsureAgentPaths,
			Agent: id,
			Cwd:   definition.Cwd,
			Home:  definition.Home,
		})
		if len(definition.Env) == 0 {
			continue
		}
		keys := make([]string, 0, len(definition.Env))
		for key := range definition.Env {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		intents = append(intents, privilegedIntent{
			Kind:    intentProvisionDeclaredEnv,
			Agent:   id,
			EnvKeys: keys,
		})
	}
	return privilegedPlan{Intents: intents}
}

func evaluatePlan(plan privilegedPlan) (coordinateResult, error) {
	result := coordinateResult{
		HostMutation: hostMutationNone,
		Applied:      []string{},
		Unsupported:  []unsupportedChange{},
	}
	for _, intent := range plan.Intents {
		switch intent.Kind {
		case intentEnsureAgentPaths:
			result.Unsupported = append(result.Unsupported, unsupportedChange{
				Kind:   intent.Kind,
				Agent:  intent.Agent,
				Reason: "agent schema has no OS user; Genesis does not create, chown, or mkdir cwd/home as root",
			})
		case intentProvisionDeclaredEnv:
			result.Unsupported = append(result.Unsupported, unsupportedChange{
				Kind:   intent.Kind,
				Agent:  intent.Agent,
				Reason: "env is a process map, not a package graph; Genesis does not install runtimes or packages",
			})
		default:
			return coordinateResult{}, fmt.Errorf("unknown privileged intent kind %q", intent.Kind)
		}
	}
	return result, nil
}
