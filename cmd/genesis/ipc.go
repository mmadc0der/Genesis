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
)

const (
	privilegedFDEnv = "GENESIS_PRIVILEGED_FD"
	maxIPCBytes     = 1 << 20
	ipcOpCoordinate = "coordinate"
	ipcOpSpawn      = "spawn"
	ipcOpWait       = "wait"
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

	sendMu     sync.Mutex
	readerOnce sync.Once
	pendingMu  sync.Mutex
	pending    map[string]chan ipcEnvelope
	readErr    error
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
	payload, err := json.Marshal(plan)
	if err != nil {
		return coordinateResult{}, err
	}
	reply, err := c.roundTrip(ctx, ipcOpCoordinate, payload)
	if err != nil {
		return coordinateResult{}, err
	}
	var result coordinateResult
	if err := decodeExactJSON(reply.Payload, &result); err != nil {
		return coordinateResult{}, fmt.Errorf("privileged ipc response: %w", err)
	}
	return result, nil
}

func (c *ipcCoordinator) roundTrip(ctx context.Context, op string, payload json.RawMessage, files ...*os.File) (ipcEnvelope, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id, err := newIPCRequestID()
	if err != nil {
		return ipcEnvelope{}, err
	}
	c.startReader()
	waiter := make(chan ipcEnvelope, 1)
	c.pendingMu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.pendingMu.Unlock()
		return ipcEnvelope{}, err
	}
	if c.pending == nil {
		c.pending = map[string]chan ipcEnvelope{}
	}
	c.pending[id] = waiter
	c.pendingMu.Unlock()

	request := ipcEnvelope{ID: id, Op: op, Payload: payload}
	c.sendMu.Lock()
	if len(files) > 0 {
		unixConn, connErr := unixConnOf(c.conn)
		if connErr != nil {
			err = connErr
		} else {
			err = writeIPCMessageWithFDs(unixConn, request, files...)
		}
	} else {
		err = writeIPCMessage(c.conn, request)
	}
	c.sendMu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return ipcEnvelope{}, err
	}

	select {
	case reply, ok := <-waiter:
		if !ok {
			c.pendingMu.Lock()
			err := c.readErr
			c.pendingMu.Unlock()
			if err == nil {
				err = errors.New("privileged ipc closed")
			}
			return ipcEnvelope{}, err
		}
		if reply.Error != "" {
			return ipcEnvelope{}, errors.New(reply.Error)
		}
		return reply, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return ipcEnvelope{}, ctx.Err()
	}
}

func (c *ipcCoordinator) startReader() {
	c.readerOnce.Do(func() {
		c.pendingMu.Lock()
		if c.pending == nil {
			c.pending = map[string]chan ipcEnvelope{}
		}
		c.pendingMu.Unlock()
		go c.readReplies()
	})
}

func (c *ipcCoordinator) readReplies() {
	for {
		var reply ipcEnvelope
		err := readIPCMessage(c.conn, &reply)
		c.pendingMu.Lock()
		if err != nil {
			c.readErr = err
			for _, waiter := range c.pending {
				close(waiter)
			}
			c.pending = map[string]chan ipcEnvelope{}
			c.pendingMu.Unlock()
			return
		}
		waiter, ok := c.pending[reply.ID]
		if ok {
			delete(c.pending, reply.ID)
		}
		c.pendingMu.Unlock()
		if ok {
			waiter <- reply
		}
	}
}

func servePrivilegedParent(conn net.Conn, logger *slog.Logger, state *privilegedState) {
	defer conn.Close()
	unixConn, err := unixConnOf(conn)
	if err != nil {
		logger.Error("privileged ipc conn", "error", err)
		return
	}
	var writeMu sync.Mutex
	writeReply := func(reply ipcEnvelope) bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := writeIPCMessage(conn, reply); err != nil {
			logger.Error("privileged ipc write", "error", err)
			return false
		}
		return true
	}
	for {
		var request ipcEnvelope
		fds, err := readIPCMessageWithFDs(unixConn, &request)
		if err != nil {
			closeFiles(fds)
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				logger.Error("privileged ipc read", "error", err)
			}
			return
		}
		reply := ipcEnvelope{ID: request.ID, Op: request.Op}
		switch request.Op {
		case ipcOpCoordinate:
			closeFiles(fds)
			var plan privilegedPlan
			if err := decodeExactJSON(request.Payload, &plan); err != nil {
				reply.Error = err.Error()
			} else {
				result, err := executePlan(plan, state)
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
		case ipcOpSpawn:
			payload, err := handleSpawnRequest(state, request.Payload, fds)
			closeFiles(fds)
			if err != nil {
				reply.Error = err.Error()
			} else {
				reply.Payload = payload
			}
		case ipcOpWait:
			closeFiles(fds)
			go func(request ipcEnvelope) {
				reply := ipcEnvelope{ID: request.ID, Op: request.Op}
				var body waitRequest
				if err := decodeExactJSON(request.Payload, &body); err != nil {
					reply.Error = err.Error()
				} else if state == nil {
					reply.Error = "privileged spawn is not configured"
				} else {
					code, err := state.wait(body.PID)
					if err != nil {
						reply.Error = err.Error()
					} else {
						payload, err := json.Marshal(waitReply{ExitCode: code})
						if err != nil {
							reply.Error = err.Error()
						} else {
							reply.Payload = payload
						}
					}
				}
				_ = writeReply(reply)
			}(request)
			continue
		default:
			closeFiles(fds)
			reply.Error = fmt.Sprintf("unknown privileged op %q", request.Op)
		}
		if !writeReply(reply) {
			return
		}
	}
}

func handleSpawnRequest(state *privilegedState, payload json.RawMessage, fds []*os.File) (json.RawMessage, error) {
	if state == nil {
		return nil, errors.New("privileged spawn is not configured")
	}
	if len(fds) != 3 {
		return nil, fmt.Errorf("spawn requires 3 file descriptors, got %d", len(fds))
	}
	var req spawnRequest
	if err := decodeExactJSON(payload, &req); err != nil {
		return nil, err
	}
	pid, err := state.spawn(req, fds[0], fds[1], fds[2])
	if err != nil {
		return nil, err
	}
	return json.Marshal(spawnReply{PID: pid})
}

func unixConnOf(conn net.Conn) (*net.UnixConn, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, errors.New("privileged ipc is not a unix socket")
	}
	return unixConn, nil
}

func writeIPCMessageWithFDs(conn *net.UnixConn, value any, files ...*os.File) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) > maxIPCBytes {
		return fmt.Errorf("privileged ipc message too large")
	}
	fds := make([]int, 0, len(files))
	for _, file := range files {
		if file == nil {
			return errors.New("spawn file descriptor is nil")
		}
		fds = append(fds, int(file.Fd()))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	message := append(header[:], payload...)
	_, _, err = conn.WriteMsgUnix(message, syscall.UnixRights(fds...), nil)
	return err
}

func readIPCMessageWithFDs(conn *net.UnixConn, value any) ([]*os.File, error) {
	oob := make([]byte, syscall.CmsgSpace(3*4))
	var header [4]byte
	n, oobn, _, _, err := conn.ReadMsgUnix(header[:], oob)
	if err != nil {
		return nil, err
	}
	if n < len(header) {
		if _, err := io.ReadFull(conn, header[n:]); err != nil {
			return nil, err
		}
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxIPCBytes {
		return nil, fmt.Errorf("privileged ipc message too large")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}
	files, err := filesFromOOB(oob[:oobn])
	if err != nil {
		return nil, err
	}
	if err := decodeExactJSON(payload, value); err != nil {
		closeFiles(files)
		return nil, err
	}
	return files, nil
}

func filesFromOOB(oob []byte) ([]*os.File, error) {
	if len(oob) == 0 {
		return nil, nil
	}
	messages, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for _, message := range messages {
		fds, err := syscall.ParseUnixRights(&message)
		if err != nil {
			closeFiles(files)
			return nil, err
		}
		for _, fd := range fds {
			files = append(files, os.NewFile(uintptr(fd), "genesis-spawn-fd"))
		}
	}
	return files, nil
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
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
