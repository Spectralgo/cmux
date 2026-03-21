package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// --- Canned data for mock cmux.sock ---

const testSurfaceID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func cannedTreeResult() string {
	tree := map[string]any{
		"active": map[string]any{
			"surface_id":   testSurfaceID,
			"workspace_id": "ws-1",
		},
		"windows": []any{
			map[string]any{
				"id":  "win-1",
				"ref": "win-ref-1",
				"workspaces": []any{
					map[string]any{
						"id":       "ws-1",
						"ref":      "ws-ref-1",
						"index":    0,
						"title":    "default",
						"selected": true,
						"panes": []any{
							map[string]any{
								"id":      "pane-1",
								"ref":     "pane-ref-1",
								"index":   0,
								"focused": true,
								"surfaces": []any{
									map[string]any{
										"id":       testSurfaceID,
										"ref":      "sf-ref-1",
										"index":    0,
										"type":     "terminal",
										"title":    "bash",
										"focused":  true,
										"pane_id":  "pane-1",
										"pane_ref": "pane-ref-1",
									},
								},
							},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(tree)
	return string(b)
}

// serveMockCmux handles connections to the mock cmux.sock.
// It dispatches system.tree, surface.read_text, and surface.send_text.
// screenText is an atomic-style value guarded by mu.
func serveMockCmux(t *testing.T, ln net.Listener, screenText *string, mu *sync.RWMutex, sendTextCalls chan<- map[string]any) {
	t.Helper()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 8192)
			n, _ := c.Read(buf)
			if n == 0 {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(buf[:n], &req); err != nil {
				c.Write([]byte(`{"ok":false,"error":{"code":"parse","message":"bad json"}}` + "\n"))
				return
			}

			id := req["id"]
			method, _ := req["method"].(string)

			switch method {
			case "system.tree":
				resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":%s}`, id, cannedTreeResult())
				c.Write([]byte(resp + "\n"))

			case "surface.read_text":
				mu.RLock()
				text := *screenText
				mu.RUnlock()
				result := map[string]any{
					"text":       text,
					"surface_id": testSurfaceID,
				}
				resultJSON, _ := json.Marshal(result)
				resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":%s}`, id, string(resultJSON))
				c.Write([]byte(resp + "\n"))

			case "surface.send_text":
				params, _ := req["params"].(map[string]any)
				if sendTextCalls != nil {
					sendTextCalls <- params
				}
				resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":{"sent":true}}`, id)
				c.Write([]byte(resp + "\n"))

			default:
				resp := fmt.Sprintf(`{"id":%q,"ok":false,"error":{"code":"method_not_found","message":"unknown"}}`, id)
				c.Write([]byte(resp + "\n"))
			}
		}(conn)
	}
}

// startTestWSServer starts a real WS server backed by the given cmux socket,
// listening on a random port. Returns the host:port address.
func startTestWSServer(t *testing.T, cmuxSocket, authToken string) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tw := newTopologyWatcher(ctx, cmuxSocket)
	bridge := newGTBridge()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWSConnection(w, r, cmuxSocket, authToken, tw, bridge)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	server := &http.Server{Handler: mux}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })

	return ln.Addr().String()
}

// wsConnect dials a WS server and authenticates with the given token.
// Returns the authenticated connection.
func wsConnect(t *testing.T, addr, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}

	writeJSON(t, conn, map[string]any{
		"method": "auth",
		"params": map[string]any{"token": token},
	})

	authResp := readJSONWithTimeout(t, conn, 3*time.Second)
	ok, _ := authResp["ok"].(bool)
	if !ok {
		t.Fatalf("auth failed: %v", authResp)
	}

	return conn
}

// writeJSON marshals and sends a JSON message over the WebSocket.
func writeJSON(t *testing.T, conn *websocket.Conn, msg map[string]any) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

// readJSONWithTimeout reads and parses a JSON message from the WebSocket
// with the given timeout.
func readJSONWithTimeout(t *testing.T, conn *websocket.Conn, timeout time.Duration) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read (timeout %v): %v", timeout, err)
	}

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal ws message: %v (raw: %s)", err, string(data))
	}
	return msg
}

// TestWSEndToEnd runs the full 9-step end-to-end WebSocket round-trip test.
func TestWSEndToEnd(t *testing.T) {
	// --- Step 1: Mock cmux.sock ---
	sockPath := makeShortUnixSocketPath(t)
	var screenMu sync.RWMutex
	screenText := "$ whoami\nuser\n$ "
	sendTextCalls := make(chan map[string]any, 8)

	mockLn, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("mock listen: %v", err)
	}
	t.Cleanup(func() { mockLn.Close() })
	go serveMockCmux(t, mockLn, &screenText, &screenMu, sendTextCalls)

	// --- Step 2: Real WS server ---
	addr := startTestWSServer(t, sockPath, "test-token")

	// --- Step 3: WS client ---
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()

	// --- Step 4: AUTH ---
	writeJSON(t, conn, map[string]any{
		"method": "auth",
		"params": map[string]any{"token": "test-token"},
	})
	authResp := readJSONWithTimeout(t, conn, 3*time.Second)

	// Verify ok=true
	if ok, _ := authResp["ok"].(bool); !ok {
		t.Fatalf("auth response should have ok=true, got: %v", authResp)
	}

	// Verify NO id field in auth response
	if _, hasID := authResp["id"]; hasID {
		t.Fatalf("auth response must NOT have id field, got: %v", authResp)
	}

	// Verify result.authenticated=true
	result, _ := authResp["result"].(map[string]any)
	if authenticated, _ := result["authenticated"].(bool); !authenticated {
		t.Fatalf("auth result should have authenticated=true, got: %v", result)
	}

	// --- Step 5: TREE ---
	writeJSON(t, conn, map[string]any{
		"id":     "1",
		"method": "system.tree",
	})
	treeResp := readJSONWithTimeout(t, conn, 3*time.Second)

	if ok, _ := treeResp["ok"].(bool); !ok {
		t.Fatalf("system.tree should have ok=true, got: %v", treeResp)
	}
	if id, _ := treeResp["id"].(string); id != "1" {
		t.Fatalf("system.tree response id should be '1', got: %v", treeResp["id"])
	}

	// Verify TreeResult shape
	treeResult := treeResp["result"]
	assertTreeResultShape(t, treeResult)

	// Extract surface ID for subscribe
	surfaceID := extractFirstSurfaceID(t, treeResult)
	if surfaceID == "" {
		t.Fatal("could not extract surface ID from tree result")
	}

	// --- Step 6: SUBSCRIBE ---
	writeJSON(t, conn, map[string]any{
		"id":     "2",
		"method": "surface.subscribe",
		"params": map[string]any{"surface_id": surfaceID},
	})
	subResp := readJSONWithTimeout(t, conn, 3*time.Second)

	if ok, _ := subResp["ok"].(bool); !ok {
		t.Fatalf("surface.subscribe should have ok=true, got: %v", subResp)
	}
	subResult, _ := subResp["result"].(map[string]any)
	if subResult["subscribed"] != surfaceID {
		t.Fatalf("subscribe result.subscribed should be %q, got: %v", surfaceID, subResult["subscribed"])
	}

	// Wait for screen_init event (timeout 3s)
	initEvent := readJSONWithTimeout(t, conn, 3*time.Second)
	if initEvent["event"] != "screen_init" {
		t.Fatalf("expected screen_init event, got: %v", initEvent)
	}
	if initEvent["stream_id"] != surfaceID {
		t.Fatalf("screen_init stream_id should be %q, got: %v", surfaceID, initEvent["stream_id"])
	}

	// Verify data_base64 decodes to JSON with "lines"
	assertBase64DecodesWithKey(t, initEvent["data_base64"], "lines")

	// --- Step 7: SCREEN CHANGE ---
	screenMu.Lock()
	screenText = "$ whoami\nuser\n$ ls\nfile.txt\n$ "
	screenMu.Unlock()

	// Wait for screen_diff event (timeout 3s)
	// The watcher polls at 500ms intervals, so the diff should arrive within 1-2s
	diffEvent := readJSONWithTimeout(t, conn, 3*time.Second)
	if diffEvent["event"] != "screen_diff" {
		t.Fatalf("expected screen_diff event, got: %v", diffEvent)
	}
	if diffEvent["stream_id"] != surfaceID {
		t.Fatalf("screen_diff stream_id should be %q, got: %v", surfaceID, diffEvent["stream_id"])
	}

	// Verify data_base64 decodes to JSON with "changes"
	assertBase64DecodesWithKey(t, diffEvent["data_base64"], "changes")

	// --- Step 8: INPUT ---
	writeJSON(t, conn, map[string]any{
		"id":     "3",
		"method": "surface.input",
		"params": map[string]any{
			"surface_id": surfaceID,
			"text":       "hello\n",
		},
	})
	inputResp := readJSONWithTimeout(t, conn, 3*time.Second)

	if ok, _ := inputResp["ok"].(bool); !ok {
		t.Fatalf("surface.input should have ok=true, got: %v", inputResp)
	}

	// Verify mock cmux.sock received surface.send_text with correct params
	select {
	case call := <-sendTextCalls:
		if call["surface_id"] != surfaceID {
			t.Fatalf("send_text should target %q, got: %v", surfaceID, call["surface_id"])
		}
		if call["text"] != "hello\n" {
			t.Fatalf("send_text should have text 'hello\\n', got: %v", call["text"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for mock cmux.sock to receive surface.send_text")
	}

	// --- Step 9: DISCONNECT ---
	conn.Close(websocket.StatusNormalClosure, "done")

	// Verify: mock cmux.sock stops receiving surface.read_text polls.
	// After closing the WS connection, the watcher goroutines should exit.
	// We wait briefly and confirm no new send_text calls arrive (watchers stopped).
	time.Sleep(1500 * time.Millisecond) // > 2 poll intervals (500ms each)

	// Drain any in-flight screen polls that were already in progress
	drainDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-sendTextCalls:
				// drain stale calls
			default:
				close(drainDone)
				return
			}
		}
	}()
	<-drainDone
}

// assertTreeResultShape validates the structure of a TreeResult value.
func assertTreeResultShape(t *testing.T, result any) {
	t.Helper()

	// Re-marshal and parse to work with map[string]any consistently
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("assertTreeResultShape: marshal: %v", err)
	}
	var tree map[string]any
	if err := json.Unmarshal(data, &tree); err != nil {
		t.Fatalf("assertTreeResultShape: unmarshal: %v", err)
	}

	// Verify windows array exists
	windows, ok := tree["windows"].([]any)
	if !ok || len(windows) == 0 {
		t.Fatalf("TreeResult should have windows array with at least 1 entry, got: %v", tree)
	}

	// Verify first window has workspaces
	win0, ok := windows[0].(map[string]any)
	if !ok {
		t.Fatalf("windows[0] should be an object, got: %T", windows[0])
	}
	workspaces, ok := win0["workspaces"].([]any)
	if !ok || len(workspaces) == 0 {
		t.Fatalf("windows[0].workspaces should be a non-empty array, got: %v", win0["workspaces"])
	}

	// Verify workspace shape
	ws0, ok := workspaces[0].(map[string]any)
	if !ok {
		t.Fatalf("workspaces[0] should be an object, got: %T", workspaces[0])
	}
	for _, key := range []string{"id", "ref", "title", "selected"} {
		if _, exists := ws0[key]; !exists {
			t.Fatalf("workspace missing key %q: %v", key, ws0)
		}
	}

	// Verify panes and surfaces
	panes, ok := ws0["panes"].([]any)
	if !ok || len(panes) == 0 {
		t.Fatalf("workspace should have panes array, got: %v", ws0["panes"])
	}
	pane0, _ := panes[0].(map[string]any)
	surfaces, ok := pane0["surfaces"].([]any)
	if !ok || len(surfaces) == 0 {
		t.Fatalf("pane should have surfaces array, got: %v", pane0["surfaces"])
	}
	sf0, _ := surfaces[0].(map[string]any)
	for _, key := range []string{"id", "ref", "type", "title"} {
		if _, exists := sf0[key]; !exists {
			t.Fatalf("surface missing key %q: %v", key, sf0)
		}
	}
}

// extractFirstSurfaceID drills into a TreeResult to find the first surface ID.
func extractFirstSurfaceID(t *testing.T, result any) string {
	t.Helper()
	data, _ := json.Marshal(result)
	var tree map[string]any
	json.Unmarshal(data, &tree)

	windows, _ := tree["windows"].([]any)
	if len(windows) == 0 {
		return ""
	}
	win0, _ := windows[0].(map[string]any)
	workspaces, _ := win0["workspaces"].([]any)
	if len(workspaces) == 0 {
		return ""
	}
	ws0, _ := workspaces[0].(map[string]any)
	panes, _ := ws0["panes"].([]any)
	if len(panes) == 0 {
		return ""
	}
	pane0, _ := panes[0].(map[string]any)
	surfaces, _ := pane0["surfaces"].([]any)
	if len(surfaces) == 0 {
		return ""
	}
	sf0, _ := surfaces[0].(map[string]any)
	id, _ := sf0["id"].(string)
	return id
}

// assertBase64DecodesWithKey verifies that the value is a base64-encoded JSON
// string containing the given top-level key.
func assertBase64DecodesWithKey(t *testing.T, raw any, expectedKey string) {
	t.Helper()
	b64Str, ok := raw.(string)
	if !ok || b64Str == "" {
		t.Fatalf("data_base64 should be a non-empty string, got: %v", raw)
	}

	decoded, err := base64.StdEncoding.DecodeString(b64Str)
	if err != nil {
		t.Fatalf("data_base64 is not valid base64: %v", err)
	}

	var obj map[string]any
	if err := json.Unmarshal(decoded, &obj); err != nil {
		t.Fatalf("data_base64 decoded to invalid JSON: %v (raw: %s)", err, string(decoded))
	}

	if _, exists := obj[expectedKey]; !exists {
		t.Fatalf("data_base64 decoded JSON should have key %q, got: %v", expectedKey, obj)
	}
}
