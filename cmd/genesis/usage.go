package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

const usageChunkType = "usage"

// tokenAccount is one run's token rollup. cache_hit is the sum of
// cacheReadTokens (prefix tokens served from cache). cache_miss is the sum
// of inputTokens, the uncached portion of the prompt. output is the sum of
// outputTokens. reasoning is the sum of reasoningTokens, a subset of output,
// and is not added into total. total is cache_hit + cache_miss + output.
// attempts is the number of usage frames summed.
type tokenAccount struct {
	CacheHit  int64 `json:"cache_hit"`
	CacheMiss int64 `json:"cache_miss"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Total     int64 `json:"total"`
	Attempts  int64 `json:"attempts"`
}

// usageRecord is the on-disk account. AttemptIDs are the usage frames already
// counted so a repeated attempt is not added again when the file is reloaded.
type usageRecord struct {
	tokenAccount
	AttemptIDs []string `json:"attempt_ids,omitempty"`
}

type usageParts struct {
	attemptID       string
	cacheHit        int64
	cacheMiss       int64
	output          int64
	reasoning       int64
	totalTokensOnly bool
}

func (account tokenAccount) stamp() string {
	return strconv.FormatInt(account.CacheHit, 10) + ":" +
		strconv.FormatInt(account.CacheMiss, 10) + ":" +
		strconv.FormatInt(account.Output, 10) + ":" +
		strconv.FormatInt(account.Reasoning, 10) + ":" +
		strconv.FormatInt(account.Total, 10) + ":" +
		strconv.FormatInt(account.Attempts, 10)
}

func (account tokenAccount) normalized() tokenAccount {
	account.Total = account.CacheHit + account.CacheMiss + account.Output
	return account
}

// noteUsage folds one journaled usage chunk into the run account and writes
// it atomically. The lifecycle event is already durable; a rollup write
// failure does not fail the run.
func (j *runJournal) noteUsage(data json.RawMessage) {
	if j == nil || j.runDir == "" {
		return
	}
	if _, ok := parseUsageFrame(data); !ok {
		return
	}
	j.ensureUsage()
	account, seen := foldUsage(j.usage, j.usageSeen, []lifecycleEvent{{
		Type: lifecycleTypeChunk,
		Data: data,
	}})
	if account == j.usage {
		return
	}
	j.usage = account
	j.usageSeen = seen
	if err := writeUsageRecord(j.runDir, account, seen); err != nil {
		return
	}
}

func (j *runJournal) ensureUsage() {
	if j == nil || j.usageLoaded {
		return
	}
	j.usageLoaded = true
	if j.runDir == "" {
		return
	}
	account, seen, ok := readUsageAccount(filepath.Join(j.runDir, usageFileName))
	if !ok {
		return
	}
	j.usage = account
	j.usageSeen = seen
}

// ensureUsageFile writes the account from events when the run has none.
// An existing file is left as it is, including when the journal also contains
// those usage frames.
func ensureUsageFile(runDir string, events []lifecycleEvent) error {
	path := filepath.Join(runDir, usageFileName)
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	account, seen := foldUsage(tokenAccount{}, nil, events)
	return writeUsageRecord(runDir, account, seen)
}

func usageFromRun(runDir string, events []lifecycleEvent) tokenAccount {
	if account, _, ok := readUsageAccount(filepath.Join(runDir, usageFileName)); ok {
		return account
	}
	account, _ := foldUsage(tokenAccount{}, nil, events)
	return account
}

// foldUsage adds usage chunks. Non-usage events are ignored. A repeated
// attempt id is skipped. Missing counts are zero. A frame's totalTokens is
// not used; total is recomputed from cache hit, cache miss, and output.
func foldUsage(account tokenAccount, seen map[string]struct{}, events []lifecycleEvent) (tokenAccount, map[string]struct{}) {
	nextSeen := make(map[string]struct{}, len(seen))
	for id := range seen {
		nextSeen[id] = struct{}{}
	}
	for _, event := range events {
		if event.Type != lifecycleTypeChunk {
			continue
		}
		frame, ok := parseUsageFrame(event.Data)
		if !ok {
			continue
		}
		if frame.totalTokensOnly {
			continue
		}
		hasCounts := frame.cacheHit != 0 || frame.cacheMiss != 0 || frame.output != 0 || frame.reasoning != 0
		if !hasCounts {
			if frame.attemptID != "" {
				if _, exists := nextSeen[frame.attemptID]; exists {
					continue
				}
				nextSeen[frame.attemptID] = struct{}{}
			}
			account.Attempts++
			continue
		}
		if frame.attemptID != "" {
			if _, exists := nextSeen[frame.attemptID]; exists {
				continue
			}
			nextSeen[frame.attemptID] = struct{}{}
		}
		account.CacheHit += frame.cacheHit
		account.CacheMiss += frame.cacheMiss
		account.Output += frame.output
		account.Reasoning += frame.reasoning
		account.Attempts++
	}
	return account.normalized(), nextSeen
}

func parseUsageFrame(data json.RawMessage) (usageParts, bool) {
	if len(bytes.TrimSpace(data)) == 0 {
		return usageParts{}, false
	}
	var body struct {
		ChunkType string `json:"chunk_type"`
		AttemptID string `json:"attempt_id"`
		Raw       struct {
			Payload struct {
				Chunk struct {
					Usage json.RawMessage `json:"usage"`
				} `json:"chunk"`
			} `json:"payload"`
		} `json:"raw"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return usageParts{}, false
	}
	if body.ChunkType != usageChunkType {
		return usageParts{}, false
	}
	rawUsage := body.Raw.Payload.Chunk.Usage
	hit, miss, output, reasoning := usageInts(rawUsage)
	return usageParts{
		attemptID:       body.AttemptID,
		cacheHit:        hit,
		cacheMiss:       miss,
		output:          output,
		reasoning:       reasoning,
		totalTokensOnly: usageIsTotalTokensOnly(rawUsage),
	}, true
}

// usageIsTotalTokensOnly is true when the usage object cites totalTokens but
// no component fields, so the frame must not lock an attempt id.
func usageIsTotalTokensOnly(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return false
	}
	if fields["totalTokens"] == nil {
		return false
	}
	for key := range fields {
		if key != "totalTokens" {
			return false
		}
	}
	return true
}

func usageInts(raw json.RawMessage) (hit, miss, output, reasoning int64) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, 0, 0, 0
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return 0, 0, 0, 0
	}
	return jsonInt64(fields["cacheReadTokens"]),
		jsonInt64(fields["inputTokens"]),
		jsonInt64(fields["outputTokens"]),
		jsonInt64(fields["reasoningTokens"])
}

func jsonInt64(raw json.RawMessage) int64 {
	trimmed := bytes.TrimSpace(raw)
	// A JSON string is not a count, even when its text is numeric.
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || trimmed[0] == '"' {
		return 0
	}
	var value int64
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return 0
	}
	return value
}

func readUsageAccount(path string) (tokenAccount, map[string]struct{}, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return tokenAccount{}, nil, false
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return tokenAccount{}, nil, false
	}
	var record usageRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return tokenAccount{}, nil, false
	}
	seen := make(map[string]struct{}, len(record.AttemptIDs))
	for _, id := range record.AttemptIDs {
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
	}
	return record.tokenAccount.normalized(), seen, true
}

func writeUsageRecord(runDir string, account tokenAccount, seen map[string]struct{}) error {
	if runDir == "" {
		return errors.New("run directory is required")
	}
	path := filepath.Join(runDir, usageFileName)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	payload, err := json.Marshal(usageRecord{
		tokenAccount: account.normalized(),
		AttemptIDs:   seenIDs(seen),
	})
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temp := path + ".tmp"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, dataFileMode)
	if err != nil {
		return fmt.Errorf("write usage account: %w", err)
	}
	_, writeErr := file.Write(payload)
	if writeErr == nil {
		writeErr = file.Chmod(dataFileMode)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("write usage account: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("write usage account: %w", closeErr)
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("write usage account: %w", err)
	}
	return nil
}

func seenIDs(seen map[string]struct{}) []string {
	if len(seen) == 0 {
		return nil
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
