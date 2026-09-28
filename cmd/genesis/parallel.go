package main

import "sync"

// parallelWait is one accepted run that has not started because its own
// agent is already at max_parallel. The queue is per agent id. A run of
// another agent is never placed here and never waits on this agent's slot.
type parallelWait struct {
	limit int
	run   func()
}

// parallelGate counts executing runs per agent. Each agent has its own
// queue. The default limit is 1, so a second run of the same agent waits
// until one of that agent's runs returns. A different agent starts
// immediately whenever it is under its own limit.
type parallelGate struct {
	mu      sync.Mutex
	active  map[string]int
	waiting map[string][]parallelWait
}

func (a agentDefinition) parallelLimit() int {
	if a.MaxParallel == nil || *a.MaxParallel < 1 {
		return 1
	}
	return *a.MaxParallel
}

func (s *eventServer) startAgent(document invocation, limit int, secretNames []string) {
	if err := writeQueuedRun(document.RunDir, document, secretNames); err != nil {
		s.log().Error("persist queued run", "run_id", document.RunID, "error", err)
	}
	s.slots.start(document.Agent, limit, func() {
		_ = removeQueuedRun(document.RunDir)
		defer s.store.release(document.RunID)
		s.runner.Run(document)
	})
}

func (g *parallelGate) start(agent string, limit int, run func()) {
	if run == nil {
		return
	}
	g.mu.Lock()
	if g.active == nil {
		g.active = map[string]int{}
	}
	if limit < 1 {
		limit = 1
	}
	if g.active[agent] < limit {
		g.active[agent]++
		g.mu.Unlock()
		go g.tracked(agent, run)
		return
	}
	if g.waiting == nil {
		g.waiting = map[string][]parallelWait{}
	}
	g.waiting[agent] = append(g.waiting[agent], parallelWait{limit: limit, run: run})
	g.mu.Unlock()
}

func (g *parallelGate) tracked(agent string, run func()) {
	defer g.release(agent)
	run()
}

func (g *parallelGate) release(agent string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[agent] > 0 {
		g.active[agent]--
	}
	waiters := g.waiting[agent]
	if len(waiters) == 0 {
		if g.active[agent] == 0 {
			delete(g.active, agent)
		}
		return
	}
	next := waiters[0]
	if next.limit > 0 && g.active[agent] >= next.limit {
		return
	}
	g.waiting[agent] = waiters[1:]
	if len(g.waiting[agent]) == 0 {
		delete(g.waiting, agent)
	}
	g.active[agent]++
	go g.tracked(agent, next.run)
}
