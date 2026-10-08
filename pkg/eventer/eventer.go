package eventer

/*
#cgo CFLAGS: -I${SRCDIR}/../../submodules/eventer/include
#cgo LDFLAGS: ${SRCDIR}/../../submodules/eventer/target/release/libeventer.a -lpthread -ldl -lm
#include "eventer.h"
#include <stdlib.h>
*/
import "C"

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"
)

// DefaultCloudEventsSchema is the embedded default Eventer schema for Genesis CloudEvents.
//
//go:embed default_schema.json
var DefaultCloudEventsSchema []byte

// Store wraps an open Eventer database instance.
type Store struct {
	mu     sync.Mutex
	handle *C.EventerStore
}

// Open opens or creates an Eventer data directory using the specified JSON schema.
func Open(dir, schemaPath string) (*Store, error) {
	cDir := C.CString(dir)
	cSchema := C.CString(schemaPath)
	defer C.free(unsafe.Pointer(cDir))
	defer C.free(unsafe.Pointer(cSchema))

	handle := C.eventer_open(cDir, cSchema)
	if handle == nil {
		return nil, fmt.Errorf("failed to open eventer store at %s with schema %s", dir, schemaPath)
	}
	return &Store{handle: handle}, nil
}

// OpenDefault opens or creates an Eventer data directory using the embedded Genesis CloudEvents schema.
// If schema.json does not exist in dir, it is written automatically.
func OpenDefault(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create eventer dir: %w", err)
	}
	schemaFile := filepath.Join(dir, "schema.json")
	if _, err := os.Stat(schemaFile); os.IsNotExist(err) {
		if err := os.WriteFile(schemaFile, DefaultCloudEventsSchema, 0o644); err != nil {
			return nil, fmt.Errorf("write default schema: %w", err)
		}
	}
	return Open(dir, schemaFile)
}

// Append writes one raw JSON event into the ingest pipeline.
func (s *Store) Append(eventJSON []byte) error {
	if len(eventJSON) == 0 {
		return errors.New("cannot append empty event")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == nil {
		return errors.New("eventer store is closed")
	}

	rc := C.eventer_append(
		s.handle,
		(*C.uint8_t)(unsafe.Pointer(&eventJSON[0])),
		C.size_t(len(eventJSON)),
	)
	if rc != 0 {
		return s.lastErrorLocked(fmt.Sprintf("append failed with code %d", rc))
	}
	return nil
}

// AppendEvent serializes any Go value to JSON and writes it into the ingest pipeline.
func (s *Store) AppendEvent(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return s.Append(payload)
}

// Flush encodes, compresses with zstd, and fsyncs queued events to segment files.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == nil {
		return errors.New("eventer store is closed")
	}

	rc := C.eventer_flush(s.handle)
	if rc != 0 {
		return s.lastErrorLocked(fmt.Sprintf("flush failed with code %d", rc))
	}
	return nil
}

// Query returns all events within [fromMs, toMs] inclusive as a JSON array ([]byte).
func (s *Store) Query(fromMs, toMs int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == nil {
		return nil, errors.New("eventer store is closed")
	}

	var reqLen C.size_t
	// Probe required size
	rc := C.eventer_query(s.handle, C.int64_t(fromMs), C.int64_t(toMs), nil, 0, &reqLen)
	if rc == 0 {
		return []byte("[]"), nil
	}
	if rc != -4 { // -4 means buffer too small, reqLen holds required capacity
		return nil, s.lastErrorLocked(fmt.Sprintf("query probe failed with code %d", rc))
	}
	if reqLen == 0 {
		return []byte("[]"), nil
	}

	buf := make([]byte, reqLen)
	rc = C.eventer_query(
		s.handle,
		C.int64_t(fromMs),
		C.int64_t(toMs),
		(*C.uint8_t)(unsafe.Pointer(&buf[0])),
		reqLen,
		&reqLen,
	)
	if rc != 0 {
		return nil, s.lastErrorLocked(fmt.Sprintf("query failed with code %d", rc))
	}
	return buf[:reqLen], nil
}

// QueryFiltered returns events within [fromMs, toMs] inclusive whose string/text column filterCol equals filterVal.
func (s *Store) QueryFiltered(fromMs, toMs int64, filterCol, filterVal string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == nil {
		return nil, errors.New("eventer store is closed")
	}

	cCol := C.CString(filterCol)
	cVal := C.CString(filterVal)
	defer C.free(unsafe.Pointer(cCol))
	defer C.free(unsafe.Pointer(cVal))

	var reqLen C.size_t
	rc := C.eventer_query_filtered(s.handle, C.int64_t(fromMs), C.int64_t(toMs), cCol, cVal, nil, 0, &reqLen)
	if rc == 0 {
		return []byte("[]"), nil
	}
	if rc != -4 {
		return nil, s.lastErrorLocked(fmt.Sprintf("query_filtered probe failed with code %d", rc))
	}
	if reqLen == 0 {
		return []byte("[]"), nil
	}

	buf := make([]byte, reqLen)
	rc = C.eventer_query_filtered(
		s.handle,
		C.int64_t(fromMs),
		C.int64_t(toMs),
		cCol,
		cVal,
		(*C.uint8_t)(unsafe.Pointer(&buf[0])),
		reqLen,
		&reqLen,
	)
	if rc != 0 {
		return nil, s.lastErrorLocked(fmt.Sprintf("query_filtered failed with code %d", rc))
	}
	return buf[:reqLen], nil
}

// QueryEvents returns matching events deserialized into individual raw JSON messages.
func (s *Store) QueryEvents(fromMs, toMs int64) ([]json.RawMessage, error) {
	raw, err := s.Query(fromMs, toMs)
	if err != nil {
		return nil, err
	}
	var events []json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, fmt.Errorf("unmarshal events query: %w", err)
	}
	return events, nil
}

// QueryEventsFiltered returns matching events filtered by filterCol==filterVal deserialized into individual raw JSON messages.
func (s *Store) QueryEventsFiltered(fromMs, toMs int64, filterCol, filterVal string) ([]json.RawMessage, error) {
	raw, err := s.QueryFiltered(fromMs, toMs, filterCol, filterVal)
	if err != nil {
		return nil, err
	}
	var events []json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, fmt.Errorf("unmarshal filtered events query: %w", err)
	}
	return events, nil
}

// QueryRun returns all events for a given runID within [fromMs, toMs].
func (s *Store) QueryRun(runID string, fromMs, toMs int64) ([]json.RawMessage, error) {
	return s.QueryEventsFiltered(fromMs, toMs, "runid", runID)
}

// QueryType returns all events of a given CloudEvent type within [fromMs, toMs].
func (s *Store) QueryType(eventType string, fromMs, toMs int64) ([]json.RawMessage, error) {
	return s.QueryEventsFiltered(fromMs, toMs, "type", eventType)
}

// QueryAgent returns all events for a given agentID within [fromMs, toMs].
func (s *Store) QueryAgent(agentID string, fromMs, toMs int64) ([]json.RawMessage, error) {
	return s.QueryEventsFiltered(fromMs, toMs, "agentid", agentID)
}

// NewRunFollower creates a follower dedicated to following events of a specific runID.
func (s *Store) NewRunFollower(startMs int64, runID string) *Follower {
	return s.NewFilteredFollower(startMs, "runid", runID)
}

// NewTypeFollower creates a follower dedicated to following events of a specific CloudEvent type.
func (s *Store) NewTypeFollower(startMs int64, eventType string) *Follower {
	return s.NewFilteredFollower(startMs, "type", eventType)
}

// Follower allows continuous following (tailing) of newly committed events
// from a specific timestamp forward, with optional column equality filtering.
type Follower struct {
	store     *Store
	cursorMs  int64
	filterCol string
	filterVal string
}

// NewFollower creates a follower starting at the given timestamp (in unix milliseconds).
func (s *Store) NewFollower(startMs int64) *Follower {
	return &Follower{store: s, cursorMs: startMs}
}

// NewFilteredFollower creates a follower starting at startMs that filters by filterCol == filterVal.
func (s *Store) NewFilteredFollower(startMs int64, filterCol, filterVal string) *Follower {
	return &Follower{store: s, cursorMs: startMs, filterCol: filterCol, filterVal: filterVal}
}

// WithFilter sets a column equality filter on the follower and returns itself.
func (f *Follower) WithFilter(filterCol, filterVal string) *Follower {
	f.filterCol = filterCol
	f.filterVal = filterVal
	return f
}

// Next polls for any new events committed since the last cursor position.
// The toMs parameter caps the upper time bound (e.g. time.Now().UnixMilli()).
// The follower advances its internal cursor to toMs + 1 when events are found.
func (f *Follower) Next(toMs int64) ([]json.RawMessage, error) {
	if f.cursorMs > toMs {
		return nil, nil
	}
	var events []json.RawMessage
	var err error
	if f.filterCol != "" {
		events, err = f.store.QueryEventsFiltered(f.cursorMs, toMs, f.filterCol, f.filterVal)
	} else {
		events, err = f.store.QueryEvents(f.cursorMs, toMs)
	}
	if err != nil {
		return nil, err
	}
	if len(events) > 0 {
		f.cursorMs = toMs + 1
	}
	return events, nil
}

// Cursor returns the current millisecond timestamp cursor of the follower.
func (f *Follower) Cursor() int64 {
	return f.cursorMs
}

// Close flushes all queued events and frees the underlying store handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == nil {
		return nil
	}
	C.eventer_close(s.handle)
	s.handle = nil
	return nil
}

func (s *Store) lastErrorLocked(fallback string) error {
	if s.handle == nil {
		return errors.New(fallback)
	}
	cErr := C.eventer_last_error(s.handle)
	if cErr != nil {
		msg := C.GoString(cErr)
		if msg != "" {
			return errors.New(msg)
		}
	}
	return errors.New(fallback)
}
