package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// eventCollector captures rpcEvent values sent via a wsClient's sendEventFn.
type eventCollector struct {
	mu     sync.Mutex
	events []rpcEvent
	notify chan struct{}
}

func newEventCollector() *eventCollector {
	return &eventCollector{notify: make(chan struct{}, 100)}
}

func (ec *eventCollector) sendEvent(_ context.Context, evt rpcEvent) error {
	ec.mu.Lock()
	ec.events = append(ec.events, evt)
	ec.mu.Unlock()
	select {
	case ec.notify <- struct{}{}:
	default:
	}
	return nil
}

func (ec *eventCollector) snapshot() []rpcEvent {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	cp := make([]rpcEvent, len(ec.events))
	copy(cp, ec.events)
	return cp
}

func (ec *eventCollector) waitForN(t *testing.T, n int, timeout time.Duration) []rpcEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		events := ec.snapshot()
		if len(events) >= n {
			return events
		}
		select {
		case <-ec.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d events; got %d", n, len(ec.snapshot()))
			return nil
		}
	}
}

func makeTestClient(ec *eventCollector) *wsClient {
	return &wsClient{
		sendEventFn: ec.sendEvent,
	}
}

// startSequenceSocket creates a mock Unix socket that responds to cmuxCall
// with results from a sequence. After exhausting the sequence it repeats the last.
func startSequenceSocket(t *testing.T, results []string) string {
	t.Helper()
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var idx int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			i := int(atomic.AddInt64(&idx, 1) - 1)
			if i >= len(results) {
				i = len(results) - 1
			}
			buf := make([]byte, 4096)
			n, _ := conn.Read(buf)
			var req map[string]any
			reqID := "test"
			if err := json.Unmarshal(buf[:n], &req); err == nil {
				if id, ok := req["id"].(string); ok {
					reqID = id
				}
			}
			resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":%s}`, reqID, results[i])
			_, _ = conn.Write([]byte(resp + "\n"))
			conn.Close()
		}
	}()
	return sockPath
}

func treeJSON(workspaces string) string {
	return fmt.Sprintf(`{"windows":[{"workspaces":%s}]}`, workspaces)
}

// --- Dedup tests ---

func TestTopologyWatcher_SameTopology_NoBroadcast(t *testing.T) {
	ws := `[{"id":"w1","name":"default","tabs":[]}]`
	sockPath := startSequenceSocket(t, []string{treeJSON(ws)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tw := newTopologyWatcher(ctx, sockPath)

	ec := newEventCollector()
	tw.addClient(makeTestClient(ec))

	// First poll broadcasts the initial topology (lastJSON starts nil).
	ec.waitForN(t, 1, 8*time.Second)

	// Wait for at least one more poll cycle; same topology should be deduped.
	time.Sleep(3 * time.Second)

	if got := len(ec.snapshot()); got != 1 {
		t.Fatalf("same topology should produce exactly 1 event (initial); got %d", got)
	}
}

func TestTopologyWatcher_TopologyChange_Broadcasts(t *testing.T) {
	wsA := `[{"id":"w1","name":"default","tabs":[]}]`
	wsB := `[{"id":"w1","name":"default","tabs":[]},{"id":"w2","name":"work","tabs":[]}]`
	sockPath := startSequenceSocket(t, []string{treeJSON(wsA), treeJSON(wsB)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tw := newTopologyWatcher(ctx, sockPath)

	ec := newEventCollector()
	tw.addClient(makeTestClient(ec))

	events := ec.waitForN(t, 2, 8*time.Second)
	last := events[len(events)-1]
	if last.Event != "topology" {
		t.Fatalf("expected topology event, got %q", last.Event)
	}

	decoded, err := base64.StdEncoding.DecodeString(last.DataBase64)
	if err != nil {
		t.Fatalf("decode data_base64: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if string(payload["workspaces"]) != wsB {
		t.Fatalf("expected workspaces=%s; got %s", wsB, payload["workspaces"])
	}
}

// --- Client management tests ---

func TestTopologyWatcher_AddRemoveClient(t *testing.T) {
	tw := &topologyWatcher{
		clients: make(map[*wsClient]struct{}),
	}

	ec1 := newEventCollector()
	cl1 := makeTestClient(ec1)
	ec2 := newEventCollector()
	cl2 := makeTestClient(ec2)

	tw.addClient(cl1)
	tw.addClient(cl2)
	tw.removeClient(cl1)

	ctx := context.Background()
	tw.broadcast(ctx, rpcEvent{Event: "topology", DataBase64: "dGVzdA=="})

	time.Sleep(50 * time.Millisecond)

	if got := ec1.snapshot(); len(got) != 0 {
		t.Fatalf("removed client should not receive events; got %d", len(got))
	}
	if got := ec2.snapshot(); len(got) != 1 {
		t.Fatalf("active client should receive 1 event; got %d", len(got))
	}
}

func TestTopologyWatcher_ConcurrentAddRemove(t *testing.T) {
	tw := &topologyWatcher{
		clients: make(map[*wsClient]struct{}),
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ec := newEventCollector()
			cl := makeTestClient(ec)
			tw.addClient(cl)
			time.Sleep(time.Millisecond)
			tw.removeClient(cl)
		}()
	}
	wg.Wait()
	// If -race doesn't fire, the test passes.
}

func TestTopologyWatcher_BroadcastToMultiple(t *testing.T) {
	tw := &topologyWatcher{
		clients: make(map[*wsClient]struct{}),
	}

	collectors := make([]*eventCollector, 3)
	for i := range collectors {
		ec := newEventCollector()
		collectors[i] = ec
		tw.addClient(makeTestClient(ec))
	}

	ctx := context.Background()
	tw.broadcast(ctx, rpcEvent{Event: "topology", DataBase64: "dGVzdA=="})

	time.Sleep(50 * time.Millisecond)

	for i, ec := range collectors {
		if got := ec.snapshot(); len(got) != 1 {
			t.Fatalf("client %d: expected 1 event, got %d", i, len(got))
		}
	}
}

// --- Broadcast format tests ---

func TestTopologyWatcher_BroadcastFormat(t *testing.T) {
	ws := `[{"id":"w1","name":"default","tabs":[{"surface_id":"s1"}]}]`
	sockPath := startSequenceSocket(t, []string{treeJSON(ws)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tw := newTopologyWatcher(ctx, sockPath)

	ec := newEventCollector()
	tw.addClient(makeTestClient(ec))

	events := ec.waitForN(t, 1, 8*time.Second)
	evt := events[0]

	if evt.Event != "topology" {
		t.Fatalf("event type = %q, want \"topology\"", evt.Event)
	}
	if evt.DataBase64 == "" {
		t.Fatalf("data_base64 should not be empty")
	}

	decoded, err := base64.StdEncoding.DecodeString(evt.DataBase64)
	if err != nil {
		t.Fatalf("data_base64 is not valid base64: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("decoded payload is not valid JSON: %v", err)
	}
	if _, ok := payload["workspaces"]; !ok {
		t.Fatalf("payload missing \"workspaces\" key: %s", decoded)
	}
}

func TestTopologyWatcher_ExtractsWorkspacesFromTree(t *testing.T) {
	ws := `[{"id":"w1","name":"default","tabs":[{"surface_id":"s1"}]}]`
	sockPath := startSequenceSocket(t, []string{treeJSON(ws)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tw := newTopologyWatcher(ctx, sockPath)

	ec := newEventCollector()
	tw.addClient(makeTestClient(ec))

	events := ec.waitForN(t, 1, 8*time.Second)
	decoded, err := base64.StdEncoding.DecodeString(events[0].DataBase64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Payload should contain extracted workspaces, NOT the full tree with "windows".
	if _, ok := payload["windows"]; ok {
		t.Fatalf("payload should contain extracted workspaces, not the full tree: %s", decoded)
	}
	if string(payload["workspaces"]) != ws {
		t.Fatalf("workspaces mismatch: got %s, want %s", payload["workspaces"], ws)
	}
}
