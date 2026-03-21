package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// findCmuxSocket tests
// ---------------------------------------------------------------------------

func TestFindCmuxSocket_FlagOverride(t *testing.T) {
	got := cmuxSocketFromFlag("/custom/path")
	if got != "/custom/path" {
		t.Fatalf("cmuxSocketFromFlag with explicit path = %q, want /custom/path", got)
	}
}

func TestFindCmuxSocket_EnvCMUX_SOCKET_PATH(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PATH", "/env/path")
	t.Setenv("CMUX_SOCKET", "")
	got := findCmuxSocket()
	if got != "/env/path" {
		t.Fatalf("findCmuxSocket() = %q, want /env/path", got)
	}
}

func TestFindCmuxSocket_EnvCMUX_SOCKET(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PATH", "")
	t.Setenv("CMUX_SOCKET", "/env2/path")
	got := findCmuxSocket()
	if got != "/env2/path" {
		t.Fatalf("findCmuxSocket() = %q, want /env2/path", got)
	}
}

func TestFindCmuxSocket_SocketAddrFile(t *testing.T) {
	// Create a real unix socket so stat succeeds
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	// Write socket_addr file
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CMUX_SOCKET_PATH", "")
	t.Setenv("CMUX_SOCKET", "")
	cmuxDir := filepath.Join(home, ".cmux")
	if err := os.MkdirAll(cmuxDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cmuxDir, "socket_addr"), []byte(sockPath+"\n"), 0o644); err != nil {
		t.Fatalf("write socket_addr: %v", err)
	}

	got := findCmuxSocket()
	if got != sockPath {
		t.Fatalf("findCmuxSocket() = %q, want %q", got, sockPath)
	}
}

func TestFindCmuxSocket_DefaultPaths(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PATH", "")
	t.Setenv("CMUX_SOCKET", "")
	// Override HOME so socket_addr file doesn't interfere
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create a unix socket at the default /tmp/cmux.sock path
	sockPath := filepath.Join(os.TempDir(), "cmux.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("cannot create /tmp/cmux.sock (may already exist): %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(sockPath)
	})

	got := findCmuxSocket()
	if got != sockPath {
		t.Fatalf("findCmuxSocket() = %q, want %q", got, sockPath)
	}
}

func TestFindCmuxSocket_FallbackOrder(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PATH", "/env1/path")
	t.Setenv("CMUX_SOCKET", "/env2/path")
	got := findCmuxSocket()
	if got != "/env1/path" {
		t.Fatalf("findCmuxSocket() = %q, want /env1/path (CMUX_SOCKET_PATH takes priority)", got)
	}
}

func TestFindCmuxSocket_NoneAvailable(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PATH", "")
	t.Setenv("CMUX_SOCKET", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No socket_addr file, no default paths — should return ""
	got := findCmuxSocket()
	if got != "" {
		t.Fatalf("findCmuxSocket() = %q, want empty string when nothing available", got)
	}
}

func TestFindCmuxSocket_AutoKeyword(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PATH", "/fallback/path")
	got := cmuxSocketFromFlag("auto")
	if got != "/fallback/path" {
		t.Fatalf("cmuxSocketFromFlag(\"auto\") = %q, want /fallback/path (should fall through)", got)
	}
}

// ---------------------------------------------------------------------------
// cmuxCall tests
// ---------------------------------------------------------------------------

func TestCmuxCall_SuccessResponse(t *testing.T) {
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		var req map[string]any
		json.Unmarshal(buf[:n], &req)
		resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":{"pong":true}}`, req["id"])
		conn.Write([]byte(resp + "\n"))
	}()

	result, err := cmuxCall(sockPath, "ping", nil)
	if err != nil {
		t.Fatalf("cmuxCall should succeed, got error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if pong, _ := parsed["pong"].(bool); !pong {
		t.Fatalf("expected pong=true, got %v", parsed)
	}
}

func TestCmuxCall_ErrorResponse(t *testing.T) {
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.Read(buf)
		conn.Write([]byte(`{"ok":false,"error":{"code":"not_found","message":"surface not found"}}` + "\n"))
	}()

	_, err = cmuxCall(sockPath, "surface.read_text", nil)
	if err == nil {
		t.Fatal("cmuxCall should return error for ok=false response")
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("error should contain 'not_found', got: %v", err)
	}
}

func TestCmuxCall_InvalidJSON(t *testing.T) {
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.Read(buf)
		conn.Write([]byte("not json\n"))
	}()

	_, err = cmuxCall(sockPath, "ping", nil)
	if err == nil {
		t.Fatal("cmuxCall should return error for invalid JSON response")
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Fatalf("error should mention unmarshal failure, got: %v", err)
	}
}

func TestCmuxCall_SocketNotFound(t *testing.T) {
	_, err := cmuxCall("/tmp/cmuxd-test-nonexistent-99999.sock", "ping", nil)
	if err == nil {
		t.Fatal("cmuxCall should fail for non-existent socket")
	}
	if !strings.Contains(err.Error(), "dial") {
		t.Fatalf("error should mention dial failure, got: %v", err)
	}
}

func TestCmuxCall_TimeoutEnforced(t *testing.T) {
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.Read(buf)
		// Never respond — let the deadline fire
		time.Sleep(10 * time.Second)
	}()

	start := time.Now()
	_, err = cmuxCall(sockPath, "ping", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("cmuxCall should timeout when socket never responds")
	}
	// The 5s deadline should fire; allow some slack
	if elapsed > 7*time.Second {
		t.Fatalf("cmuxCall took %v, should timeout within ~5s", elapsed)
	}
}

func TestCmuxCall_LargeResponse(t *testing.T) {
	// 400KB response — under the 512KB bufio reader limit
	largePayload := strings.Repeat("x", 400*1024)
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		var req map[string]any
		json.Unmarshal(buf[:n], &req)
		resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":{"data":"%s"}}`, req["id"], largePayload)
		conn.Write([]byte(resp + "\n"))
	}()

	result, err := cmuxCall(sockPath, "big.data", nil)
	if err != nil {
		t.Fatalf("cmuxCall should handle 400KB response, got error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	data, _ := parsed["data"].(string)
	if len(data) != 400*1024 {
		t.Fatalf("response data length = %d, want %d", len(data), 400*1024)
	}
}

func TestCmuxCall_OversizedResponse(t *testing.T) {
	// >512KB response — exceeds bufio reader size, ReadBytes should still work
	// (bufio auto-grows), but we verify it doesn't hang or crash
	oversizedPayload := strings.Repeat("y", 600*1024)
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		var req map[string]any
		json.Unmarshal(buf[:n], &req)
		resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":{"data":"%s"}}`, req["id"], oversizedPayload)
		conn.Write([]byte(resp + "\n"))
	}()

	result, err := cmuxCall(sockPath, "big.data", nil)
	// bufio.ReadBytes grows as needed, so this should succeed
	if err != nil {
		t.Fatalf("cmuxCall with >512KB response got error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal oversized result: %v", err)
	}
	data, _ := parsed["data"].(string)
	if len(data) != 600*1024 {
		t.Fatalf("oversized response data length = %d, want %d", len(data), 600*1024)
	}
}

func TestCmuxCall_ReadsFullResponseBeforeClose(t *testing.T) {
	// Simulate a slow writer that sends the response in 2 chunks.
	// Verifies SIGPIPE prevention (doc 29 CF-2): full response is read before conn.Close().
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		var req map[string]any
		json.Unmarshal(buf[:n], &req)

		// Send first chunk
		part1 := fmt.Sprintf(`{"id":%q,"ok":true,`, req["id"])
		conn.Write([]byte(part1))
		time.Sleep(50 * time.Millisecond)
		// Send second chunk
		conn.Write([]byte(`"result":{"chunked":true}}` + "\n"))
	}()

	result, err := cmuxCall(sockPath, "slow.response", nil)
	if err != nil {
		t.Fatalf("cmuxCall should read full chunked response, got error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal chunked result: %v", err)
	}
	if chunked, _ := parsed["chunked"].(bool); !chunked {
		t.Fatalf("expected chunked=true, got %v", parsed)
	}
}

// ---------------------------------------------------------------------------
// cmuxTopology tests
// ---------------------------------------------------------------------------

func TestCmuxTopology_ReturnsTreeResult(t *testing.T) {
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	treeJSON := `{"windows":[{"id":"w1","tabs":[]}]}`
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		var req map[string]any
		json.Unmarshal(buf[:n], &req)
		resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":%s}`, req["id"], treeJSON)
		conn.Write([]byte(resp + "\n"))
	}()

	result, err := cmuxTopology(sockPath)
	if err != nil {
		t.Fatalf("cmuxTopology should succeed, got error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal topology result: %v", err)
	}
	windows, ok := parsed["windows"].([]any)
	if !ok || len(windows) != 1 {
		t.Fatalf("expected 1 window in topology, got %v", parsed)
	}
}

func TestCmuxTopology_PassesAllWindowsParam(t *testing.T) {
	sockPath, requests := startMockV2SocketWithRequestCapture(t)

	_, err := cmuxTopology(sockPath)
	if err != nil {
		t.Fatalf("cmuxTopology should succeed, got error: %v", err)
	}

	select {
	case req := <-requests:
		if req["method"] != "system.tree" {
			t.Fatalf("expected method system.tree, got %v", req["method"])
		}
		params, _ := req["params"].(map[string]any)
		allWindows, _ := params["all_windows"].(bool)
		if !allWindows {
			t.Fatalf("expected all_windows=true in params, got %v", params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for topology request")
	}
}

// ---------------------------------------------------------------------------
// cmuxReadScreen tests
// ---------------------------------------------------------------------------

func TestCmuxReadScreen_ReturnsTextAndSurfaceID(t *testing.T) {
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		var req map[string]any
		json.Unmarshal(buf[:n], &req)
		resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":{"text":"line1\nline2","surface_id":"uuid-1"}}`, req["id"])
		conn.Write([]byte(resp + "\n"))
	}()

	text, resolvedID, err := cmuxReadScreen(sockPath, "uuid-1", 0)
	if err != nil {
		t.Fatalf("cmuxReadScreen should succeed, got error: %v", err)
	}
	if text != "line1\nline2" {
		t.Fatalf("text = %q, want %q", text, "line1\nline2")
	}
	if resolvedID != "uuid-1" {
		t.Fatalf("resolvedID = %q, want %q", resolvedID, "uuid-1")
	}
}

func TestCmuxReadScreen_PassesLinesParam(t *testing.T) {
	sockPath, requests := startMockV2SocketWithRequestCapture(t)

	_, _, err := cmuxReadScreen(sockPath, "sf-1", 80)
	if err != nil {
		t.Fatalf("cmuxReadScreen should succeed, got error: %v", err)
	}

	select {
	case req := <-requests:
		if req["method"] != "surface.read_text" {
			t.Fatalf("expected method surface.read_text, got %v", req["method"])
		}
		params, _ := req["params"].(map[string]any)
		lines, ok := params["lines"].(float64)
		if !ok || int(lines) != 80 {
			t.Fatalf("expected lines=80 in params, got %v", params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for read_text request")
	}
}

func TestCmuxReadScreen_ErrorFromSocket(t *testing.T) {
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.Read(buf)
		conn.Write([]byte(`{"ok":false,"error":{"code":"not_found","message":"surface not found"}}` + "\n"))
	}()

	text, _, err := cmuxReadScreen(sockPath, "missing-surface", 0)
	if err == nil {
		t.Fatal("cmuxReadScreen should return error when socket returns error")
	}
	if text != "" {
		t.Fatalf("text should be empty on error, got %q", text)
	}
}

// ---------------------------------------------------------------------------
// cmuxSendText / cmuxSendKey tests
// ---------------------------------------------------------------------------

func TestCmuxSendText_Dispatches(t *testing.T) {
	sockPath, requests := startMockV2SocketWithRequestCapture(t)

	err := cmuxSendText(sockPath, "sf-1", "hello world")
	if err != nil {
		t.Fatalf("cmuxSendText should succeed, got error: %v", err)
	}

	select {
	case req := <-requests:
		if req["method"] != "surface.send_text" {
			t.Fatalf("expected method surface.send_text, got %v", req["method"])
		}
		params, _ := req["params"].(map[string]any)
		if params["surface_id"] != "sf-1" {
			t.Fatalf("expected surface_id=sf-1, got %v", params["surface_id"])
		}
		if params["text"] != "hello world" {
			t.Fatalf("expected text='hello world', got %v", params["text"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for send_text request")
	}
}

func TestCmuxSendKey_Dispatches(t *testing.T) {
	sockPath, requests := startMockV2SocketWithRequestCapture(t)

	err := cmuxSendKey(sockPath, "sf-2", "enter")
	if err != nil {
		t.Fatalf("cmuxSendKey should succeed, got error: %v", err)
	}

	select {
	case req := <-requests:
		if req["method"] != "surface.send_key" {
			t.Fatalf("expected method surface.send_key, got %v", req["method"])
		}
		params, _ := req["params"].(map[string]any)
		if params["surface_id"] != "sf-2" {
			t.Fatalf("expected surface_id=sf-2, got %v", params["surface_id"])
		}
		if params["key"] != "enter" {
			t.Fatalf("expected key=enter, got %v", params["key"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for send_key request")
	}
}

// ---------------------------------------------------------------------------
// handleProxiedRequest tests
// ---------------------------------------------------------------------------

func TestHandleProxiedRequest_SystemTree(t *testing.T) {
	treeJSON := `{"windows":[{"id":"w1"}]}`
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		var req map[string]any
		json.Unmarshal(buf[:n], &req)
		resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":%s}`, req["id"], treeJSON)
		conn.Write([]byte(resp + "\n"))
	}()

	resp := handleProxiedRequest(sockPath, rpcRequest{
		ID:     "req-1",
		Method: "system.tree",
		Params: map[string]any{},
	})
	if !resp.OK {
		t.Fatalf("handleProxiedRequest system.tree should succeed: %+v", resp)
	}
	// Result should be json.RawMessage containing the tree
	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(resultBytes), "w1") {
		t.Fatalf("result should contain window data, got %s", string(resultBytes))
	}
}

func TestHandleProxiedRequest_SurfaceInput(t *testing.T) {
	sockPath, requests := startMockV2SocketWithRequestCapture(t)

	resp := handleProxiedRequest(sockPath, rpcRequest{
		ID:     "req-2",
		Method: "surface.input",
		Params: map[string]any{
			"surface_id": "sf-1",
			"text":       "typed text",
		},
	})
	if !resp.OK {
		t.Fatalf("handleProxiedRequest surface.input should succeed: %+v", resp)
	}

	select {
	case req := <-requests:
		// surface.input should be proxied as surface.send_text
		if req["method"] != "surface.send_text" {
			t.Fatalf("expected proxied method surface.send_text, got %v", req["method"])
		}
		params, _ := req["params"].(map[string]any)
		if params["surface_id"] != "sf-1" {
			t.Fatalf("expected surface_id=sf-1, got %v", params["surface_id"])
		}
		if params["text"] != "typed text" {
			t.Fatalf("expected text='typed text', got %v", params["text"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxied surface.input request")
	}
}

func TestHandleProxiedRequest_SurfaceKeys(t *testing.T) {
	sockPath, requests := startMockV2SocketWithRequestCapture(t)

	resp := handleProxiedRequest(sockPath, rpcRequest{
		ID:     "req-3",
		Method: "surface.keys",
		Params: map[string]any{
			"surface_id": "sf-2",
			"key":        "ctrl+c",
		},
	})
	if !resp.OK {
		t.Fatalf("handleProxiedRequest surface.keys should succeed: %+v", resp)
	}

	select {
	case req := <-requests:
		// surface.keys should be proxied as surface.send_key
		if req["method"] != "surface.send_key" {
			t.Fatalf("expected proxied method surface.send_key, got %v", req["method"])
		}
		params, _ := req["params"].(map[string]any)
		if params["surface_id"] != "sf-2" {
			t.Fatalf("expected surface_id=sf-2, got %v", params["surface_id"])
		}
		if params["key"] != "ctrl+c" {
			t.Fatalf("expected key=ctrl+c, got %v", params["key"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxied surface.keys request")
	}
}

func TestHandleProxiedRequest_Ping(t *testing.T) {
	// Ping should return pong without hitting cmux.sock
	resp := handleProxiedRequest("/nonexistent/path.sock", rpcRequest{
		ID:     "req-4",
		Method: "ping",
		Params: map[string]any{},
	})
	if !resp.OK {
		t.Fatalf("ping should succeed without socket: %+v", resp)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("ping result should be a map, got %T", resp.Result)
	}
	if pong, _ := result["pong"].(bool); !pong {
		t.Fatalf("ping result should have pong=true, got %v", result)
	}
}

func TestHandleProxiedRequest_UnknownMethod(t *testing.T) {
	resp := handleProxiedRequest("/nonexistent/path.sock", rpcRequest{
		ID:     "req-5",
		Method: "does_not_exist",
		Params: map[string]any{},
	})
	if resp.OK {
		t.Fatalf("unknown method should fail: %+v", resp)
	}
	if resp.Error == nil || resp.Error.Code != "method_not_found" {
		t.Fatalf("expected error code method_not_found, got %+v", resp.Error)
	}
}

func TestHandleProxiedRequest_CmuxDown(t *testing.T) {
	resp := handleProxiedRequest("/tmp/cmuxd-test-nonexistent-99999.sock", rpcRequest{
		ID:     "req-6",
		Method: "system.tree",
		Params: map[string]any{},
	})
	if resp.OK {
		t.Fatal("system.tree should fail when cmux is down")
	}
	if resp.Error == nil || resp.Error.Code != "proxy_error" {
		t.Fatalf("expected error code proxy_error, got %+v", resp.Error)
	}
}

func TestHandleProxiedRequest_CmuxTimeout(t *testing.T) {
	// Create a socket that accepts but never responds
	sockPath := makeShortUnixSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		conn.Read(buf)
		// Never respond
		time.Sleep(10 * time.Second)
	}()

	start := time.Now()
	resp := handleProxiedRequest(sockPath, rpcRequest{
		ID:     "req-7",
		Method: "system.tree",
		Params: map[string]any{},
	})
	elapsed := time.Since(start)

	if resp.OK {
		t.Fatal("system.tree should fail when cmux times out")
	}
	if resp.Error == nil || resp.Error.Code != "proxy_error" {
		t.Fatalf("expected error code proxy_error, got %+v", resp.Error)
	}
	// Should timeout within the 5s deadline + slack
	if elapsed > 7*time.Second {
		t.Fatalf("handleProxiedRequest took %v, should timeout within ~5s", elapsed)
	}
}
