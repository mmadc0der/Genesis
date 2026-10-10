package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAccount(t *testing.T, dataDir, runID string, account tokenAccount) {
	t.Helper()
	runDir := filepath.Join(dataDir, runsDirName, runID)
	if err := os.MkdirAll(runDir, dataDirMode); err != nil {
		t.Fatal(err)
	}
	if err := writeUsageRecord(runDir, account, map[string]struct{}{"a": {}}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageRollupReplaceNotAdd(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roll := newUsageRollup()
	writeAccount(t, dataDir, "gen_a", tokenAccount{CacheHit: 10, CacheMiss: 4, Output: 6, Reasoning: 2, Total: 20, Attempts: 1})
	if _, changed := roll.rescanFromDir(dataDir); !changed {
		t.Fatal("first scan should load the run")
	}
	if roll.global.Total != 20 || roll.global.CacheHit != 10 {
		t.Fatalf("global = %+v", roll.global)
	}

	writeAccount(t, dataDir, "gen_a", tokenAccount{CacheHit: 12, CacheMiss: 5, Output: 8, Reasoning: 3, Total: 25, Attempts: 2})
	touched, changed := roll.rescanFromDir(dataDir)
	if !changed {
		t.Fatal("updated usage.json should change the total")
	}
	if roll.global.Total != 25 || roll.global.CacheHit != 12 || roll.global.CacheMiss != 5 || roll.global.Output != 8 {
		t.Fatalf("global = %+v, want the latest file account", roll.global)
	}
	if roll.global.Reasoning != 3 {
		t.Fatalf("reasoning = %d, want 3 beside the total", roll.global.Reasoning)
	}
	if roll.global.Total != roll.global.CacheHit+roll.global.CacheMiss+roll.global.Output {
		t.Fatalf("total %d must stay cache_hit+cache_miss+output", roll.global.Total)
	}
	if touched["gen_a"].Total != 25 {
		t.Fatalf("touched = %+v", touched)
	}
}

func TestUsageRollupSumsRuns(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roll := newUsageRollup()
	writeAccount(t, dataDir, "gen_a", tokenAccount{CacheHit: 4, CacheMiss: 1, Output: 1, Reasoning: 1, Total: 6, Attempts: 1})
	writeAccount(t, dataDir, "gen_b", tokenAccount{CacheHit: 2, CacheMiss: 2, Output: 2, Total: 6, Attempts: 1})
	if _, changed := roll.rescanFromDir(dataDir); !changed {
		t.Fatal("scan should load runs")
	}
	if roll.global.Total != 12 {
		t.Fatalf("total = %d, want 12", roll.global.Total)
	}
	if roll.runs["gen_a"].Total != 6 || roll.runs["gen_b"].Total != 6 {
		t.Fatalf("runs = %+v", roll.runs)
	}
}

func TestUsageRollupSkipsZeroAccount(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roll := newUsageRollup()
	writeAccount(t, dataDir, "gen_a", tokenAccount{CacheHit: 8, CacheMiss: 1, Output: 1, Reasoning: 4, Total: 10, Attempts: 1})
	writeAccount(t, dataDir, "gen_zero", tokenAccount{})
	if _, changed := roll.rescanFromDir(dataDir); !changed {
		t.Fatal("scan should load the non-zero run")
	}
	if roll.global.Total != 10 {
		t.Fatalf("global = %+v", roll.global)
	}
	if _, ok := roll.runs["gen_zero"]; ok {
		t.Fatal("zero account should not create a run row")
	}
}

func TestUsageRollupRefreshRunSameGlobal(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roll := newUsageRollup()
	writeAccount(t, dataDir, "gen_a", tokenAccount{CacheHit: 5, Total: 5, Attempts: 1})
	roll.rescanFromDir(dataDir)
	writeAccount(t, dataDir, "gen_a", tokenAccount{CacheMiss: 5, Total: 5, Attempts: 1})
	touched, changed := roll.refreshRun(dataDir, "gen_a")
	if !changed {
		t.Fatal("refresh should report a row change even when global total is unchanged")
	}
	if touched["gen_a"].Total != 5 || touched["gen_a"].CacheMiss != 5 || touched["gen_a"].CacheHit != 0 {
		t.Fatalf("touched = %+v", touched["gen_a"])
	}
	if roll.global.Total != 5 {
		t.Fatalf("global = %+v", roll.global)
	}
}

func TestUsageRollupRefreshRun(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roll := newUsageRollup()
	writeAccount(t, dataDir, "gen_a", tokenAccount{CacheHit: 3, Output: 2, Total: 5, Attempts: 1})
	roll.rescanFromDir(dataDir)
	writeAccount(t, dataDir, "gen_a", tokenAccount{CacheHit: 9, Output: 2, Total: 11, Attempts: 2})
	touched, changed := roll.refreshRun(dataDir, "gen_a")
	if !changed || touched["gen_a"].Total != 11 || roll.global.Total != 11 {
		t.Fatalf("refresh = changed %v touched=%+v global=%+v", changed, touched, roll.global)
	}
}

func TestControlUsageScanFromFiles(t *testing.T) {
	dataDir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control := &controlServer{dataDir: dataDir}
	writeAccount(t, dataDir, "gen_a", tokenAccount{CacheHit: 5, CacheMiss: 1, Output: 2, Reasoning: 2, Total: 8, Attempts: 1})
	control.scanUsageFiles(true)
	control.usageMu.Lock()
	got := control.usage.global.Total
	control.usageMu.Unlock()
	if got != 8 {
		t.Fatalf("control rollup total = %d, want 8", got)
	}
}
