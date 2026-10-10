package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUsageFileIsRollupSource(t *testing.T) {
	store, _ := newUsageStore(t)
	journal, doc := acceptRun(t, store, "gen_usage_file")
	publishUsage(t, journal, "a1", map[string]any{
		"cacheReadTokens": 5,
		"inputTokens":     1,
		"outputTokens":    2,
		"reasoningTokens": 2,
		"totalTokens":     999,
	})
	assertUsage(t, mustUsage(t, doc.RunDir), tokenAccount{CacheHit: 5, CacheMiss: 1, Output: 2, Reasoning: 2, Total: 8, Attempts: 1})

	publishUsage(t, journal, "a1", map[string]any{
		"cacheReadTokens": 100,
		"inputTokens":     100,
		"outputTokens":    100,
		"reasoningTokens": 100,
	})
	assertUsage(t, mustUsage(t, doc.RunDir), tokenAccount{CacheHit: 5, CacheMiss: 1, Output: 2, Reasoning: 2, Total: 8, Attempts: 1})

	publishUsage(t, journal, "a2", map[string]any{"outputTokens": 4, "reasoningTokens": 1})
	assertUsage(t, mustUsage(t, doc.RunDir), tokenAccount{CacheHit: 5, CacheMiss: 1, Output: 6, Reasoning: 3, Total: 12, Attempts: 2})

	if _, err := os.Lstat(filepath.Join(store.dataDir, "usage-index.json")); !os.IsNotExist(err) {
		t.Fatalf("usage index should not exist: %v", err)
	}
}

func TestUsageFileIgnoresZeroOnlyFrames(t *testing.T) {
	store, _ := newUsageStore(t)
	journal, doc := acceptRun(t, store, "gen_usage_silent")
	publishUsage(t, journal, "z1", map[string]any{"totalTokens": 40})
	publishUsage(t, journal, "z2", map[string]any{"cacheReadTokens": 8, "outputTokens": 8})
	assertUsage(t, mustUsage(t, doc.RunDir), tokenAccount{CacheHit: 8, Output: 8, Total: 16, Attempts: 1})
}
