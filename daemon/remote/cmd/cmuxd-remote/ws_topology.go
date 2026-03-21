package main

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"
)

// topologyWatcher polls system.tree and pushes changes to connected clients.
type topologyWatcher struct {
	cmuxSocket string
	mu         sync.RWMutex
	clients    map[*wsClient]struct{}
	lastJSON   []byte // raw JSON of last topology for dedup
}

// newTopologyWatcher creates a topology watcher and starts the poll loop.
func newTopologyWatcher(ctx context.Context, cmuxSocket string) *topologyWatcher {
	tw := &topologyWatcher{
		cmuxSocket: cmuxSocket,
		clients:    make(map[*wsClient]struct{}),
	}
	go tw.run(ctx)
	return tw
}

// run polls system.tree at 2s intervals, diffs via bytes.Equal, and broadcasts
// topology events when the tree changes.
func (tw *topologyWatcher) run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := cmuxTopology(tw.cmuxSocket)
			if err != nil {
				continue // retry next tick
			}

			tw.mu.RLock()
			same := bytes.Equal(result, tw.lastJSON)
			tw.mu.RUnlock()
			if same {
				continue // no change
			}

			tw.mu.Lock()
			tw.lastJSON = result
			tw.mu.Unlock()

			// Extract workspaces from the tree result.
			var tree struct {
				Windows []struct {
					Workspaces json.RawMessage `json:"workspaces"`
				} `json:"windows"`
			}
			if err := json.Unmarshal(result, &tree); err != nil {
				continue
			}
			if len(tree.Windows) == 0 {
				continue
			}
			workspaces := tree.Windows[0].Workspaces

			tw.broadcast(ctx, rpcEvent{
				Event:      "topology",
				DataBase64: encodeEventData(map[string]any{"workspaces": workspaces}),
			})
		}
	}
}

// addClient registers a client for topology events.
func (tw *topologyWatcher) addClient(cl *wsClient) {
	tw.mu.Lock()
	tw.clients[cl] = struct{}{}
	tw.mu.Unlock()
}

// removeClient unregisters a client from topology events.
func (tw *topologyWatcher) removeClient(cl *wsClient) {
	tw.mu.Lock()
	delete(tw.clients, cl)
	tw.mu.Unlock()
}

// broadcast sends an event to all registered clients.
func (tw *topologyWatcher) broadcast(ctx context.Context, evt rpcEvent) {
	tw.mu.RLock()
	clients := make([]*wsClient, 0, len(tw.clients))
	for cl := range tw.clients {
		clients = append(clients, cl)
	}
	tw.mu.RUnlock()

	for _, cl := range clients {
		_ = cl.sendEventFn(ctx, evt)
	}
}
