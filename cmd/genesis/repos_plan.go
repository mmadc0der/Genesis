package main

import "fmt"

const (
	remoteMutationNone         = "none"
	repositoryNotAppliedReason = "repository apply is not implemented; Genesis did not call the provider or mutate the remote"

	intentEnsureRepository = "ensure_repository"
	intentEnsureActions    = "ensure_actions"
	intentEnsureBootstrap  = "ensure_bootstrap"
	intentEnsureSecrets    = "ensure_secrets"
	intentEnsureProtection = "ensure_protection"
	intentEnsureIdentities = "ensure_identities"
	intentRetainOnRemove   = "retain_on_remove"
)

type repositoryIntent struct {
	Kind       string                `json:"kind"`
	ID         string                `json:"id"`
	Provider   string                `json:"provider"`
	Org        string                `json:"org"`
	Name       string                `json:"name"`
	Existing   string                `json:"existing,omitempty"`
	Remove     string                `json:"remove,omitempty"`
	Settings   *repositorySettings   `json:"settings,omitempty"`
	Bootstrap  *repositoryBootstrap  `json:"bootstrap,omitempty"`
	Actions    *repositoryActions    `json:"actions,omitempty"`
	Secrets    *repositorySecrets    `json:"secrets,omitempty"`
	Protection *repositoryProtection `json:"protection,omitempty"`
	Identities []repositoryIdentity  `json:"identities,omitempty"`
}

type repositoryUnsupported struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type repositoryPlan struct {
	Requested      bool                    `json:"requested"`
	Active         bool                    `json:"active"`
	RemoteMutation string                  `json:"remote_mutation"`
	Observation    string                  `json:"observation,omitempty"`
	Applied        []string                `json:"applied"`
	Intents        []repositoryIntent      `json:"intents"`
	Observed       []repositoryObserved    `json:"observed,omitempty"`
	Drift          []repositoryDrift       `json:"drift,omitempty"`
	Unsupported    []repositoryUnsupported `json:"unsupported"`
}

func buildRepositoryPlan(repos map[string]repositoryDefinition, reposActive, requested bool) (repositoryPlan, error) {
	plan := repositoryPlan{
		Requested:      requested,
		RemoteMutation: remoteMutationNone,
		Applied:        []string{},
		Intents:        []repositoryIntent{},
		Unsupported:    []repositoryUnsupported{},
	}
	if !requested {
		return plan, repositoryPlanIsNotApplied(plan)
	}
	plan.Active = reposActive
	if !reposActive {
		return plan, repositoryPlanIsNotApplied(plan)
	}
	for _, definition := range canonicalRepositories(repos) {
		plan.Intents = append(plan.Intents, repositoryIntents(definition)...)
	}
	for _, intent := range plan.Intents {
		if !knownRepositoryIntent(intent.Kind) {
			return repositoryPlan{}, fmt.Errorf("unknown repository intent kind %q", intent.Kind)
		}
		plan.Unsupported = append(plan.Unsupported, repositoryUnsupported{
			Kind:   intent.Kind,
			ID:     intent.ID,
			Reason: repositoryNotAppliedReason,
		})
	}
	return plan, repositoryPlanIsNotApplied(plan)
}

func repositoryIntents(definition repositoryDefinition) []repositoryIntent {
	base := repositoryIntent{
		ID:       definition.ID,
		Provider: definition.Provider,
		Org:      definition.Org,
		Name:     definition.Name,
	}
	settings := definition.Settings.clone()
	ensure := base
	ensure.Kind = intentEnsureRepository
	ensure.Existing = definition.Lifecycle.Existing
	ensure.Settings = &settings

	actions := definition.Actions.clone()
	ensureActions := base
	ensureActions.Kind = intentEnsureActions
	ensureActions.Actions = &actions

	intents := []repositoryIntent{ensure, ensureActions}
	if definition.Bootstrap != nil {
		bootstrap := base
		bootstrap.Kind = intentEnsureBootstrap
		bootstrap.Bootstrap = cloneBootstrap(definition.Bootstrap)
		intents = append(intents, bootstrap)
	}
	if definition.Secrets != nil {
		secrets := base
		secrets.Kind = intentEnsureSecrets
		secrets.Secrets = cloneSecrets(definition.Secrets)
		intents = append(intents, secrets)
	}
	if definition.Protection != nil {
		protection := base
		protection.Kind = intentEnsureProtection
		protection.Protection = cloneProtection(definition.Protection)
		intents = append(intents, protection)
	}
	if len(definition.Identities) > 0 {
		identities := base
		identities.Kind = intentEnsureIdentities
		identities.Identities = append([]repositoryIdentity(nil), definition.Identities...)
		intents = append(intents, identities)
	}
	retain := base
	retain.Kind = intentRetainOnRemove
	retain.Remove = definition.Lifecycle.Remove
	intents = append(intents, retain)
	return intents
}

func knownRepositoryIntent(kind string) bool {
	switch kind {
	case intentEnsureRepository, intentEnsureActions, intentEnsureBootstrap, intentEnsureSecrets, intentEnsureProtection, intentEnsureIdentities, intentRetainOnRemove:
		return true
	default:
		return false
	}
}

func repositoryPlanIsReadOnly(plan repositoryPlan) error {
	if plan.RemoteMutation != remoteMutationNone {
		return fmt.Errorf("repository plan remote_mutation = %q", plan.RemoteMutation)
	}
	if len(plan.Applied) != 0 {
		return fmt.Errorf("repository plan applied %d intents", len(plan.Applied))
	}
	if plan.Observation != observationObserved {
		return repositoryPlanIsNotApplied(plan)
	}
	if plan.Observed == nil || plan.Drift == nil || plan.Unsupported == nil {
		return fmt.Errorf("observed repository plan is incomplete")
	}
	for _, item := range plan.Drift {
		switch item.Status {
		case driftDrift, driftUnobservable, driftUnsupported:
			if item.ID == "" || item.Field == "" {
				return fmt.Errorf("repository drift entry is incomplete")
			}
		default:
			return fmt.Errorf("repository drift status %q", item.Status)
		}
	}
	return nil
}

func repositoryPlanIsNotApplied(plan repositoryPlan) error {
	if plan.RemoteMutation != remoteMutationNone {
		return fmt.Errorf("repository plan remote_mutation = %q", plan.RemoteMutation)
	}
	if len(plan.Applied) != 0 {
		return fmt.Errorf("repository plan applied %d intents", len(plan.Applied))
	}
	if !plan.Requested {
		if plan.Active || len(plan.Intents) != 0 || len(plan.Unsupported) != 0 {
			return fmt.Errorf("unrequested repository plan is not empty")
		}
		return nil
	}
	if len(plan.Intents) != len(plan.Unsupported) {
		return fmt.Errorf("repository plan left %d intents without an unsupported result", len(plan.Intents)-len(plan.Unsupported))
	}
	for i, intent := range plan.Intents {
		unsupported := plan.Unsupported[i]
		if unsupported.Kind != intent.Kind || unsupported.ID != intent.ID || unsupported.Reason != repositoryNotAppliedReason {
			return fmt.Errorf("repository intent %s %s was not marked unsupported", intent.Kind, intent.ID)
		}
	}
	return nil
}
