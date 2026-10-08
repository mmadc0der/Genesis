package eventer

/*
#cgo CFLAGS: -I${SRCDIR}/../../submodules/eventer/include
#cgo LDFLAGS: ${SRCDIR}/../../submodules/eventer/target/release/libeventer.a -lpthread -ldl -lm
#include "eventer.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

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

// Follower allows continuous following (tailing) of newly committed events
// from a specific timestamp forward.
type Follower struct {
	store    *Store
	cursorMs int64
}

// NewFollower creates a follower starting at the given timestamp (in unix milliseconds).
func (s *Store) NewFollower(startMs int64) *Follower {
	return &Follower{store: s, cursorMs: startMs}
}

// Next polls for any new events committed since the last cursor position.
// The toMs parameter caps the upper time bound (e.g. time.Now().UnixMilli()).
// The follower advances its internal cursor to the latest event's timestamp + 1.
func (f *Follower) Next(toMs int64) ([]json.RawMessage, error) {
	if f.cursorMs > toMs {
		return nil, nil
	}
	events, err := f.store.QueryEvents(f.cursorMs, toMs)
	if err != nil {
		return nil, err
	}
	if len(events) > 0 {
		// Advance cursor to toMs + 1
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
