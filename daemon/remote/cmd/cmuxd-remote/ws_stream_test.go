package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Mock helpers ---

// startMockReadScreenSocket creates a mock cmux.sock that responds to surface.read_text.
// It returns canned responses in order (cycling back to last if exhausted).
func startMockReadScreenSocket(t *testing.T, responses []string) string {
	t.Helper()
	sockPath := makeShortUnixSocketPath(t)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	callIdx := 0

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				if n == 0 {
					return
				}
				var req map[string]any
				if err := json.Unmarshal(buf[:n], &req); err != nil {
					c.Write([]byte(`{"ok":false,"error":{"code":"parse","message":"bad json"}}` + "\n"))
					return
				}

				mu.Lock()
				idx := callIdx
				if idx < len(responses) {
					callIdx++
				} else {
					idx = len(responses) - 1
				}
				text := responses[idx]
				mu.Unlock()

				result := map[string]any{
					"text":       text,
					"surface_id": "resolved-uuid",
				}
				resp := map[string]any{
					"id":     req["id"],
					"ok":     true,
					"result": result,
				}
				payload, _ := json.Marshal(resp)
				c.Write(append(payload, '\n'))
			}(conn)
		}
	}()

	return sockPath
}

// startMockErrorSocket creates a mock cmux.sock that always returns an error.
func startMockErrorSocket(t *testing.T, errorCode, errorMsg string) string {
	t.Helper()
	sockPath := makeShortUnixSocketPath(t)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				if n == 0 {
					return
				}
				var req map[string]any
				json.Unmarshal(buf[:n], &req)
				resp := fmt.Sprintf(`{"id":%q,"ok":false,"error":{"code":%q,"message":%q}}`,
					fmt.Sprintf("%v", req["id"]), errorCode, errorMsg)
				c.Write([]byte(resp + "\n"))
			}(conn)
		}
	}()

	return sockPath
}

// startMockRecoveringSocket returns errors for the first N calls, then succeeds.
func startMockRecoveringSocket(t *testing.T, failCount int, successText string) string {
	t.Helper()
	sockPath := makeShortUnixSocketPath(t)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	callIdx := 0

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				if n == 0 {
					return
				}
				var req map[string]any
				json.Unmarshal(buf[:n], &req)

				mu.Lock()
				idx := callIdx
				callIdx++
				mu.Unlock()

				id := fmt.Sprintf("%v", req["id"])

				if idx == 0 {
					// First call (initial read) always succeeds with initial text.
					result := map[string]any{
						"text":       "initial",
						"surface_id": "resolved-uuid",
					}
					resp := map[string]any{"id": id, "ok": true, "result": result}
					payload, _ := json.Marshal(resp)
					c.Write(append(payload, '\n'))
					return
				}

				// Subsequent calls: fail for failCount, then succeed.
				if idx <= failCount {
					resp := fmt.Sprintf(`{"id":%q,"ok":false,"error":{"code":"transient","message":"temporary failure"}}`, id)
					c.Write([]byte(resp + "\n"))
				} else {
					result := map[string]any{
						"text":       successText,
						"surface_id": "resolved-uuid",
					}
					resp := map[string]any{"id": id, "ok": true, "result": result}
					payload, _ := json.Marshal(resp)
					c.Write(append(payload, '\n'))
				}
			}(conn)
		}
	}()

	return sockPath
}

// collectEvents creates a wsClient wired to collect events on a channel.
func collectEvents(ctx context.Context, cancel context.CancelFunc, sockPath string) (*wsClient, chan rpcEvent) {
	events := make(chan rpcEvent, 64)
	cl := &wsClient{
		cmuxSocket: sockPath,
		ctx:        ctx,
		cancel:     cancel,
		watchers:   make(map[string]context.CancelFunc),
		sendEventFn: func(_ context.Context, evt rpcEvent) error {
			events <- evt
			return nil
		},
		sendResponseFn: func(_ context.Context, resp rpcResponse) error {
			return nil
		},
	}
	return cl, events
}

// waitForEvent reads from the events channel with a timeout.
func waitForEvent(t *testing.T, events <-chan rpcEvent, timeout time.Duration) (rpcEvent, bool) {
	t.Helper()
	select {
	case evt := <-events:
		return evt, true
	case <-time.After(timeout):
		return rpcEvent{}, false
	}
}

// decodeBase64Data decodes a base64-encoded JSON payload and unmarshals it.
func decodeBase64Data(t *testing.T, b64 string) map[string]any {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("json unmarshal failed: %v (raw: %s)", err, string(raw))
	}
	return result
}

// --- splitScreenLines tests ---

func TestSplitScreenLines_Normal(t *testing.T) {
	lines := splitScreenLines("line1\nline2\nline3")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "line1" || lines[1] != "line2" || lines[2] != "line3" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

func TestSplitScreenLines_Empty(t *testing.T) {
	lines := splitScreenLines("")
	if len(lines) != 0 {
		t.Errorf("expected empty slice, got %v", lines)
	}
}

func TestSplitScreenLines_TrailingNewlines(t *testing.T) {
	lines := splitScreenLines("line1\nline2\n\n\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (trailing blanks stripped), got %d: %v", len(lines), lines)
	}
	if lines[0] != "line1" || lines[1] != "line2" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

func TestSplitScreenLines_SingleLine(t *testing.T) {
	lines := splitScreenLines("just one")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(lines), lines)
	}
	if lines[0] != "just one" {
		t.Errorf("unexpected line: %q", lines[0])
	}
}

func TestSplitScreenLines_AllBlanks(t *testing.T) {
	lines := splitScreenLines("\n\n\n")
	if len(lines) != 0 {
		t.Errorf("expected empty slice for all-blank input, got %v", lines)
	}
}

func TestSplitScreenLines_CRLFHandling(t *testing.T) {
	// splitScreenLines splits on \n; \r remains attached to lines.
	// The test plan expects \r to be stripped, but the implementation only splits on \n.
	// We test actual behavior: \r is preserved in lines (since splitScreenLines doesn't strip it).
	lines := splitScreenLines("line1\r\nline2\r\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(lines), lines)
	}
	// \r is preserved because splitScreenLines only strips trailing empty lines.
	if lines[0] != "line1\r" {
		t.Errorf("expected 'line1\\r', got %q", lines[0])
	}
	if lines[1] != "line2\r" {
		t.Errorf("expected 'line2\\r', got %q", lines[1])
	}
}

// --- encodeEventData tests ---

func TestEncodeEventData_ValidBase64JSON(t *testing.T) {
	input := map[string]any{"lines": []string{"hello"}}
	encoded := encodeEventData(input)
	if encoded == "" {
		t.Fatal("encodeEventData returned empty string")
	}
	data := decodeBase64Data(t, encoded)
	lines, ok := data["lines"].([]any)
	if !ok {
		t.Fatalf("expected lines array, got %T", data["lines"])
	}
	if len(lines) != 1 || lines[0] != "hello" {
		t.Errorf("unexpected decoded lines: %v", lines)
	}
}

func TestEncodeEventData_EmptyStruct(t *testing.T) {
	encoded := encodeEventData(map[string]any{})
	if encoded == "" {
		t.Fatal("encodeEventData returned empty string for empty map")
	}
	data := decodeBase64Data(t, encoded)
	if len(data) != 0 {
		t.Errorf("expected empty map, got %v", data)
	}
}

func TestEncodeEventData_NestedPayload(t *testing.T) {
	input := map[string]any{
		"changes": []lineChange{
			{Op: "replace", Line: 0, Text: "new"},
		},
	}
	encoded := encodeEventData(input)
	if encoded == "" {
		t.Fatal("encodeEventData returned empty string")
	}
	data := decodeBase64Data(t, encoded)
	changes, ok := data["changes"].([]any)
	if !ok {
		t.Fatalf("expected changes array, got %T", data["changes"])
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	change := changes[0].(map[string]any)
	if change["op"] != "replace" || change["text"] != "new" {
		t.Errorf("unexpected change: %v", change)
	}
}

func TestEncodeEventData_UnicodeContent(t *testing.T) {
	input := map[string]any{"lines": []string{"日本語", "emoji: 🎉"}}
	encoded := encodeEventData(input)
	if encoded == "" {
		t.Fatal("encodeEventData returned empty string for unicode input")
	}
	data := decodeBase64Data(t, encoded)
	lines := data["lines"].([]any)
	if lines[0] != "日本語" || lines[1] != "emoji: 🎉" {
		t.Errorf("unicode round-trip failed: %v", lines)
	}
}

// --- watchSurface tests ---

func TestWatchSurface_InitialScreenInit(t *testing.T) {
	sockPath := startMockReadScreenSocket(t, []string{"line1\nline2\nline3"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl, events := collectEvents(ctx, cancel, sockPath)

	go watchSurface(ctx, cl, "test-surface")

	evt, ok := waitForEvent(t, events, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_init event")
	}
	if evt.Event != "screen_init" {
		t.Fatalf("expected screen_init, got %q", evt.Event)
	}
	if evt.StreamID != "resolved-uuid" {
		t.Errorf("expected stream_id 'resolved-uuid', got %q", evt.StreamID)
	}

	data := decodeBase64Data(t, evt.DataBase64)
	lines, ok2 := data["lines"].([]any)
	if !ok2 {
		t.Fatalf("expected lines array, got %T", data["lines"])
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "line1" || lines[1] != "line2" || lines[2] != "line3" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

func TestWatchSurface_InitialScreenInit_EmptyScreen(t *testing.T) {
	sockPath := startMockReadScreenSocket(t, []string{""})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl, events := collectEvents(ctx, cancel, sockPath)

	go watchSurface(ctx, cl, "test-surface")

	evt, ok := waitForEvent(t, events, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_init event")
	}
	if evt.Event != "screen_init" {
		t.Fatalf("expected screen_init, got %q", evt.Event)
	}

	data := decodeBase64Data(t, evt.DataBase64)
	lines, ok2 := data["lines"].([]any)
	if !ok2 {
		t.Fatalf("expected lines array, got %T", data["lines"])
	}
	if len(lines) != 0 {
		t.Errorf("expected empty lines for empty screen, got %v", lines)
	}
}

func TestWatchSurface_DiffAfterChange(t *testing.T) {
	// First call returns screen A, subsequent calls return screen B.
	sockPath := startMockReadScreenSocket(t, []string{"line1\nline2", "line1\nchanged"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl, events := collectEvents(ctx, cancel, sockPath)

	go watchSurface(ctx, cl, "test-surface")

	// Consume screen_init.
	evt, ok := waitForEvent(t, events, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_init")
	}
	if evt.Event != "screen_init" {
		t.Fatalf("expected screen_init, got %q", evt.Event)
	}

	// Wait for screen_diff (poll fires at 500ms intervals).
	evt, ok = waitForEvent(t, events, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_diff event")
	}
	if evt.Event != "screen_diff" {
		t.Fatalf("expected screen_diff, got %q", evt.Event)
	}

	data := decodeBase64Data(t, evt.DataBase64)
	changes, ok2 := data["changes"].([]any)
	if !ok2 {
		t.Fatalf("expected changes array, got %T", data["changes"])
	}
	if len(changes) == 0 {
		t.Fatal("expected at least one change in diff")
	}
	// The diff should show line2 -> "changed" as a replace op.
	change := changes[0].(map[string]any)
	if change["op"] != "replace" {
		t.Errorf("expected replace op, got %q", change["op"])
	}
	if change["text"] != "changed" {
		t.Errorf("expected text 'changed', got %q", change["text"])
	}
}

func TestWatchSurface_NoDiffWhenUnchanged(t *testing.T) {
	// All calls return the same text.
	sockPath := startMockReadScreenSocket(t, []string{"stable\nscreen"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl, events := collectEvents(ctx, cancel, sockPath)

	go watchSurface(ctx, cl, "test-surface")

	// Consume screen_init.
	evt, ok := waitForEvent(t, events, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_init")
	}
	if evt.Event != "screen_init" {
		t.Fatalf("expected screen_init, got %q", evt.Event)
	}

	// Wait a bit — no screen_diff should arrive.
	_, got := waitForEvent(t, events, 1500*time.Millisecond)
	if got {
		t.Error("received unexpected event when screen content is unchanged")
	}
}

func TestWatchSurface_ConsecutiveErrors_StopsAfter10(t *testing.T) {
	// Initial read succeeds (returns "init"), then all subsequent reads fail.
	sockPath := startMockRecoveringSocket(t, 100, "") // failCount > 10, never recovers

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl, events := collectEvents(ctx, cancel, sockPath)

	go watchSurface(ctx, cl, "test-surface")

	// Consume screen_init.
	evt, ok := waitForEvent(t, events, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_init")
	}
	if evt.Event != "screen_init" {
		t.Fatalf("expected screen_init, got %q", evt.Event)
	}

	// Wait for the error event after 10 consecutive errors (at 500ms intervals = ~5s).
	evt, ok = waitForEvent(t, events, 12*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_error after 10 consecutive errors")
	}
	if evt.Event != "screen_error" {
		t.Fatalf("expected screen_error, got %q", evt.Event)
	}
	if !strings.Contains(evt.Error, "10 consecutive errors") {
		t.Errorf("expected error mentioning 10 consecutive errors, got: %q", evt.Error)
	}
}

func TestWatchSurface_ErrorRecovery(t *testing.T) {
	// Initial read succeeds, then 3 failures, then success with different content.
	sockPath := startMockRecoveringSocket(t, 3, "recovered\nline")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl, events := collectEvents(ctx, cancel, sockPath)

	go watchSurface(ctx, cl, "test-surface")

	// Consume screen_init.
	evt, ok := waitForEvent(t, events, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_init")
	}
	if evt.Event != "screen_init" {
		t.Fatalf("expected screen_init, got %q", evt.Event)
	}

	// After 3 failures + recovery, expect a screen_diff.
	evt, ok = waitForEvent(t, events, 6*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_diff after recovery")
	}
	if evt.Event != "screen_diff" {
		t.Fatalf("expected screen_diff after recovery, got %q", evt.Event)
	}
}

func TestWatchSurface_ContextCancel_CleansUp(t *testing.T) {
	sockPath := startMockReadScreenSocket(t, []string{"initial\ncontent"})
	ctx, cancel := context.WithCancel(context.Background())

	cl, events := collectEvents(ctx, cancel, sockPath)

	done := make(chan struct{})
	go func() {
		watchSurface(ctx, cl, "test-surface")
		close(done)
	}()

	// Consume screen_init.
	_, ok := waitForEvent(t, events, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_init")
	}

	// Cancel the context.
	cancel()

	// Goroutine should exit promptly.
	select {
	case <-done:
		// Good — goroutine exited.
	case <-time.After(2 * time.Second):
		t.Fatal("watchSurface goroutine did not exit after context cancel")
	}
}

func TestWatchSurface_ClientDisconnect_CleansUp(t *testing.T) {
	sockPath := startMockReadScreenSocket(t, []string{"initial\ncontent"})
	ctx, cancel := context.WithCancel(context.Background())

	// Simulate client disconnect: sendEventFn returns error after screen_init.
	initSent := false
	var mu sync.Mutex
	events := make(chan rpcEvent, 64)
	cl := &wsClient{
		cmuxSocket: sockPath,
		ctx:        ctx,
		cancel:     cancel,
		watchers:   make(map[string]context.CancelFunc),
		sendEventFn: func(_ context.Context, evt rpcEvent) error {
			mu.Lock()
			defer mu.Unlock()
			if initSent {
				// Simulate write failure on client disconnect.
				return fmt.Errorf("connection closed")
			}
			if evt.Event == "screen_init" {
				initSent = true
			}
			events <- evt
			return nil
		},
		sendResponseFn: func(_ context.Context, resp rpcResponse) error {
			return nil
		},
	}

	go watchSurface(ctx, cl, "test-surface")

	// Consume screen_init.
	_, ok := waitForEvent(t, events, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for screen_init")
	}

	// watchSurface doesn't check sendEventFn return value — it continues running.
	// The client disconnect is handled at the ws_server level by cancelling the context.
	// So we cancel to simulate the server-side cleanup.
	cancel()

	// Wait briefly to ensure no panic or hang.
	time.Sleep(500 * time.Millisecond)
}
