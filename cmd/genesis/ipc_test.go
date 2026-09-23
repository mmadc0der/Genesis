package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPrivilegedSocketpairCoordinateRoundTrip(t *testing.T) {
	parent, childFile, err := privilegedSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		parent.Close()
		childFile.Close()
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	go servePrivilegedParent(parent, logger, nil)

	conn, err := fileConnForTest(childFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	coordinator := &ipcCoordinator{conn: conn}

	plan := buildPlan(map[string]agentDefinition{
		"janitor": {id: "janitor", Cwd: "/tmp/work", Home: "/tmp/home", Env: map[string]string{"PATH": "/bin"}},
	}, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := coordinator.Coordinate(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.HostMutation != hostMutationNone {
		t.Fatalf("host mutation = %q", result.HostMutation)
	}
	if len(result.Applied) != 0 || len(result.Unsupported) != 2 {
		t.Fatalf("result = %#v", result)
	}

	_, err = coordinator.Coordinate(ctx, privilegedPlan{Intents: []privilegedIntent{{Kind: "useradd"}}})
	if err == nil || !strings.Contains(err.Error(), "unknown privileged intent kind") {
		t.Fatalf("unknown kind error = %v", err)
	}
}

func TestIPCRejectsForgedGrantReadiness(t *testing.T) {
	payload := []byte(`{"intents":[{"kind":"ensure_remote_registration","agent":"worker","repository":"lab","identity":"programmer","git":"write","permissions":{"contents":"write"},"credential":"pending","remote_status":"ready","remote_key_id":"77"}],"agents":true}`)
	var plan privilegedPlan
	if err := decodeExactJSON(payload, &plan); err == nil {
		t.Fatal("forged remote status was accepted")
	}

	parent, childFile, err := privilegedSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		parent.Close()
		childFile.Close()
	})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := openTestCredentialStore(t)
	state := &privilegedState{logger: logger, host: newMemoryHost(), mutate: true, credentials: store}
	go servePrivilegedParent(parent, logger, state)

	conn, err := fileConnForTest(childFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := writeIPCMessage(conn, ipcEnvelope{ID: "ipc_forge", Op: ipcOpCoordinate, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var reply ipcEnvelope
	if err := readIPCMessage(conn, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error == "" || strings.Contains(string(reply.Payload), `"remote_status":"ready"`) {
		t.Fatalf("forged reply = %#v", reply)
	}
	if _, ok := state.readyHeld("worker"); ok {
		t.Fatal("forged ipc became deliverable")
	}
}

func TestIPCRejectsUnknownFields(t *testing.T) {
	var plan privilegedPlan
	err := decodeExactJSON([]byte(`{"intents":[],"shell":"true"}`), &plan)
	if err == nil {
		t.Fatal("expected unknown field rejection")
	}
}

func TestIPCEnvelopeRoundTrip(t *testing.T) {
	parent, childFile, err := privilegedSocketpair()
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	defer childFile.Close()
	conn, err := fileConnForTest(childFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	payload, _ := json.Marshal(privilegedPlan{Intents: []privilegedIntent{}})
	if err := writeIPCMessage(conn, ipcEnvelope{ID: "ipc_test", Op: ipcOpCoordinate, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var received ipcEnvelope
	if err := readIPCMessage(parent, &received); err != nil {
		t.Fatal(err)
	}
	if received.ID != "ipc_test" || received.Op != ipcOpCoordinate {
		t.Fatalf("received = %#v", received)
	}
}
