# WebSocket Transport Layer — Test & Review Plan

> Comprehensive test battery for the 5 new Go files added by WS-TRANSPORT-DESIGN.md.
> Covers unit tests per file, integration test, protocol compliance, and code review checklist.

---

## 1. UNIT TESTS (per file)

### 1.1 ws_cmux_proxy_test.go

Tests for the cmux.sock proxy layer — socket discovery, raw JSON-RPC calls, and request dispatch.

#### findCmuxSocket()

| Test | Setup | Assert |
|------|-------|--------|
| `TestFindCmuxSocket_FlagOverride` | Set `--cmux-socket /custom/path` | Returns `/custom/path` |
| `TestFindCmuxSocket_EnvCMUX_SOCKET_PATH` | `t.Setenv("CMUX_SOCKET_PATH", "/env/path")` | Returns `/env/path` |
| `TestFindCmuxSocket_EnvCMUX_SOCKET` | `t.Setenv("CMUX_SOCKET", "/env2/path")` | Returns `/env2/path` |
| `TestFindCmuxSocket_SocketAddrFile` | Write `~/.cmux/socket_addr` with path to a real temp socket | Returns that path |
| `TestFindCmuxSocket_DefaultPaths` | Create `/tmp/cmux.sock` (temp Unix listener) | Returns `/tmp/cmux.sock` |
| `TestFindCmuxSocket_FallbackOrder` | Set both `CMUX_SOCKET_PATH` and `CMUX_SOCKET`; only env1 is valid | Returns `CMUX_SOCKET_PATH` (higher priority) |
| `TestFindCmuxSocket_NoneAvailable` | Clear all env vars, no files exist | Returns `""` |
| `TestFindCmuxSocket_AutoKeyword` | Pass `"auto"` as flag value | Ignores flag, falls through to env/defaults |

#### cmuxCall()

| Test | Setup | Assert |
|------|-------|--------|
| `TestCmuxCall_SuccessResponse` | Mock Unix socket returns `{"id":"x","ok":true,"result":{"pong":true}}` | Returns `json.RawMessage` with `{"pong":true}`, nil error |
| `TestCmuxCall_ErrorResponse` | Mock returns `{"ok":false,"error":{"code":"not_found","message":"..."}}` | Returns nil result, error containing "not_found" |
| `TestCmuxCall_InvalidJSON` | Mock returns `not json\n` | Returns error (unmarshal failure) |
| `TestCmuxCall_SocketNotFound` | Use non-existent socket path | Returns error with "dial" or "connect" |
| `TestCmuxCall_TimeoutEnforced` | Mock socket accepts but never responds (sleep > 5s) | Returns error within ~5s (deadline exceeded), NOT hang forever |
| `TestCmuxCall_LargeResponse` | Mock returns 400KB JSON body | Succeeds (under 512KB limit) |
| `TestCmuxCall_OversizedResponse` | Mock returns >512KB | Returns error or truncation |
| `TestCmuxCall_ReadsFullResponseBeforeClose` | Mock sends response slowly (2 chunks) | Full response received (SIGPIPE prevention per doc 29 CF-2) |

**Implementation pattern:**
```go
func TestCmuxCall_SuccessResponse(t *testing.T) {
    sockPath := makeShortUnixSocketPath(t)
    ln, _ := net.Listen("unix", sockPath)
    t.Cleanup(func() { ln.Close() })
    go func() {
        conn, _ := ln.Accept()
        defer conn.Close()
        buf := make([]byte, 4096)
        n, _ := conn.Read(buf) // read request
        var req map[string]any
        json.Unmarshal(buf[:n], &req)
        resp := fmt.Sprintf(`{"id":%q,"ok":true,"result":{"pong":true}}`, req["id"])
        conn.Write([]byte(resp + "\n"))
    }()
    result, err := cmuxCall(sockPath, "ping", nil)
    // assert err == nil
    // assert json.Unmarshal(result) contains "pong": true
}
```

#### cmuxTopology()

| Test | Setup | Assert |
|------|-------|--------|
| `TestCmuxTopology_ReturnsTreeResult` | Mock returns canned TreeResult JSON | Raw JSON matches expected shape |
| `TestCmuxTopology_PassesAllWindowsParam` | Capture request at mock | `params` contains `"all_windows": true` |

#### cmuxReadScreen()

| Test | Setup | Assert |
|------|-------|--------|
| `TestCmuxReadScreen_ReturnsTextAndSurfaceID` | Mock returns `{"text":"line1\nline2","surface_id":"uuid-1"}` | text = `"line1\nline2"`, resolvedID = `"uuid-1"` |
| `TestCmuxReadScreen_PassesLinesParam` | Capture request at mock | `params.lines` = 80 |
| `TestCmuxReadScreen_ErrorFromSocket` | Mock returns error response | Returns empty text, error |

#### cmuxSendText() / cmuxSendKey()

| Test | Setup | Assert |
|------|-------|--------|
| `TestCmuxSendText_Dispatches` | Capture at mock | Method is `"surface.send_text"`, params has `surface_id` and `text` |
| `TestCmuxSendKey_Dispatches` | Capture at mock | Method is `"surface.send_key"`, params has `surface_id` and `key` |

#### handleProxiedRequest()

| Test | Setup | Assert |
|------|-------|--------|
| `TestHandleProxiedRequest_SystemTree` | Mock cmux.sock, call with `method: "system.tree"` | Returns ok=true, result is TreeResult shape |
| `TestHandleProxiedRequest_SurfaceInput` | Call with `method: "surface.input"`, params `{surface_id, text}` | Proxied as `surface.send_text` to cmux.sock |
| `TestHandleProxiedRequest_SurfaceKeys` | Call with `method: "surface.keys"`, params `{surface_id, key}` | Proxied as `surface.send_key` to cmux.sock |
| `TestHandleProxiedRequest_Ping` | Call with `method: "ping"` | Returns `{ok:true, result:{pong:true}}` without hitting cmux.sock |
| `TestHandleProxiedRequest_UnknownMethod` | Call with `method: "does_not_exist"` | Returns `{ok:false, error:{code:"method_not_found"}}` |
| `TestHandleProxiedRequest_CmuxDown` | Use invalid socket path | Returns `{ok:false, error:{code:"cmux_unavailable"}}` |
| `TestHandleProxiedRequest_CmuxTimeout` | Mock socket that hangs | Returns `{ok:false, error:{code:"cmux_timeout"}}` within deadline |

---

### 1.2 ws_stream_test.go

Tests for per-client surface output streaming — watch lifecycle, screen events, diff, encoding.

#### watchSurface — Initial screen_init

| Test | Setup | Assert |
|------|-------|--------|
| `TestWatchSurface_InitialScreenInit` | Mock cmux.sock returns screen text; create wsClient with mock conn; start watchSurface | First message sent to WS conn is `screen_init` event with `stream_id` = surfaceID, `data_base64` decodes to `{"lines": [...]}` |
| `TestWatchSurface_InitialScreenInit_EmptyScreen` | Mock returns empty string | `screen_init` sent with `{"lines": []}` |

**Implementation pattern:**
```go
func TestWatchSurface_InitialScreenInit(t *testing.T) {
    // 1. Start mock cmux.sock that returns canned surface.read_text response
    // 2. Create a wsClient with a pipe-backed or channel-backed WS connection
    //    (use nhooyr websocket test helpers or a real localhost WS server)
    // 3. ctx, cancel := context.WithCancel(context.Background())
    // 4. go watchSurface(ctx, client, "surface-uuid")
    // 5. Read first message from the WS connection
    // 6. Assert: event="screen_init", stream_id="surface-uuid"
    // 7. Decode data_base64, assert lines match mock screen text
    // 8. cancel() // cleanup
}
```

#### watchSurface — Poll and diff cycle

| Test | Setup | Assert |
|------|-------|--------|
| `TestWatchSurface_DiffAfterChange` | Mock returns screen A, then screen B on subsequent reads | After `screen_init`, a `screen_diff` event arrives with changes reflecting A→B diff |
| `TestWatchSurface_NoDiffWhenUnchanged` | Mock returns same screen each time | Only `screen_init` sent; no `screen_diff` events within a reasonable wait |

#### encodeEventData()

| Test | Setup | Assert |
|------|-------|--------|
| `TestEncodeEventData_ValidBase64JSON` | Call with `map[string]any{"lines": []string{"hello"}}` | Result base64-decodes to `{"lines":["hello"]}` |
| `TestEncodeEventData_EmptyStruct` | Call with `map[string]any{}` | Result base64-decodes to `{}` |
| `TestEncodeEventData_NestedPayload` | Call with `{"changes": [{"op":"replace","line":0,"text":"new"}]}` | Correctly round-trips through base64 decode → JSON parse |
| `TestEncodeEventData_UnicodeContent` | Call with lines containing `"日本語"` | Correctly base64-encodes the UTF-8 JSON |

#### splitScreenLines()

| Test | Setup | Assert |
|------|-------|--------|
| `TestSplitScreenLines_Normal` | `"line1\nline2\nline3"` | `["line1", "line2", "line3"]` |
| `TestSplitScreenLines_Empty` | `""` | `[]` (empty slice, not `[""]`) |
| `TestSplitScreenLines_TrailingNewlines` | `"line1\nline2\n\n\n"` | Trailing blank lines stripped: `["line1", "line2"]` |
| `TestSplitScreenLines_SingleLine` | `"just one"` | `["just one"]` |
| `TestSplitScreenLines_AllBlanks` | `"\n\n\n"` | `[]` |
| `TestSplitScreenLines_CRLFHandling` | `"line1\r\nline2\r\n"` | `["line1", "line2"]` (strip `\r`) |

#### Error escalation

| Test | Setup | Assert |
|------|-------|--------|
| `TestWatchSurface_ConsecutiveErrors_StopsAfter10` | Mock cmux.sock fails on every read | After 10 errors, watcher sends error event and stops (goroutine exits) |
| `TestWatchSurface_ErrorRecovery` | Mock fails 3 times, then succeeds | Watcher recovers; screen_diff event sent after recovery |

#### Context cancellation

| Test | Setup | Assert |
|------|-------|--------|
| `TestWatchSurface_ContextCancel_CleansUp` | Start watchSurface, then cancel context | Goroutine exits promptly (< 1s); no more cmux.sock calls after cancel |
| `TestWatchSurface_ClientDisconnect_CleansUp` | Close the WS connection from the client side | Watcher detects write failure, exits cleanly |

---

### 1.3 ws_topology_test.go

Tests for the shared topology poller — dedup, change detection, client management, broadcast.

#### Dedup (no-change suppression)

| Test | Setup | Assert |
|------|-------|--------|
| `TestTopologyWatcher_SameTopology_NoBroadcast` | Mock cmux.sock returns identical `system.tree` twice | Client receives exactly 0 topology events after initial stabilization (or 1 if initial push) |
| `TestTopologyWatcher_TopologyChange_Broadcasts` | Mock returns topology A, then topology B | Client receives topology event with B's workspaces |

#### Client management

| Test | Setup | Assert |
|------|-------|--------|
| `TestTopologyWatcher_AddRemoveClient` | addClient(cl1), addClient(cl2), removeClient(cl1) | cl2 still receives broadcasts; cl1 does not |
| `TestTopologyWatcher_ConcurrentAddRemove` | Spawn 50 goroutines doing add/remove | No data race (run with `-race`) |
| `TestTopologyWatcher_BroadcastToMultiple` | Add 3 clients, trigger topology change | All 3 receive the event |

**Implementation pattern for concurrent test:**
```go
func TestTopologyWatcher_ConcurrentAddRemove(t *testing.T) {
    tw := newTopologyWatcher("/nonexistent") // won't actually poll
    var wg sync.WaitGroup
    for i := 0; i < 50; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            cl := &wsClient{} // minimal stub
            tw.addClient(cl)
            time.Sleep(time.Millisecond)
            tw.removeClient(cl)
        }()
    }
    wg.Wait()
    // If -race doesn't fire, the test passes
}
```

#### Broadcast format

| Test | Setup | Assert |
|------|-------|--------|
| `TestTopologyWatcher_BroadcastFormat` | Trigger change with known topology JSON | Event has `event: "topology"`, `data_base64` decodes to `{"workspaces": [...]}` |
| `TestTopologyWatcher_ExtractsWorkspacesFromTree` | Feed full TreeResult with windows[0].workspaces | data_base64 payload contains `workspaces` array, NOT the full tree |

---

### 1.4 ws_docker_test.go

Tests for the Gas Town integration layer — command whitelist, docker exec, REST fallback.

#### Command whitelist

| Test | Setup | Assert |
|------|-------|--------|
| `TestGTBridge_AllowedCommands` | Call `runCommand("status", nil)`, `runCommand("mail", ["inbox"])`, `runCommand("hook", nil)`, `runCommand("mol", nil)`, `runCommand("rig", nil)`, `runCommand("crew", nil)` | All pass whitelist check (may fail on exec if no Docker, but no rejection error) |
| `TestGTBridge_BlockedCommands` | Call `runCommand("rm", ["-rf", "/"])`, `runCommand("bash", ["-c", "evil"])`, `runCommand("exec", nil)` | All rejected with whitelist/security error before any exec attempt |
| `TestGTBridge_EmptyCommand` | Call `runCommand("", nil)` | Rejected with invalid params |

#### Docker exec construction

| Test | Setup | Assert |
|------|-------|--------|
| `TestGTBridge_DockerExecArgs` | Mock `exec.Command` or test command construction | Command is `docker exec -i <container> gt <cmd> <args>` |
| `TestGTBridge_ContainerNameDefault` | Create `newGTBridge()` without config | `containerName` is `"gastown"` |

#### REST fallback chain

| Test | Setup | Assert |
|------|-------|--------|
| `TestGTBridge_StatusRESTSuccess` | HTTP test server returns `{"agents":[...]}` on `/api/options` | Returns `{gt_available: true, agents: [...]}` without docker exec |
| `TestGTBridge_StatusRESTFail_DockerFallback` | No REST server; mock docker success | Returns result from docker |
| `TestGTBridge_StatusAllFail` | No REST, no Docker | Returns `{gt_available: false}` |

#### handleGTRequest dispatch

| Test | Setup | Assert |
|------|-------|--------|
| `TestHandleGTRequest_Status` | `method: "gt.status"` | Calls `bridge.status()`, returns result |
| `TestHandleGTRequest_Command` | `method: "gt.command"`, params `{command: "mail inbox"}` | Calls `bridge.runCommand`, returns `{output, exit_code}` |
| `TestHandleGTRequest_UnknownMethod` | `method: "gt.unknown"` | Returns `method_not_found` error |

---

### 1.5 ws_server_test.go

Tests for WebSocket accept, auth, read loop, and client lifecycle.

#### Authentication

| Test | Setup | Assert |
|------|-------|--------|
| `TestWSAuth_CorrectToken` | Start WS server with token `"secret123"`; client sends `{"method":"auth","params":{"token":"secret123"}}` | Response: `{"ok":true,"result":{"authenticated":true}}` with **NO `id` field** |
| `TestWSAuth_WrongToken` | Client sends `{"method":"auth","params":{"token":"wrong"}}` | Response: `{"ok":false,"error":{"code":"auth_failed",...}}` + connection closed |
| `TestWSAuth_MissingToken` | Client sends `{"method":"auth","params":{}}` | Response: auth_failed error + connection closed |
| `TestWSAuth_NonAuthFirstMessage` | Client sends `{"id":"1","method":"ping"}` as first message | Connection closed (first message must be auth) |
| `TestWSAuth_NoIDInResponse` | Correct token; inspect raw response bytes | `"id"` key is absent from JSON (omitempty works) |

**CRITICAL verification for auth response shape:**
```go
func TestWSAuth_NoIDInResponse(t *testing.T) {
    // ... start WS server, connect, send auth ...
    _, data, _ := conn.Read(ctx)
    var raw map[string]json.RawMessage
    json.Unmarshal(data, &raw)
    if _, hasID := raw["id"]; hasID {
        t.Fatal("auth response MUST NOT include id field")
    }
}
```

#### Method routing

| Test | Setup | Assert |
|------|-------|--------|
| `TestWSDispatch_SystemTree` | Authenticated client sends `{"id":"1","method":"system.tree"}` | Response routed through handleProxiedRequest, returns TreeResult shape |
| `TestWSDispatch_SurfaceSubscribe` | Send `{"id":"2","method":"surface.subscribe","params":{"surface_id":"uuid"}}` | Response: `{ok:true, result:{subscribed:"uuid"}}`, starts watcher goroutine |
| `TestWSDispatch_SurfaceUnsubscribe` | Subscribe then unsubscribe | Watcher goroutine cancelled |
| `TestWSDispatch_SurfaceInput` | Send `{"id":"3","method":"surface.input","params":{"surface_id":"uuid","text":"ls\n"}}` | Proxied to cmux.sock as `surface.send_text` |
| `TestWSDispatch_SurfaceKeys` | Send `{"id":"4","method":"surface.keys","params":{"surface_id":"uuid","key":"ctrl+c"}}` | Proxied to cmux.sock as `surface.send_key` |
| `TestWSDispatch_GTStatus` | Send `{"id":"5","method":"gt.status"}` | Routed to handleGTRequest |
| `TestWSDispatch_GTCommand` | Send `{"id":"6","method":"gt.command","params":{"command":"status"}}` | Routed to handleGTRequest |
| `TestWSDispatch_Ping` | Send `{"id":"7","method":"ping"}` | Response: `{ok:true, result:{pong:true}}` |

#### Error handling

| Test | Setup | Assert |
|------|-------|--------|
| `TestWSDispatch_UnknownMethod` | Send `{"id":"8","method":"nonexistent"}` | Response: `{ok:false, error:{code:"method_not_found"}}` |
| `TestWSDispatch_InvalidJSON` | Send raw bytes `not json at all` | Response: `{ok:false, error:{code:"parse_error"}}` OR connection remains open |
| `TestWSDispatch_MissingMethod` | Send `{"id":"9"}` | Response: `{ok:false, error:{code:"invalid_request","message":"method is required"}}` |

#### Client cleanup

| Test | Setup | Assert |
|------|-------|--------|
| `TestWSClient_CleanupOnDisconnect` | Subscribe to 3 surfaces, then close WS conn | All 3 watcher goroutines exit (verify with a WaitGroup or channel); client removed from topologyWatcher |
| `TestWSClient_CancelCascadesToWatchers` | Subscribe to a surface, then cancel client context | Watcher goroutine exits; no more cmux.sock polls |

#### WebSocket configuration

| Test | Setup | Assert |
|------|-------|--------|
| `TestWSServer_HealthEndpoint` | GET `/health` | Returns 200 OK |
| `TestWSServer_WSEndpoint` | Connect to `/ws` | WebSocket upgrade succeeds |
| `TestWSServer_ReadLimit` | Send message > 64KB | Connection error (read limit enforced) |

---

## 2. INTEGRATION TEST

### TestWSEndToEnd

Full round-trip test from WS connect through to cmux.sock mock and back.

```
Test sequence:
1. Start mock cmux.sock server (Unix socket)
   - Handles system.tree → returns canned TreeResult
   - Handles surface.read_text → returns canned screen (changes between calls)
   - Handles surface.send_text → records call, returns ok
2. Start real WS server on localhost:0 (random port)
   - Points to mock cmux.sock
   - Auth token = "test-token"
3. Connect WebSocket client (using coder/websocket or gorilla)
4. AUTH: Send {"method":"auth","params":{"token":"test-token"}}
   - Verify: response has ok=true, result.authenticated=true, NO id field
5. TREE: Send {"id":"1","method":"system.tree"}
   - Verify: response has ok=true, result matches TreeResult shape
   - Verify: result.windows[0].workspaces is array
   - Verify: each workspace has id, ref, index, title, selected, panes
   - Verify: each pane has surfaces with id, ref, type, title, etc.
6. SUBSCRIBE: Send {"id":"2","method":"surface.subscribe","params":{"surface_id":"<uuid from tree>"}}
   - Verify: response has ok=true, result.subscribed = surfaceID
   - Wait for screen_init event (timeout 3s)
   - Verify: event="screen_init", stream_id=surfaceID
   - Verify: data_base64 decodes to {"lines": [...]}
7. SCREEN CHANGE: Mock cmux.sock changes surface.read_text response
   - Wait for screen_diff event (timeout 3s)
   - Verify: event="screen_diff", stream_id=surfaceID
   - Verify: data_base64 decodes to {"changes": [{op, line, text, ...}]}
8. INPUT: Send {"id":"3","method":"surface.input","params":{"surface_id":"<uuid>","text":"hello\n"}}
   - Verify: response ok=true
   - Verify: mock cmux.sock received surface.send_text with correct params
9. DISCONNECT: Close WS connection
   - Verify: watcher goroutines exit within 2s
   - Verify: mock cmux.sock stops receiving surface.read_text polls
```

**Implementation sketch:**
```go
func TestWSEndToEnd(t *testing.T) {
    // --- Mock cmux.sock ---
    sockPath := makeShortUnixSocketPath(t)
    screenText := atomic.Value{}
    screenText.Store("$ whoami\nuser\n$ ")

    mockLn, _ := net.Listen("unix", sockPath)
    t.Cleanup(func() { mockLn.Close() })
    go serveMockCmux(mockLn, &screenText) // handles system.tree + surface.read_text

    // --- Real WS server ---
    addr := startTestWSServer(t, sockPath, "test-token")

    // --- WS client ---
    ctx := context.Background()
    conn, _, _ := websocket.Dial(ctx, "ws://"+addr+"/ws", nil)
    defer conn.CloseNow()

    // Step 4: Auth
    writeJSON(conn, map[string]any{"method":"auth","params":map[string]any{"token":"test-token"}})
    authResp := readJSON(conn)
    assertNoIDField(t, authResp)
    assert(t, authResp["ok"] == true)

    // Step 5: system.tree
    writeJSON(conn, map[string]any{"id":"1","method":"system.tree"})
    treeResp := readJSON(conn)
    assertTreeResultShape(t, treeResp)

    // Step 6: surface.subscribe
    surfaceID := extractFirstSurfaceID(treeResp)
    writeJSON(conn, map[string]any{"id":"2","method":"surface.subscribe","params":map[string]any{"surface_id":surfaceID}})
    subResp := readJSON(conn)
    assert(t, subResp["ok"] == true)
    initEvent := readJSONWithTimeout(conn, 3*time.Second)
    assert(t, initEvent["event"] == "screen_init")
    assertBase64DecodesTo(t, initEvent["data_base64"], "lines")

    // Step 7: Screen change
    screenText.Store("$ whoami\nuser\n$ ls\nfile.txt\n$ ")
    diffEvent := readJSONWithTimeout(conn, 3*time.Second)
    assert(t, diffEvent["event"] == "screen_diff")
    assertBase64DecodesTo(t, diffEvent["data_base64"], "changes")

    // Step 8: surface.input
    writeJSON(conn, map[string]any{"id":"3","method":"surface.input","params":map[string]any{"surface_id":surfaceID,"text":"hello\n"}})
    inputResp := readJSON(conn)
    assert(t, inputResp["ok"] == true)

    // Step 9: Cleanup verification
    conn.Close(websocket.StatusNormalClosure, "done")
    // ... verify goroutines stopped (mock stops receiving polls)
}
```

---

## 3. PROTOCOL COMPLIANCE TEST

### TestProtocolCompliance

Verifies that Go struct JSON serialization exactly matches the TypeScript `protocol.ts` types.

#### 3.1 Go structs to define

```go
// Mirror of protocol.ts — used only in test
type tsRpcRequest struct {
    ID     *string         `json:"id,omitempty"`
    Method string          `json:"method"`
    Params map[string]any  `json:"params,omitempty"`
}

type tsRpcResponse struct {
    ID     *string      `json:"id,omitempty"`
    OK     bool         `json:"ok"`
    Result any          `json:"result,omitempty"`
    Error  *tsRpcError  `json:"error,omitempty"`
}

type tsRpcError struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}

type tsRpcEvent struct {
    Event      string `json:"event"`
    StreamID   string `json:"stream_id,omitempty"`
    DataBase64 string `json:"data_base64,omitempty"`
    Error      string `json:"error,omitempty"`
}

type tsSurface struct {
    ID      string `json:"id"`
    Ref     string `json:"ref"`
    Index   int    `json:"index"`
    Type    string `json:"type"`
    Title   string `json:"title"`
    Focused bool   `json:"focused"`
    PaneID  string `json:"pane_id"`
    PaneRef string `json:"pane_ref"`
}

type tsPane struct {
    ID       string      `json:"id"`
    Ref      string      `json:"ref"`
    Index    int         `json:"index"`
    Focused  bool        `json:"focused"`
    Surfaces []tsSurface `json:"surfaces"`
}

type tsWorkspace struct {
    ID       string   `json:"id"`
    Ref      string   `json:"ref"`
    Index    int      `json:"index"`
    Title    string   `json:"title"`
    Selected bool     `json:"selected"`
    Panes    []tsPane `json:"panes"`
}

type tsTreeResult struct {
    Active  *struct {
        SurfaceID   string `json:"surface_id"`
        WorkspaceID string `json:"workspace_id"`
    } `json:"active"`
    Windows []struct {
        ID         string        `json:"id"`
        Ref        string        `json:"ref"`
        Workspaces []tsWorkspace `json:"workspaces"`
    } `json:"windows"`
}
```

#### 3.2 JSON field name tests

| Test | What | Assert |
|------|------|--------|
| `TestProtocol_RpcRequest_Fields` | Marshal `rpcRequest{ID:"1",Method:"ping",Params:map...}` | JSON has keys exactly: `id`, `method`, `params` |
| `TestProtocol_RpcResponse_Fields` | Marshal `rpcResponse{ID:"1",OK:true,Result:...}` | JSON has: `id`, `ok`, `result` — NOT `Id`, `OK`, `Result` |
| `TestProtocol_RpcResponse_OmitEmpty` | Marshal `rpcResponse{OK:true}` with nil ID and nil Error | JSON does NOT contain `id` or `error` keys |
| `TestProtocol_RpcEvent_Fields` | Marshal `rpcEvent{Event:"screen_init",StreamID:"uuid",DataBase64:"..."}` | JSON has: `event`, `stream_id`, `data_base64` |
| `TestProtocol_RpcEvent_OmitEmpty` | Marshal `rpcEvent{Event:"topology"}` with empty StreamID | JSON does NOT contain `stream_id` |
| `TestProtocol_TreeResult_Shape` | Marshal full TreeResult through Go structs | JSON has: `active`, `windows[].id`, `windows[].ref`, `windows[].workspaces[].id`, etc. — all snake_case |
| `TestProtocol_Surface_AllFields` | Marshal Surface struct | JSON has exactly: `id`, `ref`, `index`, `type`, `title`, `focused`, `pane_id`, `pane_ref` |

#### 3.3 Base64 encoding verification

| Test | What | Assert |
|------|------|--------|
| `TestProtocol_ScreenInitData_Base64` | Create screen_init event data | `data_base64` field is valid base64; decodes to JSON with `"lines"` array |
| `TestProtocol_ScreenDiffData_Base64` | Create screen_diff event data | `data_base64` decodes to JSON with `"changes"` array containing `{op, line?, text?, count?}` |
| `TestProtocol_TopologyData_Base64` | Create topology event data | `data_base64` decodes to JSON with `"workspaces"` array |
| `TestProtocol_Base64_NotRawJSON` | Verify data_base64 is NOT raw JSON string | `data_base64` must NOT start with `{` — it must start with base64 alphabet chars |

#### 3.4 Client-matching assertions

| Test | What | Assert |
|------|------|--------|
| `TestProtocol_IsEvent_Discriminator` | Event has `event` key, no `ok` key | Matches `isEvent()` check: `'event' in msg` |
| `TestProtocol_IsResponse_Discriminator` | Response has `ok` key, no `event` key | Matches `isResponse()` check: `'ok' in msg` |
| `TestProtocol_AuthResponse_NoID` | Auth success response | `!msg.id` check passes (id field absent) |
| `TestProtocol_NormalResponse_HasID` | Non-auth response (e.g. system.tree) | `msg.id` is present and matches request id |

---

## 4. CODE REVIEW CHECKLIST

Specific items to verify during code review of the 5 new files:

### 4.1 Concurrency Safety

- [ ] **`wsClient.mu` guards `wsClient.watchers`**: Every read/write to the `watchers` map is under `mu.Lock()`
- [ ] **`topologyWatcher.mu` guards `clients` and `lastJSON`**: `addClient`, `removeClient`, `broadcast`, and the `lastJSON` comparison in `run()` all hold the appropriate lock
- [ ] **`topologyWatcher` uses `RWMutex` correctly**: `broadcast` takes `RLock` (read), `addClient`/`removeClient` take `Lock` (write), `run` comparison takes `Lock` (write, since it mutates `lastJSON`)
- [ ] **No data race on `wsClient.conn`**: `Conn.Write()` is goroutine-safe per coder/websocket docs. `Conn.Read()` is called only from `readLoop` (single reader).
- [ ] **Run all tests with `-race` flag**: `go test -race ./...` must pass

### 4.2 Goroutine Lifecycle

- [ ] **Context cancellation cascades**: Client context (`wsClient.ctx`) is parent of all watcher contexts. `cancel()` on client ctx cancels all watchers.
- [ ] **`watchSurface` exits on ctx.Done()**: The poll loop `select` includes `case <-ctx.Done(): return`
- [ ] **`topologyWatcher.run` exits on ctx.Done()**: Poll loop respects context
- [ ] **`handleSubscribe` cancels old watcher**: If re-subscribing to same surface, existing watcher's cancel func is called before starting new one
- [ ] **`cleanup()` cancels all watchers**: Iterates `wsClient.watchers`, calls each cancel func, clears the map
- [ ] **No goroutine leak on disconnect**: After client disconnect, all goroutines (watchers + topology membership) are cleaned up. Verifiable with `runtime.NumGoroutine()` before/after in integration test.

### 4.3 Error Handling

- [ ] **cmux.sock unreachable**: `cmuxCall` returns a clear error that gets translated to `{code:"cmux_unavailable"}` in the RPC response
- [ ] **cmux.sock timeout (5s deadline)**: `cmuxCall` sets `conn.SetDeadline(time.Now().Add(5*time.Second))`; timeout produces `{code:"cmux_timeout"}`
- [ ] **Docker not running**: `gtBridge.runCommand` returns a clear error; `gt.command` response includes the error message
- [ ] **Surface not found in cmux**: `cmuxReadScreen` returns error → watcher pushes error event and stops
- [ ] **10-consecutive-error escalation**: Counter resets on success; exactly at 10 errors, watcher pushes error event and returns

### 4.4 SIGPIPE Prevention (doc 29 CF-2)

- [ ] **`cmuxCall()` always reads full response before Close()**: The function does `defer conn.Close()` and reads the full response body in the function body before returning. Never abandons a connection mid-read.
- [ ] **`io.ReadAll` or equivalent**: Response is fully consumed, not just the first line

### 4.5 Auth Response Shape

- [ ] **No `id` field on auth success**: `rpcResponse{OK: true, Result: ...}` — ID field left as nil/zero. With `json:"id,omitempty"`, nil `any` value is omitted.
- [ ] **Test with `json.RawMessage`**: Marshal the actual auth response, parse as `map[string]json.RawMessage`, verify `"id"` key is absent
- [ ] **Client check matches**: RN client checks `isResponse(msg) && !msg.id && msg.ok && result.authenticated` — all conditions must be satisfiable

### 4.6 Base64 Encoding

- [ ] **All `data_base64` fields are base64-encoded**: `encodeEventData()` does `json.Marshal` then `base64.StdEncoding.EncodeToString`
- [ ] **NOT raw JSON in data_base64**: A common mistake would be putting `{"lines":[...]}` directly — must be `eyJsaW5lcy...`
- [ ] **Client decode path matches**: `atob(msg.data_base64)` → `JSON.parse()` — Go must produce output that survives this pipeline

### 4.7 Method Name Translation

- [ ] **`surface.input` → `surface.send_text`**: In `handleProxiedRequest`, method `"surface.input"` calls `cmuxSendText()` which sends `"surface.send_text"` to cmux.sock
- [ ] **`surface.keys` → `surface.send_key`**: Similarly, `"surface.keys"` maps to `"surface.send_key"`
- [ ] **`system.tree` passthrough**: Sent as `"system.tree"` to cmux.sock (no translation needed)

### 4.8 WebSocket Transport Safety

- [ ] **`Conn.Write()` goroutine safety**: coder/websocket v1.8.14 confirms `Write` is safe for concurrent use. `sendResponse` and `sendEvent` can be called from different goroutines.
- [ ] **`Conn.Read()` single reader**: Only `readLoop()` calls `conn.Read()`. No other goroutine reads.
- [ ] **Context for WS lifetime**: `websocket.Accept` uses `context.Background()` for the connection, NOT `r.Context()` (which dies when the HTTP handler returns)
- [ ] **Read limit set**: `conn.SetReadLimit(65536)` — prevents client from sending oversized frames

### 4.9 Context Lifecycle

- [ ] **Per-client context**: Each `wsClient` has its own `ctx, cancel = context.WithCancel(serverCtx)`
- [ ] **Child contexts for watchers**: Each watcher uses `context.WithCancel(clientCtx)`
- [ ] **Server context**: `runWSServer` creates a server-level context; cancelling it tears down everything
- [ ] **No context.TODO() or context.Background() leaks**: Every long-lived goroutine has a properly parented context

### 4.10 Connection Handling

- [ ] **`InsecureSkipVerify: true`**: Appropriate for Tailscale (encrypted transport layer)
- [ ] **`CompressionMode: CompressionContextTakeover`**: Good for repeated JSON payloads
- [ ] **`/health` endpoint returns 200**: For container/load-balancer health checks
- [ ] **Graceful shutdown on SIGINT/SIGTERM**: Server stops accepting, existing connections drain

---

## 5. TEST EXECUTION STRATEGY

### Running the tests

```bash
# Unit tests (fast, no external deps)
go test -v -run "TestFindCmuxSocket|TestCmuxCall|TestSplitScreenLines|TestEncodeEventData|TestTopologyWatcher|TestGTBridge|TestWSAuth|TestWSDispatch|TestProtocol" ./daemon/remote/cmd/cmuxd-remote/

# Race detector (critical for concurrency tests)
go test -race -v ./daemon/remote/cmd/cmuxd-remote/

# Integration test only
go test -v -run TestWSEndToEnd ./daemon/remote/cmd/cmuxd-remote/

# Protocol compliance only
go test -v -run TestProtocol ./daemon/remote/cmd/cmuxd-remote/

# All tests
go test -v -race -count=1 ./daemon/remote/cmd/cmuxd-remote/
```

### Test helper reuse

The existing `cli_test.go` provides excellent mock infrastructure to reuse:
- `makeShortUnixSocketPath(t)` — temp Unix socket paths
- `startMockSocket(t, response)` — canned v1 Unix mock
- `startMockV2Socket(t)` — echo-back v2 Unix mock
- `startMockV2SocketWithRequestCapture(t)` — v2 mock with request recording

For WS tests, add:
- `startTestWSServer(t, cmuxSocket, token) string` — starts WS server on random port, returns `host:port`
- `wsConnect(t, addr, token) *websocket.Conn` — connect + authenticate helper
- `readJSONWithTimeout(conn, timeout) map[string]any` — read + parse with deadline

### Test file organization

| File | Tests |
|------|-------|
| `ws_cmux_proxy_test.go` | findCmuxSocket, cmuxCall, cmuxTopology, cmuxReadScreen, cmuxSendText, cmuxSendKey, handleProxiedRequest |
| `ws_stream_test.go` | watchSurface, splitScreenLines, encodeEventData, error escalation, context cancellation |
| `ws_topology_test.go` | dedup, change detection, addClient/removeClient, broadcast, concurrent safety |
| `ws_docker_test.go` | command whitelist, docker exec args, REST fallback, handleGTRequest |
| `ws_server_test.go` | auth flow, method routing, error handling, client cleanup, health endpoint |
| `ws_integration_test.go` | TestWSEndToEnd |
| `ws_protocol_test.go` | TestProtocolCompliance (all 3.x subtests) |
