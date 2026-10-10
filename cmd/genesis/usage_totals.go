package main

import (
	"os"
	"path/filepath"
	"slices"
	"time"
)

const usageScanEvery = 15 * time.Minute

// usageWire is the /api/live op "usage" payload. Global is the sum of the
// latest account per run. Runs is either every run (snapshot) or the runs
// touched by the latest rescan.
type usageWire struct {
	CacheHit  int64                      `json:"cache_hit"`
	CacheMiss int64                      `json:"cache_miss"`
	Output    int64                      `json:"output"`
	Reasoning int64                      `json:"reasoning"`
	Total     int64                      `json:"total"`
	Snapshot  bool                       `json:"snapshot,omitempty"`
	Runs      map[string]usageReportData `json:"runs,omitempty"`
}

// usageRollup keeps the latest usage.json account per run. Rescanning replaces
// each run row and rebuilds global from the map; nothing is added on top of
// file totals.
type usageRollup struct {
	global usageReportData
	runs   map[string]usageReportData
}

func newUsageRollup() *usageRollup {
	return &usageRollup{
		runs: map[string]usageReportData{},
	}
}

func sumUsageRuns(runs map[string]usageReportData) usageReportData {
	var global usageReportData
	for _, row := range runs {
		global.CacheHit += row.CacheHit
		global.CacheMiss += row.CacheMiss
		global.Output += row.Output
		global.Reasoning += row.Reasoning
		global.Total += row.Total
	}
	return global
}

func usageRowFromFile(dataDir, runID string) (usageReportData, bool) {
	if dataDir == "" || runID == "" {
		return usageReportData{}, false
	}
	if err := validateRunID(runID); err != nil {
		return usageReportData{}, false
	}
	account, _, ok := readUsageAccount(filepath.Join(dataDir, runsDirName, runID, usageFileName))
	if !ok || !account.reportable() {
		return usageReportData{}, false
	}
	return reportData(runID, account), true
}

func listRunIDs(dataDir string) []string {
	runs := filepath.Join(dataDir, runsDirName)
	entries, err := os.ReadDir(runs)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names
}

// rescanFromDir reads every usage.json under dataDir/runs. touched lists runs
// whose row changed; globalChanged is true when the summed total moved.
func (r *usageRollup) rescanFromDir(dataDir string) (touched map[string]usageReportData, globalChanged bool) {
	touched = map[string]usageReportData{}
	next := map[string]usageReportData{}
	for _, runID := range listRunIDs(dataDir) {
		row, ok := usageRowFromFile(dataDir, runID)
		if !ok {
			if _, had := r.runs[runID]; had {
				touched[runID] = usageReportData{RunID: runID}
				globalChanged = true
			}
			continue
		}
		next[runID] = row
		old := r.runs[runID]
		if old != row {
			touched[runID] = row
			globalChanged = true
		}
	}
	for runID, old := range r.runs {
		if _, ok := next[runID]; !ok && old.RunID != "" {
			touched[runID] = usageReportData{RunID: runID}
			globalChanged = true
		}
	}
	r.runs = next
	nextGlobal := sumUsageRuns(r.runs)
	if nextGlobal != r.global {
		globalChanged = true
	}
	r.global = nextGlobal
	return touched, globalChanged
}

// refreshRun rereads one run's usage.json and replaces that row.
func (r *usageRollup) refreshRun(dataDir, runID string) (touched map[string]usageReportData, changed bool) {
	touched = map[string]usageReportData{}
	row, ok := usageRowFromFile(dataDir, runID)
	if !ok {
		if _, had := r.runs[runID]; had {
			delete(r.runs, runID)
			touched[runID] = usageReportData{RunID: runID}
			changed = true
		}
	} else {
		old := r.runs[runID]
		if old != row {
			r.runs[runID] = row
			touched[runID] = row
			changed = true
		}
	}
	if changed {
		r.global = sumUsageRuns(r.runs)
	}
	return touched, changed
}

func (c *controlServer) loopUsageReports() {
	c.scanUsageFiles(true)
	interval := usageScanEvery
	if c.usageScanEvery > 0 {
		interval = c.usageScanEvery
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		c.scanUsageFiles(false)
	}
}

func (c *controlServer) scanUsageFiles(snapshot bool) {
	if c == nil || c.dataDir == "" {
		return
	}
	c.usageMu.Lock()
	if c.usage == nil {
		c.usage = newUsageRollup()
	}
	touched, changed := c.usage.rescanFromDir(c.dataDir)
	global := c.usage.global
	runCount := len(c.usage.runs)
	c.usageMu.Unlock()
	if snapshot {
		if c.logger != nil {
			c.logger.Info("usage scan loaded", "total", global.Total, "runs", runCount)
		}
		c.pushUsage(nil, true)
		return
	}
	if changed {
		c.pushUsage(touched, false)
	}
}

func (c *controlServer) noteUsageFile(runID string) {
	if c == nil || c.dataDir == "" || runID == "" {
		return
	}
	c.usageMu.Lock()
	if c.usage == nil {
		c.usage = newUsageRollup()
	}
	touched, changed := c.usage.refreshRun(c.dataDir, runID)
	c.usageMu.Unlock()
	if changed {
		c.pushUsage(touched, false)
	}
}

func (c *controlServer) pushUsage(touched map[string]usageReportData, snapshot bool) {
	c.usageMu.Lock()
	if c.usage == nil {
		c.usageMu.Unlock()
		return
	}
	wire := &usageWire{
		CacheHit:  c.usage.global.CacheHit,
		CacheMiss: c.usage.global.CacheMiss,
		Output:    c.usage.global.Output,
		Reasoning: c.usage.global.Reasoning,
		Total:     c.usage.global.Total,
		Snapshot:  snapshot,
	}
	if snapshot {
		wire.Runs = map[string]usageReportData{}
		for id, row := range c.usage.runs {
			wire.Runs[id] = row
		}
	} else if len(touched) > 0 {
		wire.Runs = touched
	}
	c.usageMu.Unlock()
	frame := liveFrame{Op: "usage", Usage: wire}
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	for subs := range c.liveSet {
		subs.emit(frame)
	}
}

func (c *controlServer) offerUsageSnapshot(subs *liveSubs) {
	c.usageMu.Lock()
	if c.usage == nil {
		c.usageMu.Unlock()
		return
	}
	wire := &usageWire{
		CacheHit:  c.usage.global.CacheHit,
		CacheMiss: c.usage.global.CacheMiss,
		Output:    c.usage.global.Output,
		Reasoning: c.usage.global.Reasoning,
		Total:     c.usage.global.Total,
		Snapshot:  true,
		Runs:      map[string]usageReportData{},
	}
	for id, row := range c.usage.runs {
		wire.Runs[id] = row
	}
	c.usageMu.Unlock()
	subs.emit(liveFrame{Op: "usage", Usage: wire})
}
