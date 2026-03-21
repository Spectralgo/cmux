package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/coder/websocket"
)

// runWSServer starts an HTTP server with /ws and /health endpoints.
func runWSServer(addr, cmuxSocket, authToken string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tw := newTopologyWatcher(ctx, cmuxSocket)
	bridge := newGTBridge()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWSConnection(w, r, cmuxSocket, authToken, tw, bridge)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	log.Printf("WebSocket server listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}

// handleWSConnection accepts a WebSocket connection, authenticates, and runs the read loop.
func handleWSConnection(w http.ResponseWriter, r *http.Request, cmuxSocket, authToken string, tw *topologyWatcher, bridge *gtBridge) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionContextTakeover,
	})
	if err != nil {
		log.Printf("ws accept error: %v", err)
		return
	}
	conn.SetReadLimit(65536) // 64KB

	// Use context.Background() for WS lifetime, NOT r.Context().
	ctx, cancel := context.WithCancel(context.Background())

	cl := &wsClient{
		cmuxSocket: cmuxSocket,
		ctx:        ctx,
		cancel:     cancel,
		watchers:   make(map[string]context.CancelFunc),
	}

	// Wire up the send functions.
	cl.sendEventFn = func(sendCtx context.Context, evt rpcEvent) error {
		return cl.sendEvent(conn, sendCtx, evt)
	}
	cl.sendResponseFn = func(sendCtx context.Context, resp rpcResponse) error {
		return cl.sendResponse(conn, sendCtx, resp)
	}

	defer cl.cleanup(conn, tw)

	if !cl.authenticate(conn, ctx, authToken) {
		return
	}

	tw.addClient(cl)

	cl.readLoop(conn, ctx, cmuxSocket, bridge)
}

// authenticate reads the first message and validates the auth token.
func (cl *wsClient) authenticate(conn *websocket.Conn, ctx context.Context, expectedToken string) bool {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return false
	}

	var req rpcRequest
	if err := json.Unmarshal(data, &req); err != nil {
		cl.sendResponse(conn, ctx, rpcResponse{
			OK: false,
			Error: &rpcError{
				Code:    "auth_failed",
				Message: "invalid auth message",
			},
		})
		return false
	}

	if req.Method != "auth" {
		cl.sendResponse(conn, ctx, rpcResponse{
			OK: false,
			Error: &rpcError{
				Code:    "auth_failed",
				Message: "first message must be auth",
			},
		})
		return false
	}

	token, _ := getStringParam(req.Params, "token")
	if expectedToken != "" && token != expectedToken {
		cl.sendResponse(conn, ctx, rpcResponse{
			OK: false,
			Error: &rpcError{
				Code:    "auth_failed",
				Message: "invalid token",
			},
		})
		return false
	}

	// Auth response MUST NOT include id field (omitempty drops it when ID is nil).
	cl.sendResponse(conn, ctx, rpcResponse{
		OK:     true,
		Result: map[string]any{"authenticated": true},
	})
	return true
}

// readLoop reads messages from the WebSocket and dispatches them.
func (cl *wsClient) readLoop(conn *websocket.Conn, ctx context.Context, cmuxSocket string, bridge *gtBridge) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var req rpcRequest
		if err := json.Unmarshal(data, &req); err != nil {
			cl.sendError(conn, ctx, nil, "invalid_request", "invalid JSON")
			continue
		}

		cl.dispatch(conn, ctx, req, cmuxSocket, bridge)
	}
}

// dispatch routes a parsed request to the appropriate handler.
func (cl *wsClient) dispatch(conn *websocket.Conn, ctx context.Context, req rpcRequest, cmuxSocket string, bridge *gtBridge) {
	switch req.Method {
	case "system.tree", "surface.input", "surface.keys":
		resp := handleProxiedRequest(cmuxSocket, req)
		cl.sendResponse(conn, ctx, resp)

	case "surface.subscribe":
		cl.handleSubscribe(ctx, req)

	case "surface.unsubscribe":
		cl.handleUnsubscribe(ctx, req)

	case "gt.status", "gt.command":
		resp := handleGTRequest(bridge, req)
		cl.sendResponse(conn, ctx, resp)

	case "ping":
		cl.sendResponse(conn, ctx, rpcResponse{
			ID:     req.ID,
			OK:     true,
			Result: map[string]any{"pong": true},
		})

	default:
		cl.sendError(conn, ctx, req.ID, "method_not_found", fmt.Sprintf("unknown method %q", req.Method))
	}
}

// sendResponse marshals and writes a response frame to the WebSocket.
func (cl *wsClient) sendResponse(conn *websocket.Conn, ctx context.Context, resp rpcResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return conn.Write(ctx, websocket.MessageText, data)
}

// sendEvent marshals and writes an event frame to the WebSocket.
func (cl *wsClient) sendEvent(conn *websocket.Conn, ctx context.Context, evt rpcEvent) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return conn.Write(ctx, websocket.MessageText, data)
}

// sendError sends a convenience error response.
func (cl *wsClient) sendError(conn *websocket.Conn, ctx context.Context, id any, code, message string) {
	cl.sendResponse(conn, ctx, rpcResponse{
		ID: id,
		OK: false,
		Error: &rpcError{
			Code:    code,
			Message: message,
		},
	})
}

// cleanup cancels all watchers, removes from topology watcher, and closes the connection.
func (cl *wsClient) cleanup(conn *websocket.Conn, tw *topologyWatcher) {
	tw.removeClient(cl)
	cl.cleanupWatchers()
	cl.cancel()
	conn.Close(websocket.StatusNormalClosure, "")
}
