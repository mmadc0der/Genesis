package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	privilegedFDEnv = "GENESIS_PRIVILEGED_FD"
	maxIPCBytes     = 1 << 20
	ipcOpCoordinate = "coordinate"
)

type privilegedCoordinator interface {
	Coordinate(ctx context.Context, plan privilegedPlan) (coordinateResult, error)
}

type ipcEnvelope struct {
	ID      string          `json:"id"`
	Op      string          `json:"op"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type ipcCoordinator struct {
	conn net.Conn
	mu   sync.Mutex
}

func privilegedSocketpair() (parent net.Conn, child *os.File, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	parentFile := os.NewFile(uintptr(fds[0]), "genesis-privileged-parent")
	childFile := os.NewFile(uintptr(fds[1]), "genesis-privileged-child")
	conn, err := net.FileConn(parentFile)
	parentFile.Close()
	if err != nil {
		childFile.Close()
		return nil, nil, err
	}
	return conn, childFile, nil
}

func inheritedCoordinator() (privilegedCoordinator, func(), error) {
	raw := os.Getenv(privilegedFDEnv)
	if raw == "" {
		return nil, func() {}, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 0 {
		return nil, nil, fmt.Errorf("invalid %s", privilegedFDEnv)
	}
	if err := os.Unsetenv(privilegedFDEnv); err != nil {
		return nil, nil, err
	}
	syscall.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), "genesis-privileged")
	conn, err := net.FileConn(file)
	file.Close()
	if err != nil {
		return nil, nil, err
	}
	return &ipcCoordinator{conn: conn}, func() { _ = conn.Close() }, nil
}

func (c *ipcCoordinator) Coordinate(ctx context.Context, plan privilegedPlan) (coordinateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	payload, err := json.Marshal(plan)
	if err != nil {
		return coordinateResult{}, err
	}
	id, err := newIPCRequestID()
	if err != nil {
		return coordinateResult{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.conn.SetDeadline(deadline); err != nil {
			return coordinateResult{}, err
		}
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}
	if err := writeIPCMessage(c.conn, ipcEnvelope{ID: id, Op: ipcOpCoordinate, Payload: payload}); err != nil {
		return coordinateResult{}, err
	}
	var reply ipcEnvelope
	if err := readIPCMessage(c.conn, &reply); err != nil {
		return coordinateResult{}, err
	}
	if reply.ID != id {
		return coordinateResult{}, fmt.Errorf("privileged ipc id mismatch")
	}
	if reply.Error != "" {
		return coordinateResult{}, errors.New(reply.Error)
	}
	var result coordinateResult
	if err := decodeExactJSON(reply.Payload, &result); err != nil {
		return coordinateResult{}, fmt.Errorf("privileged ipc response: %w", err)
	}
	return result, nil
}

func servePrivilegedParent(conn net.Conn, logger *slog.Logger) {
	defer conn.Close()
	for {
		var request ipcEnvelope
		if err := readIPCMessage(conn, &request); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				logger.Error("privileged ipc read", "error", err)
			}
			return
		}
		reply := ipcEnvelope{ID: request.ID, Op: request.Op}
		switch request.Op {
		case ipcOpCoordinate:
			var plan privilegedPlan
			if err := decodeExactJSON(request.Payload, &plan); err != nil {
				reply.Error = err.Error()
			} else {
				result, err := evaluatePlan(plan)
				if err != nil {
					reply.Error = err.Error()
				} else {
					payload, err := json.Marshal(result)
					if err != nil {
						reply.Error = err.Error()
					} else {
						reply.Payload = payload
					}
				}
			}
		default:
			reply.Error = fmt.Sprintf("unknown privileged op %q", request.Op)
		}
		if err := writeIPCMessage(conn, reply); err != nil {
			logger.Error("privileged ipc write", "error", err)
			return
		}
	}
}

func writeIPCMessage(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) > maxIPCBytes {
		return fmt.Errorf("privileged ipc message too large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readIPCMessage(r io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxIPCBytes {
		return fmt.Errorf("privileged ipc message too large")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	return decodeExactJSON(payload, value)
}

func decodeExactJSON(payload []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("privileged ipc payload must contain one JSON value")
		}
		return err
	}
	return nil
}

func newIPCRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "ipc_" + hex.EncodeToString(random[:]), nil
}
