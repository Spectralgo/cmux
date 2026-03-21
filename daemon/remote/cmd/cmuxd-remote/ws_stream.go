package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// wsClient represents a connected WebSocket client with surface watchers.
// The conn field and sendEvent/sendResponse/sendError methods will be provided
// by ws_server.go (D-5). This file defines the subscription and streaming logic.
type wsClient struct {
	cmuxSocket string
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	watchers   map[string]context.CancelFunc // surface UUID -> cancel

	// sendEvent sends an rpcEvent frame to the client. Set by ws_server.go.
	sendEventFn func(ctx context.Context, evt rpcEvent) error
	// sendResponse sends an rpcResponse frame to the client. Set by ws_server.go.
	sendResponseFn func(ctx context.Context, resp rpcResponse) error
}

// handleSubscribe starts or restarts a surface watcher.
// Method: "surface.subscribe", params: {surface_id: "<UUID>"}
func (cl *wsClient) handleSubscribe(ctx context.Context, req rpcRequest) {
	surfaceID, ok := getStringParam(req.Params, "surface_id")
	if !ok || surfaceID == "" {
		cl.sendResponseFn(ctx, rpcResponse{
			ID: req.ID,
			OK: false,
			Error: &rpcError{
				Code:    "invalid_params",
				Message: "surface.subscribe requires surface_id",
			},
		})
		return
	}

	cl.mu.Lock()
	// Cancel existing watcher for this surface if any.
	if cancelFn, exists := cl.watchers[surfaceID]; exists {
		cancelFn()
	}
	watchCtx, watchCancel := context.WithCancel(cl.ctx)
	cl.watchers[surfaceID] = watchCancel
	cl.mu.Unlock()

	// Acknowledge the subscription.
	cl.sendResponseFn(ctx, rpcResponse{
		ID: req.ID,
		OK: true,
		Result: map[string]any{
			"subscribed": surfaceID,
		},
	})

	go watchSurface(watchCtx, cl, surfaceID)
}

// handleUnsubscribe stops a surface watcher.
// Method: "surface.unsubscribe", params: {surface_id: "<UUID>"}
func (cl *wsClient) handleUnsubscribe(ctx context.Context, req rpcRequest) {
	surfaceID, ok := getStringParam(req.Params, "surface_id")
	if !ok || surfaceID == "" {
		cl.sendResponseFn(ctx, rpcResponse{
			ID: req.ID,
			OK: false,
			Error: &rpcError{
				Code:    "invalid_params",
				Message: "surface.unsubscribe requires surface_id",
			},
		})
		return
	}

	cl.mu.Lock()
	cancelFn, exists := cl.watchers[surfaceID]
	if exists {
		cancelFn()
		delete(cl.watchers, surfaceID)
	}
	cl.mu.Unlock()

	cl.sendResponseFn(ctx, rpcResponse{
		ID: req.ID,
		OK: true,
		Result: map[string]any{
			"unsubscribed": surfaceID,
		},
	})
}

// watchSurface polls cmuxReadScreen at 500ms intervals, diffs against previous
// state, and pushes screen_init / screen_diff events to the client.
func watchSurface(ctx context.Context, cl *wsClient, surfaceID string) {
	// Initial read: send screen_init.
	text, resolvedID, err := cmuxReadScreen(cl.cmuxSocket, surfaceID, 80)
	if err != nil {
		cl.sendEventFn(ctx, rpcEvent{
			Event:    "screen_error",
			StreamID: surfaceID,
			Error:    fmt.Sprintf("initial read failed: %v", err),
		})
		return
	}

	// Use the resolved surface ID for all subsequent operations.
	if resolvedID != "" {
		surfaceID = resolvedID
	}

	prevLines := splitScreenLines(text)

	cl.sendEventFn(ctx, rpcEvent{
		Event:      "screen_init",
		StreamID:   surfaceID,
		DataBase64: encodeEventData(map[string]any{"lines": prevLines}),
	})

	// Poll loop.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	consecutiveErrors := 0
	const maxConsecutiveErrors = 10

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			currText, _, readErr := cmuxReadScreen(cl.cmuxSocket, surfaceID, 80)
			if readErr != nil {
				consecutiveErrors++

				// Check for surface-not-found (surface closed in cmux).
				if strings.Contains(readErr.Error(), "not_found") {
					cl.sendEventFn(ctx, rpcEvent{
						Event:    "screen_close",
						StreamID: surfaceID,
						Error:    "surface closed",
					})
					cl.removeSurfaceWatcher(surfaceID)
					return
				}

				if consecutiveErrors >= maxConsecutiveErrors {
					cl.sendEventFn(ctx, rpcEvent{
						Event:    "screen_error",
						StreamID: surfaceID,
						Error:    fmt.Sprintf("stopped after %d consecutive errors: %v", maxConsecutiveErrors, readErr),
					})
					cl.removeSurfaceWatcher(surfaceID)
					return
				}
				// Skip this tick, retry next.
				continue
			}

			consecutiveErrors = 0

			currLines := splitScreenLines(currText)
			changes := diffLines(prevLines, currLines)
			if len(changes) > 0 {
				prevLines = currLines
				cl.sendEventFn(ctx, rpcEvent{
					Event:      "screen_diff",
					StreamID:   surfaceID,
					DataBase64: encodeEventData(map[string]any{"changes": changes}),
				})
			}
		}
	}
}

// removeSurfaceWatcher cancels and removes a watcher from the client's map.
func (cl *wsClient) removeSurfaceWatcher(surfaceID string) {
	cl.mu.Lock()
	if cancelFn, exists := cl.watchers[surfaceID]; exists {
		cancelFn()
		delete(cl.watchers, surfaceID)
	}
	cl.mu.Unlock()
}

// cleanupWatchers cancels all active watchers for this client.
func (cl *wsClient) cleanupWatchers() {
	cl.mu.Lock()
	for id, cancelFn := range cl.watchers {
		cancelFn()
		delete(cl.watchers, id)
	}
	cl.mu.Unlock()
}

// splitScreenLines splits screen text on newlines and strips trailing blank lines.
func splitScreenLines(text string) []string {
	lines := strings.Split(text, "\n")
	// Strip trailing blank lines.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// encodeEventData JSON-marshals a value and base64-encodes the result.
// The RN client decodes with: JSON.parse(atob(msg.data_base64))
func encodeEventData(v any) string {
	jsonBytes, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(jsonBytes)
}
