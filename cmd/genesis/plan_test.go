package main

import (
	"strings"
	"testing"
)

func TestBuildAndEvaluatePlanMarksHostEffectsUnsupported(t *testing.T) {
	agents := map[string]agentDefinition{
		"janitor": {
			id:   "janitor",
			Cwd:  "/tmp/work",
			Home: "/tmp/home",
			Env:  map[string]string{"PATH": "/usr/bin", "LANG": "C"},
		},
	}
	plan := buildPlan(agents, true)
	if len(plan.Intents) != 2 {
		t.Fatalf("intents = %#v", plan.Intents)
	}
	if plan.Intents[0].Kind != intentEnsureAgentPaths || plan.Intents[0].Cwd != "/tmp/work" {
		t.Fatalf("path intent = %#v", plan.Intents[0])
	}
	if plan.Intents[1].Kind != intentProvisionDeclaredEnv ||
		strings.Join(plan.Intents[1].EnvKeys, ",") != "LANG,PATH" {
		t.Fatalf("env intent = %#v", plan.Intents[1])
	}

	result, err := evaluatePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.HostMutation != hostMutationNone || len(result.Applied) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Unsupported) != 2 {
		t.Fatalf("unsupported = %#v", result.Unsupported)
	}
	if result.Unsupported[0].Reason == "" || result.Unsupported[1].Reason == "" {
		t.Fatalf("missing reasons: %#v", result.Unsupported)
	}

	empty := buildPlan(agents, false)
	if len(empty.Intents) != 0 {
		t.Fatalf("rules-only plan = %#v", empty)
	}
	emptyResult, err := evaluatePlan(empty)
	if err != nil {
		t.Fatal(err)
	}
	if emptyResult.HostMutation != hostMutationNone || len(emptyResult.Unsupported) != 0 {
		t.Fatalf("empty result = %#v", emptyResult)
	}
}

func TestEvaluatePlanRejectsUnknownIntentKind(t *testing.T) {
	_, err := evaluatePlan(privilegedPlan{Intents: []privilegedIntent{{Kind: "useradd", Agent: "janitor"}}})
	if err == nil || !strings.Contains(err.Error(), "unknown privileged intent kind") {
		t.Fatalf("error = %v", err)
	}
}
