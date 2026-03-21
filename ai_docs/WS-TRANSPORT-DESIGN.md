# WebSocket Transport Layer Design — cmuxd-remote

> Design spec for 5 new Go files adding WebSocket transport to cmuxd-remote.
> Enables spectralMimux React Native app to connect over Tailscale for
> real-time terminal monitoring.

---

## 0. Critical Protocol Mismatches Resolved

The implementation plan (doc 26, Epic 3) uses method/event names that
**do not match** the actual RN client. This design corrects them.

| Aspect | Plan (WRONG) | RN Client (CORRECT) | Resolution |
|--------|-------------|---------------------|------------|
| Topology request | `cmux.topology` | `system.tree` | Use `system.tree` |
| Subscribe | `cmux.subscribe` | `surface.subscribe` | Use `surface.subscribe` |
| Unsubscribe | `cmux.unsubscribe` | `surface.unsubscribe` | Use `surface.unsubscribe` |
| Send text | `cmux.send_text` | `surface.input` | Use `surface.input` |
| Send key | `cmux.send_key` | `surface.keys` | Use `surface.keys` |
| Init screen event | `cmux.screen` | `screen_init` | Use `screen_init` |
| Diff event | `cmux.output` | `screen_diff` | Use `screen_diff` |
| Topology event | *(not defined)* | `topology` | Use `topology` |
| DataBase64 encoding | Raw JSON string | `base64(JSON)` | Use proper base64 |
| Auth response ID | `req.ID` (could be set) | `!msg.id` (must be absent) | Omit ID field |

**Source of truth:** `protocol.ts`, `ws-client.ts`, `useConnection.ts`, `store.ts`
in the RN app. The Go server MUST match what the client sends and expects.

---

## 1. File-by-File Specification

### 1.1 ws_server.go (~130 LOC)

**Purpose:** WebSocket accept, token auth, per-client read loop, dispatch.

**Dependency:** `github.com/coder/websocket` v1.8.14

#### Structs

```go
// wsClient represents a connected WebSocket client.
type wsClient struct {
    conn       *websocket.Conn
    cmuxSocket string
    ctx        context.Context
    cancel     context.CancelFunc
    mu         sync.Mutex
    watchers   map[string]context.CancelFunc // surface UUID -> cancel
}
```

#### Functions

| Function | Signature | Purpose |
|----------|-----------|---------|
| `runWSServer` | `(addr, cmuxSocket, authToken string) error` | HTTP server with `/ws` and `/health` endpoints |
| `handleWSConnection` | `(w, r, cmuxSocket, authToken string)` | Accept WS, auth gate, read loop |
| `(*wsClient).authenticate` | `(ctx, expectedToken string) bool` | Read first message, validate token |
| `(*wsClient).readLoop` | `(ctx)` | Parse messages, dispatch to handlers |
| `(*wsClient).dispatch` | `(ctx, req rpcRequest)` | Route method to sync handler or subscription handler |
| `(*wsClient).sendResponse` | `(ctx, resp rpcResponse)` | Marshal + write response frame |
| `(*wsClient).sendEvent` | `(ctx, evt rpcEvent)` | Marshal + write event frame |
| `(*wsClient).sendError` | `(ctx, id, code, message string)` | Convenience error sender |
| `(*wsClient).cleanup` | `()` | Cancel all watchers, close connection |

#### Auth Flow

```
Client connects to ws://<addr>/ws
  │
  ├─ Client sends: {"method":"auth","params":{"token":"<token>"}}
  │  (NO id field — RpcRequest.id is undefined)
  │
  ├─ Go reads first message, checks method == "auth"
  │  extracts token from params, compares to --token flag
  │
  ├─ SUCCESS → Go sends: {"ok":true,"result":{"authenticated":true}}
  │  (NO id field — omitempty on ID drops it)
  │  Client checks: isResponse(msg) && !msg.id && msg.ok && result.authenticated
  │  → onConnectionChange(true) fires
  │
  └─ FAILURE → Go sends: {"ok":false,"error":{"code":"auth_failed","message":"..."}}
     → Client falls through (no authenticated flag), connection closes
```

**CRITICAL:** The auth response MUST NOT include an `id` field. The Go
`rpcResponse` struct uses `json:"id,omitempty"` — when `req.ID` is nil
(auth message has no id), the field is omitted. This matches the client
check `!msg.id`.

#### WebSocket Configuration

```go
websocket.Accept(w, r, &websocket.AcceptOptions{
    InsecureSkipVerify: true,        // Tailscale provides encryption
    CompressionMode:    websocket.CompressionContextTakeover,
})
conn.SetReadLimit(65536)             // 64KB (default 32KB too small)
```

- `Conn.Write()` IS goroutine-safe (confirmed in doc 29 DC-3)
- `Conn.Read()` is NOT concurrent-safe — one reader per client (the read loop)
- Use `context.Background()` for WS lifetime, NOT `r.Context()`

#### Read Loop Dispatch

```
readLoop:
  for {
    _, data, err := conn.Read(ctx)
    parse as rpcRequest
    switch req.Method:
      "system.tree"         → handleSyncRequest → sendResponse
      "surface.input"       → handleSyncRequest → sendResponse
      "surface.keys"        → handleSyncRequest → sendResponse
      "surface.subscribe"   → handleSubscribe (spawns goroutine)
      "surface.unsubscribe" → handleUnsubscribe (cancels goroutine)
      "gt.status"           → handleSyncRequest → sendResponse
      "gt.command"          → handleSyncRequest → sendResponse
      "ping"                → sendResponse({ok:true, result:{pong:true}})
      default               → sendError(method_not_found)
  }
```

**Design decision:** The WS server does NOT share `rpcServer` state with
the stdio transport. Our methods call `cmuxCall()` directly (from ws_cmux_proxy.go).
This keeps transports isolated per doc 29 finding AF-2.

---

### 1.2 ws_cmux_proxy.go (~150 LOC)

**Purpose:** Proxy requests to cmux.sock via direct Unix socket dial.
Does NOT reuse `socketRoundTripV2` from cli.go — instead uses `cmuxCall()`
which returns `json.RawMessage` (avoids double marshal/unmarshal per doc 33).

#### Functions

| Function | Signature | Returns | Purpose |
|----------|-----------|---------|---------|
| `findCmuxSocket` | `() string` | socket path | Auto-discover cmux.sock: env → file → defaults |
| `cmuxCall` | `(socketPath, method string, params map[string]any) (json.RawMessage, error)` | raw result | Send JSON-RPC v2 to cmux.sock with 5s deadline |
| `cmuxTopology` | `(socketPath string) (json.RawMessage, error)` | tree JSON | Call `system.tree` with `all_windows: true` |
| `cmuxReadScreen` | `(socketPath, surfaceID string, lines int) (string, string, error)` | text, resolvedID | Call `surface.read_text`, validate response surface |
| `cmuxSendText` | `(socketPath, surfaceID, text string) error` | — | Call `surface.send_text` |
| `cmuxSendKey` | `(socketPath, surfaceID, key string) error` | — | Call `surface.send_key` |
| `handleProxiedRequest` | `(cmuxSocket string, req rpcRequest) rpcResponse` | response | Route sync methods to cmux.sock |

#### cmuxCall Implementation

```go
func cmuxCall(socketPath, method string, params map[string]any) (json.RawMessage, error) {
    conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
    // Set 5s total deadline (per doc 29 AF-1 — no timeout in socketRoundTripV2)
    conn.SetDeadline(time.Now().Add(5 * time.Second))
    // Send JSON-RPC request with \n terminator
    // Read response (up to 512KB)
    // Parse, check ok, return result as RawMessage
    // Always read full response before Close() (per doc 29 CF-2 — SIGPIPE)
}
```

**Why not reuse socketRoundTripV2:**
1. `socketRoundTripV2` returns `string` (re-serialized JSON) — forces double unmarshal
2. Has no read timeout (doc 29 AF-1)
3. Includes relay auth logic we don't need (local Unix socket)
4. `cmuxCall` returns `json.RawMessage` — one unmarshal cycle

#### Socket Discovery Order

```
1. --cmux-socket flag (if provided and not "auto")
2. CMUX_SOCKET_PATH env
3. CMUX_SOCKET env
4. ~/.cmux/socket_addr file (readSocketAddrFile from cli.go)
5. /tmp/cmux.sock → /tmp/cmux-debug.sock → /tmp/cmux-nightly.sock
6. ~/Library/Application Support/cmux/cmux.sock
```

#### handleProxiedRequest Dispatch

```go
func handleProxiedRequest(cmuxSocket string, req rpcRequest) rpcResponse {
    switch req.Method {
    case "system.tree":
        result, err := cmuxTopology(cmuxSocket)
        // Return raw JSON result (TreeResult shape) directly

    case "surface.input":
        surfaceID := params["surface_id"]
        text := params["text"]
        cmuxSendText(cmuxSocket, surfaceID, text)

    case "surface.keys":
        surfaceID := params["surface_id"]
        key := params["key"]
        cmuxSendKey(cmuxSocket, surfaceID, key)

    case "ping":
        return {ok: true, result: {pong: true}}

    default:
        return {ok: false, error: method_not_found}
    }
}
```

#### system.tree Response Verification

The RN client expects `TreeResult` shape from `system.tree`:

```typescript
// protocol.ts TreeResult
{
  active: { surface_id, workspace_id } | null,
  windows: [{
    id: string,
    ref: string,
    workspaces: [{
      id, ref, index, title, selected,
      panes: [{
        id, ref, index, focused,
        surfaces: [{
          id, ref, index, type, title, focused, pane_id, pane_ref
        }]
      }]
    }]
  }]
}
```

cmux.sock `system.tree` returns this exact shape (confirmed doc 29 CF-4).
The proxy passes it through as `json.RawMessage` — **zero transformation needed**.

The client unpacks it at `useConnection.ts:43`:
```js
const workspaces = tree.windows?.[0]?.workspaces ?? [];
```

---

### 1.3 ws_stream.go (~150 LOC)

**Purpose:** Per-client surface output streaming via poll + diff + push.

#### Functions

| Function | Signature | Purpose |
|----------|-----------|---------|
| `(*wsClient).handleSubscribe` | `(ctx, req)` | Start/restart watcher for surface |
| `(*wsClient).handleUnsubscribe` | `(ctx, req)` | Stop watcher for surface |
| `watchSurface` | `(ctx, cl *wsClient, surfaceID string)` | Poll loop: read → diff → push |
| `splitScreenLines` | `(text string) []string` | Split text on \n, strip trailing blanks |
| `encodeEventData` | `(v any) string` | JSON marshal → base64 encode |

#### watchSurface Lifecycle

```
handleSubscribe("surface.subscribe", {surface_id: "<UUID>"})
  │
  ├─ Lock mu, cancel existing watcher if any
  ├─ Create watchCtx (child of client ctx)
  ├─ Store cancel in cl.watchers[surfaceID]
  ├─ Send response: {ok: true, result: {subscribed: surfaceID}}
  │
  └─ go watchSurface(watchCtx, cl, surfaceID)
       │
       ├─ INITIAL: cmuxReadScreen(surfaceID, 80)
       │  ├─ Store as prevLines
       │  └─ Push event:
       │     {
       │       "event": "screen_init",
       │       "stream_id": "<surfaceID>",
       │       "data_base64": base64({"lines": ["line1", "line2", ...]})
       │     }
       │
       └─ POLL LOOP (every 500ms):
          select {
          case <-ctx.Done(): return
          case <-ticker.C:
            currText := cmuxReadScreen(surfaceID, 80)
            currLines := splitScreenLines(currText)
            changes := diffLines(prevLines, currLines)
            if len(changes) > 0:
              prevLines = currLines
              Push event:
              {
                "event": "screen_diff",
                "stream_id": "<surfaceID>",
                "data_base64": base64({"changes": [...]})
              }
          }
```

#### Event Data Encoding

**CRITICAL MISMATCH FIXED:** The implementation plan puts raw JSON in
`data_base64`. The client decodes with `atob()` (base64 decode). We MUST
base64-encode the JSON payload.

```go
func encodeEventData(v any) string {
    jsonBytes, _ := json.Marshal(v)
    return base64.StdEncoding.EncodeToString(jsonBytes)
}
```

Client decode path (useConnection.ts:83):
```js
const data = msg.data_base64 ? JSON.parse(atob(msg.data_base64)) : null;
```

#### Event → Store Mapping

| Go Event | data_base64 payload | Client handler | Store method |
|----------|-------------------|----------------|--------------|
| `screen_init` | `{lines: string[]}` | `handleMessage` case `screen_init` | `store.setScreen(stream_id, data.lines)` |
| `screen_diff` | `{changes: lineChange[]}` | `handleMessage` case `screen_diff` | `store.applyChanges(stream_id, data.changes)` |
| `topology` | `{workspaces: Workspace[]}` | `handleMessage` case `topology` | `store.setTopology(data.workspaces)` |

The `stream_id` field maps to the surface UUID. The client uses
`msg.stream_id` as the key into `store.screens`.

#### lineChange Shape Verification

Go struct (ws_diff.go):
```go
type lineChange struct {
    Op    string `json:"op"`
    Line  int    `json:"line,omitempty"`
    Text  string `json:"text,omitempty"`
    Count int    `json:"count,omitempty"`
}
```

Store expectation (store.ts:35):
```typescript
Array<{op: string; line?: number; text?: string; count?: number}>
```

**Match confirmed.** Operations: `replace` (line + text), `append` (text only),
`delete` (line + count).

#### Polling Interval

- Active polling: **500ms** (2 reads/sec per surface)
- Concurrent reads are fast: 3 concurrent = 9ms total (doc 33)
- At 6 surfaces × 2 reads/sec = 12 reads/sec — trivial for cmux (doc 29 CF-6)
- No adaptive polling in v1 — keep simple

#### Error Handling

- `cmuxReadScreen` returns error → skip this tick, retry next
- After 10 consecutive errors → push error event, stop watcher
- Context cancelled → clean exit (client disconnect or unsubscribe)
- Surface closed in cmux → `not_found` error → push close event, stop watcher

---

### 1.4 ws_topology.go (~80 LOC)

**Purpose:** Periodic topology polling + change detection + push to all clients.

#### Structs

```go
// topologyWatcher polls system.tree and pushes changes to connected clients.
type topologyWatcher struct {
    cmuxSocket string
    mu         sync.RWMutex
    clients    map[*wsClient]struct{}
    lastJSON   []byte // raw JSON of last topology for dedup
}
```

#### Functions

| Function | Signature | Purpose |
|----------|-----------|---------|
| `newTopologyWatcher` | `(cmuxSocket string) *topologyWatcher` | Create and start watcher |
| `(*topologyWatcher).run` | `(ctx)` | Poll loop: system.tree → diff → broadcast |
| `(*topologyWatcher).addClient` | `(cl *wsClient)` | Register client for topology events |
| `(*topologyWatcher).removeClient` | `(cl *wsClient)` | Unregister client |
| `(*topologyWatcher).broadcast` | `(ctx, evt rpcEvent)` | Send event to all registered clients |

#### Poll Loop

```
run(ctx):
  ticker := time.NewTicker(2 * time.Second)
  for {
    select {
    case <-ctx.Done(): return
    case <-ticker.C:
      result, err := cmuxTopology(cmuxSocket)
      if err: continue (retry next tick)
      if bytes.Equal(result, lastJSON): continue (no change)
      lastJSON = result

      // Extract workspaces from the tree result
      var tree struct {
          Windows []struct {
              Workspaces json.RawMessage `json:"workspaces"`
          } `json:"windows"`
      }
      json.Unmarshal(result, &tree)
      workspaces := tree.Windows[0].Workspaces // first window

      broadcast event:
      {
        "event": "topology",
        "data_base64": base64({"workspaces": <workspaces>})
      }
  }
```

**Polling interval:** 2 seconds. Topology changes (workspace create/close,
surface add/remove) are infrequent. 2s is responsive enough for mobile UX.

**Dedup:** Compare raw JSON bytes. Only broadcast when topology actually changes.
This avoids unnecessary re-renders on the client.

#### Client Lifecycle Integration

- `handleWSConnection` → after auth → `topologyWatcher.addClient(cl)`
- `(*wsClient).cleanup` → `topologyWatcher.removeClient(cl)`

---

### 1.5 ws_docker.go (~100 LOC)

**Purpose:** Gas Town integration layer. REST + SSE for gt status, docker exec
fallback for gt commands.

#### Structs

```go
// gtBridge provides Gas Town dashboard data and command execution.
type gtBridge struct {
    containerName string // Docker container running Gas Town (default: "gastown")
    apiBase       string // REST API base (default: "http://localhost:19286")
}
```

#### Functions

| Function | Signature | Purpose |
|----------|-----------|---------|
| `newGTBridge` | `() *gtBridge` | Create bridge, detect container |
| `(*gtBridge).status` | `() (json.RawMessage, error)` | GET /api/options → gt_available + agent statuses |
| `(*gtBridge).runCommand` | `(cmd string, args []string) (string, error)` | docker exec gt <cmd> |
| `(*gtBridge).available` | `() bool` | Quick check: is GT running? |
| `handleGTRequest` | `(bridge *gtBridge, req rpcRequest) rpcResponse` | Route gt.status/gt.command |

#### Method Dispatch

```go
case "gt.status":
    result, err := bridge.status()
    // Returns: {gt_available: bool, agents: [...]}

case "gt.command":
    cmd := params["command"].(string)
    args := params["args"].([]string)
    output, err := bridge.runCommand(cmd, args)
    // Returns: {output: string, exit_code: int}
```

#### GT Status Flow

```
gt.status request
  │
  ├─ Try REST: GET http://localhost:19286/api/options
  │  ├─ Success → return {gt_available: true, ...}
  │  └─ Fail ↓
  │
  ├─ Try Docker: docker exec gastown gt status --json
  │  ├─ Success → parse JSON, return
  │  └─ Fail ↓
  │
  └─ Return {gt_available: false}
```

#### GT Command Execution

```
gt.command {command: "mail inbox"}
  │
  ├─ Security: whitelist allowed commands
  │  Allowed: status, mail, hook, mol, rig, crew
  │  Blocked: everything else (prevent arbitrary exec)
  │
  └─ docker exec -i gastown gt <command>
     └─ Return stdout + exit code
```

---

## 2. Protocol Verification Matrix

### 2.1 RPC Types (Go ↔ RN)

| Field | Go struct | Go JSON tag | TS interface | Match? |
|-------|-----------|-------------|-------------|--------|
| **RpcRequest** | | | | |
| id | `ID any` | `json:"id"` | `id?: string` | Yes (optional in both) |
| method | `Method string` | `json:"method"` | `method: string` | Yes |
| params | `Params map[string]any` | `json:"params"` | `params?: Record<string, unknown>` | Yes |
| **RpcResponse** | | | | |
| id | `ID any` | `json:"id,omitempty"` | `id?: string` | Yes (omitempty = optional) |
| ok | `OK bool` | `json:"ok"` | `ok: boolean` | Yes |
| result | `Result any` | `json:"result,omitempty"` | `result?: unknown` | Yes |
| error | `Error *rpcError` | `json:"error,omitempty"` | `error?: {code, message}` | Yes |
| **RpcEvent** | | | | |
| event | `Event string` | `json:"event"` | `event: string` | Yes |
| stream_id | `StreamID string` | `json:"stream_id,omitempty"` | `stream_id?: string` | Yes |
| data_base64 | `DataBase64 string` | `json:"data_base64,omitempty"` | `data_base64?: string` | Yes |
| error | `Error string` | `json:"error,omitempty"` | `error?: string` | Yes |

### 2.2 Method Names (Go server handles ← RN client sends)

| RN Client sends | Go WS server handles | Proxied to cmux.sock as | Response shape |
|----------------|---------------------|------------------------|----------------|
| `auth` | `authenticate()` — first message only | *(not proxied)* | `{ok, result:{authenticated}}` |
| `system.tree` | `handleProxiedRequest` | `system.tree` | `TreeResult` (passthrough) |
| `surface.subscribe` | `handleSubscribe` | *(starts watcher)* | `{ok, result:{subscribed}}` |
| `surface.input` | `handleProxiedRequest` | `surface.send_text` | `{ok, result:{sent}}` |
| `surface.keys` | `handleProxiedRequest` | `surface.send_key` | `{ok, result:{sent}}` |
| `ping` | `handleProxiedRequest` | *(local)* | `{ok, result:{pong}}` |

**Note:** `surface.input` maps to cmux's `surface.send_text` and `surface.keys`
maps to cmux's `surface.send_key`. The WS server translates method names.

### 2.3 Event Names (Go server pushes → RN client handles)

| Go pushes event | stream_id | data_base64 decoded payload | RN handler (useConnection.ts) |
|----------------|-----------|---------------------------|-------------------------------|
| `screen_init` | surface UUID | `{lines: string[]}` | `store.setScreen(stream_id, data.lines)` |
| `screen_diff` | surface UUID | `{changes: lineChange[]}` | `store.applyChanges(stream_id, data.changes)` |
| `topology` | *(empty)* | `{workspaces: Workspace[]}` | `store.setTopology(data.workspaces)` |

### 2.4 TreeResult Shape (Go → RN)

| Go field path | JSON key | TS type | Required? |
|--------------|----------|---------|-----------|
| `result.active` | `active` | `{surface_id, workspace_id} \| null` | Yes |
| `result.windows[].id` | `id` | `string` | Yes |
| `result.windows[].ref` | `ref` | `string` | Yes |
| `result.windows[].workspaces[].id` | `id` | `string` (UUID) | Yes |
| `result.windows[].workspaces[].ref` | `ref` | `string` | Yes |
| `result.windows[].workspaces[].index` | `index` | `number` | Yes |
| `result.windows[].workspaces[].title` | `title` | `string` | Yes |
| `result.windows[].workspaces[].selected` | `selected` | `boolean` | Yes |
| `...panes[].id` | `id` | `string` | Yes |
| `...panes[].ref` | `ref` | `string` | Yes |
| `...panes[].index` | `index` | `number` | Yes |
| `...panes[].focused` | `focused` | `boolean` | Yes |
| `...surfaces[].id` | `id` | `string` (UUID) | Yes |
| `...surfaces[].ref` | `ref` | `string` | Yes |
| `...surfaces[].index` | `index` | `number` | Yes |
| `...surfaces[].type` | `type` | `string` | Yes |
| `...surfaces[].title` | `title` | `string` | Yes |
| `...surfaces[].focused` | `focused` | `boolean` | Yes |
| `...surfaces[].pane_id` | `pane_id` | `string` | Yes |
| `...surfaces[].pane_ref` | `pane_ref` | `string` | Yes |

cmux.sock returns this shape directly. The proxy passes `json.RawMessage`
through — no transformation needed.

---

## 3. Auth Flow Diagram

```
┌─────────────────┐                    ┌──────────────────┐
│  RN App          │                    │  Go WS Server    │
│  (ws-client.ts)  │                    │  (ws_server.go)  │
└────────┬────────┘                    └────────┬─────────┘
         │                                      │
         │  WS connect to ws://<addr>/ws        │
         │─────────────────────────────────────>│
         │                                      │  websocket.Accept()
         │                                      │
         │  onopen fires                        │
         │  send: {"method":"auth",             │
         │         "params":{"token":"abc"}}    │
         │─────────────────────────────────────>│
         │                                      │  authenticate():
         │                                      │  read first message
         │                                      │  check method == "auth"
         │                                      │  compare token
         │                                      │
         │  recv: {"ok":true,                   │  (no "id" field!)
         │         "result":{                   │
         │           "authenticated":true,      │
         │           "version":"..."}}          │
         │<─────────────────────────────────────│
         │                                      │
         │  ws-client checks:                   │
         │  isResponse && !msg.id &&            │
         │  msg.ok && result.authenticated      │
         │  → onConnectionChange(true)          │
         │                                      │
         │  send: {"id":"1",                    │  → readLoop dispatches
         │         "method":"system.tree"}      │
         │─────────────────────────────────────>│
         │                                      │
```

---

## 4. Data Flow Diagrams

### 4.1 Synchronous Request (system.tree)

```
RN App                    Go WS Server              cmux.sock (Swift)
  │                           │                          │
  │ {"id":"1",                │                          │
  │  "method":"system.tree"}  │                          │
  │──────────────────────────>│                          │
  │                           │                          │
  │                           │ cmuxCall("system.tree",  │
  │                           │   {all_windows: true})   │
  │                           │─────────────────────────>│
  │                           │                          │ DispatchQueue.main.sync
  │                           │                          │ build tree from windows
  │                           │ raw result (RawMessage)  │
  │                           │<─────────────────────────│
  │                           │                          │
  │ {"id":"1","ok":true,      │                          │
  │  "result":{windows:[...]}}│                          │
  │<──────────────────────────│                          │
```

### 4.2 Surface Subscription + Streaming

```
RN App                    Go WS Server              cmux.sock
  │                           │                        │
  │ surface.subscribe         │                        │
  │ {surface_id: "UUID-1"}    │                        │
  │──────────────────────────>│                        │
  │                           │ spawn watchSurface()   │
  │ {ok, subscribed: UUID-1}  │                        │
  │<──────────────────────────│                        │
  │                           │                        │
  │                           │ INITIAL READ:          │
  │                           │ surface.read_text      │
  │                           │ {surface_id: "UUID-1"} │
  │                           │───────────────────────>│
  │                           │ {text: "...",          │
  │                           │  surface_id: "UUID-1"} │
  │                           │<───────────────────────│
  │                           │                        │
  │ {event: "screen_init",    │ split → lines[]        │
  │  stream_id: "UUID-1",    │ base64({lines:[...]})  │
  │  data_base64: "eyJ..."}  │                        │
  │<──────────────────────────│                        │
  │                           │                        │
  │ atob → JSON.parse         │                        │
  │ store.setScreen(          │                        │
  │   "UUID-1", lines)        │                        │
  │                           │                        │
  │                           │ POLL (every 500ms):    │
  │                           │ surface.read_text      │
  │                           │───────────────────────>│
  │                           │<───────────────────────│
  │                           │                        │
  │                           │ diffLines(prev, curr)  │
  │                           │ changes found?         │
  │                           │                        │
  │ {event: "screen_diff",    │ YES: push diff         │
  │  stream_id: "UUID-1",    │                        │
  │  data_base64: "eyJ..."}  │                        │
  │<──────────────────────────│                        │
  │                           │                        │
  │ store.applyChanges(       │                        │
  │   "UUID-1", changes)      │                        │
  │ store.setActivity(        │                        │
  │   "UUID-1", true)         │                        │
```

### 4.3 Topology Push (Background)

```
topologyWatcher                cmux.sock
  │                               │
  │ (every 2s)                    │
  │ cmuxTopology()                │
  │──────────────────────────────>│
  │ {windows:[{workspaces:...}]}  │
  │<──────────────────────────────│
  │                               │
  │ bytes.Equal(result, last)?    │
  │ NO → changed!                 │
  │                               │
  │ broadcast to all clients:     │
  │ {event: "topology",           │
  │  data_base64: base64(         │
  │    {workspaces: [...]})}      │
  │                               │
  → client: store.setTopology()   │
```

---

## 5. Polling Intervals & Resource Management

### Goroutine Budget (per client)

| Goroutine | Count | Lifetime | Purpose |
|-----------|-------|----------|---------|
| Read loop | 1 | Client connection | Parse incoming messages |
| Surface watcher | 1 per subscribed surface | Until unsubscribe/disconnect | Poll + diff + push |

**Typical client:** 1 read loop + 3-6 watchers = 4-7 goroutines.

### Shared Goroutines (server-wide)

| Goroutine | Count | Lifetime | Purpose |
|-----------|-------|----------|---------|
| Topology watcher | 1 | Server lifetime | Poll system.tree, broadcast changes |
| HTTP server | pool | Server lifetime | Accept connections |

### Polling Rates

| What | Interval | cmux.sock calls/sec | Notes |
|------|----------|-------------------|-------|
| Surface content | 500ms | 2/surface | Only for subscribed surfaces |
| Topology | 2s | 0.5 | Shared across all clients, deduped |

**Worst case** (1 client, 6 surfaces): 12 + 0.5 = 12.5 cmux.sock calls/sec.
cmux handles this trivially (doc 33: 3 concurrent reads = 9ms).

### Goroutine Lifecycle

```
Client connects → auth → addClient to topologyWatcher
  → client sends surface.subscribe → spawn watchSurface goroutine
  → client sends surface.unsubscribe → cancel watcher context
  → client disconnects (read error) → cleanup():
      cancel all watcher contexts (ctx.Done() fires)
      removeClient from topologyWatcher
      conn.CloseNow()
```

All goroutines use `context.Context` for cancellation. Parent context
is per-client. Cancelling client context cascades to all watchers.

---

## 6. Error Handling Strategy

### Connection Errors

| Error | Response | Recovery |
|-------|----------|----------|
| cmux.sock not found | `{ok:false, error:{code:"cmux_unavailable"}}` | Client shows "cmux not running" |
| cmux.sock timeout (5s) | `{ok:false, error:{code:"cmux_timeout"}}` | Client retries on next request |
| Invalid JSON from client | `{ok:false, error:{code:"parse_error"}}` | Continue read loop |
| Unknown method | `{ok:false, error:{code:"method_not_found"}}` | Continue read loop |
| Auth failure | `{ok:false, error:{code:"auth_failed"}}` | Close connection |

### Streaming Errors

| Error | Behavior |
|-------|----------|
| cmuxReadScreen returns error | Skip this tick, retry next poll |
| 10 consecutive errors | Push `{event:"screen_diff", error:"surface unavailable"}`, stop watcher |
| Surface not found (closed) | Push error event, stop watcher, client auto-removes from UI on next topology |
| Surface ref mismatch (silent fallback) | Use UUIDs from system.tree, not short refs (doc 33 recommendation) |
| Client disconnect during push | `conn.Write` returns error → watcher sees `ctx.Done()` → exits |

### SIGPIPE Prevention (doc 29 CF-2)

`cmuxCall()` ALWAYS reads the full response before closing the connection.
Never abandon a connection mid-read. The `defer conn.Close()` pattern is safe
because it runs after the response has been fully read.

---

## 7. Dependency on Existing Code

### From main.go (reuse as-is)

| Item | Usage |
|------|-------|
| `rpcRequest` struct | Message parsing in WS read loop |
| `rpcResponse` struct | Response construction |
| `rpcEvent` struct | Event construction |
| `rpcError` struct | Error responses |
| `getStringParam()` | Extract string params from request |
| `getIntParam()` | Extract int params from request |
| `version` variable | Include in auth response |

### From cli.go (reuse as-is)

| Item | Usage |
|------|-------|
| `readSocketAddrFile()` | Socket discovery fallback in `findCmuxSocket()` |
| `randomHex()` | Generate request IDs for cmux.sock calls |

### NOT reused

| Item | Why |
|------|-----|
| `socketRoundTripV2()` | Returns string, no timeout, has relay auth logic. We use `cmuxCall()` instead. |
| `rpcServer` struct | WS transport manages its own state. No shared state with stdio. |
| `stdioFrameWriter` | WS uses `websocket.Conn.Write()` directly. |
| `streamPump()` | For proxy.stream.*, not relevant to our cmux.sock polling. |

---

## 8. Modifications to main.go

### 8.1 New Flags on `serve` Command

**Current** (lines 122-143):
```go
case "serve":
    fs := flag.NewFlagSet("serve", flag.ContinueOnError)
    fs.SetOutput(stderr)
    stdio := fs.Bool("stdio", false, "serve over stdin/stdout")
    if err := fs.Parse(args[1:]); err != nil { return 2 }
    if !*stdio { ... }
    return runStdioServer(stdin, stdout)
```

**Modified:**
```go
case "serve":
    fs := flag.NewFlagSet("serve", flag.ContinueOnError)
    fs.SetOutput(stderr)
    stdio := fs.Bool("stdio", false, "serve over stdin/stdout")
    listen := fs.String("listen", "", "WebSocket listen address (e.g. :19285)")
    cmuxSocket := fs.String("cmux-socket", "", "path to cmux.sock (auto-detect if empty)")
    authToken := fs.String("token", "", "auth token for WebSocket clients")
    if err := fs.Parse(args[1:]); err != nil { return 2 }

    if !*stdio && *listen == "" {
        fmt.Fprintln(stderr, "serve requires --stdio and/or --listen")
        return 2
    }

    socketPath := *cmuxSocket
    if socketPath == "" || socketPath == "auto" {
        socketPath = findCmuxSocket()
    }

    if *listen != "" {
        go func() {
            if err := runWSServer(*listen, socketPath, *authToken); err != nil {
                fmt.Fprintf(stderr, "WebSocket server failed: %v\n", err)
            }
        }()
    }

    if *stdio {
        if err := runStdioServer(stdin, stdout); err != nil {
            fmt.Fprintf(stderr, "serve failed: %v\n", err)
            return 1
        }
        return 0
    }

    // WebSocket-only mode: block until signal
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    <-sigCh
    return 0
```

### 8.2 New Imports in main.go

```go
import (
    // ... existing imports ...
    "os/signal"
    "syscall"
)
```

### 8.3 NO New handleRequest Cases

The WS server does NOT add cases to `handleRequest()`. It has its own
dispatch in `ws_server.go`. The two transports are independent:

- **stdio transport:** `runStdioServer` → `handleRequest()` → proxy/session methods
- **WS transport:** `runWSServer` → `handleWSConnection` → `handleProxiedRequest()` → cmux.sock

This keeps the existing contract unchanged. `handleRequest()` signature
(`func (s *rpcServer) handleRequest(req rpcRequest) rpcResponse`) is
synchronous and tied to `rpcServer` state — the WS transport doesn't
need any of it.

### 8.4 Usage String Update

```go
func usage(w io.Writer) {
    fmt.Fprintln(w, "Usage:")
    fmt.Fprintln(w, "  cmuxd-remote version")
    fmt.Fprintln(w, "  cmuxd-remote serve --stdio")
    fmt.Fprintln(w, "  cmuxd-remote serve --listen :19285 [--cmux-socket path] [--token secret]")
    fmt.Fprintln(w, "  cmuxd-remote serve --stdio --listen :19285 [--cmux-socket path] [--token secret]")
    fmt.Fprintln(w, "  cmuxd-remote cli <command> [args...]")
}
```

---

## 9. go.mod Change

```
require github.com/coder/websocket v1.8.14
```

Single new dependency. No transitive dependencies — `coder/websocket`
is a pure Go library.

---

## 10. Launch Command

```bash
cmuxd-remote serve \
    --listen $(tailscale ip -4):19285 \
    --cmux-socket auto \
    --token $(openssl rand -hex 16)
```

Or alongside stdio (dual transport):
```bash
cmuxd-remote serve \
    --stdio \
    --listen :19285 \
    --token mytoken
```

---

## 11. LOC Summary

| File | Est. LOC | New/Modify |
|------|----------|------------|
| `ws_server.go` | ~130 | CREATE |
| `ws_cmux_proxy.go` | ~150 | CREATE |
| `ws_stream.go` | ~150 | CREATE |
| `ws_topology.go` | ~80 | CREATE |
| `ws_docker.go` | ~100 | CREATE |
| `main.go` | ~25 lines changed | MODIFY |
| `go.mod` / `go.sum` | ~2 lines | MODIFY |
| **Total new** | **~610** | |
