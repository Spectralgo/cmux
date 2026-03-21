package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// findCmuxSocket auto-discovers the cmux.sock path using a priority chain:
// 1. CMUX_SOCKET_PATH env
// 2. CMUX_SOCKET env
// 3. ~/.cmux/socket_addr file
// 4. /tmp defaults: cmux.sock, cmux-debug.sock, cmux-nightly.sock
// 5. ~/Library/Application Support/cmux/cmux.sock
func findCmuxSocket() string {
	if p := os.Getenv("CMUX_SOCKET_PATH"); p != "" {
		return p
	}
	if p := os.Getenv("CMUX_SOCKET"); p != "" {
		return p
	}
	if p := readSocketAddrFile(); p != "" {
		return p
	}
	for _, name := range []string{"cmux.sock", "cmux-debug.sock", "cmux-nightly.sock"} {
		p := filepath.Join(os.TempDir(), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, "Library", "Application Support", "cmux", "cmux.sock")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// cmuxCall sends a JSON-RPC request to cmux.sock and returns the raw result.
// Uses a 5s total deadline and always reads the full response before closing
// (SIGPIPE prevention per doc 29 CF-2).
func cmuxCall(socketPath, method string, params map[string]any) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cmuxCall: dial %s: %w", socketPath, err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	id := randomHex(8)
	reqMap := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		reqMap["params"] = params
	} else {
		reqMap["params"] = map[string]any{}
	}

	payload, err := json.Marshal(reqMap)
	if err != nil {
		return nil, fmt.Errorf("cmuxCall: marshal: %w", err)
	}

	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("cmuxCall: write: %w", err)
	}

	// Read full response before Close() to prevent SIGPIPE on cmux side.
	reader := bufio.NewReaderSize(conn, 512*1024)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("cmuxCall: read: %w", err)
	}

	var resp struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error,omitempty"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("cmuxCall: unmarshal: %w", err)
	}
	if !resp.OK {
		if resp.Error != nil {
			return nil, fmt.Errorf("cmuxCall: %s: [%s] %s", method, resp.Error.Code, resp.Error.Message)
		}
		return nil, fmt.Errorf("cmuxCall: %s: server returned error", method)
	}

	return resp.Result, nil
}

// cmuxTopology calls system.tree on cmux.sock with all_windows: true.
func cmuxTopology(socketPath string) (json.RawMessage, error) {
	return cmuxCall(socketPath, "system.tree", map[string]any{
		"all_windows": true,
	})
}

// cmuxReadScreen calls surface.read_text on cmux.sock and returns the screen
// text and the resolved surface ID.
func cmuxReadScreen(socketPath, surfaceID string, lines int) (string, string, error) {
	params := map[string]any{
		"surface_id": surfaceID,
	}
	if lines > 0 {
		params["lines"] = lines
	}

	raw, err := cmuxCall(socketPath, "surface.read_text", params)
	if err != nil {
		return "", "", err
	}

	var result struct {
		Text      string `json:"text"`
		SurfaceID string `json:"surface_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", fmt.Errorf("cmuxReadScreen: unmarshal result: %w", err)
	}

	resolvedID := result.SurfaceID
	if resolvedID == "" {
		resolvedID = surfaceID
	}
	return result.Text, resolvedID, nil
}

// cmuxSendText calls surface.send_text on cmux.sock.
func cmuxSendText(socketPath, surfaceID, text string) error {
	_, err := cmuxCall(socketPath, "surface.send_text", map[string]any{
		"surface_id": surfaceID,
		"text":       text,
	})
	return err
}

// cmuxSendKey calls surface.send_key on cmux.sock.
func cmuxSendKey(socketPath, surfaceID, key string) error {
	_, err := cmuxCall(socketPath, "surface.send_key", map[string]any{
		"surface_id": surfaceID,
		"key":        key,
	})
	return err
}

// handleProxiedRequest dispatches sync methods from the WS client to cmux.sock.
// It translates RN client method names to cmux.sock method names where needed.
func handleProxiedRequest(cmuxSocket string, req rpcRequest) rpcResponse {
	switch req.Method {
	case "system.tree":
		result, err := cmuxTopology(cmuxSocket)
		if err != nil {
			return rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "proxy_error",
					Message: err.Error(),
				},
			}
		}
		return rpcResponse{
			ID:     req.ID,
			OK:     true,
			Result: result,
		}

	case "surface.input":
		// RN client sends "surface.input" → proxy calls cmux.sock "surface.send_text"
		surfaceID, _ := getStringParam(req.Params, "surface_id")
		text, _ := getStringParam(req.Params, "text")
		if surfaceID == "" || text == "" {
			return rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "invalid_params",
					Message: "surface.input requires surface_id and text",
				},
			}
		}
		if err := cmuxSendText(cmuxSocket, surfaceID, text); err != nil {
			return rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "proxy_error",
					Message: err.Error(),
				},
			}
		}
		return rpcResponse{
			ID:     req.ID,
			OK:     true,
			Result: map[string]any{"sent": true},
		}

	case "surface.keys":
		// RN client sends "surface.keys" → proxy calls cmux.sock "surface.send_key"
		surfaceID, _ := getStringParam(req.Params, "surface_id")
		key, _ := getStringParam(req.Params, "key")
		if surfaceID == "" || key == "" {
			return rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "invalid_params",
					Message: "surface.keys requires surface_id and key",
				},
			}
		}
		if err := cmuxSendKey(cmuxSocket, surfaceID, key); err != nil {
			return rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "proxy_error",
					Message: err.Error(),
				},
			}
		}
		return rpcResponse{
			ID:     req.ID,
			OK:     true,
			Result: map[string]any{"sent": true},
		}

	case "gt.status":
		result, err := cmuxCall(cmuxSocket, "gt.status", req.Params)
		if err != nil {
			return rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "proxy_error",
					Message: err.Error(),
				},
			}
		}
		return rpcResponse{
			ID:     req.ID,
			OK:     true,
			Result: result,
		}

	case "gt.command":
		result, err := cmuxCall(cmuxSocket, "gt.command", req.Params)
		if err != nil {
			return rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "proxy_error",
					Message: err.Error(),
				},
			}
		}
		return rpcResponse{
			ID:     req.ID,
			OK:     true,
			Result: result,
		}

	case "ping":
		return rpcResponse{
			ID:     req.ID,
			OK:     true,
			Result: map[string]any{"pong": true},
		}

	default:
		return rpcResponse{
			ID: req.ID,
			OK: false,
			Error: &rpcError{
				Code:    "method_not_found",
				Message: fmt.Sprintf("unknown method %q", req.Method),
			},
		}
	}
}

// cmuxSocketFromFlag returns the cmux socket path, using the flag value if
// provided and not "auto", otherwise falling back to findCmuxSocket().
func cmuxSocketFromFlag(flagValue string) string {
	if flagValue != "" && flagValue != "auto" {
		return flagValue
	}
	return findCmuxSocket()
}

// proxyPassthrough forwards an arbitrary method and params to cmux.sock and
// wraps the result in an rpcResponse. Used by dispatch for methods that need
// no translation (e.g. gt.status, gt.command).
func proxyPassthrough(cmuxSocket string, req rpcRequest) rpcResponse {
	result, err := cmuxCall(cmuxSocket, req.Method, req.Params)
	if err != nil {
		return rpcResponse{
			ID: req.ID,
			OK: false,
			Error: &rpcError{
				Code:    "proxy_error",
				Message: err.Error(),
			},
		}
	}
	return rpcResponse{
		ID:     req.ID,
		OK:     true,
		Result: result,
	}
}

