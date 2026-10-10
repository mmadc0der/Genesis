package main

// usageReportData is the per-run row in usage live pushes and matches the
// token fields in usage.json after normalization.
type usageReportData struct {
	RunID     string `json:"run_id"`
	CacheHit  int64  `json:"cache_hit"`
	CacheMiss int64  `json:"cache_miss"`
	Output    int64  `json:"output"`
	Reasoning int64  `json:"reasoning"`
	Total     int64  `json:"total"`
	Attempts  int64  `json:"attempts"`
}

func reportData(runID string, account tokenAccount) usageReportData {
	account = account.normalized()
	return usageReportData{
		RunID:     runID,
		CacheHit:  account.CacheHit,
		CacheMiss: account.CacheMiss,
		Output:    account.Output,
		Reasoning: account.Reasoning,
		Total:     account.Total,
		Attempts:  account.Attempts,
	}
}

// reportable is false for a missing or all-zero account. Attempts alone do
// not count: empty usage frames must not appear in the rollup.
func (account tokenAccount) reportable() bool {
	account = account.normalized()
	return account.CacheHit != 0 || account.CacheMiss != 0 || account.Output != 0 || account.Reasoning != 0
}
