package main

import (
	"fmt"
	"slices"
)

const (
	intentEnsureAgentPaths     = "ensure_agent_paths"
	intentEnsureAgentUser      = "ensure_agent_user"
	intentProvisionDeclaredEnv = "provision_declared_env"
	hostMutationNone           = "none"
	hostMutationApplied        = "applied"
	agentShell                 = "/bin/bash"
	workspacePrivate           = "private"
	workspaceSharedRead        = "shared-read"
	workspaceSharedWrite       = "shared-write"
)

type privilegedIntent struct {
	Kind        string            `json:"kind"`
	Agent       string            `json:"agent,omitempty"`
	User        string            `json:"user,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Home        string            `json:"home,omitempty"`
	Shell       string            `json:"shell,omitempty"`
	Groups      []string          `json:"groups,omitempty"`
	Workspace   string            `json:"workspace,omitempty"`
	EnvKeys     []string          `json:"env_keys,omitempty"`
	Repository  string            `json:"repository,omitempty"`
	Identity    string            `json:"identity,omitempty"`
	Git         string            `json:"git,omitempty"`
	Permissions map[string]string `json:"permissions,omitempty"`
	Credential  string            `json:"credential,omitempty"`
}

type privilegedPlan struct {
	Intents []privilegedIntent `json:"intents"`
	Agents  bool               `json:"agents,omitempty"`
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
	Retained     []string            `json:"retained,omitempty"`
}

func buildPlan(agents map[string]agentDefinition, includeAgents bool) privilegedPlan {
	if !includeAgents {
		return privilegedPlan{Intents: []privilegedIntent{}, Agents: false}
	}
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	intents := make([]privilegedIntent, 0, len(ids)*2)
	for _, id := range ids {
		definition := agents[id]
		if definition.User != "" {
			setup := definition.setupContract()
			intents = append(intents, privilegedIntent{
				Kind:      intentEnsureAgentUser,
				Agent:     id,
				User:      definition.User,
				Cwd:       definition.Cwd,
				Home:      definition.Home,
				Shell:     agentShell,
				Groups:    append([]string(nil), setup.Groups...),
				Workspace: setup.workspaceMode(),
			})
		} else {
			intents = append(intents, privilegedIntent{
				Kind:  intentEnsureAgentPaths,
				Agent: id,
				Cwd:   definition.Cwd,
				Home:  definition.Home,
			})
		}
		if len(definition.Env) > 0 {
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
		intents = appendGrantIntents(intents, id, definition)
	}
	return privilegedPlan{Intents: intents, Agents: true}
}

func evaluatePlan(plan privilegedPlan) (coordinateResult, error) {
	result := coordinateResult{
		HostMutation: hostMutationNone,
		Applied:      []string{},
		Unsupported:  []unsupportedChange{},
	}
	for _, intent := range plan.Intents {
		switch intent.Kind {
		case intentEnsureAgentUser:
			return coordinateResult{}, fmt.Errorf("dedicated OS user %q for agent %q requires root genesis launch to reconcile the account", intent.User, intent.Agent)
		case intentEnsureAgentPaths:
			result.Unsupported = append(result.Unsupported, unsupportedChange{
				Kind:   intent.Kind,
				Agent:  intent.Agent,
				Reason: "agent has no OS user; Genesis does not create, chown, or mkdir cwd/home as root",
			})
		case intentProvisionDeclaredEnv:
			result.Unsupported = append(result.Unsupported, unsupportedChange{
				Kind:   intent.Kind,
				Agent:  intent.Agent,
				Reason: "env is a process map, not a package graph; Genesis does not install runtimes or packages",
			})
		default:
			if change, ok := classifyGrantIntent(intent); ok {
				result.Unsupported = append(result.Unsupported, change)
				continue
			}
			return coordinateResult{}, fmt.Errorf("unknown privileged intent kind %q", intent.Kind)
		}
	}
	return result, nil
}

func executePlan(plan privilegedPlan, state *privilegedState) (coordinateResult, error) {
	if state != nil && state.canMutate() {
		return state.apply(plan)
	}
	return evaluatePlan(plan)
}
